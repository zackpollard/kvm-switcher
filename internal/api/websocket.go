package api

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/zackpollard/kvm-switcher/internal/ikvm"
	"github.com/zackpollard/kvm-switcher/internal/models"
	kvmoidc "github.com/zackpollard/kvm-switcher/internal/oidc"
	"github.com/zackpollard/kvm-switcher/internal/tlsutil"
	vncbridge "github.com/zackpollard/kvm-switcher/internal/vnc"
)

// wssClient tracks a single WebSocket viewer connected to a WSS proxy.
type wssClient struct {
	ws      *websocket.Conn
	writeMu sync.Mutex // serialise writes to this client
}

// wssProxy holds a shared backend WSS connection and fans out to multiple clients.
type wssProxy struct {
	backend  *websocket.Conn
	clients  map[*websocket.Conn]*wssClient
	mu       sync.Mutex
	done     chan struct{} // closed when broadcast loop exits
}

// wsUpgrader returns a WebSocket upgrader that respects the configured CORS origins.
func (s *Server) wsUpgrader() *websocket.Upgrader {
	origins := s.Config.Settings.CORSOrigins
	return &websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin: func(r *http.Request) bool {
			for _, o := range origins {
				if o == "*" {
					return true
				}
			}
			origin := r.Header.Get("Origin")
			for _, o := range origins {
				if o == origin {
					return true
				}
			}
			return false
		},
		Subprotocols: []string{"binary"},
	}
}

// HandleKVMWebSocket godoc
// @Summary KVM WebSocket proxy
// @Description Upgrades to a WebSocket connection and proxies VNC/iKVM traffic between the browser and the BMC. Uses the "binary" WebSocket subprotocol. For iKVM sessions, the bridge authenticates with the BMC and establishes an IVTP tunnel. For WebSocket sessions, proxies to the remote WSS endpoint. For VNC sessions, proxies to the raw TCP VNC port.
// @Tags websocket
// @Param id path string true "Session ID"
// @Success 101 "WebSocket upgrade successful (binary subprotocol)"
// @Failure 404 {object} models.ErrorResponse "Session not found or not connected"
// @Router /ws/kvm/{id} [get]
func (s *Server) HandleKVMWebSocket(w http.ResponseWriter, r *http.Request) {
	log.Printf("KVM WS request URL: %s (query: %s)", r.URL.Path, r.URL.RawQuery)
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

	// Audit log: connection established
	userEmail := ""
	if user := kvmoidc.UserFromContext(r.Context()); user != nil {
		userEmail = user.Email
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	s.logAudit("kvm_connect", userEmail, session.ServerName, sessionID, ip, map[string]string{"conn_mode": string(session.ConnMode)})

	switch session.ConnMode {
	case models.KVMModeVNC:
		s.proxyVNC(w, r, session)
	case models.KVMModeWebSocket:
		s.proxyWSS(w, r, session)
	case models.KVMModeIKVM:
		s.proxyIKVM(w, r, session)
	default:
		http.Error(w, "unsupported KVM mode", http.StatusBadRequest)
	}

	// Audit log: connection ended
	s.logAudit("kvm_disconnect", userEmail, session.ServerName, sessionID, ip, nil)
}

// proxyWSS proxies browser WebSocket to a remote WSS endpoint (iDRAC9 HTML5 KVM).
// Multiple clients share a single backend WSS connection to the BMC. The first
// client triggers the backend connection; subsequent clients reuse it. Input is
// gated so only the controlling viewer's messages reach the BMC.
func (s *Server) proxyWSS(w http.ResponseWriter, r *http.Request, session *models.KVMSession) {
	log.Printf("Session %s: proxying WebSocket to WSS %s", session.ID, session.KVMTarget)

	// Register viewer for tracking
	viewerID := r.URL.Query().Get("viewer_id")
	if viewerID == "" {
		viewerID = uuid.New().String()
	}
	displayName, _, viewerIP := s.resolveViewerIdentity(r)
	registry := s.ensureViewerRegistry(session.ID)
	registry.Add(viewerID, displayName, viewerIP)
	defer registry.Remove(viewerID)

	clientConn, err := s.wsUpgrader().Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Session %s: WebSocket upgrade failed: %v", session.ID, err)
		return
	}
	defer clientConn.Close()

	// Ensure the shared backend WSS connection exists
	proxy, err := s.ensureWSSProxy(session)
	if err != nil {
		log.Printf("Session %s: WSS proxy backend failed: %v", session.ID, err)
		return
	}

	// Register this client for broadcast
	client := &wssClient{ws: clientConn}
	proxy.mu.Lock()
	proxy.clients[clientConn] = client
	clientCount := len(proxy.clients)
	proxy.mu.Unlock()

	defer func() {
		proxy.mu.Lock()
		delete(proxy.clients, clientConn)
		remaining := len(proxy.clients)
		proxy.mu.Unlock()
		log.Printf("Session %s: WSS viewer %s disconnected [%d remaining]", session.ID, viewerID, remaining)

		// If no clients left, tear down the backend connection
		if remaining == 0 {
			s.StopWSSProxy(session.ID)
		}
	}()

	log.Printf("Session %s: WSS viewer %s (%s) connected [%d total]", session.ID, viewerID, displayName, clientCount)

	// Read from this client (client -> BMC) with input gating.
	// The BMC -> client direction is handled by the broadcast goroutine.
	inputAllowed := func() bool {
		return registry.HasControl(viewerID)
	}

	for {
		select {
		case <-proxy.done:
			return // backend closed
		default:
		}

		msgType, data, err := clientConn.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("Session %s: WSS client read error: %v", session.ID, err)
			}
			return
		}

		// Only forward input from the controlling viewer
		if !inputAllowed() {
			continue
		}

		proxy.mu.Lock()
		backend := proxy.backend
		proxy.mu.Unlock()
		if backend == nil {
			return
		}

		if err := backend.WriteMessage(msgType, data); err != nil {
			log.Printf("Session %s: WSS backend write error: %v", session.ID, err)
			return
		}
	}
}

// ensureWSSProxy returns the shared WSS proxy for the session, creating one if needed.
func (s *Server) ensureWSSProxy(session *models.KVMSession) (*wssProxy, error) {
	s.wssProxiesMu.Lock()
	proxy := s.WSSProxies[session.ID]
	if proxy != nil {
		// Check if the backend is still alive
		select {
		case <-proxy.done:
			// Backend died, need a new one
			delete(s.WSSProxies, session.ID)
			proxy = nil
		default:
			s.wssProxiesMu.Unlock()
			return proxy, nil
		}
	}
	s.wssProxiesMu.Unlock()

	// Build auth headers for the iDRAC9 WSS endpoint
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

	// Look up server config for TLS settings
	skipVerify := true
	for i := range s.Config.Servers {
		if s.Config.Servers[i].Name == session.ServerName {
			skipVerify = tlsutil.SkipVerify(&s.Config.Servers[i])
			break
		}
	}

	dialer := websocket.Dialer{
		Subprotocols:    []string{"binary"},
		TLSClientConfig: tlsutil.ConfigForServer(skipVerify),
	}

	backendConn, _, err := dialer.Dial(session.KVMTarget, headers)
	if err != nil {
		return nil, fmt.Errorf("WSS backend dial: %w", err)
	}

	proxy = &wssProxy{
		backend: backendConn,
		clients: make(map[*websocket.Conn]*wssClient),
		done:    make(chan struct{}),
	}

	// Start the broadcast goroutine (backend -> all clients)
	go s.wssBroadcastLoop(session.ID, proxy)

	s.wssProxiesMu.Lock()
	s.WSSProxies[session.ID] = proxy
	s.wssProxiesMu.Unlock()

	log.Printf("Session %s: WSS backend connection established", session.ID)
	return proxy, nil
}

// wssBroadcastLoop reads from the backend WSS connection and fans out to all clients.
func (s *Server) wssBroadcastLoop(sessionID string, proxy *wssProxy) {
	defer close(proxy.done)

	for {
		msgType, data, err := proxy.backend.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("Session %s: WSS backend read error: %v", sessionID, err)
			}
			return
		}

		proxy.mu.Lock()
		for ws, client := range proxy.clients {
			client.writeMu.Lock()
			// Short write deadline to avoid one slow client blocking others
			ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := ws.WriteMessage(msgType, data); err != nil {
				log.Printf("Session %s: WSS broadcast write failed, removing client: %v", sessionID, err)
				ws.Close()
				delete(proxy.clients, ws)
			}
			ws.SetWriteDeadline(time.Time{})
			client.writeMu.Unlock()
		}
		proxy.mu.Unlock()
	}
}

// StopWSSProxy stops and removes the shared WSS proxy for a session.
func (s *Server) StopWSSProxy(sessionID string) {
	s.wssProxiesMu.Lock()
	proxy := s.WSSProxies[sessionID]
	delete(s.WSSProxies, sessionID)
	s.wssProxiesMu.Unlock()

	if proxy == nil {
		return
	}

	// Close the backend connection; this will cause the broadcast loop to exit
	proxy.mu.Lock()
	if proxy.backend != nil {
		proxy.backend.Close()
	}
	// Close all client connections
	for ws := range proxy.clients {
		ws.Close()
	}
	proxy.clients = make(map[*websocket.Conn]*wssClient)
	proxy.mu.Unlock()

	log.Printf("Session %s: WSS proxy stopped", sessionID)
}

// proxyVNC bridges a browser WebSocket to a raw TCP VNC connection.
// Multiple clients share a single persistent TCP connection to the BMC via the
// VNC bridge's fan-out. Input is gated so only the controlling viewer can send
// keyboard/mouse events.
func (s *Server) proxyVNC(w http.ResponseWriter, r *http.Request, session *models.KVMSession) {
	log.Printf("Session %s: VNC connect (bridge running=%v)", session.ID, s.vncBridgeRunning(session.ID))

	// Register viewer for tracking
	viewerID := r.URL.Query().Get("viewer_id")
	if viewerID == "" {
		viewerID = uuid.New().String()
	}
	displayName, _, viewerIP := s.resolveViewerIdentity(r)
	registry := s.ensureViewerRegistry(session.ID)
	registry.Add(viewerID, displayName, viewerIP)
	defer registry.Remove(viewerID)

	clientConn, err := s.wsUpgrader().Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Session %s: WebSocket upgrade failed: %v", session.ID, err)
		return
	}
	defer clientConn.Close()

	bridge, err := s.ensureVNCBridge(session)
	if err != nil {
		log.Printf("Session %s: VNC bridge failed: %v", session.ID, err)
		return
	}

	log.Printf("Session %s: VNC viewer %s (%s) connected [%d total]", session.ID, viewerID, displayName, registry.Count())

	if err := bridge.ServeWebSocketWithControl(clientConn, func() bool {
		return registry.HasControl(viewerID)
	}); err != nil {
		log.Printf("Session %s: VNC error: %v", session.ID, err)
	}
	log.Printf("Session %s: VNC viewer %s (%s) disconnected [%d remaining]", session.ID, viewerID, displayName, registry.Count())
}

func (s *Server) ensureVNCBridge(session *models.KVMSession) (*vncbridge.Bridge, error) {
	s.vncConnsMu.Lock()
	bridge := s.VNCBridges[session.ID]
	if bridge != nil && bridge.Running() {
		s.vncConnsMu.Unlock()
		return bridge, nil
	}
	s.vncConnsMu.Unlock()

	bridge = vncbridge.NewBridge(session.KVMTarget, session.KVMPassword)

	// iDRAC8 needs special handling: Raw-only encodings (its VNC server
	// fails silently with Tight/ZRLE) and ServerInit name rewrite to
	// trigger noVNC's 8bpp mode. iDRAC9 handles all encodings fine and
	// should use its native pixel format for best performance.
	for _, srv := range s.Config.Servers {
		if srv.Name == session.ServerName && srv.BoardType == "dell_idrac8" {
			bridge.FilterEncodings = true
			bridge.RewriteName = true
			break
		}
	}

	if err := bridge.Start(); err != nil {
		return nil, err
	}

	s.vncConnsMu.Lock()
	s.VNCBridges[session.ID] = bridge
	s.vncConnsMu.Unlock()
	return bridge, nil
}

func (s *Server) StopVNCBridge(sessionID string) {
	s.vncConnsMu.Lock()
	bridge := s.VNCBridges[sessionID]
	delete(s.VNCBridges, sessionID)
	s.vncConnsMu.Unlock()
	if bridge != nil { bridge.Stop() }
}

func (s *Server) vncBridgeRunning(sessionID string) bool {
	s.vncConnsMu.Lock()
	defer s.vncConnsMu.Unlock()
	b := s.VNCBridges[sessionID]
	return b != nil && b.Running()
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

// proxyIKVM bridges a browser WebSocket to a BMC using the native IVTP protocol.
// The Go process speaks the BMC's IVTP protocol directly and translates it to
// VNC/RFB for noVNC.
//
// The bridge runs independently of WebSocket clients. On the first WebSocket
// connection it creates and starts the bridge (BMC connection, IVTP read loop,
// periodic refresh). Subsequent reconnections reuse the running bridge. The
// bridge is only torn down when the session is explicitly destroyed.
func (s *Server) proxyIKVM(w http.ResponseWriter, r *http.Request, session *models.KVMSession) {
	log.Printf("Session %s: iKVM WebSocket connect (bridge running=%v)", session.ID, s.ikvmBridgeRunning(session.ID))

	// Upgrade to WebSocket FIRST -- before any slow BMC auth -- so the browser
	// has an established connection and doesn't time out / trigger reconnect.
	clientConn, err := s.wsUpgrader().Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Session %s: WebSocket upgrade failed: %v", session.ID, err)
		return
	}
	defer clientConn.Close()

	// Register this viewer
	viewerID := r.URL.Query().Get("viewer_id")
	if viewerID == "" {
		viewerID = uuid.New().String()
	}
	displayName, _, viewerIP := s.resolveViewerIdentity(r)
	registry := s.ensureViewerRegistry(session.ID)
	registry.Add(viewerID, displayName, viewerIP)
	defer registry.Remove(viewerID)
	log.Printf("Session %s: viewer %s (%s) connected [%d total]", session.ID, viewerID, displayName, registry.Count())

	// Ensure the background bridge is running (creates + starts it on first call).
	bridge, err := s.ensureIKVMBridge(session)
	if err != nil {
		log.Printf("Session %s: iKVM bridge start failed: %v", session.ID, err)
		return
	}

	// Serve this WebSocket client with input gating based on viewer control.
	// Blocks until the client disconnects or the bridge shuts down.
	if err := bridge.ServeWebSocketWithControl(clientConn, func() bool {
		return registry.HasControl(viewerID)
	}); err != nil {
		log.Printf("Session %s: iKVM WebSocket error: %v", session.ID, err)
	}
	log.Printf("Session %s: viewer %s (%s) disconnected [%d remaining]", session.ID, viewerID, displayName, registry.Count())
}

// ensureIKVMBridge returns the running bridge for the session, creating and
// starting one if it doesn't exist yet or has stopped.
func (s *Server) ensureIKVMBridge(session *models.KVMSession) (*ikvm.Bridge, error) {
	s.bridgesMu.Lock()
	bridge := s.Bridges[session.ID]
	if bridge != nil && bridge.Running() {
		s.bridgesMu.Unlock()
		return bridge, nil
	}
	// Need to create a new bridge -- release the lock during the (potentially slow)
	// BMC connect, but mark that we're working on it.
	s.bridgesMu.Unlock()

	// Use the tokens stored at session creation time -- no re-authentication.
	args := session.IKVMArgs
	if args == nil {
		return nil, fmt.Errorf("no iKVM args (session may have been created before tokens were stored)")
	}

	// Look up the server config to find the proxy entry
	var serverCfg *models.ServerConfig
	for i := range s.Config.Servers {
		if s.Config.Servers[i].Name == session.ServerName {
			serverCfg = &s.Config.Servers[i]
			break
		}
	}

	// Mark the BMC session as KVM-active so the session manager won't logout/renew it.
	if serverCfg != nil {
		entry := getOrCreateProxy(serverCfg, serverCfg.Name)
		entry.mu.Lock()
		entry.kvmActive = true
		entry.mu.Unlock()
	}

	webSecPort := 443
	if args.WebSecurePort != "" {
		fmt.Sscanf(args.WebSecurePort, "%d", &webSecPort)
	}
	kvmPort := 80
	if args.KVMPort != "" {
		fmt.Sscanf(args.KVMPort, "%d", &kvmPort)
	}

	bridge = ikvm.NewBridge(ikvm.ClientConfig{
		Host:          args.Hostname,
		Port:          kvmPort,
		WebSecurePort: webSecPort,
		WebCookie:     args.WebCookie,
		KVMToken:      args.KVMToken,
		UseSSL:        args.KVMSecure == "1",
	})

	// Start the background bridge (connects to BMC, starts read loop).
	if err := bridge.Start(context.Background()); err != nil {
		// Clear KVM-active on failure
		if serverCfg != nil {
			entry := getOrCreateProxy(serverCfg, serverCfg.Name)
			entry.mu.Lock()
			entry.kvmActive = false
			entry.mu.Unlock()
		}
		return nil, fmt.Errorf("IVTP start: %w", err)
	}

	// Register bridge so API endpoints (power, screenshot, etc.) can use it.
	s.bridgesMu.Lock()
	s.Bridges[session.ID] = bridge
	s.bridgesMu.Unlock()

	log.Printf("Session %s: iKVM background bridge started", session.ID)
	return bridge, nil
}

// StopIKVMBridge stops and removes the iKVM bridge for a session.
// Called when the session is destroyed.
func (s *Server) StopIKVMBridge(sessionID string) {
	s.bridgesMu.Lock()
	bridge := s.Bridges[sessionID]
	delete(s.Bridges, sessionID)
	s.bridgesMu.Unlock()

	if bridge == nil {
		return
	}

	bridge.Stop()

	// Clear KVM-active flag so the session manager can resume normal operation.
	if session, ok := s.Sessions.Get(sessionID); ok {
		for i := range s.Config.Servers {
			if s.Config.Servers[i].Name == session.ServerName {
				entry := getOrCreateProxy(&s.Config.Servers[i], s.Config.Servers[i].Name)
				entry.mu.Lock()
				entry.kvmActive = false
				entry.mu.Unlock()
				break
			}
		}
	}
	log.Printf("Session %s: iKVM bridge stopped", sessionID)
}

// ikvmBridgeRunning returns whether a bridge is running for the given session.
func (s *Server) ikvmBridgeRunning(sessionID string) bool {
	s.bridgesMu.Lock()
	bridge := s.Bridges[sessionID]
	s.bridgesMu.Unlock()
	return bridge != nil && bridge.Running()
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
