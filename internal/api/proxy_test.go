package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zackpollard/kvm-switcher/internal/models"
	kvmoidc "github.com/zackpollard/kvm-switcher/internal/oidc"
)

// newMockBMC creates a test HTTP server simulating a BMC web interface.
func newMockBMC(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><head><link href="/css/style.css"></head><body><a href="/dashboard">Dashboard</a><script src="/js/app.js"></script></body></html>`)
	})

	mux.HandleFunc("GET /dashboard", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><h1>BMC Dashboard</h1><a href="/">Home</a></body></html>`)
	})

	mux.HandleFunc("GET /css/style.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		fmt.Fprint(w, `body { background: url("/images/bg.png"); }`)
	})

	mux.HandleFunc("GET /js/app.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		fmt.Fprint(w, `var apiURL = "/rpc/status"; fetch("/api/data");`)
	})

	mux.HandleFunc("GET /images/logo.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte{0x89, 0x50, 0x4E, 0x47}) // PNG magic bytes
	})

	mux.HandleFunc("GET /redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard", http.StatusFound)
	})

	mux.HandleFunc("POST /rpc/login", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "SessionCookie", Value: "abc123", Path: "/"})
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok"}`)
	})

	mux.HandleFunc("GET /api/check-cookies", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Check that our auth cookies are NOT forwarded
		hasKVMSession := false
		for _, c := range r.Cookies() {
			if c.Name == "kvm_session" {
				hasKVMSession = true
			}
		}
		fmt.Fprintf(w, `{"has_kvm_session":%v}`, hasKVMSession)
	})

	return httptest.NewServer(mux)
}

// configWithBMC creates a test config pointing to the mock BMC server.
func configWithBMC(t *testing.T, bmcAddr string, oidcEnabled bool) *models.AppConfig {
	t.Helper()
	// Parse host:port from the test server address
	host, portStr, _ := strings.Cut(strings.TrimPrefix(bmcAddr, "http://"), ":")
	port := 80
	if portStr != "" {
		fmt.Sscanf(portStr, "%d", &port)
	}

	cfg := &models.AppConfig{
		Servers: []models.ServerConfig{
			{Name: "test-bmc", BMCIP: host, BMCPort: port, BoardType: "ami_megarac", Username: "admin", CredentialEnv: "PASS"},
			{Name: "other-bmc", BMCIP: "192.168.1.1", BMCPort: 80, BoardType: "ami_megarac", Username: "admin", CredentialEnv: "PASS2"},
		},
		Settings: models.Settings{MaxConcurrentSessions: 4, Runtime: "docker"},
	}

	if oidcEnabled {
		cfg.OIDC = models.OIDCConfig{
			Enabled:   true,
			RoleClaim: "groups",
			RoleMappings: map[string]*models.RoleMapping{
				"admin": {Servers: []string{"*"}},
				"ops":   {Servers: []string{"test-bmc"}},
			},
		}
	}

	return cfg
}

func TestIPMIProxy_HTMLRewriting(t *testing.T) {
	bmc := newMockBMC(t)
	defer bmc.Close()

	srv := NewServer(configWithBMC(t, bmc.URL, false), &mockContainerManager{})

	req := httptest.NewRequest("GET", "/ipmi/test-bmc/", nil)
	w := httptest.NewRecorder()
	srv.HandleIPMIProxy(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	body := w.Body.String()

	// Check that absolute paths have been rewritten
	if !strings.Contains(body, `href="/ipmi/test-bmc/css/style.css"`) {
		t.Errorf("CSS link not rewritten.\nbody: %s", body)
	}
	if !strings.Contains(body, `href="/ipmi/test-bmc/dashboard"`) {
		t.Errorf("dashboard link not rewritten.\nbody: %s", body)
	}
	if !strings.Contains(body, `src="/ipmi/test-bmc/js/app.js"`) {
		t.Errorf("JS src not rewritten.\nbody: %s", body)
	}
}

func TestIPMIProxy_CSSRewriting(t *testing.T) {
	bmc := newMockBMC(t)
	defer bmc.Close()

	srv := NewServer(configWithBMC(t, bmc.URL, false), &mockContainerManager{})

	req := httptest.NewRequest("GET", "/ipmi/test-bmc/css/style.css", nil)
	w := httptest.NewRecorder()
	srv.HandleIPMIProxy(w, req)

	body := w.Body.String()
	if !strings.Contains(body, `url("/ipmi/test-bmc/images/bg.png")`) {
		t.Errorf("CSS url() not rewritten.\nbody: %s", body)
	}
}

func TestIPMIProxy_JSRewriting(t *testing.T) {
	bmc := newMockBMC(t)
	defer bmc.Close()

	srv := NewServer(configWithBMC(t, bmc.URL, false), &mockContainerManager{})

	req := httptest.NewRequest("GET", "/ipmi/test-bmc/js/app.js", nil)
	w := httptest.NewRecorder()
	srv.HandleIPMIProxy(w, req)

	body := w.Body.String()
	if !strings.Contains(body, `"/ipmi/test-bmc/rpc/status"`) {
		t.Errorf("JS string literal not rewritten.\nbody: %s", body)
	}
	if !strings.Contains(body, `"/ipmi/test-bmc/api/data"`) {
		t.Errorf("JS fetch URL not rewritten.\nbody: %s", body)
	}
}

func TestIPMIProxy_BinaryPassthrough(t *testing.T) {
	bmc := newMockBMC(t)
	defer bmc.Close()

	srv := NewServer(configWithBMC(t, bmc.URL, false), &mockContainerManager{})

	req := httptest.NewRequest("GET", "/ipmi/test-bmc/images/logo.png", nil)
	w := httptest.NewRecorder()
	srv.HandleIPMIProxy(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	body := w.Body.Bytes()
	// PNG magic bytes should pass through unmodified
	if len(body) < 4 || body[0] != 0x89 || body[1] != 0x50 {
		t.Errorf("binary content was corrupted")
	}
}

func TestIPMIProxy_RedirectRewriting(t *testing.T) {
	bmc := newMockBMC(t)
	defer bmc.Close()

	srv := NewServer(configWithBMC(t, bmc.URL, false), &mockContainerManager{})

	req := httptest.NewRequest("GET", "/ipmi/test-bmc/redirect", nil)
	w := httptest.NewRecorder()
	srv.HandleIPMIProxy(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}

	loc := w.Header().Get("Location")
	if loc != "/ipmi/test-bmc/dashboard" {
		t.Errorf("Location = %q, want /ipmi/test-bmc/dashboard", loc)
	}
}

func TestIPMIProxy_CookiePathRewriting(t *testing.T) {
	bmc := newMockBMC(t)
	defer bmc.Close()

	srv := NewServer(configWithBMC(t, bmc.URL, false), &mockContainerManager{})

	req := httptest.NewRequest("POST", "/ipmi/test-bmc/rpc/login", nil)
	w := httptest.NewRecorder()
	srv.HandleIPMIProxy(w, req)

	cookies := w.Result().Cookies()
	for _, c := range cookies {
		if c.Name == "SessionCookie" {
			if !strings.HasPrefix(c.Path, "/ipmi/test-bmc") {
				t.Errorf("cookie path = %q, want prefix /ipmi/test-bmc", c.Path)
			}
			return
		}
	}
	t.Error("SessionCookie not found in response")
}

func TestIPMIProxy_AuthCookiesStripped(t *testing.T) {
	bmc := newMockBMC(t)
	defer bmc.Close()

	srv := NewServer(configWithBMC(t, bmc.URL, false), &mockContainerManager{})

	req := httptest.NewRequest("GET", "/ipmi/test-bmc/api/check-cookies", nil)
	req.AddCookie(&http.Cookie{Name: "kvm_session", Value: "should-be-stripped"})
	req.AddCookie(&http.Cookie{Name: "BMCSession", Value: "should-pass-through"})
	w := httptest.NewRecorder()
	srv.HandleIPMIProxy(w, req)

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["has_kvm_session"] == true {
		t.Error("kvm_session cookie should have been stripped before forwarding to BMC")
	}
}

func TestIPMIProxy_ServerNotFound(t *testing.T) {
	srv := NewServer(newTestConfig(false), &mockContainerManager{})

	req := httptest.NewRequest("GET", "/ipmi/nonexistent/", nil)
	w := httptest.NewRecorder()
	srv.HandleIPMIProxy(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestIPMIProxy_MissingServerName(t *testing.T) {
	srv := NewServer(newTestConfig(false), &mockContainerManager{})

	req := httptest.NewRequest("GET", "/ipmi/", nil)
	w := httptest.NewRecorder()
	srv.HandleIPMIProxy(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestIPMIProxy_OIDCForbidden(t *testing.T) {
	bmc := newMockBMC(t)
	defer bmc.Close()

	srv := NewServer(configWithBMC(t, bmc.URL, true), &mockContainerManager{})

	// User with "ops" role can access "test-bmc" but not "other-bmc"
	user := &models.UserInfo{Email: "ops@test.com", Roles: []string{"ops"}}
	ctx := context.WithValue(context.Background(), kvmoidc.UserContextKey, user)

	req := httptest.NewRequest("GET", "/ipmi/other-bmc/", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleIPMIProxy(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestIPMIProxy_OIDCAllowed(t *testing.T) {
	bmc := newMockBMC(t)
	defer bmc.Close()

	srv := NewServer(configWithBMC(t, bmc.URL, true), &mockContainerManager{})

	user := &models.UserInfo{Email: "ops@test.com", Roles: []string{"ops"}}
	ctx := context.WithValue(context.Background(), kvmoidc.UserContextKey, user)

	req := httptest.NewRequest("GET", "/ipmi/test-bmc/", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleIPMIProxy(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestRewriteURL(t *testing.T) {
	prefix := "/ipmi/srv1"
	origin := "http://10.0.0.1:80"
	originNoPort := "http://10.0.0.1"

	tests := []struct {
		input string
		want  string
	}{
		{"/dashboard", "/ipmi/srv1/dashboard"},
		{"/", "/ipmi/srv1/"},
		{"http://10.0.0.1:80/page", "/ipmi/srv1/page"},
		{"http://10.0.0.1:80", "/ipmi/srv1"},
		{"http://10.0.0.1/page", "/ipmi/srv1/page"},
		{"http://10.0.0.1", "/ipmi/srv1"},
		{"/ipmi/srv1/already", "/ipmi/srv1/already"}, // no double rewrite
		{"https://external.com/page", "https://external.com/page"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := rewriteURL(tt.input, prefix, origin, originNoPort)
			if got != tt.want {
				t.Errorf("rewriteURL(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsTextContent(t *testing.T) {
	tests := []struct {
		ct   string
		want bool
	}{
		{"text/html; charset=utf-8", true},
		{"text/css", true},
		{"application/javascript", true},
		{"text/javascript", true},
		{"application/json", true},
		{"text/xml", true},
		{"image/png", false},
		{"application/octet-stream", false},
		{"font/woff2", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.ct, func(t *testing.T) {
			if got := isTextContent(tt.ct); got != tt.want {
				t.Errorf("isTextContent(%q) = %v, want %v", tt.ct, got, tt.want)
			}
		})
	}
}

func TestRewriteAbsolutePaths(t *testing.T) {
	prefix := []byte("/ipmi/srv1")

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"double-quoted href", `href="/page"`, `href="/ipmi/srv1/page"`},
		{"single-quoted src", `src='/js/app.js'`, `src='/ipmi/srv1/js/app.js'`},
		{"css url paren", `url(/images/bg.png)`, `url(/ipmi/srv1/images/bg.png)`},
		{"css url quoted", `url("/images/bg.png")`, `url("/ipmi/srv1/images/bg.png")`},
		{"js string", `var x = "/api/call"`, `var x = "/ipmi/srv1/api/call"`},
		{"protocol-relative", `src="//cdn.example.com/js"`, `src="//cdn.example.com/js"`},
		{"already rewritten", `href="/ipmi/srv1/page"`, `href="/ipmi/srv1/page"`},
		{"equals no quotes", `action=/login`, `action=/ipmi/srv1/login`},
		{"multiple paths", `href="/a" src="/b"`, `href="/ipmi/srv1/a" src="/ipmi/srv1/b"`},
		{"no paths to rewrite", `<p>hello</p>`, `<p>hello</p>`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(rewriteAbsolutePaths([]byte(tt.input), prefix))
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
