// Package vnc provides a VNC bridge that keeps a single TCP connection
// to a BMC alive across WebSocket client reconnects.
package vnc

import (
	"crypto/des"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// wsClient tracks a single WebSocket viewer connected to the bridge.
type wsClient struct {
	ws      *websocket.Conn
	hasCtrl func() bool // nil means all input allowed
}

// Bridge holds a persistent VNC TCP connection to a BMC.
type Bridge struct {
	target          string
	password        string
	conn            net.Conn
	connWriteMu     sync.Mutex // serialise writes to b.conn from multiple clients
	serverInit      []byte     // saved for replay to new clients
	FilterEncodings bool       // true = rewrite SetEncodings to Raw only (iDRAC8)
	RewriteName     bool       // true = rewrite ServerInit name to Intel AMT (iDRAC8 8bpp)

	mu      sync.Mutex
	running bool

	// Multi-viewer state
	clients   map[*websocket.Conn]*wsClient
	clientsMu sync.Mutex
}

func NewBridge(target, password string) *Bridge {
	return &Bridge{
		target:  target,
		password: password,
		clients: make(map[*websocket.Conn]*wsClient),
	}
}

func (b *Bridge) Running() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.running
}

// Start connects to the BMC and performs the VNC handshake.
func (b *Bridge) Start() error {
	b.mu.Lock()
	if b.running {
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()

	conn, err := net.DialTimeout("tcp", b.target, 10*time.Second)
	if err != nil {
		return fmt.Errorf("dial %s: %w", b.target, err)
	}

	if err := b.bmcHandshake(conn); err != nil {
		conn.Close()
		return fmt.Errorf("handshake: %w", err)
	}

	b.conn = conn
	b.mu.Lock()
	b.running = true
	b.mu.Unlock()

	log.Printf("VNC bridge: connected to %s", b.target)
	return nil
}

func (b *Bridge) Stop() {
	b.mu.Lock()
	if !b.running {
		b.mu.Unlock()
		return
	}
	b.running = false
	b.mu.Unlock()

	if b.conn != nil {
		b.conn.Close()
	}

	// Close all client connections
	b.clientsMu.Lock()
	for ws := range b.clients {
		ws.Close()
	}
	b.clients = make(map[*websocket.Conn]*wsClient)
	b.clientsMu.Unlock()
}

func (b *Bridge) clientCount() int {
	b.clientsMu.Lock()
	defer b.clientsMu.Unlock()
	return len(b.clients)
}

// ServeWebSocket handles a noVNC client with all input forwarded.
func (b *Bridge) ServeWebSocket(ws *websocket.Conn) error {
	return b.ServeWebSocketWithControl(ws, nil)
}

// ServeWebSocketWithControl handles a noVNC WebSocket client with optional
// input gating. Blocks until the client disconnects or the bridge stops.
func (b *Bridge) ServeWebSocketWithControl(ws *websocket.Conn, inputAllowed func() bool) error {
	if !b.Running() {
		return fmt.Errorf("bridge not running")
	}

	// Check if BMC connection is still alive (only when no other clients
	// are using it — avoid interfering with active reads).
	if b.clientCount() == 0 {
		b.conn.SetReadDeadline(time.Now().Add(1 * time.Millisecond))
		one := make([]byte, 1)
		_, err := b.conn.Read(one)
		b.conn.SetReadDeadline(time.Time{})
		if err != nil && !isTimeout(err) {
			log.Printf("VNC bridge: TCP connection lost, reconnecting...")
			b.conn.Close()
			b.mu.Lock()
			b.running = false
			b.mu.Unlock()
			if err := b.Start(); err != nil {
				return fmt.Errorf("reconnect failed: %w", err)
			}
		}
	}

	// VNC handshake with client (using saved ServerInit)
	if err := b.clientHandshake(ws); err != nil {
		return fmt.Errorf("client handshake: %w", err)
	}

	// Register this client
	client := &wsClient{ws: ws, hasCtrl: inputAllowed}
	b.clientsMu.Lock()
	b.clients[ws] = client
	numClients := len(b.clients)
	b.clientsMu.Unlock()

	defer func() {
		b.clientsMu.Lock()
		delete(b.clients, ws)
		b.clientsMu.Unlock()
	}()

	log.Printf("VNC bridge: client connected [%d total]", numClients)

	var retErr error
	if numClients == 1 {
		// Single client: direct pipe for best performance (zero-copy)
		retErr = b.serveSingleClient(ws, inputAllowed)
	} else {
		// Multiple clients: just read input, broadcast is handled elsewhere
		retErr = b.readClientInput(ws, inputAllowed)
	}

	log.Printf("VNC bridge: client disconnected [%d remaining]", b.clientCount())
	return retErr
}

// serveSingleClient runs the original direct pipe between one WS client
// and the BMC. No allocations, no copies, minimal latency. If a second
// client connects, the pipe is broken and both clients switch to broadcast.
func (b *Bridge) serveSingleClient(ws *websocket.Conn, inputAllowed func() bool) error {
	errCh := make(chan error, 2)

	// BMC → client (direct pipe)
	go func() {
		buf := make([]byte, 262144) // 256KB
		for {
			n, err := b.conn.Read(buf)
			if err != nil {
				errCh <- fmt.Errorf("BMC read: %w", err)
				return
			}

			// Check if more clients joined — switch to broadcast
			if b.clientCount() > 1 {
				// Send this chunk to ALL clients, then switch to broadcast mode
				b.broadcastToAll(buf[:n])
				// Start broadcast loop for remaining data
				errCh <- b.runBroadcast()
				return
			}

			if err := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
				errCh <- fmt.Errorf("WS write: %w", err)
				return
			}
		}
	}()

	// Client → BMC
	go func() {
		errCh <- b.readClientInput(ws, inputAllowed)
	}()

	return <-errCh
}

// broadcastToAll sends data to all registered clients.
func (b *Bridge) broadcastToAll(data []byte) {
	b.clientsMu.Lock()
	var wg sync.WaitGroup
	for ws := range b.clients {
		wg.Add(1)
		go func(ws *websocket.Conn) {
			defer wg.Done()
			ws.SetWriteDeadline(time.Now().Add(2 * time.Second))
			ws.WriteMessage(websocket.BinaryMessage, data)
			ws.SetWriteDeadline(time.Time{})
		}(ws)
	}
	b.clientsMu.Unlock()
	wg.Wait()
}

// runBroadcast reads from BMC and broadcasts to all clients until
// the BMC connection drops or the bridge stops.
func (b *Bridge) runBroadcast() error {
	buf := make([]byte, 262144)
	for {
		if !b.Running() {
			return nil
		}

		n, err := b.conn.Read(buf)
		if err != nil {
			if !b.Running() {
				return nil
			}
			return fmt.Errorf("BMC read: %w", err)
		}

		// Copy data for goroutines (buf is reused)
		data := make([]byte, n)
		copy(data, buf[:n])

		b.broadcastToAll(data)
	}
}

// readClientInput reads VNC messages from a WebSocket client and forwards
// them to the BMC with input gating.
func (b *Bridge) readClientInput(ws *websocket.Conn, inputAllowed func() bool) error {
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return fmt.Errorf("WS read: %w", err)
		}
		if len(data) == 0 {
			continue
		}

		msgType := data[0]

		// Input gating
		if (msgType == 4 || msgType == 5) && inputAllowed != nil && !inputAllowed() {
			continue
		}

		// Encoding filter for iDRAC8
		if b.FilterEncodings && msgType == 2 && len(data) >= 4 {
			data = rewriteSetEncodings()
		}

		b.connWriteMu.Lock()
		_, writeErr := b.conn.Write(data)
		b.connWriteMu.Unlock()
		if writeErr != nil {
			return fmt.Errorf("BMC write: %w", writeErr)
		}
	}
}

func isTimeout(err error) bool {
	if ne, ok := err.(net.Error); ok {
		return ne.Timeout()
	}
	return false
}

// bmcHandshake performs VNC auth with the BMC and saves the ServerInit.
func (b *Bridge) bmcHandshake(conn net.Conn) error {
	// Version
	ver := make([]byte, 12)
	if _, err := io.ReadFull(conn, ver); err != nil {
		return err
	}
	if _, err := conn.Write([]byte("RFB 003.008\n")); err != nil {
		return err
	}

	// Security types
	numTypes := make([]byte, 1)
	if _, err := io.ReadFull(conn, numTypes); err != nil {
		return err
	}
	if numTypes[0] == 0 {
		lenBuf := make([]byte, 4)
		io.ReadFull(conn, lenBuf)
		reason := make([]byte, binary.BigEndian.Uint32(lenBuf))
		io.ReadFull(conn, reason)
		return fmt.Errorf("rejected: %s", reason)
	}
	types := make([]byte, numTypes[0])
	io.ReadFull(conn, types)

	var chosen byte
	for _, t := range types {
		if t == 2 { chosen = 2; break }
		if t == 1 { chosen = 1 }
	}
	if chosen == 0 {
		return fmt.Errorf("no supported auth type")
	}
	conn.Write([]byte{chosen})

	// VNC auth
	if chosen == 2 {
		challenge := make([]byte, 16)
		io.ReadFull(conn, challenge)
		conn.Write(vncEncrypt(challenge, b.password))
	}

	// Result
	result := make([]byte, 4)
	io.ReadFull(conn, result)
	if binary.BigEndian.Uint32(result) != 0 {
		return fmt.Errorf("auth failed (result=%d)", binary.BigEndian.Uint32(result))
	}

	// ClientInit (shared)
	conn.Write([]byte{1})

	// ServerInit
	header := make([]byte, 24)
	io.ReadFull(conn, header)
	nameLen := binary.BigEndian.Uint32(header[20:24])
	name := make([]byte, nameLen)
	if nameLen > 0 {
		io.ReadFull(conn, name)
	}
	b.serverInit = make([]byte, 24+nameLen)
	copy(b.serverInit, header)
	copy(b.serverInit[24:], name)

	w := binary.BigEndian.Uint16(header[0:2])
	h := binary.BigEndian.Uint16(header[2:4])
	log.Printf("VNC bridge: ServerInit %dx%d %dbpp %q", w, h, header[4], string(name))
	return nil
}

// clientHandshake performs VNC handshake with noVNC using saved ServerInit.
func (b *Bridge) clientHandshake(ws *websocket.Conn) error {
	ws.WriteMessage(websocket.BinaryMessage, []byte("RFB 003.008\n"))
	if _, _, err := ws.ReadMessage(); err != nil { return err }

	ws.WriteMessage(websocket.BinaryMessage, []byte{1, 1}) // 1 type: None
	if _, _, err := ws.ReadMessage(); err != nil { return err }

	ws.WriteMessage(websocket.BinaryMessage, []byte{0, 0, 0, 0}) // OK
	if _, _, err := ws.ReadMessage(); err != nil { return err } // ClientInit

	// Send ServerInit — optionally rewrite name for noVNC 8bpp trigger (iDRAC8)
	if b.RewriteName {
		name := []byte("Intel(r) AMT KVM")
		si := make([]byte, 24+len(name))
		copy(si, b.serverInit[:24])
		binary.BigEndian.PutUint32(si[20:24], uint32(len(name)))
		copy(si[24:], name)
		ws.WriteMessage(websocket.BinaryMessage, si)
	} else {
		ws.WriteMessage(websocket.BinaryMessage, b.serverInit)
	}

	log.Printf("VNC bridge: client handshake complete")
	return nil
}

func rewriteSetEncodings() []byte {
	encs := []int32{0, 1, -223} // Raw, CopyRect, DesktopSize
	buf := make([]byte, 4+len(encs)*4)
	buf[0] = 2
	binary.BigEndian.PutUint16(buf[2:4], uint16(len(encs)))
	for i, e := range encs {
		binary.BigEndian.PutUint32(buf[4+i*4:], uint32(e))
	}
	return buf
}

func vncEncrypt(challenge []byte, password string) []byte {
	key := make([]byte, 8)
	copy(key, []byte(password))
	for i := range key {
		key[i] = reverseBits(key[i])
	}
	block, _ := des.NewCipher(key)
	resp := make([]byte, 16)
	block.Encrypt(resp[0:8], challenge[0:8])
	block.Encrypt(resp[8:16], challenge[8:16])
	return resp
}

func reverseBits(b byte) byte {
	var r byte
	for i := 0; i < 8; i++ {
		r = (r << 1) | (b & 1)
		b >>= 1
	}
	return r
}
