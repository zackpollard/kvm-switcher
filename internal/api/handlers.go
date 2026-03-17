package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"net/url"
	"time"

	"github.com/gorilla/websocket"

	"github.com/zackpollard/kvm-switcher/internal/auth"
	"github.com/zackpollard/kvm-switcher/internal/config"
	containermgr "github.com/zackpollard/kvm-switcher/internal/container"
	"github.com/zackpollard/kvm-switcher/internal/models"
	kvmoidc "github.com/zackpollard/kvm-switcher/internal/oidc"

	"github.com/google/uuid"
)

// Server holds the API dependencies.
type Server struct {
	Config      *models.AppConfig
	Sessions    *models.SessionStore
	Container   containermgr.Manager
	BMCCreds    map[string]*models.BMCCredentials // session ID -> BMC creds for logout
	StatusCache *StatusCache
}

// NewServer creates a new API server and starts background pollers.
func NewServer(cfg *models.AppConfig, cm containermgr.Manager) *Server {
	srv := newServerCore(cfg, cm)
	StartSessionManager(cfg.Servers, srv.StatusCache)
	StartStatusPoller(cfg.Servers, srv.StatusCache)
	return srv
}

// newServerCore creates a Server without starting background goroutines.
// Used by tests to avoid background pollers racing with test assertions.
func newServerCore(cfg *models.AppConfig, cm containermgr.Manager) *Server {
	sc := NewStatusCache()
	return &Server{
		Config:      cfg,
		Sessions:    models.NewSessionStore(),
		Container:   cm,
		BMCCreds:    make(map[string]*models.BMCCredentials),
		StatusCache: sc,
	}
}

// ServerInfo is the JSON response for a server listing.
type ServerInfo struct {
	Name      string `json:"name"`
	BMCIP     string `json:"bmc_ip"`
	BMCPort   int    `json:"bmc_port"`
	BoardType string `json:"board_type"`
	HasActive bool   `json:"has_active_session"`
}

// ListServers handles GET /api/servers.
func (s *Server) ListServers(w http.ResponseWriter, r *http.Request) {
	user := kvmoidc.UserFromContext(r.Context())
	oidcEnabled := s.Config.OIDC.Enabled

	var servers []ServerInfo
	for _, srv := range s.Config.Servers {
		if oidcEnabled && !kvmoidc.UserCanAccessServer(&s.Config.OIDC, user, srv.Name) {
			continue
		}
		_, hasSession := s.Sessions.FindByServer(srv.Name)
		servers = append(servers, ServerInfo{
			Name:      srv.Name,
			BMCIP:     srv.BMCIP,
			BMCPort:   srv.BMCPort,
			BoardType: srv.BoardType,
			HasActive: hasSession,
		})
	}
	if servers == nil {
		servers = []ServerInfo{}
	}
	writeJSON(w, http.StatusOK, servers)
}

// CreateSessionRequest is the JSON body for POST /api/sessions.
type CreateSessionRequest struct {
	ServerName string `json:"server_name"`
}

// CreateSession handles POST /api/sessions.
func (s *Server) CreateSession(w http.ResponseWriter, r *http.Request) {
	var req CreateSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Find the server config
	var serverCfg *models.ServerConfig
	for i := range s.Config.Servers {
		if s.Config.Servers[i].Name == req.ServerName {
			serverCfg = &s.Config.Servers[i]
			break
		}
	}
	if serverCfg == nil {
		writeError(w, http.StatusNotFound, "server not found")
		return
	}

	// Check OIDC authorization
	if s.Config.OIDC.Enabled {
		user := kvmoidc.UserFromContext(r.Context())
		if !kvmoidc.UserCanAccessServer(&s.Config.OIDC, user, req.ServerName) {
			writeError(w, http.StatusForbidden, "access denied to this server")
			return
		}
	}

	// Check if there's already an active session
	if existing, ok := s.Sessions.FindByServer(req.ServerName); ok {
		writeJSON(w, http.StatusOK, existing)
		return
	}

	// Check concurrent session limit
	activeSessions := s.Sessions.List()
	activeCount := 0
	for _, sess := range activeSessions {
		if sess.Status == models.SessionStarting || sess.Status == models.SessionConnected {
			activeCount++
		}
	}
	if activeCount >= s.Config.Settings.MaxConcurrentSessions {
		writeError(w, http.StatusTooManyRequests, "maximum concurrent sessions reached")
		return
	}

	// Create session
	session := &models.KVMSession{
		ID:           uuid.New().String()[:8],
		ServerName:   req.ServerName,
		BMCIP:        serverCfg.BMCIP,
		Status:       models.SessionStarting,
		CreatedAt:    time.Now(),
		LastActivity: time.Now(),
	}
	s.Sessions.Set(session)

	// Start the session asynchronously
	go s.startSession(session, serverCfg)

	writeJSON(w, http.StatusAccepted, session)
}

func (s *Server) startSession(session *models.KVMSession, serverCfg *models.ServerConfig) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Get BMC password
	password, err := config.GetPassword(serverCfg)
	if err != nil {
		log.Printf("Session %s: failed to get password: %v", session.ID, err)
		session.Status = models.SessionError
		session.Error = "BMC password not configured"
		s.Sessions.Set(session)
		return
	}

	// Authenticate with BMC
	authenticator, ok := auth.Get(serverCfg.BoardType)
	if !ok {
		log.Printf("Session %s: unsupported board type: %s", session.ID, serverCfg.BoardType)
		session.Status = models.SessionError
		session.Error = "unsupported board type: " + serverCfg.BoardType
		s.Sessions.Set(session)
		return
	}

	log.Printf("Session %s: authenticating with BMC %s...", session.ID, serverCfg.BMCIP)
	creds, connectInfo, err := authenticator.Authenticate(ctx, serverCfg.BMCIP, serverCfg.BMCPort, serverCfg.Username, password)
	if err != nil {
		log.Printf("Session %s: BMC authentication failed: %v", session.ID, err)
		session.Status = models.SessionError
		session.Error = "BMC authentication failed"
		s.Sessions.Set(session)
		return
	}

	// Store BMC creds for later logout
	s.BMCCreds[session.ID] = creds
	session.ConnMode = connectInfo.Mode

	switch connectInfo.Mode {
	case models.KVMModeContainer:
		s.startContainerSession(ctx, session, serverCfg, authenticator, creds, connectInfo)
	case models.KVMModeWebSocket, models.KVMModeVNC:
		s.startDirectSession(ctx, session, serverCfg, authenticator, creds, connectInfo)
	default:
		session.Status = models.SessionError
		session.Error = "unknown KVM mode: " + string(connectInfo.Mode)
		s.Sessions.Set(session)
	}
}

// startContainerSession launches a JViewer container (AMI MegaRAC flow).
func (s *Server) startContainerSession(ctx context.Context, session *models.KVMSession, serverCfg *models.ServerConfig, authenticator auth.BMCAuthenticator, creds *models.BMCCredentials, connectInfo *models.KVMConnectInfo) {
	log.Printf("Session %s: starting container for %s...", session.ID, serverCfg.Name)
	wsPort, err := s.Container.StartContainer(ctx, session, connectInfo.ContainerArgs)
	if err != nil {
		log.Printf("Session %s: failed to start container: %v", session.ID, err)
		session.Status = models.SessionError
		session.Error = "failed to start KVM container"
		s.Sessions.Set(session)
		_ = authenticator.Logout(ctx, serverCfg.BMCIP, serverCfg.BMCPort, creds)
		return
	}

	session.WebSocketPort = wsPort
	s.Sessions.Set(session)

	// Wait for websockify to accept connections
	log.Printf("Session %s: waiting for container websockify on port %d...", session.ID, wsPort)
	wsURL := url.URL{
		Scheme: "ws",
		Host:   net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", wsPort)),
		Path:   "/websockify",
	}
	probeDialer := websocket.Dialer{Subprotocols: []string{"binary"}}
	ready := false
	for i := 0; i < 30; i++ {
		probeConn, _, err := probeDialer.Dial(wsURL.String(), nil)
		if err == nil {
			probeConn.Close()
			ready = true
			break
		}
		time.Sleep(time.Second)
	}
	if !ready {
		log.Printf("Session %s: websockify never became reachable", session.ID)
		session.Status = models.SessionError
		session.Error = "KVM container started but websockify not reachable"
		s.Sessions.Set(session)
		_ = s.Container.StopContainer(ctx, session.ContainerID)
		_ = authenticator.Logout(ctx, serverCfg.BMCIP, serverCfg.BMCPort, creds)
		return
	}

	session.Status = models.SessionConnected
	session.LastActivity = time.Now()
	s.Sessions.Set(session)
	log.Printf("Session %s: connected to %s on port %d", session.ID, serverCfg.Name, wsPort)
}

// startDirectSession sets up a direct proxy session (WSS or VNC, no container).
func (s *Server) startDirectSession(ctx context.Context, session *models.KVMSession, serverCfg *models.ServerConfig, authenticator auth.BMCAuthenticator, creds *models.BMCCredentials, connectInfo *models.KVMConnectInfo) {
	switch connectInfo.Mode {
	case models.KVMModeWebSocket:
		session.KVMTarget = connectInfo.TargetURL
		log.Printf("Session %s: direct WSS proxy to %s", session.ID, connectInfo.TargetURL)
	case models.KVMModeVNC:
		session.KVMTarget = connectInfo.TargetAddr
		session.KVMPassword = connectInfo.VNCPassword
		log.Printf("Session %s: direct VNC proxy to %s", session.ID, connectInfo.TargetAddr)
	}

	session.Status = models.SessionConnected
	session.LastActivity = time.Now()
	s.Sessions.Set(session)
	log.Printf("Session %s: connected to %s (direct %s)", session.ID, serverCfg.Name, connectInfo.Mode)
}

// GetSession handles GET /api/sessions/{id}.
func (s *Server) GetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	session, ok := s.Sessions.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}

	// Check if container is still running (only relevant for container-mode sessions)
	if session.Status == models.SessionConnected && session.ContainerID != "" && session.ConnMode == models.KVMModeContainer {
		if !s.Container.IsContainerRunning(r.Context(), session.ContainerID) {
			session.Status = models.SessionDisconnected
			s.Sessions.Set(session)
		}
	}

	writeJSON(w, http.StatusOK, session)
}

// ListSessions handles GET /api/sessions.
func (s *Server) ListSessions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Sessions.List())
}

// DeleteSession handles DELETE /api/sessions/{id}.
func (s *Server) DeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	session, ok := s.Sessions.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}

	ctx := r.Context()

	// Stop the Docker container
	if session.ContainerID != "" {
		log.Printf("Session %s: stopping container...", id)
		if err := s.Container.StopContainer(ctx, session.ContainerID); err != nil {
			log.Printf("Session %s: error stopping container: %v", id, err)
		}
	}

	// Logout from BMC
	if creds, ok := s.BMCCreds[id]; ok {
		var serverCfg *models.ServerConfig
		for i := range s.Config.Servers {
			if s.Config.Servers[i].Name == session.ServerName {
				serverCfg = &s.Config.Servers[i]
				break
			}
		}
		if serverCfg != nil {
			if authenticator, ok := auth.Get(serverCfg.BoardType); ok {
				_ = authenticator.Logout(ctx, serverCfg.BMCIP, serverCfg.BMCPort, creds)
			}
		}
		delete(s.BMCCreds, id)
	}

	session.Status = models.SessionDisconnected
	s.Sessions.Set(session)

	log.Printf("Session %s: terminated", id)
	writeJSON(w, http.StatusOK, session)
}

// GetServerStatuses returns cached status for all devices.
func (s *Server) GetServerStatuses(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.StatusCache.GetAll())
}

// CreateIPMISession handles POST /api/ipmi-session/{name}.
// Pre-authenticates with the BMC so the IPMI web UI loads directly to the dashboard.
func (s *Server) CreateIPMISession(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	var serverCfg *models.ServerConfig
	for i := range s.Config.Servers {
		if s.Config.Servers[i].Name == name {
			serverCfg = &s.Config.Servers[i]
			break
		}
	}
	if serverCfg == nil {
		writeError(w, http.StatusNotFound, "server not found")
		return
	}

	if s.Config.OIDC.Enabled {
		user := kvmoidc.UserFromContext(r.Context())
		if !kvmoidc.UserCanAccessServer(&s.Config.OIDC, user, name) {
			writeError(w, http.StatusForbidden, "access denied")
			return
		}
	}

	creds, err := ensureBMCSession(serverCfg)
	if err != nil {
		log.Printf("IPMI session for %s: %v", name, err)
		writeError(w, http.StatusBadGateway, "BMC authentication failed")
		return
	}

	log.Printf("IPMI session for %s: authenticated, credentials injected into proxy", name)

	writeJSON(w, http.StatusOK, map[string]any{
		"board_type":     serverCfg.BoardType,
		"session_cookie": creds.SessionCookie,
		"csrf_token":     creds.CSRFToken,
		"username":       creds.Username,
		"privilege":      creds.Privilege,
		"extended_priv":  creds.ExtendedPriv,
	})
}

// ensureBMCSession creates or renews a BMC web session for the given server.
// Returns the credentials, or an error if authentication fails.
func ensureBMCSession(cfg *models.ServerConfig) (*models.BMCCredentials, error) {
	password, err := config.GetPassword(cfg)
	if err != nil {
		return nil, fmt.Errorf("password not configured: %w", err)
	}

	authenticator, ok := auth.Get(cfg.BoardType)
	if !ok {
		return nil, fmt.Errorf("unsupported board type: %s", cfg.BoardType)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	entry := getOrCreateProxy(cfg, cfg.Name)

	// Log out any existing session to prevent session buildup
	if oldCreds := entry.getBMCCredentials(); oldCreds != nil {
		_ = authenticator.Logout(ctx, cfg.BMCIP, cfg.BMCPort, oldCreds)
		entry.setBMCCredentials(nil)
	}

	creds, err := authenticator.CreateWebSession(ctx, cfg.BMCIP, cfg.BMCPort, cfg.Username, password)
	if err != nil {
		return nil, err
	}

	entry.setBMCCredentials(creds)
	return creds, nil
}

// StartSessionManager creates BMC web sessions for all servers on startup
// and renews them periodically in the background.
func StartSessionManager(servers []models.ServerConfig, sc *StatusCache) {
	createAll := func(sc *StatusCache) {
		var wg sync.WaitGroup
		for i := range servers {
			wg.Add(1)
			go func(cfg *models.ServerConfig) {
				defer wg.Done()
				entry := getOrCreateProxy(cfg, cfg.Name)
				if creds := entry.getBMCCredentials(); creds != nil {
					// iDRAC8 uses Basic Auth for status — doesn't need web sessions.
					// For other types, check if the status poller got data.
					// If the device is online but has no power_state, the session is stale.
					if cfg.BoardType == "dell_idrac8" {
						return
					}
					if sc != nil {
						if st, ok := sc.Get(cfg.Name); ok && st.Online && st.PowerState != "" {
							return // session is working
						}
					}
				}
				if _, err := ensureBMCSession(cfg); err != nil {
					log.Printf("Session for %s: %v", cfg.Name, err)
				} else {
					log.Printf("Session for %s: authenticated", cfg.Name)
				}
			}(&servers[i])
		}
		wg.Wait()
	}

	go func() {
		log.Printf("Session manager: creating initial sessions for %d servers", len(servers))
		createAll(nil)

		// Check for stale/missing sessions every 2 minutes
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			createAll(sc)
		}
	}()
}

// HandleNanoKVMWebSocket proxies WebSocket connections from the NanoKVM SPA
// to the actual NanoKVM device. The NanoKVM SPA connects to /api/ws (HID) and
// /api/stream/h264 (video) using absolute paths. The Go server identifies
// which NanoKVM to proxy to using the nano-kvm-token cookie.
func (s *Server) HandleNanoKVMWebSocket(w http.ResponseWriter, r *http.Request) {
	// Find which NanoKVM this token belongs to
	token := ""
	if c, err := r.Cookie("nano-kvm-token"); err == nil {
		token = c.Value
	}
	if token == "" {
		log.Printf("NanoKVM WS: no nano-kvm-token cookie in request for %s", r.URL.Path)
		http.Error(w, "missing nano-kvm-token", http.StatusUnauthorized)
		return
	}
	log.Printf("NanoKVM WS: proxying %s (token: %s...)", r.URL.Path, token[:20])

	// Find the NanoKVM server that has this token
	var targetCfg *models.ServerConfig
	entries := GetAllProxyEntries()
	for _, cfg := range s.Config.Servers {
		if cfg.BoardType != "nanokvm" {
			continue
		}
		if entry, ok := entries[cfg.Name]; ok {
			if creds := entry.getBMCCredentials(); creds != nil && creds.SessionCookie == token {
				targetCfg = &cfg
				break
			}
		}
	}
	if targetCfg == nil {
		http.Error(w, "unknown NanoKVM token", http.StatusUnauthorized)
		return
	}

	// Build target WebSocket URL
	targetURL := fmt.Sprintf("ws://%s:%d%s", targetCfg.BMCIP, targetCfg.BMCPort, r.URL.Path)
	if targetCfg.BMCPort == 0 {
		targetURL = fmt.Sprintf("ws://%s%s", targetCfg.BMCIP, r.URL.Path)
	}

	// Connect to the NanoKVM
	dialer := websocket.Dialer{}
	header := http.Header{}
	header.Set("Cookie", "nano-kvm-token="+token)
	targetConn, _, err := dialer.Dial(targetURL, header)
	if err != nil {
		log.Printf("NanoKVM WS proxy: failed to connect to %s: %v", targetURL, err)
		http.Error(w, "failed to connect to NanoKVM", http.StatusBadGateway)
		return
	}
	defer targetConn.Close()

	// Upgrade the client connection
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	clientConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("NanoKVM WS proxy: upgrade failed: %v", err)
		return
	}
	defer clientConn.Close()

	// Bidirectional proxy
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			msgType, msg, err := targetConn.ReadMessage()
			if err != nil {
				return
			}
			if err := clientConn.WriteMessage(msgType, msg); err != nil {
				return
			}
		}
	}()

	for {
		msgType, msg, err := clientConn.ReadMessage()
		if err != nil {
			break
		}
		if err := targetConn.WriteMessage(msgType, msg); err != nil {
			break
		}
	}
	<-done
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
