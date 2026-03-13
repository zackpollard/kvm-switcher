package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/zackpollard/kvm-switcher/internal/models"
)

// mockOIDCServer sets up a test OIDC provider with discovery, JWKS, and token endpoint.
type mockOIDCServer struct {
	server     *httptest.Server
	privateKey *rsa.PrivateKey
	keyID      string
}

func newMockOIDCServer(t *testing.T) *mockOIDCServer {
	t.Helper()

	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}

	m := &mockOIDCServer{
		privateKey: privKey,
		keyID:      "test-key-1",
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", m.handleDiscovery)
	mux.HandleFunc("GET /jwks", m.handleJWKS)
	mux.HandleFunc("POST /token", m.handleToken)
	mux.HandleFunc("GET /authorize", m.handleAuthorize)

	m.server = httptest.NewServer(mux)
	return m
}

func (m *mockOIDCServer) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	doc := map[string]interface{}{
		"issuer":                 m.server.URL,
		"authorization_endpoint": m.server.URL + "/authorize",
		"token_endpoint":         m.server.URL + "/token",
		"jwks_uri":               m.server.URL + "/jwks",
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"subject_types_supported":               []string{"public"},
		"response_types_supported":              []string{"code"},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(doc)
}

func (m *mockOIDCServer) handleJWKS(w http.ResponseWriter, r *http.Request) {
	jwk := jose.JSONWebKey{
		Key:       &m.privateKey.PublicKey,
		KeyID:     m.keyID,
		Algorithm: string(jose.RS256),
		Use:       "sig",
	}
	jwks := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{jwk}}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(jwks)
}

func (m *mockOIDCServer) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	// Simulate IdP redirecting back with auth code
	state := r.URL.Query().Get("state")
	redirectURI := r.URL.Query().Get("redirect_uri")
	u, _ := url.Parse(redirectURI)
	q := u.Query()
	q.Set("code", "test-auth-code")
	q.Set("state", state)
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (m *mockOIDCServer) handleToken(w http.ResponseWriter, r *http.Request) {
	// Issue a signed ID token
	idToken := m.issueIDToken(map[string]interface{}{
		"email":  "testuser@example.com",
		"name":   "Test User",
		"groups": []string{"admin", "ops"},
	})

	resp := map[string]interface{}{
		"access_token":  "mock-access-token",
		"token_type":    "Bearer",
		"expires_in":    3600,
		"id_token":      idToken,
		"refresh_token": "mock-refresh-token",
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (m *mockOIDCServer) issueIDToken(extraClaims map[string]interface{}) string {
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: m.privateKey},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", m.keyID),
	)
	if err != nil {
		panic(err)
	}

	now := time.Now()
	claims := jwt.Claims{
		Issuer:    m.server.URL,
		Subject:   "test-subject",
		Audience:  jwt.Audience{"test-client"},
		IssuedAt:  jwt.NewNumericDate(now),
		Expiry:    jwt.NewNumericDate(now.Add(time.Hour)),
		NotBefore: jwt.NewNumericDate(now.Add(-time.Minute)),
	}

	token, err := jwt.Signed(signer).Claims(claims).Claims(extraClaims).Serialize()
	if err != nil {
		panic(err)
	}
	return token
}

func (m *mockOIDCServer) close() {
	m.server.Close()
}

// TestIntegration_FullLoginFlow tests the complete OIDC flow:
// 1. Initialize provider with mock OIDC server
// 2. Hit /auth/login -> get redirect + state cookie
// 3. Hit /auth/callback with code + state -> get session cookie
// 4. Hit /auth/me with session cookie -> get user info
// 5. Hit protected API with middleware -> passes
// 6. Hit /auth/logout -> session cleared
func TestIntegration_FullLoginFlow(t *testing.T) {
	mock := newMockOIDCServer(t)
	defer mock.close()

	cfg := &models.OIDCConfig{
		Enabled:         true,
		IssuerURL:       mock.server.URL,
		ClientID:        "test-client",
		ClientSecretEnv: "TEST_OIDC_SECRET",
		RedirectURL:     "http://localhost:9999/auth/callback",
		RoleClaim:       "groups",
		RoleMappings: map[string]*models.RoleMapping{
			"admin": {Servers: []string{"*"}},
			"ops":   {Servers: []string{"server-1", "server-2"}},
		},
	}

	t.Setenv("TEST_OIDC_SECRET", "test-secret")

	provider, err := NewProvider(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewProvider() error: %v", err)
	}

	// Step 1: Login redirect
	loginReq := httptest.NewRequest("GET", "/auth/login", nil)
	loginW := httptest.NewRecorder()
	provider.HandleLogin(loginW, loginReq)

	if loginW.Code != http.StatusFound {
		t.Fatalf("login status = %d, want %d", loginW.Code, http.StatusFound)
	}

	var stateCookieValue string
	for _, c := range loginW.Result().Cookies() {
		if c.Name == stateCookieName {
			stateCookieValue = c.Value
			break
		}
	}
	if stateCookieValue == "" {
		t.Fatal("expected state cookie after login")
	}

	// Step 2: Callback with code and state
	callbackURL := fmt.Sprintf("/auth/callback?code=test-auth-code&state=%s", stateCookieValue)
	callbackReq := httptest.NewRequest("GET", callbackURL, nil)
	callbackReq.AddCookie(&http.Cookie{Name: stateCookieName, Value: stateCookieValue})
	callbackW := httptest.NewRecorder()
	provider.HandleCallback(callbackW, callbackReq)

	if callbackW.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want %d (body: %s)", callbackW.Code, http.StatusFound, callbackW.Body.String())
	}

	var sessionCookieValue string
	for _, c := range callbackW.Result().Cookies() {
		if c.Name == sessionCookieName {
			sessionCookieValue = c.Value
			break
		}
	}
	if sessionCookieValue == "" {
		t.Fatal("expected session cookie after callback")
	}

	// Step 3: Check /auth/me returns user info
	meReq := httptest.NewRequest("GET", "/auth/me", nil)
	meReq.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionCookieValue})
	meW := httptest.NewRecorder()
	provider.HandleMe(meW, meReq)

	var meResp map[string]interface{}
	json.NewDecoder(meW.Body).Decode(&meResp)
	if meResp["authenticated"] != true {
		t.Errorf("authenticated = %v, want true", meResp["authenticated"])
	}
	if meResp["email"] != "testuser@example.com" {
		t.Errorf("email = %v, want testuser@example.com", meResp["email"])
	}
	if meResp["name"] != "Test User" {
		t.Errorf("name = %v, want Test User", meResp["name"])
	}

	roles, _ := meResp["roles"].([]interface{})
	if len(roles) != 2 {
		t.Errorf("roles = %v, want [admin ops]", roles)
	}

	// Step 4: Middleware passes for authenticated user
	var ctxUser *models.UserInfo
	protected := provider.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctxUser = UserFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	apiReq := httptest.NewRequest("GET", "/api/servers", nil)
	apiReq.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionCookieValue})
	apiW := httptest.NewRecorder()
	protected.ServeHTTP(apiW, apiReq)

	if apiW.Code != http.StatusOK {
		t.Errorf("protected API status = %d, want %d", apiW.Code, http.StatusOK)
	}
	if ctxUser == nil {
		t.Fatal("expected user in context after middleware")
	}
	if ctxUser.Email != "testuser@example.com" {
		t.Errorf("context user email = %q, want testuser@example.com", ctxUser.Email)
	}

	// Step 5: Verify role-based access
	if !UserCanAccessServer(cfg, ctxUser, "server-1") {
		t.Error("admin+ops user should access server-1")
	}
	if !UserCanAccessServer(cfg, ctxUser, "server-2") {
		t.Error("admin+ops user should access server-2")
	}
	if !UserCanAccessServer(cfg, ctxUser, "any-server") {
		t.Error("admin user should access any server via wildcard")
	}

	// Step 6: Logout
	logoutReq := httptest.NewRequest("GET", "/auth/logout", nil)
	logoutReq.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionCookieValue})
	logoutW := httptest.NewRecorder()
	provider.HandleLogout(logoutW, logoutReq)

	if logoutW.Code != http.StatusFound {
		t.Errorf("logout status = %d, want %d", logoutW.Code, http.StatusFound)
	}

	// Step 7: After logout, /auth/me should return unauthenticated
	meReq2 := httptest.NewRequest("GET", "/auth/me", nil)
	meReq2.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionCookieValue})
	meW2 := httptest.NewRecorder()
	provider.HandleMe(meW2, meReq2)

	var meResp2 map[string]interface{}
	json.NewDecoder(meW2.Body).Decode(&meResp2)
	if meResp2["authenticated"] != false {
		t.Errorf("after logout, authenticated = %v, want false", meResp2["authenticated"])
	}

	// Step 8: Middleware blocks after logout
	apiReq2 := httptest.NewRequest("GET", "/api/servers", nil)
	apiReq2.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionCookieValue})
	apiW2 := httptest.NewRecorder()
	protected.ServeHTTP(apiW2, apiReq2)

	if apiW2.Code != http.StatusUnauthorized {
		t.Errorf("after logout, API status = %d, want %d", apiW2.Code, http.StatusUnauthorized)
	}
}

// TestIntegration_RoleMappingFiltering tests that role mappings correctly filter access.
func TestIntegration_RoleMappingFiltering(t *testing.T) {
	mock := newMockOIDCServer(t)
	defer mock.close()

	cfg := &models.OIDCConfig{
		Enabled:         true,
		IssuerURL:       mock.server.URL,
		ClientID:        "test-client",
		ClientSecretEnv: "TEST_OIDC_SECRET",
		RedirectURL:     "http://localhost:9999/auth/callback",
		RoleClaim:       "groups",
		RoleMappings: map[string]*models.RoleMapping{
			"admin":    {Servers: []string{"*"}},
			"ops":      {Servers: []string{"server-1", "server-2"}},
			"dev":      {Servers: []string{"server-3"}},
			"readonly": {Servers: []string{"server-1"}},
		},
	}

	tests := []struct {
		name           string
		roles          []string
		canAccess      []string
		cannotAccess   []string
	}{
		{
			name:         "admin accesses everything",
			roles:        []string{"admin"},
			canAccess:    []string{"server-1", "server-2", "server-3", "server-99"},
			cannotAccess: nil,
		},
		{
			name:         "ops limited to server-1 and server-2",
			roles:        []string{"ops"},
			canAccess:    []string{"server-1", "server-2"},
			cannotAccess: []string{"server-3", "server-99"},
		},
		{
			name:         "dev limited to server-3",
			roles:        []string{"dev"},
			canAccess:    []string{"server-3"},
			cannotAccess: []string{"server-1", "server-2"},
		},
		{
			name:         "readonly limited to server-1",
			roles:        []string{"readonly"},
			canAccess:    []string{"server-1"},
			cannotAccess: []string{"server-2", "server-3"},
		},
		{
			name:         "ops+dev union",
			roles:        []string{"ops", "dev"},
			canAccess:    []string{"server-1", "server-2", "server-3"},
			cannotAccess: []string{"server-99"},
		},
		{
			name:         "unknown role",
			roles:        []string{"unknown"},
			canAccess:    nil,
			cannotAccess: []string{"server-1", "server-2", "server-3"},
		},
		{
			name:         "empty roles",
			roles:        []string{},
			canAccess:    nil,
			cannotAccess: []string{"server-1", "server-2", "server-3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := &models.UserInfo{Email: "test@test.com", Roles: tt.roles}

			for _, srv := range tt.canAccess {
				if !UserCanAccessServer(cfg, user, srv) {
					t.Errorf("roles %v should access %q but cannot", tt.roles, srv)
				}
			}
			for _, srv := range tt.cannotAccess {
				if UserCanAccessServer(cfg, user, srv) {
					t.Errorf("roles %v should NOT access %q but can", tt.roles, srv)
				}
			}
		})
	}
}

// TestIntegration_CallbackStateMismatch tests that state mismatches are rejected.
func TestIntegration_CallbackStateMismatch(t *testing.T) {
	mock := newMockOIDCServer(t)
	defer mock.close()

	cfg := &models.OIDCConfig{
		Enabled:         true,
		IssuerURL:       mock.server.URL,
		ClientID:        "test-client",
		ClientSecretEnv: "TEST_OIDC_SECRET",
		RedirectURL:     "http://localhost:9999/auth/callback",
		RoleClaim:       "groups",
		RoleMappings:    map[string]*models.RoleMapping{"admin": {Servers: []string{"*"}}},
	}

	t.Setenv("TEST_OIDC_SECRET", "test-secret")

	provider, err := NewProvider(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewProvider() error: %v", err)
	}

	// Callback with mismatched state
	callbackReq := httptest.NewRequest("GET", "/auth/callback?code=test-code&state=wrong-state", nil)
	callbackReq.AddCookie(&http.Cookie{Name: stateCookieName, Value: "correct-state"})
	callbackW := httptest.NewRecorder()
	provider.HandleCallback(callbackW, callbackReq)

	if callbackW.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", callbackW.Code, http.StatusBadRequest)
	}
}

// TestIntegration_CallbackMissingStateCookie tests callback without state cookie.
func TestIntegration_CallbackMissingStateCookie(t *testing.T) {
	mock := newMockOIDCServer(t)
	defer mock.close()

	cfg := &models.OIDCConfig{
		Enabled:         true,
		IssuerURL:       mock.server.URL,
		ClientID:        "test-client",
		ClientSecretEnv: "TEST_OIDC_SECRET",
		RedirectURL:     "http://localhost:9999/auth/callback",
		RoleClaim:       "groups",
		RoleMappings:    map[string]*models.RoleMapping{"admin": {Servers: []string{"*"}}},
	}

	t.Setenv("TEST_OIDC_SECRET", "test-secret")

	provider, err := NewProvider(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewProvider() error: %v", err)
	}

	callbackReq := httptest.NewRequest("GET", "/auth/callback?code=test-code&state=some-state", nil)
	// No state cookie
	callbackW := httptest.NewRecorder()
	provider.HandleCallback(callbackW, callbackReq)

	if callbackW.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", callbackW.Code, http.StatusBadRequest)
	}
}

// TestIntegration_MiddlewareBlocksUnauthenticated tests the complete flow of
// middleware blocking unauthenticated requests.
func TestIntegration_MiddlewareBlocksUnauthenticated(t *testing.T) {
	mock := newMockOIDCServer(t)
	defer mock.close()

	cfg := &models.OIDCConfig{
		Enabled:         true,
		IssuerURL:       mock.server.URL,
		ClientID:        "test-client",
		ClientSecretEnv: "TEST_OIDC_SECRET",
		RedirectURL:     "http://localhost:9999/auth/callback",
		RoleClaim:       "groups",
		RoleMappings:    map[string]*models.RoleMapping{"admin": {Servers: []string{"*"}}},
	}

	t.Setenv("TEST_OIDC_SECRET", "test-secret")

	provider, err := NewProvider(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewProvider() error: %v", err)
	}

	protected := provider.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called for unauthenticated request")
	}))

	tests := []struct {
		name   string
		cookie *http.Cookie
	}{
		{"no cookie", nil},
		{"empty cookie", &http.Cookie{Name: sessionCookieName, Value: ""}},
		{"invalid session", &http.Cookie{Name: sessionCookieName, Value: "nonexistent"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/servers", nil)
			if tt.cookie != nil {
				req.AddCookie(tt.cookie)
			}
			w := httptest.NewRecorder()
			protected.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
			}
		})
	}
}
