package api

import (
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
// and the container's websockify instance.
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

	// Connect to the container's websockify
	containerURL := url.URL{
		Scheme: "ws",
		Host:   net.JoinHostPort("127.0.0.1", itoa(session.WebSocketPort)),
		Path:   "/websockify",
	}

	log.Printf("Session %s: proxying WebSocket to %s", sessionID, containerURL.String())

	// Dial the container websockify with retries
	dialer := websocket.Dialer{
		Subprotocols: []string{"binary"},
	}
	var backendConn *websocket.Conn
	var err error
	for i := 0; i < 10; i++ {
		backendConn, _, err = dialer.Dial(containerURL.String(), nil)
		if err == nil {
			break
		}
		log.Printf("Session %s: waiting for container websockify (attempt %d)...", sessionID, i+1)
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		log.Printf("Session %s: failed to connect to container websockify: %v", sessionID, err)
		http.Error(w, "failed to connect to KVM container", http.StatusBadGateway)
		return
	}
	defer backendConn.Close()

	// Upgrade the client connection
	clientConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Session %s: WebSocket upgrade failed: %v", sessionID, err)
		return
	}
	defer clientConn.Close()

	log.Printf("Session %s: WebSocket proxy established", sessionID)

	// Bidirectional proxy
	errCh := make(chan error, 2)

	// Client -> Backend
	go func() {
		errCh <- proxyWebSocket(clientConn, backendConn)
	}()

	// Backend -> Client
	go func() {
		errCh <- proxyWebSocket(backendConn, clientConn)
	}()

	// Wait for either direction to close
	err = <-errCh
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
