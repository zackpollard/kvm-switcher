package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/zackpollard/kvm-switcher/internal/auth"
	"github.com/zackpollard/kvm-switcher/internal/config"
	"github.com/zackpollard/kvm-switcher/internal/ikvm"
	vnc "github.com/zackpollard/kvm-switcher/internal/vnc"
	"github.com/zackpollard/kvm-switcher/internal/models"
	kvmoidc "github.com/zackpollard/kvm-switcher/internal/oidc"

	"github.com/google/uuid"
)

// Server holds the API dependencies.
type Server struct {
	Config      *models.AppConfig
	Sessions    models.SessionStoreInterface
	BMCCreds    map[string]*models.BMCCredEntry // session ID -> BMC creds for logout
	bmcCredsMu  sync.Mutex
	StatusCache *StatusCache
	AuditDB     models.AuditLogger // optional audit logging backend
	Bridges      map[string]*ikvm.Bridge        // active iKVM bridges by session ID
	bridgesMu    sync.Mutex
	VNCBridges   map[string]*vnc.Bridge         // active VNC bridges by session ID
	vncConnsMu   sync.Mutex
}

// NewServer creates a new API server with an in-memory session store and starts background pollers.
func NewServer(cfg *models.AppConfig) *Server {
	srv := newServerCore(cfg)
	StartSessionManager(cfg.Servers, srv.StatusCache)
	StartStatusPoller(cfg.Servers, srv.StatusCache)
	return srv
}

// NewServerWithStore creates a new API server with a custom session store and starts background pollers.
func NewServerWithStore(cfg *models.AppConfig, sessions models.SessionStoreInterface, auditDB models.AuditLogger) *Server {
	sc := NewStatusCache()
	srv := &Server{
		Config:      cfg,
		Sessions:    sessions,
		BMCCreds:    make(map[string]*models.BMCCredEntry),
		Bridges:  make(map[string]*ikvm.Bridge),
		VNCBridges: make(map[string]*vnc.Bridge),
		StatusCache: sc,
		AuditDB:     auditDB,
	}
	StartSessionManager(cfg.Servers, srv.StatusCache)
	StartStatusPoller(cfg.Servers, srv.StatusCache)
	return srv
}

// newServerCore creates a Server without starting background goroutines.
// Used by tests to avoid background pollers racing with test assertions.
func newServerCore(cfg *models.AppConfig) *Server {
	sc := NewStatusCache()
	return &Server{
		Config:      cfg,
		Sessions:    models.NewSessionStore(),
		BMCCreds:    make(map[string]*models.BMCCredEntry),
		Bridges:  make(map[string]*ikvm.Bridge),
		VNCBridges: make(map[string]*vnc.Bridge),
		StatusCache: sc,
	}
}

// ServerInfo is the JSON response for a server listing.
type ServerInfo struct {
	Name      string `json:"name"`
	BMCIP     string `json:"bmc_ip"`
	BMCPort   int    `json:"bmc_port"`
	BoardType string `json:"board_type"`
	HasActive       bool   `json:"has_active_session"`
	ActiveSessionID string `json:"active_session_id,omitempty"`
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
		existingSession, hasSession := s.Sessions.FindByServer(srv.Name)
		info := ServerInfo{
			Name:      srv.Name,
			BMCIP:     srv.BMCIP,
			BMCPort:   srv.BMCPort,
			BoardType: srv.BoardType,
			HasActive: hasSession,
		}
		if hasSession {
			info.ActiveSessionID = existingSession.ID
		}
		servers = append(servers, info)
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

	// Snapshot for the HTTP response before the goroutine mutates the session.
	snapshot := *session

	// Start the session asynchronously
	go s.startSession(session, serverCfg)

	// Audit log
	userEmail := ""
	if user := kvmoidc.UserFromContext(r.Context()); user != nil {
		userEmail = user.Email
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	s.logAudit("session_create", userEmail, req.ServerName, session.ID, ip, nil)

	writeJSON(w, http.StatusAccepted, &snapshot)
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

	// For MegaRAC boards, use the session manager's existing web session
	// to fetch a KVM token. This avoids creating a separate web session that
	// the BMC might evict when session slots are full.
	if serverCfg.BoardType == "ami_megarac" {
		megaAuth, ok := authenticator.(*auth.MegaRACAuthenticator)
		if !ok {
			session.Status = models.SessionError
			session.Error = "authenticator type mismatch"
			s.Sessions.Set(session)
			return
		}

		entry := getOrCreateProxy(serverCfg, serverCfg.Name)
		existingCreds := entry.getBMCCredentials()
		if existingCreds == nil || existingCreds.SessionCookie == "" {
			// No existing session — create one
			log.Printf("Session %s: creating BMC session for %s...", session.ID, serverCfg.Name)
			var err error
			existingCreds, err = authenticator.CreateWebSession(ctx, serverCfg.BMCIP, serverCfg.BMCPort, serverCfg.Username, password)
			if err != nil {
				session.Status = models.SessionError
				session.Error = "BMC authentication failed"
				s.Sessions.Set(session)
				return
			}
			entry.setBMCCredentials(existingCreds)
		}

		log.Printf("Session %s: fetching KVM token for %s using shared session...", session.ID, serverCfg.Name)
		connectInfo, err := megaAuth.FetchKVMToken(ctx, serverCfg.BMCIP, serverCfg.BMCPort, existingCreds)
		if err != nil {
			log.Printf("Session %s: KVM token fetch failed: %v", session.ID, err)
			session.Status = models.SessionError
			session.Error = "KVM token fetch failed"
			s.Sessions.Set(session)
			return
		}

		session.ConnMode = models.KVMModeIKVM
		session.IKVMArgs = connectInfo.IKVMArgs
		s.startIKVMSession(session, connectInfo)
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
	s.bmcCredsMu.Lock()
	s.BMCCreds[session.ID] = &models.BMCCredEntry{Creds: creds, CreatedAt: time.Now()}
	s.bmcCredsMu.Unlock()
	session.ConnMode = connectInfo.Mode

	switch connectInfo.Mode {
	case models.KVMModeWebSocket, models.KVMModeVNC:
		s.startDirectSession(ctx, session, serverCfg, authenticator, creds, connectInfo)
	case models.KVMModeIKVM:
		session.IKVMArgs = connectInfo.IKVMArgs
		s.startIKVMSession(session, connectInfo)
	default:
		session.Status = models.SessionError
		session.Error = "unknown KVM mode: " + string(connectInfo.Mode)
		s.Sessions.Set(session)
	}
}

// startIKVMSession sets up a native iKVM session.
// The session is marked "connected" immediately since the actual BMC connection
// happens on-demand when the WebSocket client connects. The WebSocket handler
// authenticates fresh and establishes the IVTP tunnel.
func (s *Server) startIKVMSession(session *models.KVMSession, _ *models.KVMConnectInfo) {
	session.Status = models.SessionConnected
	session.LastActivity = time.Now()
	s.Sessions.Set(session)
	log.Printf("Session %s: iKVM session ready (auth deferred to WebSocket connect)", session.ID)
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

// sessionResponse wraps a KVMSession with additional computed fields.
type sessionResponse struct {
	*models.KVMSession
	IdleTimeoutRemaining *float64 `json:"idle_timeout_remaining_seconds,omitempty"`
}

// GetSession handles GET /api/sessions/{id}.
func (s *Server) GetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	session, ok := s.Sessions.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}

	resp := sessionResponse{KVMSession: session}
	if session.Status == models.SessionConnected {
		idleTimeout := time.Duration(s.Config.Settings.IdleTimeoutMinutes) * time.Minute
		remaining := idleTimeout - time.Since(session.LastActivity)
		if remaining < 0 {
			remaining = 0
		}
		secs := remaining.Seconds()
		resp.IdleTimeoutRemaining = &secs
	}

	writeJSON(w, http.StatusOK, resp)
}

// KeepAliveSession handles PATCH /api/sessions/{id}/keepalive.
func (s *Server) KeepAliveSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	session, ok := s.Sessions.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}

	session.LastActivity = time.Now()
	s.Sessions.Set(session)

	// Audit log
	userEmail := ""
	if user := kvmoidc.UserFromContext(r.Context()); user != nil {
		userEmail = user.Email
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	s.logAudit("session_keepalive", userEmail, session.ServerName, id, ip, nil)

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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

	// Logout from BMC
	s.bmcCredsMu.Lock()
	credEntry, hasCreds := s.BMCCreds[id]
	if hasCreds {
		delete(s.BMCCreds, id)
	}
	s.bmcCredsMu.Unlock()

	if hasCreds {
		var serverCfg *models.ServerConfig
		for i := range s.Config.Servers {
			if s.Config.Servers[i].Name == session.ServerName {
				serverCfg = &s.Config.Servers[i]
				break
			}
		}
		if serverCfg != nil {
			if authenticator, ok := auth.Get(serverCfg.BoardType); ok {
				_ = authenticator.Logout(ctx, serverCfg.BMCIP, serverCfg.BMCPort, credEntry.Creds)
			}
		}
	}

	// Stop any running bridges for this session
	s.StopIKVMBridge(id)
	s.StopVNCBridge(id)

	session.Status = models.SessionDisconnected
	s.Sessions.Set(session)

	log.Printf("Session %s: terminated", id)

	// Audit log
	userEmail := ""
	if user := kvmoidc.UserFromContext(r.Context()); user != nil {
		userEmail = user.Email
	}
	delIP, _, _ := net.SplitHostPort(r.RemoteAddr)
	s.logAudit("session_delete", userEmail, session.ServerName, id, delIP, nil)

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

	// Audit log
	ipmiUserEmail := ""
	if user := kvmoidc.UserFromContext(r.Context()); user != nil {
		ipmiUserEmail = user.Email
	}
	ipmiIP, _, _ := net.SplitHostPort(r.RemoteAddr)
	s.logAudit("ipmi_session", ipmiUserEmail, name, "", ipmiIP, nil)

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

	// Don't touch the session if an iKVM bridge is actively using it.
	entry.mu.RLock()
	active := entry.kvmActive
	entry.mu.RUnlock()
	if active {
		return entry.getBMCCredentials(), nil
	}

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
	upgrader := s.wsUpgrader()
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
	buf, err := json.Marshal(data)
	if err != nil {
		log.Printf("writeJSON: marshal error: %v", err)
		http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(buf); err != nil {
		log.Printf("writeJSON: write error: %v", err)
	}
	w.Write([]byte("\n"))
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// GetAuditLog handles GET /api/audit-log.
func (s *Server) GetAuditLog(w http.ResponseWriter, r *http.Request) {
	if s.AuditDB == nil {
		writeError(w, http.StatusNotFound, "audit log not enabled")
		return
	}

	filter := models.AuditFilter{
		EventType:  r.URL.Query().Get("event_type"),
		ServerName: r.URL.Query().Get("server_name"),
		UserEmail:  r.URL.Query().Get("user_email"),
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		fmt.Sscanf(v, "%d", &filter.Limit)
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		fmt.Sscanf(v, "%d", &filter.Offset)
	}

	entries, err := s.AuditDB.QueryAudit(filter)
	if err != nil {
		log.Printf("GetAuditLog: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to query audit log")
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

// --- iKVM control endpoints ---
// These send commands through the active IVTP tunnel to the BMC.

// getBridge returns the active iKVM bridge for a session, or nil.
func (s *Server) getBridge(sessionID string) *ikvm.Bridge {
	s.bridgesMu.Lock()
	defer s.bridgesMu.Unlock()
	return s.Bridges[sessionID]
}

// KVMPowerControl sends a power command to the BMC through the KVM tunnel.
// POST /api/sessions/{id}/power  body: {"action": "on"|"off"|"cycle"|"reset"|"soft_reset"}
func (s *Server) KVMPowerControl(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	bridge := s.getBridge(id)
	if bridge == nil {
		writeError(w, http.StatusNotFound, "no active KVM bridge for this session")
		return
	}

	var req struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// BMC cold reset is handled via IPMI, not the IVTP power command
	if req.Action == "bmc_reset" {
		session, _ := s.Sessions.Get(id)
		if session == nil {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		// Look up server config to get IPMI credentials
		var serverCfg *models.ServerConfig
		for i := range s.Config.Servers {
			if s.Config.Servers[i].Name == session.ServerName {
				serverCfg = &s.Config.Servers[i]
				break
			}
		}
		if serverCfg == nil {
			writeError(w, http.StatusNotFound, "server config not found")
			return
		}
		password := os.Getenv(serverCfg.CredentialEnv)
		out, err := exec.Command("ipmitool", "-I", "lanplus",
			"-H", session.BMCIP, "-U", serverCfg.Username, "-P", password,
			"mc", "reset", "cold").CombinedOutput()
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("BMC cold reset failed: %s: %v", string(out), err))
			return
		}
		log.Printf("KVM BMC cold reset: %s (session %s)", session.ServerName, id)
		s.logAudit("kvm_bmc_reset", "", session.ServerName, id, r.RemoteAddr, nil)
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "action": req.Action})
		return
	}

	var code byte
	switch req.Action {
	case "off":
		code = 0
	case "on":
		code = 1
	case "cycle":
		code = 2
	case "reset":
		code = 3
	case "soft_reset":
		code = 5
	default:
		writeError(w, http.StatusBadRequest, "invalid action: must be on/off/cycle/reset/soft_reset/bmc_reset")
		return
	}

	if err := bridge.SendPowerCommand(code); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	session, _ := s.Sessions.Get(id)
	serverName := ""
	if session != nil {
		serverName = session.ServerName
	}
	log.Printf("KVM power control: %s on %s (session %s)", req.Action, serverName, id)
	s.logAudit("kvm_power_control", "", serverName, id, r.RemoteAddr, map[string]string{"action": req.Action})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "action": req.Action})
}

// KVMDisplayLock locks or unlocks the host display.
// POST /api/sessions/{id}/display-lock  body: {"lock": true|false}
func (s *Server) KVMDisplayLock(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	bridge := s.getBridge(id)
	if bridge == nil {
		writeError(w, http.StatusNotFound, "no active KVM bridge for this session")
		return
	}

	var req struct {
		Lock bool `json:"lock"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := bridge.SendDisplayLock(req.Lock); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Audit log
	session, _ := s.Sessions.Get(id)
	serverName := ""
	if session != nil {
		serverName = session.ServerName
	}
	userEmail := ""
	if user := kvmoidc.UserFromContext(r.Context()); user != nil {
		userEmail = user.Email
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	s.logAudit("kvm_display_lock", userEmail, serverName, id, ip, map[string]any{"lock": req.Lock})

	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "lock": req.Lock})
}

// KVMResetVideo sends a pause/resume cycle to reset the BMC's video capture engine.
// POST /api/sessions/{id}/reset-video
func (s *Server) KVMResetVideo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	bridge := s.getBridge(id)
	if bridge == nil {
		writeError(w, http.StatusNotFound, "no active KVM bridge for this session")
		return
	}

	if err := bridge.ResetVideo(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Audit log
	session, _ := s.Sessions.Get(id)
	serverName := ""
	if session != nil {
		serverName = session.ServerName
	}
	userEmail := ""
	if user := kvmoidc.UserFromContext(r.Context()); user != nil {
		userEmail = user.Email
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	s.logAudit("kvm_reset_video", userEmail, serverName, id, ip, nil)

	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// KVMMouseMode sets the mouse input mode.
// POST /api/sessions/{id}/mouse-mode  body: {"mode": "relative"|"absolute"}
func (s *Server) KVMMouseMode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	bridge := s.getBridge(id)
	if bridge == nil {
		writeError(w, http.StatusNotFound, "no active KVM bridge for this session")
		return
	}

	var req struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	var code byte
	switch req.Mode {
	case "relative":
		code = 1
	case "absolute":
		code = 2
	default:
		writeError(w, http.StatusBadRequest, "invalid mode: must be relative or absolute")
		return
	}

	if err := bridge.SetMouseMode(code); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Audit log
	session, _ := s.Sessions.Get(id)
	serverName := ""
	if session != nil {
		serverName = session.ServerName
	}
	userEmail := ""
	if user := kvmoidc.UserFromContext(r.Context()); user != nil {
		userEmail = user.Email
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	s.logAudit("kvm_mouse_mode", userEmail, serverName, id, ip, map[string]string{"mode": req.Mode})

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "mode": req.Mode})
}

// KVMKeyboardLayout changes the keyboard layout.
// POST /api/sessions/{id}/keyboard-layout  body: {"layout": "en"|"fr"|"de"|"es"|"jp"}
func (s *Server) KVMKeyboardLayout(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	bridge := s.getBridge(id)
	if bridge == nil {
		writeError(w, http.StatusNotFound, "no active KVM bridge for this session")
		return
	}

	var req struct {
		Layout string `json:"layout"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Keyboard layout strings (from JViewer's -keyboardlayout argument)
	var layoutCode string
	switch req.Layout {
	case "en":
		layoutCode = "AD"
	case "fr":
		layoutCode = "FR"
	case "de":
		layoutCode = "DE"
	case "es":
		layoutCode = "ES"
	case "jp":
		layoutCode = "JP"
	default:
		writeError(w, http.StatusBadRequest, "invalid layout: must be en/fr/de/es/jp")
		return
	}

	if err := bridge.SetKeyboardLayout(layoutCode); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Audit log
	session, _ := s.Sessions.Get(id)
	serverName := ""
	if session != nil {
		serverName = session.ServerName
	}
	userEmail := ""
	if user := kvmoidc.UserFromContext(r.Context()); user != nil {
		userEmail = user.Email
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	s.logAudit("kvm_keyboard_layout", userEmail, serverName, id, ip, map[string]string{"layout": req.Layout})

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "layout": req.Layout})
}

// KVMIPMICommand sends a raw IPMI command through the KVM tunnel.
// POST /api/sessions/{id}/ipmi  body: {"data": [byte array as hex string]}
func (s *Server) KVMIPMICommand(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	bridge := s.getBridge(id)
	if bridge == nil {
		writeError(w, http.StatusNotFound, "no active KVM bridge for this session")
		return
	}

	var req struct {
		Data []byte `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Data) == 0 {
		writeError(w, http.StatusBadRequest, "data is required")
		return
	}

	if err := bridge.SendIPMICommand(req.Data); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Audit log
	session, _ := s.Sessions.Get(id)
	serverName := ""
	if session != nil {
		serverName = session.ServerName
	}
	userEmail := ""
	if user := kvmoidc.UserFromContext(r.Context()); user != nil {
		userEmail = user.Email
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	s.logAudit("kvm_ipmi_command", userEmail, serverName, id, ip, map[string]int{"data_length": len(req.Data)})

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// logAudit is a helper to write audit entries when the audit backend is configured.
func (s *Server) logAudit(eventType, userEmail, serverName, sessionID, remoteAddr string, details any) {
	if s.AuditDB == nil {
		return
	}
	if err := s.AuditDB.LogAudit(models.AuditEntry{
		EventType:  eventType,
		UserEmail:  userEmail,
		ServerName: serverName,
		SessionID:  sessionID,
		RemoteAddr: remoteAddr,
		Details:    details,
	}); err != nil {
		log.Printf("audit: failed to log %s: %v", eventType, err)
	}
}

// CleanupStaleBMCCreds removes BMC credentials older than the configured TTL
// whose sessions are no longer active. This is called periodically from the
// session cleanup goroutine.
func (s *Server) CleanupStaleBMCCreds(ttlMinutes int) {
	threshold := time.Now().Add(-time.Duration(ttlMinutes) * time.Minute)

	s.bmcCredsMu.Lock()
	var stale []string
	for id, entry := range s.BMCCreds {
		if entry.CreatedAt.Before(threshold) {
			// Only clean up if session is gone or not active
			session, ok := s.Sessions.Get(id)
			if !ok || session.Status == models.SessionDisconnected || session.Status == models.SessionError {
				stale = append(stale, id)
			}
		}
	}
	// Remove from map while holding lock, collect creds for logout
	type logoutInfo struct {
		creds      *models.BMCCredentials
		serverName string
	}
	var toLogout []logoutInfo
	for _, id := range stale {
		entry := s.BMCCreds[id]
		delete(s.BMCCreds, id)
		// Find the session to get server name
		session, ok := s.Sessions.Get(id)
		if ok {
			toLogout = append(toLogout, logoutInfo{creds: entry.Creds, serverName: session.ServerName})
		}
	}
	s.bmcCredsMu.Unlock()

	// Perform BMC logout outside the lock
	for _, info := range toLogout {
		for i := range s.Config.Servers {
			if s.Config.Servers[i].Name == info.serverName {
				if authenticator, ok := auth.Get(s.Config.Servers[i].BoardType); ok {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					_ = authenticator.Logout(ctx, s.Config.Servers[i].BMCIP, s.Config.Servers[i].BMCPort, info.creds)
					cancel()
				}
				break
			}
		}
	}

	if len(stale) > 0 {
		log.Printf("Cleaned up %d stale BMC credential(s)", len(stale))
	}
}

// KVMScreenshot returns the current framebuffer as a PNG image.
// GET /api/sessions/{id}/screenshot
func (s *Server) KVMScreenshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	bridge := s.getBridge(id)
	if bridge == nil {
		writeError(w, http.StatusNotFound, "no active KVM bridge for this session")
		return
	}
	pngData := bridge.Screenshot()
	if pngData == nil {
		writeError(w, http.StatusServiceUnavailable, "no framebuffer available")
		return
	}

	// Audit log
	session, _ := s.Sessions.Get(id)
	serverName := ""
	if session != nil {
		serverName = session.ServerName
	}
	userEmail := ""
	if user := kvmoidc.UserFromContext(r.Context()); user != nil {
		userEmail = user.Email
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	s.logAudit("kvm_screenshot", userEmail, serverName, id, ip, nil)

	w.Header().Set("Content-Type", "image/png")
	w.Write(pngData)
}
