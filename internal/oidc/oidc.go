package oidc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/zackpollard/kvm-switcher/internal/models"
	"golang.org/x/oauth2"
)

const (
	sessionCookieName = "kvm_session"
	stateCookieName   = "kvm_oauth_state"
)

// Provider manages OIDC authentication.
type Provider struct {
	config    *models.OIDCConfig
	oauth2Cfg *oauth2.Config
	verifier  *gooidc.IDTokenVerifier
	sessions  map[string]*models.UserSession
	mu        sync.RWMutex
}

// NewProvider creates and initializes an OIDC provider.
func NewProvider(ctx context.Context, cfg *models.OIDCConfig) (*Provider, error) {
	provider, err := gooidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, err
	}

	clientSecret := os.Getenv(cfg.ClientSecretEnv)

	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = []string{gooidc.ScopeOpenID, "profile", "email"}
	}

	oauth2Cfg := &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: clientSecret,
		RedirectURL:  cfg.RedirectURL,
		Endpoint:     provider.Endpoint(),
		Scopes:       scopes,
	}

	verifier := provider.Verifier(&gooidc.Config{ClientID: cfg.ClientID})

	return &Provider{
		config:    cfg,
		oauth2Cfg: oauth2Cfg,
		verifier:  verifier,
		sessions:  make(map[string]*models.UserSession),
	}, nil
}

// HandleLogin redirects the user to the OIDC provider.
func (p *Provider) HandleLogin(w http.ResponseWriter, r *http.Request) {
	state, err := randomString(32)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    state,
		Path:     "/",
		MaxAge:   300,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	http.Redirect(w, r, p.oauth2Cfg.AuthCodeURL(state), http.StatusFound)
}

// HandleCallback processes the OIDC callback.
func (p *Provider) HandleCallback(w http.ResponseWriter, r *http.Request) {
	// Verify state
	stateCookie, err := r.Cookie(stateCookieName)
	if err != nil || stateCookie.Value == "" {
		http.Error(w, "missing state cookie", http.StatusBadRequest)
		return
	}

	if r.URL.Query().Get("state") != stateCookie.Value {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}

	// Clear state cookie
	http.SetCookie(w, &http.Cookie{
		Name:   stateCookieName,
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})

	// Exchange code for token
	oauth2Token, err := p.oauth2Cfg.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		log.Printf("OIDC token exchange failed: %v", err)
		http.Error(w, "authentication failed", http.StatusUnauthorized)
		return
	}

	// Extract and verify ID token
	rawIDToken, ok := oauth2Token.Extra("id_token").(string)
	if !ok {
		http.Error(w, "missing id_token", http.StatusUnauthorized)
		return
	}

	idToken, err := p.verifier.Verify(r.Context(), rawIDToken)
	if err != nil {
		log.Printf("OIDC token verification failed: %v", err)
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	// Extract claims
	var claims map[string]interface{}
	if err := idToken.Claims(&claims); err != nil {
		log.Printf("OIDC claims extraction failed: %v", err)
		http.Error(w, "failed to read claims", http.StatusInternalServerError)
		return
	}

	user := &models.UserInfo{
		Email: stringClaim(claims, "email"),
		Name:  stringClaim(claims, "name"),
		Roles: p.extractRoles(claims),
	}

	// Rotate session: invalidate any existing session to prevent session fixation.
	if oldCookie, err := r.Cookie(sessionCookieName); err == nil && oldCookie.Value != "" {
		p.mu.Lock()
		delete(p.sessions, oldCookie.Value)
		p.mu.Unlock()
	}

	// Create server-side session
	sessionID, err := randomString(32)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	session := &models.UserSession{
		ID:           sessionID,
		User:         user,
		IDToken:      rawIDToken,
		RefreshToken: oauth2Token.RefreshToken,
		ExpiresAt:    oauth2Token.Expiry,
	}

	p.mu.Lock()
	p.sessions[sessionID] = session
	p.mu.Unlock()

	log.Printf("OIDC login: %s (%s) with roles %v", user.Email, user.Name, user.Roles)

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400, // 24 hours
	})

	http.Redirect(w, r, "/", http.StatusFound)
}

// HandleLogout clears the session.
func (p *Provider) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		p.mu.Lock()
		delete(p.sessions, cookie.Value)
		p.mu.Unlock()
	}

	http.SetCookie(w, &http.Cookie{
		Name:   sessionCookieName,
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})

	http.Redirect(w, r, "/", http.StatusFound)
}

// HandleMe returns the current user info.
func (p *Provider) HandleMe(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"authenticated":false}`))
		return
	}

	p.mu.RLock()
	session, ok := p.sessions[cookie.Value]
	p.mu.RUnlock()

	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"authenticated":false}`))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"authenticated": true,
		"email":         session.User.Email,
		"name":          session.User.Name,
		"roles":         session.User.Roles,
	})
}

// ContextKey is the type used for context value keys.
type ContextKey string

// UserContextKey is the context key for storing/retrieving user info.
const UserContextKey ContextKey = "user"

// UserFromContext retrieves user info from the request context.
func UserFromContext(ctx context.Context) *models.UserInfo {
	user, _ := ctx.Value(UserContextKey).(*models.UserInfo)
	return user
}

// Middleware returns HTTP middleware that enforces OIDC authentication on API routes.
func (p *Provider) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil || cookie.Value == "" {
			http.Error(w, `{"error":"authentication required"}`, http.StatusUnauthorized)
			return
		}

		p.mu.RLock()
		session, ok := p.sessions[cookie.Value]
		p.mu.RUnlock()

		if !ok {
			http.Error(w, `{"error":"session expired"}`, http.StatusUnauthorized)
			return
		}

		ctx := context.WithValue(r.Context(), UserContextKey, session.User)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// extractRoles gets the user's roles from the configured claim.
func (p *Provider) extractRoles(claims map[string]interface{}) []string {
	claimKey := p.config.RoleClaim
	if claimKey == "" {
		claimKey = "groups"
	}

	raw, ok := claims[claimKey]
	if !ok {
		return nil
	}

	switch v := raw.(type) {
	case []interface{}:
		roles := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				roles = append(roles, s)
			}
		}
		return roles
	case string:
		return []string{v}
	default:
		return nil
	}
}

// UserCanAccessServer checks if a user's roles grant access to a server.
func UserCanAccessServer(cfg *models.OIDCConfig, user *models.UserInfo, serverName string) bool {
	if user == nil {
		return false
	}

	for _, role := range user.Roles {
		mapping, ok := cfg.RoleMappings[role]
		if !ok {
			continue
		}
		for _, s := range mapping.Servers {
			if s == "*" || s == serverName {
				return true
			}
		}
	}
	return false
}

func stringClaim(claims map[string]interface{}, key string) string {
	if v, ok := claims[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
