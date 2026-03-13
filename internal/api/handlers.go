package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/zackpollard/kvm-switcher/internal/auth"
	"github.com/zackpollard/kvm-switcher/internal/config"
	containermgr "github.com/zackpollard/kvm-switcher/internal/container"
	"github.com/zackpollard/kvm-switcher/internal/models"

	"github.com/google/uuid"
)

// Server holds the API dependencies.
type Server struct {
	Config    *models.AppConfig
	Sessions  *models.SessionStore
	Container containermgr.Manager
	BMCCreds  map[string]*models.BMCCredentials // session ID -> BMC creds for logout
}

// NewServer creates a new API server.
func NewServer(cfg *models.AppConfig, cm containermgr.Manager) *Server {
	return &Server{
		Config:    cfg,
		Sessions:  models.NewSessionStore(),
		Container: cm,
		BMCCreds:  make(map[string]*models.BMCCredentials),
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
	servers := make([]ServerInfo, len(s.Config.Servers))
	for i, srv := range s.Config.Servers {
		_, hasSession := s.Sessions.FindByServer(srv.Name)
		servers[i] = ServerInfo{
			Name:      srv.Name,
			BMCIP:     srv.BMCIP,
			BMCPort:   srv.BMCPort,
			BoardType: srv.BoardType,
			HasActive: hasSession,
		}
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
	creds, args, err := authenticator.Authenticate(ctx, serverCfg.BMCIP, serverCfg.BMCPort, serverCfg.Username, password)
	if err != nil {
		log.Printf("Session %s: BMC authentication failed: %v", session.ID, err)
		session.Status = models.SessionError
		session.Error = "BMC authentication failed"
		s.Sessions.Set(session)
		return
	}

	// Store BMC creds for later logout
	s.BMCCreds[session.ID] = creds

	// Start Docker container
	log.Printf("Session %s: starting Docker container for %s...", session.ID, serverCfg.Name)
	wsPort, err := s.Container.StartContainer(ctx, session, args)
	if err != nil {
		log.Printf("Session %s: failed to start container: %v", session.ID, err)
		session.Status = models.SessionError
		session.Error = "failed to start KVM container"
		s.Sessions.Set(session)
		_ = authenticator.Logout(ctx, serverCfg.BMCIP, serverCfg.BMCPort, creds)
		return
	}

	session.WebSocketPort = wsPort
	session.Status = models.SessionConnected
	session.LastActivity = time.Now()
	s.Sessions.Set(session)

	log.Printf("Session %s: connected to %s on port %d", session.ID, serverCfg.Name, wsPort)
}

// GetSession handles GET /api/sessions/{id}.
func (s *Server) GetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	session, ok := s.Sessions.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}

	// Check if container is still running
	if session.Status == models.SessionConnected && session.ContainerID != "" {
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

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
