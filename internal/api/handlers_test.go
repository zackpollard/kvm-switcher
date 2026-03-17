package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zackpollard/kvm-switcher/internal/models"
	kvmoidc "github.com/zackpollard/kvm-switcher/internal/oidc"
)

// mockContainerManager implements container.Manager for testing.
type mockContainerManager struct{}

func (m *mockContainerManager) StartContainer(_ context.Context, _ *models.KVMSession, _ *models.JViewerArgs) (int, error) {
	return 0, nil
}
func (m *mockContainerManager) StopContainer(_ context.Context, _ string) error { return nil }
func (m *mockContainerManager) IsContainerRunning(_ context.Context, _ string) bool {
	return true
}
func (m *mockContainerManager) GetContainerLogs(_ context.Context, _ string) (string, error) {
	return "", nil
}
func (m *mockContainerManager) CleanupOrphans(_ context.Context) error { return nil }
func (m *mockContainerManager) Close() error                           { return nil }

func newTestConfig(oidcEnabled bool) *models.AppConfig {
	cfg := &models.AppConfig{
		Servers: []models.ServerConfig{
			{Name: "server-1", BMCIP: "10.0.0.1", BMCPort: 80, BoardType: "ami_megarac", Username: "admin", CredentialEnv: "PASS1"},
			{Name: "server-2", BMCIP: "10.0.0.2", BMCPort: 80, BoardType: "ami_megarac", Username: "admin", CredentialEnv: "PASS2"},
			{Name: "server-3", BMCIP: "10.0.0.3", BMCPort: 80, BoardType: "ami_megarac", Username: "admin", CredentialEnv: "PASS3"},
		},
		Settings: models.Settings{
			MaxConcurrentSessions: 4,
			Runtime:               "docker",
		},
	}
	if oidcEnabled {
		cfg.OIDC = models.OIDCConfig{
			Enabled:   true,
			RoleClaim: "groups",
			RoleMappings: map[string]*models.RoleMapping{
				"admin": {Servers: []string{"*"}},
				"ops":   {Servers: []string{"server-1", "server-2"}},
				"dev":   {Servers: []string{"server-3"}},
			},
		}
	}
	return cfg
}

func TestListServers_NoOIDC(t *testing.T) {
	srv := newServerCore(newTestConfig(false), &mockContainerManager{})

	req := httptest.NewRequest("GET", "/api/servers", nil)
	w := httptest.NewRecorder()
	srv.ListServers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var servers []ServerInfo
	if err := json.NewDecoder(w.Body).Decode(&servers); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(servers) != 3 {
		t.Errorf("servers = %d, want 3", len(servers))
	}
}

func TestListServers_OIDCAdmin(t *testing.T) {
	srv := newServerCore(newTestConfig(true), &mockContainerManager{})

	user := &models.UserInfo{Email: "admin@test.com", Roles: []string{"admin"}}
	ctx := context.WithValue(context.Background(), kvmoidc.UserContextKey, user)
	req := httptest.NewRequest("GET", "/api/servers", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.ListServers(w, req)

	var servers []ServerInfo
	if err := json.NewDecoder(w.Body).Decode(&servers); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(servers) != 3 {
		t.Errorf("admin should see all 3 servers, got %d", len(servers))
	}
}

func TestListServers_OIDCOps(t *testing.T) {
	srv := newServerCore(newTestConfig(true), &mockContainerManager{})

	user := &models.UserInfo{Email: "ops@test.com", Roles: []string{"ops"}}
	ctx := context.WithValue(context.Background(), kvmoidc.UserContextKey, user)
	req := httptest.NewRequest("GET", "/api/servers", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.ListServers(w, req)

	var servers []ServerInfo
	if err := json.NewDecoder(w.Body).Decode(&servers); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(servers) != 2 {
		t.Errorf("ops should see 2 servers, got %d", len(servers))
	}
	for _, s := range servers {
		if s.Name == "server-3" {
			t.Error("ops should not see server-3")
		}
	}
}

func TestListServers_OIDCDev(t *testing.T) {
	srv := newServerCore(newTestConfig(true), &mockContainerManager{})

	user := &models.UserInfo{Email: "dev@test.com", Roles: []string{"dev"}}
	ctx := context.WithValue(context.Background(), kvmoidc.UserContextKey, user)
	req := httptest.NewRequest("GET", "/api/servers", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.ListServers(w, req)

	var servers []ServerInfo
	if err := json.NewDecoder(w.Body).Decode(&servers); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(servers) != 1 {
		t.Errorf("dev should see 1 server, got %d", len(servers))
	}
	if len(servers) > 0 && servers[0].Name != "server-3" {
		t.Errorf("dev should see server-3, got %q", servers[0].Name)
	}
}

func TestListServers_OIDCNoMatchingRole(t *testing.T) {
	srv := newServerCore(newTestConfig(true), &mockContainerManager{})

	user := &models.UserInfo{Email: "nobody@test.com", Roles: []string{"viewer"}}
	ctx := context.WithValue(context.Background(), kvmoidc.UserContextKey, user)
	req := httptest.NewRequest("GET", "/api/servers", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.ListServers(w, req)

	var servers []ServerInfo
	if err := json.NewDecoder(w.Body).Decode(&servers); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(servers) != 0 {
		t.Errorf("viewer should see 0 servers, got %d", len(servers))
	}
}

func TestListServers_OIDCMultiRole(t *testing.T) {
	srv := newServerCore(newTestConfig(true), &mockContainerManager{})

	user := &models.UserInfo{Email: "multi@test.com", Roles: []string{"ops", "dev"}}
	ctx := context.WithValue(context.Background(), kvmoidc.UserContextKey, user)
	req := httptest.NewRequest("GET", "/api/servers", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.ListServers(w, req)

	var servers []ServerInfo
	if err := json.NewDecoder(w.Body).Decode(&servers); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(servers) != 3 {
		t.Errorf("ops+dev should see all 3 servers, got %d", len(servers))
	}
}

func TestCreateSession_OIDCForbidden(t *testing.T) {
	srv := newServerCore(newTestConfig(true), &mockContainerManager{})

	body := `{"server_name":"server-3"}`
	user := &models.UserInfo{Email: "ops@test.com", Roles: []string{"ops"}}
	ctx := context.WithValue(context.Background(), kvmoidc.UserContextKey, user)
	req := httptest.NewRequest("POST", "/api/sessions", strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.CreateSession(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp["error"] != "access denied to this server" {
		t.Errorf("error = %q, want 'access denied to this server'", resp["error"])
	}
}

func TestCreateSession_OIDCAllowed(t *testing.T) {
	srv := newServerCore(newTestConfig(true), &mockContainerManager{})

	body := `{"server_name":"server-1"}`
	user := &models.UserInfo{Email: "ops@test.com", Roles: []string{"ops"}}
	ctx := context.WithValue(context.Background(), kvmoidc.UserContextKey, user)
	req := httptest.NewRequest("POST", "/api/sessions", strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.CreateSession(w, req)

	// Should be 202 Accepted (session creation started)
	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want %d", w.Code, http.StatusAccepted)
	}
}

func TestCreateSession_NoOIDC(t *testing.T) {
	srv := newServerCore(newTestConfig(false), &mockContainerManager{})

	body := `{"server_name":"server-1"}`
	req := httptest.NewRequest("POST", "/api/sessions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.CreateSession(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want %d", w.Code, http.StatusAccepted)
	}
}

func TestCreateSession_ServerNotFound(t *testing.T) {
	srv := newServerCore(newTestConfig(false), &mockContainerManager{})

	body := `{"server_name":"nonexistent"}`
	req := httptest.NewRequest("POST", "/api/sessions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.CreateSession(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestCreateSession_InvalidBody(t *testing.T) {
	srv := newServerCore(newTestConfig(false), &mockContainerManager{})

	req := httptest.NewRequest("POST", "/api/sessions", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.CreateSession(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestListServers_ActiveSessionFlag(t *testing.T) {
	srv := newServerCore(newTestConfig(false), &mockContainerManager{})

	// Add an active session for server-1
	srv.Sessions.Set(&models.KVMSession{
		ID:         "test-sess",
		ServerName: "server-1",
		Status:     models.SessionConnected,
	})

	req := httptest.NewRequest("GET", "/api/servers", nil)
	w := httptest.NewRecorder()
	srv.ListServers(w, req)

	var servers []ServerInfo
	if err := json.NewDecoder(w.Body).Decode(&servers); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	for _, s := range servers {
		if s.Name == "server-1" && !s.HasActive {
			t.Error("server-1 should have has_active_session=true")
		}
		if s.Name == "server-2" && s.HasActive {
			t.Error("server-2 should have has_active_session=false")
		}
	}
}

func TestListServers_EmptyArrayNotNull(t *testing.T) {
	srv := newServerCore(newTestConfig(true), &mockContainerManager{})

	// User with no matching roles
	user := &models.UserInfo{Email: "nobody@test.com", Roles: []string{"viewer"}}
	ctx := context.WithValue(context.Background(), kvmoidc.UserContextKey, user)
	req := httptest.NewRequest("GET", "/api/servers", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.ListServers(w, req)

	// Should return [] not null
	body := strings.TrimSpace(w.Body.String())
	if body != "[]" {
		t.Errorf("body = %q, want []", body)
	}
}
