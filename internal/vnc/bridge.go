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

// Bridge holds a persistent VNC TCP connection to a BMC.
type Bridge struct {
	target     string
	password   string
	conn       net.Conn
	serverInit []byte // saved for replay to new clients

	mu      sync.Mutex
	running bool
}

func NewBridge(target, password string) *Bridge {
	return &Bridge{target: target, password: password}
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
}

// ServeWebSocket handles a noVNC client. Performs VNC handshake with the client
// using the saved ServerInit, then pipes data bidirectionally to the BMC.
func (b *Bridge) ServeWebSocket(ws *websocket.Conn) error {
	if !b.Running() {
		return fmt.Errorf("bridge not running")
	}

	// Check the TCP connection is still alive
	b.conn.SetReadDeadline(time.Now().Add(1 * time.Millisecond))
	one := make([]byte, 1)
	_, err := b.conn.Read(one)
	b.conn.SetReadDeadline(time.Time{})
	if err != nil && !isTimeout(err) {
		// Connection dead — try to reconnect
		log.Printf("VNC bridge: TCP connection lost, reconnecting...")
		b.conn.Close()
		b.mu.Lock()
		b.running = false
		b.mu.Unlock()
		if err := b.Start(); err != nil {
			return fmt.Errorf("reconnect failed: %w", err)
		}
	}

	// VNC handshake with client (using saved ServerInit)
	if err := b.clientHandshake(ws); err != nil {
		return fmt.Errorf("client handshake: %w", err)
	}

	// Pipe bidirectionally
	errCh := make(chan error, 2)

	// BMC → client
	go func() {
		buf := make([]byte, 65536)
		for {
			n, err := b.conn.Read(buf)
			if err != nil {
				errCh <- fmt.Errorf("BMC read: %w", err)
				return
			}
			if err := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
				errCh <- fmt.Errorf("WS write: %w", err)
				return
			}
		}
	}()

	// Client → BMC (with SetEncodings rewrite for iDRAC8)
	go func() {
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				errCh <- fmt.Errorf("WS read: %w", err)
				return
			}
			// iDRAC8 fails silently with unsupported encodings
			if len(data) > 0 && data[0] == 2 && len(data) >= 4 {
				data = rewriteSetEncodings()
			}
			if _, err := b.conn.Write(data); err != nil {
				errCh <- fmt.Errorf("BMC write: %w", err)
				return
			}
		}
	}()

	err = <-errCh
	// Don't close the TCP connection — keep it for the next client.
	// Just close the WebSocket side by returning.
	return err
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

	// Send ServerInit with rewritten name for noVNC 8bpp trigger
	name := []byte("Intel(r) AMT KVM")
	si := make([]byte, 24+len(name))
	copy(si, b.serverInit[:24])
	binary.BigEndian.PutUint32(si[20:24], uint32(len(name)))
	copy(si[24:], name)
	ws.WriteMessage(websocket.BinaryMessage, si)

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
