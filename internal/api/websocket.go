package api

import (
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
	"github.com/zackpollard/kvm-switcher/internal/models"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins for now; tighten in production
	},
	Subprotocols: []string{"binary"},
}

// HandleKVMWebSocket proxies WebSocket connections between the browser (noVNC)
// and the KVM backend (container websockify, remote WSS, or raw VNC).
func (s *Server) HandleKVMWebSocket(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")

	session, ok := s.Sessions.Get(sessionID)
	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	if session.Status != models.SessionConnected {
		http.Error(w, "session not connected", http.StatusBadGateway)
		return
	}

	// Update activity timestamp
	session.LastActivity = time.Now()
	s.Sessions.Set(session)

	switch session.ConnMode {
	case models.KVMModeVNC:
		s.proxyVNC(w, r, session)
	case models.KVMModeWebSocket:
		s.proxyWSS(w, r, session)
	default:
		// Container mode (original flow)
		s.proxyContainer(w, r, session)
	}
}

// proxyContainer proxies to a local container's websockify (original AMI MegaRAC flow).
func (s *Server) proxyContainer(w http.ResponseWriter, r *http.Request, session *models.KVMSession) {
	containerURL := url.URL{
		Scheme: "ws",
		Host:   net.JoinHostPort("127.0.0.1", itoa(session.WebSocketPort)),
		Path:   "/websockify",
	}

	log.Printf("Session %s: proxying WebSocket to container %s", session.ID, containerURL.String())

	dialer := websocket.Dialer{
		Subprotocols: []string{"binary"},
	}
	var backendConn *websocket.Conn
	var err error
	for i := 0; i < 3; i++ {
		backendConn, _, err = dialer.Dial(containerURL.String(), nil)
		if err == nil {
			break
		}
		log.Printf("Session %s: waiting for container websockify (attempt %d)...", session.ID, i+1)
		time.Sleep(time.Second)
	}
	if err != nil {
		log.Printf("Session %s: failed to connect to container websockify: %v", session.ID, err)
		http.Error(w, "failed to connect to KVM container", http.StatusBadGateway)
		return
	}
	defer backendConn.Close()

	clientConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Session %s: WebSocket upgrade failed: %v", session.ID, err)
		return
	}
	defer clientConn.Close()

	log.Printf("Session %s: WebSocket proxy established (container)", session.ID)
	bidirectionalWSProxy(session.ID, clientConn, backendConn)
}

// proxyWSS proxies browser WebSocket to a remote WSS endpoint (iDRAC9 HTML5 KVM).
func (s *Server) proxyWSS(w http.ResponseWriter, r *http.Request, session *models.KVMSession) {
	log.Printf("Session %s: proxying WebSocket to WSS %s", session.ID, session.KVMTarget)

	// The iDRAC9 WSS endpoint needs the session cookie + XSRF token for auth.
	s.bmcCredsMu.Lock()
	credEntry := s.BMCCreds[session.ID]
	s.bmcCredsMu.Unlock()

	headers := http.Header{}
	if credEntry != nil && credEntry.Creds != nil {
		if credEntry.Creds.SessionCookie != "" {
			cookie := (&http.Cookie{Name: "-http-session-", Value: credEntry.Creds.SessionCookie}).String()
			headers.Set("Cookie", cookie)
		}
		if credEntry.Creds.CSRFToken != "" {
			headers.Set("XSRF-TOKEN", credEntry.Creds.CSRFToken)
		}
	}

	dialer := websocket.Dialer{
		Subprotocols:  []string{"binary"},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}

	backendConn, _, err := dialer.Dial(session.KVMTarget, headers)
	if err != nil {
		log.Printf("Session %s: failed to connect to WSS backend: %v", session.ID, err)
		http.Error(w, "failed to connect to KVM", http.StatusBadGateway)
		return
	}
	defer backendConn.Close()

	clientConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Session %s: WebSocket upgrade failed: %v", session.ID, err)
		return
	}
	defer clientConn.Close()

	log.Printf("Session %s: WebSocket proxy established (WSS)", session.ID)
	bidirectionalWSProxy(session.ID, clientConn, backendConn)
}

// proxyVNC bridges a browser WebSocket to a raw TCP VNC connection.
// It handles the VNC handshake with exact message parsing (rewriting ServerInit
// to trigger noVNC's 8bpp mode), then switches to raw bidirectional proxying
// with SetEncodings filtering for iDRAC8 compatibility.
func (s *Server) proxyVNC(w http.ResponseWriter, r *http.Request, session *models.KVMSession) {
	log.Printf("Session %s: proxying WebSocket to VNC %s", session.ID, session.KVMTarget)

	// Connect to VNC server via raw TCP
	tcpConn, err := net.DialTimeout("tcp", session.KVMTarget, 10*time.Second)
	if err != nil {
		log.Printf("Session %s: failed to connect to VNC %s: %v", session.ID, session.KVMTarget, err)
		http.Error(w, "failed to connect to VNC", http.StatusBadGateway)
		return
	}
	defer tcpConn.Close()

	// Upgrade browser connection to WebSocket
	clientConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Session %s: WebSocket upgrade failed: %v", session.ID, err)
		return
	}
	defer clientConn.Close()

	log.Printf("Session %s: WebSocket↔VNC bridge established", session.ID)

	// Phase 1: Handle VNC handshake with exact message parsing.
	// Uses io.ReadFull for precise byte reads instead of counting TCP reads
	// (which don't align to VNC message boundaries).
	if err := vncHandshake(session.ID, clientConn, tcpConn); err != nil {
		log.Printf("Session %s: VNC handshake failed: %v", session.ID, err)
		return
	}

	// Phase 2: Post-handshake bidirectional proxy.
	errCh := make(chan error, 2)

	// Browser WS → VNC TCP (with SetEncodings rewrite for iDRAC8)
	go func() {
		for {
			_, data, err := clientConn.ReadMessage()
			if err != nil {
				errCh <- err
				return
			}
			data = rewriteVNCClientMessage(session.ID, data)
			if data == nil {
				continue
			}
			if _, err := tcpConn.Write(data); err != nil {
				errCh <- err
				return
			}
		}
	}()

	// VNC TCP → Browser WS (pass-through after handshake)
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := tcpConn.Read(buf)
			if err != nil {
				errCh <- err
				return
			}
			if err := clientConn.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
				errCh <- err
				return
			}
		}
	}()

	err = <-errCh
	if err != nil && !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
		log.Printf("Session %s: VNC proxy error: %v", session.ID, err)
	}
	log.Printf("Session %s: VNC proxy closed", session.ID)
}

// vncHandshake performs the VNC RFB handshake between the browser (via WebSocket)
// and the VNC server (via TCP), using exact byte reads for each handshake message.
// It rewrites the ServerInit desktop name to trigger noVNC's 8bpp pixel format,
// which is required for iDRAC8 compatibility (iDRAC8 crashes on 32bpp).
func vncHandshake(sessionID string, clientConn *websocket.Conn, tcpConn net.Conn) error {
	// iDRAC8 can be slow (5-12s between messages), so give plenty of time
	tcpConn.SetDeadline(time.Now().Add(30 * time.Second))
	defer tcpConn.SetDeadline(time.Time{})

	// 1. Server → Client: ProtocolVersion (12 bytes, e.g. "RFB 003.008\n")
	version := make([]byte, 12)
	if _, err := io.ReadFull(tcpConn, version); err != nil {
		return fmt.Errorf("reading server version: %w", err)
	}
	log.Printf("Session %s: VNC server version: %q", sessionID, string(version))
	if err := clientConn.WriteMessage(websocket.BinaryMessage, version); err != nil {
		return fmt.Errorf("forwarding server version: %w", err)
	}

	// 2. Client → Server: ProtocolVersion
	_, clientVersion, err := clientConn.ReadMessage()
	if err != nil {
		return fmt.Errorf("reading client version: %w", err)
	}
	if _, err := tcpConn.Write(clientVersion); err != nil {
		return fmt.Errorf("forwarding client version: %w", err)
	}

	// 3. Server → Client: Security types (1-byte count + N type bytes)
	numTypesBuf := make([]byte, 1)
	if _, err := io.ReadFull(tcpConn, numTypesBuf); err != nil {
		return fmt.Errorf("reading security type count: %w", err)
	}
	numTypes := int(numTypesBuf[0])
	if numTypes == 0 {
		// Server rejected connection — read and forward error reason
		reasonLenBuf := make([]byte, 4)
		if _, err := io.ReadFull(tcpConn, reasonLenBuf); err != nil {
			return fmt.Errorf("reading error reason length: %w", err)
		}
		reasonLen := binary.BigEndian.Uint32(reasonLenBuf)
		reason := make([]byte, reasonLen)
		if _, err := io.ReadFull(tcpConn, reason); err != nil {
			return fmt.Errorf("reading error reason: %w", err)
		}
		msg := append(numTypesBuf, reasonLenBuf...)
		msg = append(msg, reason...)
		clientConn.WriteMessage(websocket.BinaryMessage, msg)
		return fmt.Errorf("server rejected connection: %s", string(reason))
	}
	secTypes := make([]byte, numTypes)
	if _, err := io.ReadFull(tcpConn, secTypes); err != nil {
		return fmt.Errorf("reading security types: %w", err)
	}
	secMsg := make([]byte, 1+numTypes)
	secMsg[0] = numTypesBuf[0]
	copy(secMsg[1:], secTypes)
	if err := clientConn.WriteMessage(websocket.BinaryMessage, secMsg); err != nil {
		return fmt.Errorf("forwarding security types: %w", err)
	}

	// 4. Client → Server: Security type selection (1 byte)
	_, secChoiceMsg, err := clientConn.ReadMessage()
	if err != nil {
		return fmt.Errorf("reading security type selection: %w", err)
	}
	if _, err := tcpConn.Write(secChoiceMsg); err != nil {
		return fmt.Errorf("forwarding security type selection: %w", err)
	}
	secType := secChoiceMsg[0]
	log.Printf("Session %s: VNC security type: %d", sessionID, secType)

	// 5. Handle authentication based on selected type
	switch secType {
	case 1: // None — no auth exchange
	case 2: // VNC Authentication — 16-byte challenge/response
		challenge := make([]byte, 16)
		if _, err := io.ReadFull(tcpConn, challenge); err != nil {
			return fmt.Errorf("reading VNC auth challenge: %w", err)
		}
		if err := clientConn.WriteMessage(websocket.BinaryMessage, challenge); err != nil {
			return fmt.Errorf("forwarding VNC auth challenge: %w", err)
		}
		_, authResp, err := clientConn.ReadMessage()
		if err != nil {
			return fmt.Errorf("reading VNC auth response: %w", err)
		}
		if _, err := tcpConn.Write(authResp); err != nil {
			return fmt.Errorf("forwarding VNC auth response: %w", err)
		}
	default:
		return fmt.Errorf("unsupported VNC security type: %d", secType)
	}

	// 6. Server → Client: SecurityResult (4 bytes, 0 = OK)
	secResult := make([]byte, 4)
	if _, err := io.ReadFull(tcpConn, secResult); err != nil {
		return fmt.Errorf("reading security result: %w", err)
	}
	if err := clientConn.WriteMessage(websocket.BinaryMessage, secResult); err != nil {
		return fmt.Errorf("forwarding security result: %w", err)
	}
	if binary.BigEndian.Uint32(secResult) != 0 {
		return fmt.Errorf("VNC authentication failed")
	}
	log.Printf("Session %s: VNC authentication successful", sessionID)

	// 7. Client → Server: ClientInit (1 byte — shared-flag)
	_, clientInit, err := clientConn.ReadMessage()
	if err != nil {
		return fmt.Errorf("reading ClientInit: %w", err)
	}
	if _, err := tcpConn.Write(clientInit); err != nil {
		return fmt.Errorf("forwarding ClientInit: %w", err)
	}

	// 8. Server → Client: ServerInit
	// Format: width(2) + height(2) + pixel-format(16) + name-length(4) = 24 bytes, then name
	serverInitHdr := make([]byte, 24)
	if _, err := io.ReadFull(tcpConn, serverInitHdr); err != nil {
		return fmt.Errorf("reading ServerInit header: %w", err)
	}
	nameLen := binary.BigEndian.Uint32(serverInitHdr[20:24])
	if nameLen > 4096 {
		return fmt.Errorf("ServerInit name too large: %d bytes", nameLen)
	}
	nameBuf := make([]byte, nameLen)
	if nameLen > 0 {
		if _, err := io.ReadFull(tcpConn, nameBuf); err != nil {
			return fmt.Errorf("reading ServerInit name: %w", err)
		}
	}

	serverInit := append(serverInitHdr, nameBuf...)
	serverInit = rewriteVNCServerInit(sessionID, serverInit)
	if err := clientConn.WriteMessage(websocket.BinaryMessage, serverInit); err != nil {
		return fmt.Errorf("forwarding ServerInit: %w", err)
	}

	log.Printf("Session %s: VNC handshake complete", sessionID)
	return nil
}

// rewriteVNCClientMessage intercepts and rewrites VNC client-to-server messages
// to work around iDRAC8 VNC server limitations.
//
// iDRAC8 fails to send framebuffer data when it sees encoding types it doesn't
// support. We rewrite SetEncodings to only include Raw, CopyRect, and pseudo-
// encodings that iDRAC8 handles.
//
// SetPixelFormat is passed through unchanged — noVNC will request 8bpp (because
// we set the desktop name to trigger low-color mode) which iDRAC8 supports.
//
// Returns nil to drop the message entirely.
func rewriteVNCClientMessage(sessionID string, data []byte) []byte {
	if len(data) == 0 {
		return data
	}

	if data[0] == 2 && len(data) >= 4 { // SetEncodings
		// Replace with a minimal encoding list that iDRAC8 supports.
		encodings := []int32{
			0,    // Raw
			1,    // CopyRect
			-223, // DesktopSize
		}
		buf := make([]byte, 4+len(encodings)*4)
		buf[0] = 2 // SetEncodings type
		binary.BigEndian.PutUint16(buf[2:4], uint16(len(encodings)))
		for i, enc := range encodings {
			binary.BigEndian.PutUint32(buf[4+i*4:], uint32(enc))
		}
		log.Printf("Session %s: VNC proxy: rewrote SetEncodings to %d supported types", sessionID, len(encodings))
		return buf
	}

	return data
}

// rewriteVNCServerInit rewrites the ServerInit message's desktop name to
// "Intel(r) AMT KVM". This triggers noVNC's low-color mode (8bpp) which
// is compatible with iDRAC8's limited VNC server. iDRAC8 crashes on 32bpp
// SetPixelFormat but handles 8bpp correctly.
func rewriteVNCServerInit(sessionID string, data []byte) []byte {
	if len(data) < 24 {
		return data
	}
	nameLen := binary.BigEndian.Uint32(data[20:24])
	if uint32(len(data)) < 24+nameLen {
		return data
	}
	origName := string(data[24 : 24+nameLen])
	log.Printf("Session %s: VNC ServerInit desktop name: %q", sessionID, origName)

	// Replace with "Intel(r) AMT KVM" to trigger noVNC's 8-bit mode
	newName := []byte("Intel(r) AMT KVM")
	result := make([]byte, 24+len(newName))
	copy(result[:20], data[:20])                                      // pixel format
	binary.BigEndian.PutUint32(result[20:24], uint32(len(newName)))   // name length
	copy(result[24:], newName)                                        // name
	log.Printf("Session %s: VNC proxy: rewrote desktop name to trigger 8bpp mode", sessionID)
	return result
}

// bidirectionalWSProxy copies messages bidirectionally between two WebSocket connections.
func bidirectionalWSProxy(sessionID string, clientConn, backendConn *websocket.Conn) {
	errCh := make(chan error, 2)

	go func() {
		errCh <- proxyWebSocket(clientConn, backendConn)
	}()

	go func() {
		errCh <- proxyWebSocket(backendConn, clientConn)
	}()

	err := <-errCh
	if err != nil && !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
		log.Printf("Session %s: WebSocket proxy error: %v", sessionID, err)
	}
	log.Printf("Session %s: WebSocket proxy closed", sessionID)
}

// proxyWebSocket copies messages from src to dst.
func proxyWebSocket(src, dst *websocket.Conn) error {
	for {
		msgType, reader, err := src.NextReader()
		if err != nil {
			return err
		}

		writer, err := dst.NextWriter(msgType)
		if err != nil {
			return err
		}

		if _, err := io.Copy(writer, reader); err != nil {
			return err
		}

		if err := writer.Close(); err != nil {
			return err
		}
	}
}

func itoa(i int) string {
	return fmt.Sprintf("%d", i)
}
