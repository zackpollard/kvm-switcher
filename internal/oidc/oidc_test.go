package oidc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zackpollard/kvm-switcher/internal/models"
	"golang.org/x/oauth2"
)

func TestUserCanAccessServer(t *testing.T) {
	cfg := &models.OIDCConfig{
		RoleMappings: map[string]*models.RoleMapping{
			"admin": {Servers: []string{"*"}},
			"ops":   {Servers: []string{"server-1", "server-2"}},
			"dev":   {Servers: []string{"server-3"}},
		},
	}

	tests := []struct {
		name       string
		user       *models.UserInfo
		serverName string
		want       bool
	}{
		{
			name:       "nil user denied",
			user:       nil,
			serverName: "server-1",
			want:       false,
		},
		{
			name:       "admin wildcard access",
			user:       &models.UserInfo{Roles: []string{"admin"}},
			serverName: "server-1",
			want:       true,
		},
		{
			name:       "admin wildcard any server",
			user:       &models.UserInfo{Roles: []string{"admin"}},
			serverName: "server-999",
			want:       true,
		},
		{
			name:       "ops can access server-1",
			user:       &models.UserInfo{Roles: []string{"ops"}},
			serverName: "server-1",
			want:       true,
		},
		{
			name:       "ops can access server-2",
			user:       &models.UserInfo{Roles: []string{"ops"}},
			serverName: "server-2",
			want:       true,
		},
		{
			name:       "ops cannot access server-3",
			user:       &models.UserInfo{Roles: []string{"ops"}},
			serverName: "server-3",
			want:       false,
		},
		{
			name:       "dev can access server-3",
			user:       &models.UserInfo{Roles: []string{"dev"}},
			serverName: "server-3",
			want:       true,
		},
		{
			name:       "dev cannot access server-1",
			user:       &models.UserInfo{Roles: []string{"dev"}},
			serverName: "server-1",
			want:       false,
		},
		{
			name:       "multi-role user gets union of access",
			user:       &models.UserInfo{Roles: []string{"ops", "dev"}},
			serverName: "server-3",
			want:       true,
		},
		{
			name:       "multi-role checks all roles",
			user:       &models.UserInfo{Roles: []string{"ops", "dev"}},
			serverName: "server-1",
			want:       true,
		},
		{
			name:       "unknown role denied",
			user:       &models.UserInfo{Roles: []string{"viewer"}},
			serverName: "server-1",
			want:       false,
		},
		{
			name:       "empty roles denied",
			user:       &models.UserInfo{Roles: []string{}},
			serverName: "server-1",
			want:       false,
		},
		{
			name:       "nil roles denied",
			user:       &models.UserInfo{Roles: nil},
			serverName: "server-1",
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := UserCanAccessServer(cfg, tt.user, tt.serverName)
			if got != tt.want {
				t.Errorf("UserCanAccessServer() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExtractRoles(t *testing.T) {
	tests := []struct {
		name      string
		roleClaim string
		claims    map[string]interface{}
		want      []string
	}{
		{
			name:      "array claim",
			roleClaim: "groups",
			claims:    map[string]interface{}{"groups": []interface{}{"admin", "ops"}},
			want:      []string{"admin", "ops"},
		},
		{
			name:      "string claim",
			roleClaim: "role",
			claims:    map[string]interface{}{"role": "admin"},
			want:      []string{"admin"},
		},
		{
			name:      "missing claim",
			roleClaim: "groups",
			claims:    map[string]interface{}{"email": "user@test.com"},
			want:      nil,
		},
		{
			name:      "default claim key is groups",
			roleClaim: "",
			claims:    map[string]interface{}{"groups": []interface{}{"ops"}},
			want:      []string{"ops"},
		},
		{
			name:      "non-string items in array are skipped",
			roleClaim: "groups",
			claims:    map[string]interface{}{"groups": []interface{}{"admin", 42, true}},
			want:      []string{"admin"},
		},
		{
			name:      "numeric claim returns nil",
			roleClaim: "groups",
			claims:    map[string]interface{}{"groups": 42},
			want:      nil,
		},
		{
			name:      "empty array",
			roleClaim: "groups",
			claims:    map[string]interface{}{"groups": []interface{}{}},
			want:      []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Provider{
				config: &models.OIDCConfig{RoleClaim: tt.roleClaim},
			}
			got := p.extractRoles(tt.claims)
			if tt.want == nil {
				if got != nil {
					t.Errorf("extractRoles() = %v, want nil", got)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Errorf("extractRoles() = %v, want %v", got, tt.want)
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("extractRoles()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestStringClaim(t *testing.T) {
	claims := map[string]interface{}{
		"email": "user@test.com",
		"count": 42,
		"flag":  true,
	}

	if got := stringClaim(claims, "email"); got != "user@test.com" {
		t.Errorf("stringClaim(email) = %q, want %q", got, "user@test.com")
	}
	if got := stringClaim(claims, "count"); got != "" {
		t.Errorf("stringClaim(count) = %q, want empty", got)
	}
	if got := stringClaim(claims, "missing"); got != "" {
		t.Errorf("stringClaim(missing) = %q, want empty", got)
	}
}

func TestUserFromContext(t *testing.T) {
	t.Run("no user in context", func(t *testing.T) {
		ctx := context.Background()
		if user := UserFromContext(ctx); user != nil {
			t.Errorf("expected nil, got %v", user)
		}
	})

	t.Run("user in context", func(t *testing.T) {
		user := &models.UserInfo{Email: "test@example.com", Roles: []string{"admin"}}
		ctx := context.WithValue(context.Background(), UserContextKey, user)
		got := UserFromContext(ctx)
		if got == nil {
			t.Fatal("expected user, got nil")
		}
		if got.Email != "test@example.com" {
			t.Errorf("email = %q, want %q", got.Email, "test@example.com")
		}
	})
}

// newTestProvider creates a Provider with pre-loaded sessions for testing handlers.
func newTestProvider(sessions map[string]*models.UserSession) *Provider {
	return &Provider{
		config: &models.OIDCConfig{
			RoleClaim: "groups",
			RoleMappings: map[string]*models.RoleMapping{
				"admin": {Servers: []string{"*"}},
			},
		},
		sessions: sessions,
	}
}

func TestHandleMe_Unauthenticated(t *testing.T) {
	p := newTestProvider(map[string]*models.UserSession{})

	req := httptest.NewRequest("GET", "/auth/me", nil)
	w := httptest.NewRecorder()
	p.HandleMe(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["authenticated"] != false {
		t.Errorf("authenticated = %v, want false", resp["authenticated"])
	}
}

func TestHandleMe_Authenticated(t *testing.T) {
	sessions := map[string]*models.UserSession{
		"test-session-id": {
			ID: "test-session-id",
			User: &models.UserInfo{
				Email: "admin@test.com",
				Name:  "Admin User",
				Roles: []string{"admin"},
			},
		},
	}
	p := newTestProvider(sessions)

	req := httptest.NewRequest("GET", "/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "test-session-id"})
	w := httptest.NewRecorder()
	p.HandleMe(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["authenticated"] != true {
		t.Errorf("authenticated = %v, want true", resp["authenticated"])
	}
	if resp["email"] != "admin@test.com" {
		t.Errorf("email = %v, want admin@test.com", resp["email"])
	}
	if resp["name"] != "Admin User" {
		t.Errorf("name = %v, want Admin User", resp["name"])
	}
}

func TestHandleMe_ExpiredSession(t *testing.T) {
	p := newTestProvider(map[string]*models.UserSession{})

	req := httptest.NewRequest("GET", "/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "nonexistent-id"})
	w := httptest.NewRecorder()
	p.HandleMe(w, req)

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["authenticated"] != false {
		t.Errorf("authenticated = %v, want false for expired session", resp["authenticated"])
	}
}

func TestMiddleware_NoCookie(t *testing.T) {
	p := newTestProvider(map[string]*models.UserSession{})
	handler := p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called")
	}))

	req := httptest.NewRequest("GET", "/api/servers", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestMiddleware_InvalidSession(t *testing.T) {
	p := newTestProvider(map[string]*models.UserSession{})
	handler := p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called")
	}))

	req := httptest.NewRequest("GET", "/api/servers", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "bad-session"})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestMiddleware_ValidSession(t *testing.T) {
	user := &models.UserInfo{Email: "test@test.com", Roles: []string{"admin"}}
	sessions := map[string]*models.UserSession{
		"valid-session": {ID: "valid-session", User: user},
	}
	p := newTestProvider(sessions)

	var ctxUser *models.UserInfo
	handler := p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctxUser = UserFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/api/servers", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "valid-session"})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if ctxUser == nil {
		t.Fatal("expected user in context, got nil")
	}
	if ctxUser.Email != "test@test.com" {
		t.Errorf("email = %q, want %q", ctxUser.Email, "test@test.com")
	}
}

func TestHandleLogout(t *testing.T) {
	sessions := map[string]*models.UserSession{
		"sess-to-delete": {
			ID:   "sess-to-delete",
			User: &models.UserInfo{Email: "user@test.com"},
		},
	}
	p := newTestProvider(sessions)

	req := httptest.NewRequest("GET", "/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "sess-to-delete"})
	w := httptest.NewRecorder()
	p.HandleLogout(w, req)

	// Should redirect
	if w.Code != http.StatusFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusFound)
	}

	// Session should be removed
	p.mu.RLock()
	_, exists := p.sessions["sess-to-delete"]
	p.mu.RUnlock()
	if exists {
		t.Error("session should have been deleted after logout")
	}

	// Cookie should be cleared
	cookies := w.Result().Cookies()
	for _, c := range cookies {
		if c.Name == sessionCookieName && c.MaxAge != -1 {
			t.Errorf("session cookie MaxAge = %d, want -1", c.MaxAge)
		}
	}
}

func TestHandleLogin_RedirectsWithState(t *testing.T) {
	p := &Provider{
		config: &models.OIDCConfig{},
		oauth2Cfg: &oauth2.Config{
			ClientID: "test-client",
			Endpoint: oauth2.Endpoint{
				AuthURL:  "https://idp.example.com/authorize",
				TokenURL: "https://idp.example.com/token",
			},
		},
		sessions: make(map[string]*models.UserSession),
	}

	req := httptest.NewRequest("GET", "/auth/login", nil)
	w := httptest.NewRecorder()
	p.HandleLogin(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusFound)
	}

	// Should set state cookie
	cookies := w.Result().Cookies()
	var stateCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == stateCookieName {
			stateCookie = c
			break
		}
	}
	if stateCookie == nil {
		t.Fatal("expected state cookie to be set")
	}
	if stateCookie.Value == "" {
		t.Error("state cookie value should not be empty")
	}
	if !stateCookie.HttpOnly {
		t.Error("state cookie should be HttpOnly")
	}

	// Should redirect to auth URL containing the IdP authorize endpoint
	location := w.Header().Get("Location")
	if location == "" {
		t.Fatal("expected Location header")
	}
	if !strings.Contains(location, "idp.example.com/authorize") {
		t.Errorf("Location = %q, should contain IdP authorize URL", location)
	}
	if !strings.Contains(location, "client_id=test-client") {
		t.Errorf("Location = %q, should contain client_id", location)
	}
}

func TestRandomString(t *testing.T) {
	s1, err := randomString(32)
	if err != nil {
		t.Fatalf("randomString error: %v", err)
	}
	if len(s1) != 64 { // hex encoding doubles length
		t.Errorf("len = %d, want 64", len(s1))
	}

	s2, _ := randomString(32)
	if s1 == s2 {
		t.Error("two random strings should not be equal")
	}
}
