package models

import (
	"sync"
	"time"
)

// ServerConfig represents a BMC/IPMI server entry from the config file.
type ServerConfig struct {
	Name           string `yaml:"name"`
	BMCIP          string `yaml:"bmc_ip"`
	BMCPort        int    `yaml:"bmc_port"`
	BoardType      string `yaml:"board_type"`
	Username       string `yaml:"username"`
	CredentialEnv  string `yaml:"credential_env"`
	TLSSkipVerify  *bool  `yaml:"tls_skip_verify"` // nil = default true (BMCs use self-signed certs)
}

// Settings holds global application settings.
type Settings struct {
	MaxConcurrentSessions int    `yaml:"max_concurrent_sessions"`
	SessionTimeoutMinutes int    `yaml:"session_timeout_minutes"`
	IdleTimeoutMinutes    int    `yaml:"idle_timeout_minutes"`
	ListenAddress         string `yaml:"listen_address"`

	// Production hardening settings
	CORSOrigins        []string `yaml:"cors_origins"`          // default ["*"]
	RateLimitRPM         int      `yaml:"rate_limit_rpm"`           // default 60
	BMCProxyRateLimitRPM int      `yaml:"bmc_proxy_rate_limit_rpm"` // default 300
	DBPath               string   `yaml:"db_path"`                  // default "data/kvm-switcher.db"
	AuditLog           *bool    `yaml:"audit_log"`             // default true (pointer for nil=default-true)
	MetricsEnabled     bool     `yaml:"metrics_enabled"`       // default false
	BMCCredsTTLMinutes int      `yaml:"bmc_creds_ttl_minutes"` // default 120
	AuditRetentionDays int      `yaml:"audit_retention_days"`  // default 90
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
	ID           string        `json:"id"`
	ServerName   string        `json:"server_name"`
	BMCIP        string        `json:"bmc_ip"`
	Status       SessionStatus `json:"status"`
	ConnMode     KVMMode       `json:"conn_mode,omitempty"`
	KVMTarget    string        `json:"-"`                      // WSS URL or VNC host:port (internal only)
	KVMPassword  string        `json:"kvm_password,omitempty"` // VNC auth password (if needed)
	IKVMArgs     *JViewerArgs  `json:"-"`                      // Native iKVM connection args
	CreatedAt    time.Time     `json:"created_at"`
	LastActivity time.Time     `json:"last_activity"`
	Error        string        `json:"error,omitempty"`
}

// KVMMode describes how a KVM session connects to the BMC.
type KVMMode string

const (
	KVMModeWebSocket KVMMode = "websocket" // Proxy WS → remote WSS (iDRAC9 HTML5)
	KVMModeVNC       KVMMode = "vnc"       // Proxy WS → raw TCP VNC (iDRAC8)
	KVMModeIKVM      KVMMode = "ikvm"      // Native IVTP protocol (AMI MegaRAC)
)

// KVMConnectInfo describes how to reach the KVM stream for a session.
type KVMConnectInfo struct {
	Mode        KVMMode
	IKVMArgs    *JViewerArgs // For iKVM mode (MegaRAC IVTP connection parameters)
	TargetURL   string       // For websocket mode (wss://...)
	TargetAddr  string       // For vnc mode (host:port)
	VNCPassword string       // For vnc mode: password for VNC auth
}

// BMCCredEntry wraps BMCCredentials with metadata for TTL-based cleanup.
type BMCCredEntry struct {
	Creds     *BMCCredentials
	CreatedAt time.Time
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
	Extra         map[string]string // Board-specific extra tokens
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

// SessionStoreInterface defines the contract for session storage backends.
type SessionStoreInterface interface {
	Get(id string) (*KVMSession, bool)
	Set(session *KVMSession)
	Delete(id string)
	List() []*KVMSession
	FindByServer(serverName string) (*KVMSession, bool)
}

// SessionStore provides in-memory thread-safe access to active sessions.
// Implements SessionStoreInterface.
type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*KVMSession
}

// Verify interface compliance at compile time.
var _ SessionStoreInterface = (*SessionStore)(nil)

// NewSessionStore creates a new in-memory SessionStore.
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

// AuditEntry represents a single audit log event.
type AuditEntry struct {
	ID         int64     `json:"id"`
	Timestamp  time.Time `json:"timestamp"`
	EventType  string    `json:"event_type"`
	UserEmail  string    `json:"user_email,omitempty"`
	ServerName string    `json:"server_name,omitempty"`
	SessionID  string    `json:"session_id,omitempty"`
	RemoteAddr string    `json:"remote_addr,omitempty"`
	Details    any       `json:"details,omitempty"`
}

// AuditFilter specifies query parameters for retrieving audit entries.
type AuditFilter struct {
	EventType  string
	ServerName string
	UserEmail  string
	Limit      int
	Offset     int
}

// AuditLogger is the interface for audit logging backends.
type AuditLogger interface {
	LogAudit(entry AuditEntry) error
	QueryAudit(filter AuditFilter) ([]AuditEntry, error)
}

// --- Swagger request/response types for API documentation ---

// PowerControlRequest is the JSON body for POST /api/sessions/{id}/power.
type PowerControlRequest struct {
	Action string `json:"action" example:"cycle" enums:"on,off,cycle,reset,soft_reset,bmc_reset"`
}

// PowerControlResponse is the JSON response for a successful power command.
type PowerControlResponse struct {
	Status string `json:"status" example:"ok"`
	Action string `json:"action" example:"cycle"`
}

// DisplayLockRequest is the JSON body for POST /api/sessions/{id}/display-lock.
type DisplayLockRequest struct {
	Lock bool `json:"lock" example:"true"`
}

// DisplayLockResponse is the JSON response for a successful display lock command.
type DisplayLockResponse struct {
	Status string `json:"status" example:"ok"`
	Lock   bool   `json:"lock" example:"true"`
}

// MouseModeRequest is the JSON body for POST /api/sessions/{id}/mouse-mode.
type MouseModeRequest struct {
	Mode string `json:"mode" example:"absolute" enums:"relative,absolute"`
}

// MouseModeResponse is the JSON response for a successful mouse mode change.
type MouseModeResponse struct {
	Status string `json:"status" example:"ok"`
	Mode   string `json:"mode" example:"absolute"`
}

// KeyboardLayoutRequest is the JSON body for POST /api/sessions/{id}/keyboard-layout.
type KeyboardLayoutRequest struct {
	Layout string `json:"layout" example:"en" enums:"en,fr,de,es,jp"`
}

// KeyboardLayoutResponse is the JSON response for a successful keyboard layout change.
type KeyboardLayoutResponse struct {
	Status string `json:"status" example:"ok"`
	Layout string `json:"layout" example:"en"`
}

// IPMICommandRequest is the JSON body for POST /api/sessions/{id}/ipmi.
type IPMICommandRequest struct {
	// Data contains raw IPMI command bytes, base64-encoded
	Data []byte `json:"data"`
}

// StatusOkResponse is a generic success response.
type StatusOkResponse struct {
	Status string `json:"status" example:"ok"`
}

// ErrorResponse is a generic error response.
type ErrorResponse struct {
	Error string `json:"error" example:"session not found"`
}

// IPMISessionResponse is the JSON response for POST /api/ipmi-session/{name}.
type IPMISessionResponse struct {
	BoardType    string `json:"board_type" example:"ami_megarac"`
	SessionCookie string `json:"session_cookie" example:"SessionCookie=abc123"`
	CSRFToken    string `json:"csrf_token" example:"def456"`
	Username     string `json:"username" example:"admin"`
	Privilege    int    `json:"privilege" example:"4"`
	ExtendedPriv int    `json:"extended_priv" example:"259"`
}

// HealthResponse is the JSON response for GET /healthz.
type HealthResponse struct {
	Status string `json:"status" example:"ok"`
}

// ReadyResponse is the JSON response for GET /readyz.
type ReadyResponse struct {
	Status string `json:"status" example:"ready"`
}

// ReadyUnavailableResponse is the JSON response for GET /readyz when the database is unreachable.
type ReadyUnavailableResponse struct {
	Status string `json:"status" example:"unavailable"`
	Reason string `json:"reason" example:"database unreachable"`
}

// AuthStatusResponse is the JSON response for GET /auth/me.
type AuthStatusResponse struct {
	Authenticated bool     `json:"authenticated" example:"true"`
	OIDCEnabled   bool     `json:"oidc_enabled" example:"true"`
	Email         string   `json:"email,omitempty" example:"user@example.com"`
	Name          string   `json:"name,omitempty" example:"Jane Doe"`
	Roles         []string `json:"roles,omitempty"`
}

// DeviceStatus holds polled status information for a single device.
type DeviceStatus struct {
	Online              bool    `json:"online"`
	PowerState          string  `json:"power_state,omitempty"`           // "on", "off", ""
	Model               string  `json:"model,omitempty"`
	Health              string  `json:"health,omitempty"`                 // "ok", "warning", "critical", ""
	LoadWatts           float64 `json:"load_watts,omitempty"`
	LoadPct             float64 `json:"load_pct,omitempty"`              // UPS: percentage of rated capacity
	LoadAmps            float64 `json:"load_amps,omitempty"`
	Voltage             float64 `json:"voltage,omitempty"`
	BatteryPct          float64 `json:"battery_pct,omitempty"`
	RuntimeMin          float64 `json:"runtime_min,omitempty"`
	TemperatureC        float64 `json:"temperature_c,omitempty"`
	AppVersion          string  `json:"app_version,omitempty"`            // NanoKVM: application version
	ImageVersion        string  `json:"image_version,omitempty"`          // NanoKVM: firmware image version
	UpdateAvail         bool    `json:"update_available,omitempty"`       // NanoKVM: firmware update available
	CircuitBreakerState string    `json:"circuit_breaker_state,omitempty"` // "closed", "open", "half-open"
	LastUpdated         time.Time `json:"last_updated"`
	Error               string    `json:"error,omitempty"`
}
