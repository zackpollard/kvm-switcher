package models

import (
	"sync"
	"time"
)

// ServerConfig represents a BMC/IPMI server entry from the config file.
type ServerConfig struct {
	Name          string `yaml:"name"`
	BMCIP         string `yaml:"bmc_ip"`
	BMCPort       int    `yaml:"bmc_port"`
	BoardType     string `yaml:"board_type"`
	Username      string `yaml:"username"`
	CredentialEnv string `yaml:"credential_env"`
}

// Settings holds global application settings.
type Settings struct {
	MaxConcurrentSessions int    `yaml:"max_concurrent_sessions"`
	SessionTimeoutMinutes int    `yaml:"session_timeout_minutes"`
	IdleTimeoutMinutes    int    `yaml:"idle_timeout_minutes"`
	DockerImage           string `yaml:"docker_image"`
	ContainerImage        string `yaml:"container_image"`
	ListenAddress         string `yaml:"listen_address"`
	Runtime               string `yaml:"runtime"`        // "docker" (default) or "kubernetes"
	KubeNamespace         string `yaml:"kube_namespace"` // default: "kvm-switcher"
	KubeConfig            string `yaml:"kube_config"`    // path to kubeconfig; empty = in-cluster
}

// OIDCConfig holds optional OIDC authentication settings.
type OIDCConfig struct {
	Enabled         bool                    `yaml:"enabled"`
	IssuerURL       string                  `yaml:"issuer_url"`
	ClientID        string                  `yaml:"client_id"`
	ClientSecretEnv string                  `yaml:"client_secret_env"`
	RedirectURL     string                  `yaml:"redirect_url"`
	Scopes          []string                `yaml:"scopes"`
	RoleClaim       string                  `yaml:"role_claim"`
	RoleMappings    map[string]*RoleMapping `yaml:"role_mappings"`
}

// RoleMapping defines which servers a role has access to.
type RoleMapping struct {
	Servers []string `yaml:"servers"`
}

// AppConfig is the top-level configuration structure.
type AppConfig struct {
	Servers  []ServerConfig `yaml:"servers"`
	Settings Settings       `yaml:"settings"`
	OIDC     OIDCConfig     `yaml:"oidc"`
}

// UserInfo represents an authenticated user.
type UserInfo struct {
	Email string   `json:"email"`
	Name  string   `json:"name"`
	Roles []string `json:"roles"`
}

// UserSession stores server-side auth session data.
type UserSession struct {
	ID           string
	User         *UserInfo
	IDToken      string
	RefreshToken string
	ExpiresAt    time.Time
}

// SessionStatus represents the lifecycle state of a KVM session.
type SessionStatus string

const (
	SessionStarting     SessionStatus = "starting"
	SessionConnected    SessionStatus = "connected"
	SessionDisconnected SessionStatus = "disconnected"
	SessionError        SessionStatus = "error"
)

// KVMSession represents an active KVM session to a server.
type KVMSession struct {
	ID            string        `json:"id"`
	ServerName    string        `json:"server_name"`
	BMCIP         string        `json:"bmc_ip"`
	Status        SessionStatus `json:"status"`
	ContainerID   string        `json:"container_id,omitempty"`
	WebSocketPort int           `json:"websocket_port,omitempty"`
	CreatedAt     time.Time     `json:"created_at"`
	LastActivity  time.Time     `json:"last_activity"`
	Error         string        `json:"error,omitempty"`
}

// BMCCredentials holds the authentication tokens for a BMC session.
type BMCCredentials struct {
	SessionCookie string
	CSRFToken     string
	KVMToken      string
	WebCookie     string
	Username      string // BMC username (from getrole.asp)
	Privilege     int    // BMC privilege number (from getrole.asp), e.g. 4=Admin
	ExtendedPriv  int    // Extended privileges bitmask (from getrole.asp)
}

// JViewerArgs holds all arguments needed to launch JViewer.
type JViewerArgs struct {
	Hostname          string
	KVMToken          string
	KVMSecure         string
	KVMPort           string
	VMSecure          string
	CDState           string
	FDState           string
	HDState           string
	CDNum             string
	FDNum             string
	HDNum             string
	ExtendedPriv      string
	Localization      string
	KeyboardLayout    string
	WebSecurePort     string
	SinglePortEnabled string
	WebCookie         string
	OEMFeatures       string
}

// SessionStore provides thread-safe access to active sessions.
type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*KVMSession
}

// NewSessionStore creates a new SessionStore.
func NewSessionStore() *SessionStore {
	return &SessionStore{
		sessions: make(map[string]*KVMSession),
	}
}

// Get returns a session by ID.
func (s *SessionStore) Get(id string) (*KVMSession, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	session, ok := s.sessions[id]
	return session, ok
}

// Set stores a session.
func (s *SessionStore) Set(session *KVMSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[session.ID] = session
}

// Delete removes a session by ID.
func (s *SessionStore) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

// List returns all sessions.
func (s *SessionStore) List() []*KVMSession {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*KVMSession, 0, len(s.sessions))
	for _, session := range s.sessions {
		result = append(result, session)
	}
	return result
}

// FindByServer returns a session for the given server name, if one exists.
func (s *SessionStore) FindByServer(serverName string) (*KVMSession, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, session := range s.sessions {
		if session.ServerName == serverName && session.Status != SessionDisconnected && session.Status != SessionError {
			return session, true
		}
	}
	return nil, false
}
