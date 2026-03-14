package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/zackpollard/kvm-switcher/internal/models"
)

// newMockBMC creates a test HTTP server simulating a BMC web interface.
func newMockBMC(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><head><title>BMC</title></head><body>Hello</body></html>`)
	})

	mux.HandleFunc("GET /page/login.html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body>Login</body></html>`)
	})

	mux.HandleFunc("GET /rpc/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok"}`)
	})

	mux.HandleFunc("GET /redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/page/login.html", http.StatusFound)
	})

	mux.HandleFunc("GET /api/check-cookies", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		hasKVMSession := false
		sessionCookieVal := ""
		csrfToken := r.Header.Get("CSRFTOKEN")
		for _, c := range r.Cookies() {
			if c.Name == "kvm_session" {
				hasKVMSession = true
			}
			if c.Name == "SessionCookie" {
				sessionCookieVal = c.Value
			}
		}
		fmt.Fprintf(w, `{"has_kvm_session":%v,"session_cookie":%q,"csrf_token":%q}`, hasKVMSession, sessionCookieVal, csrfToken)
	})

	return httptest.NewServer(mux)
}

func newTestBMCServer(t *testing.T, bmc *httptest.Server) *Server {
	t.Helper()
	host, portStr, _ := strings.Cut(strings.TrimPrefix(bmc.URL, "http://"), ":")
	port := 80
	fmt.Sscanf(portStr, "%d", &port)

	cfg := &models.AppConfig{
		Servers: []models.ServerConfig{
			{Name: "test-bmc", BMCIP: host, BMCPort: port, BoardType: "ami_megarac", Username: "admin", CredentialEnv: "PASS"},
		},
		Settings: models.Settings{MaxConcurrentSessions: 4, Runtime: "docker"},
	}

	// Clear cached proxy from previous tests
	bmcProxies.Delete("test-bmc")

	return NewServer(cfg, &mockContainerManager{})
}

func TestHandleBMCProxy_BasicRequest(t *testing.T) {
	bmc := newMockBMC(t)
	defer bmc.Close()
	srv := newTestBMCServer(t, bmc)

	req := httptest.NewRequest("GET", "/__bmc/test-bmc/", nil)
	w := httptest.NewRecorder()
	srv.HandleBMCProxy(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "<title>BMC</title>") {
		t.Errorf("unexpected body: %s", body)
	}
}

func TestHandleBMCProxy_SubPath(t *testing.T) {
	bmc := newMockBMC(t)
	defer bmc.Close()
	srv := newTestBMCServer(t, bmc)

	req := httptest.NewRequest("GET", "/__bmc/test-bmc/page/login.html", nil)
	w := httptest.NewRecorder()
	srv.HandleBMCProxy(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "Login") {
		t.Errorf("unexpected body: %s", body)
	}
}

func TestHandleBMCProxy_UnknownServer(t *testing.T) {
	cfg := &models.AppConfig{
		Servers:  []models.ServerConfig{},
		Settings: models.Settings{MaxConcurrentSessions: 4, Runtime: "docker"},
	}
	srv := NewServer(cfg, &mockContainerManager{})

	req := httptest.NewRequest("GET", "/__bmc/nonexistent/", nil)
	w := httptest.NewRecorder()
	srv.HandleBMCProxy(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestHandleBMCProxy_CookieStripping(t *testing.T) {
	bmc := newMockBMC(t)
	defer bmc.Close()
	srv := newTestBMCServer(t, bmc)

	req := httptest.NewRequest("GET", "/__bmc/test-bmc/api/check-cookies", nil)
	req.AddCookie(&http.Cookie{Name: "kvm_session", Value: "should-strip"})
	req.AddCookie(&http.Cookie{Name: "BMCSession", Value: "should-keep"})
	w := httptest.NewRecorder()
	srv.HandleBMCProxy(w, req)

	var result map[string]interface{}
	json.NewDecoder(w.Body).Decode(&result)
	if result["has_kvm_session"] == true {
		t.Error("kvm_session cookie should have been stripped")
	}
}

func TestHandleBMCProxy_LocationRewrite(t *testing.T) {
	bmc := newMockBMC(t)
	defer bmc.Close()
	srv := newTestBMCServer(t, bmc)

	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	// Create a real test server to get actual HTTP redirect behavior
	ts := httptest.NewServer(http.HandlerFunc(srv.HandleBMCProxy))
	defer ts.Close()

	resp, err := client.Get(ts.URL + "/__bmc/test-bmc/redirect")
	if err != nil {
		t.Fatalf("request error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want 302", resp.StatusCode)
	}

	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/__bmc/test-bmc/") {
		t.Errorf("Location = %q, want prefix /__bmc/test-bmc/", loc)
	}
}

func TestHandleBMCProxy_JSONPassthrough(t *testing.T) {
	bmc := newMockBMC(t)
	defer bmc.Close()
	srv := newTestBMCServer(t, bmc)

	req := httptest.NewRequest("GET", "/__bmc/test-bmc/rpc/status", nil)
	w := httptest.NewRecorder()
	srv.HandleBMCProxy(w, req)

	var result map[string]string
	json.NewDecoder(w.Body).Decode(&result)
	if result["status"] != "ok" {
		t.Errorf("JSON response corrupted: %v", result)
	}
}

func TestHandleBMCProxy_NoContentRewriting(t *testing.T) {
	bmc := newMockBMC(t)
	defer bmc.Close()
	srv := newTestBMCServer(t, bmc)

	req := httptest.NewRequest("GET", "/__bmc/test-bmc/", nil)
	w := httptest.NewRecorder()
	srv.HandleBMCProxy(w, req)

	bodyBytes, _ := io.ReadAll(w.Body)
	body := string(bodyBytes)

	if !strings.Contains(body, `<title>BMC</title>`) {
		t.Errorf("HTML content was modified.\nbody: %s", body)
	}
	if strings.Contains(body, "__bmc") || strings.Contains(body, "ipmi") {
		t.Errorf("proxy prefix found in content, should be unmodified.\nbody: %s", body)
	}
}

func TestHandleBMCProxy_BMCCredentialInjection(t *testing.T) {
	bmc := newMockBMC(t)
	defer bmc.Close()
	srv := newTestBMCServer(t, bmc)

	// Inject pre-authenticated BMC credentials into the proxy
	serverCfg := &srv.Config.Servers[0]
	entry := getOrCreateProxy(serverCfg, "test-bmc")
	entry.setBMCCredentials(&models.BMCCredentials{
		SessionCookie: "test-session-123",
		CSRFToken:     "test-csrf-456",
	})

	// Make a request — credentials should be injected automatically
	req := httptest.NewRequest("GET", "/__bmc/test-bmc/api/check-cookies", nil)
	// Also add a browser SessionCookie to verify it gets replaced, not duplicated
	req.AddCookie(&http.Cookie{Name: "SessionCookie", Value: "stale-browser-cookie"})
	w := httptest.NewRecorder()
	srv.HandleBMCProxy(w, req)

	var result map[string]interface{}
	json.NewDecoder(w.Body).Decode(&result)

	if result["session_cookie"] != "test-session-123" {
		t.Errorf("SessionCookie = %q, want %q", result["session_cookie"], "test-session-123")
	}
	if result["csrf_token"] != "test-csrf-456" {
		t.Errorf("X-CSRFTOKEN = %q, want %q", result["csrf_token"], "test-csrf-456")
	}
}

// --- Dell iDRAC tests ---

func newDellIDRAC8BMC(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/start.html", http.StatusFound)
	})

	mux.HandleFunc("GET /session", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Note: no X_Language header (firmware bug)
		fmt.Fprint(w, `{"aimGetIntProp":{"scl_int_enabled":0,"status":"OK"}}`)
	})

	mux.HandleFunc("POST /data/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprint(w, `<?xml version="1.0"?><root><status>ok</status><authResult>0</authResult><forwardUrl>index.html?ST1=a,ST2=b</forwardUrl></root>`)
	})

	mux.HandleFunc("POST /data/logout", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprint(w, `<?xml version="1.0"?><root><status>ok</status></root>`)
	})

	return httptest.NewTLSServer(mux)
}

func newDellIDRAC9BMC(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body>iDRAC9</body></html>`)
	})

	mux.HandleFunc("POST /sysmgmt/2015/bmc/session", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("XSRF-TOKEN", "real-csrf")
		http.SetCookie(w, &http.Cookie{Name: "-http-session-", Value: "real-session"})
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"authResult":0}`)
	})

	return httptest.NewTLSServer(mux)
}

func newTestDellServer(t *testing.T, bmc *httptest.Server, boardType string) *Server {
	t.Helper()
	// Parse URL to handle both http:// and https:// schemes
	u, err := url.Parse(bmc.URL)
	if err != nil {
		t.Fatalf("failed to parse BMC URL: %v", err)
	}
	host := u.Hostname()
	port := 443
	if p := u.Port(); p != "" {
		fmt.Sscanf(p, "%d", &port)
	}

	name := "test-dell"
	cfg := &models.AppConfig{
		Servers: []models.ServerConfig{
			{Name: name, BMCIP: host, BMCPort: port, BoardType: boardType, Username: "root", CredentialEnv: "PASS"},
		},
		Settings: models.Settings{MaxConcurrentSessions: 4, Runtime: "docker"},
	}

	bmcProxies.Delete(name)
	return NewServer(cfg, &mockContainerManager{})
}

func TestHandleBMCProxy_IDRAC8_XLanguageInjection(t *testing.T) {
	bmc := newDellIDRAC8BMC(t)
	defer bmc.Close()
	srv := newTestDellServer(t, bmc, "dell_idrac8")

	req := httptest.NewRequest("GET", "/__bmc/test-dell/session?aimGetIntProp=scl_int_enabled", nil)
	w := httptest.NewRecorder()
	srv.HandleBMCProxy(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	xlang := w.Header().Get("X_Language")
	if xlang != "en" {
		t.Errorf("X_Language = %q, want %q", xlang, "en")
	}
}

func TestHandleBMCProxy_IDRAC8_LoginIntercept(t *testing.T) {
	bmc := newDellIDRAC8BMC(t)
	defer bmc.Close()
	srv := newTestDellServer(t, bmc, "dell_idrac8")

	// Inject cached credentials
	entry := getOrCreateProxy(&srv.Config.Servers[0], "test-dell")
	entry.setBMCCredentials(&models.BMCCredentials{
		SessionCookie: "cached-session",
		CSRFToken:     "cached-csrf",
		Extra:         map[string]string{"st1": "cached-st1"},
	})

	req := httptest.NewRequest("POST", "/__bmc/test-dell/data/login", strings.NewReader("user=root&password=test"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.HandleBMCProxy(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "<authResult>0</authResult>") {
		t.Errorf("body should contain authResult 0, got: %s", body)
	}
	if !strings.Contains(body, "ST1=cached-st1") {
		t.Errorf("body should contain cached ST1, got: %s", body)
	}
	if !strings.Contains(body, "ST2=cached-csrf") {
		t.Errorf("body should contain cached ST2 (CSRF), got: %s", body)
	}

	// Verify session cookie is set
	cookies := w.Result().Cookies()
	found := false
	for _, c := range cookies {
		if c.Name == "-http-session-" && c.Value == "cached-session" {
			found = true
		}
	}
	if !found {
		t.Error("expected -http-session- cookie with cached value")
	}
}

func TestHandleBMCProxy_IDRAC8_LogoutIntercept(t *testing.T) {
	bmc := newDellIDRAC8BMC(t)
	defer bmc.Close()
	srv := newTestDellServer(t, bmc, "dell_idrac8")

	// Inject cached credentials
	entry := getOrCreateProxy(&srv.Config.Servers[0], "test-dell")
	entry.setBMCCredentials(&models.BMCCredentials{
		SessionCookie: "cached-session",
		CSRFToken:     "cached-csrf",
	})

	// The login page POSTs to /data/logout before login — this should be intercepted
	req := httptest.NewRequest("POST", "/__bmc/test-dell/data/logout", nil)
	w := httptest.NewRecorder()
	srv.HandleBMCProxy(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "<status>ok</status>") {
		t.Errorf("body should contain fake OK, got: %s", body)
	}

	// Credentials should still be cached (not invalidated)
	if creds := entry.getBMCCredentials(); creds == nil || creds.SessionCookie != "cached-session" {
		t.Error("credentials should still be cached after intercepted logout")
	}
}

func TestHandleBMCProxy_IDRAC9_LoginIntercept(t *testing.T) {
	bmc := newDellIDRAC9BMC(t)
	defer bmc.Close()
	srv := newTestDellServer(t, bmc, "dell_idrac9")

	// Inject cached credentials
	entry := getOrCreateProxy(&srv.Config.Servers[0], "test-dell")
	entry.setBMCCredentials(&models.BMCCredentials{
		SessionCookie: "idrac9-session",
		CSRFToken:     "idrac9-csrf",
	})

	req := httptest.NewRequest("POST", "/__bmc/test-dell/sysmgmt/2015/bmc/session", strings.NewReader(`{"UserName":"root","Password":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.HandleBMCProxy(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", w.Code)
	}

	body := w.Body.String()
	if body != `{"authResult":0}` {
		t.Errorf("unexpected body: %s", body)
	}

	// Check XSRF-TOKEN header
	xsrf := w.Header().Get("XSRF-TOKEN")
	if xsrf != "idrac9-csrf" {
		t.Errorf("XSRF-TOKEN = %q, want %q", xsrf, "idrac9-csrf")
	}

	// Check session cookie
	cookies := w.Result().Cookies()
	found := false
	for _, c := range cookies {
		if c.Name == "-http-session-" && c.Value == "idrac9-session" {
			found = true
		}
	}
	if !found {
		t.Error("expected -http-session- cookie with cached value")
	}
}

func TestHandleBMCProxy_IDRAC9_NoInterceptWithoutCreds(t *testing.T) {
	bmc := newDellIDRAC9BMC(t)
	defer bmc.Close()
	srv := newTestDellServer(t, bmc, "dell_idrac9")

	// No cached credentials — login should pass through to real BMC
	req := httptest.NewRequest("POST", "/__bmc/test-dell/sysmgmt/2015/bmc/session", strings.NewReader(`{"UserName":"root","Password":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.HandleBMCProxy(w, req)

	// Should get the real BMC response (201)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 from real BMC", w.Code)
	}

	// XSRF-TOKEN should be from the real BMC
	xsrf := w.Header().Get("XSRF-TOKEN")
	if xsrf != "real-csrf" {
		t.Errorf("XSRF-TOKEN = %q, want %q (from real BMC)", xsrf, "real-csrf")
	}
}

func TestHandleBMCProxy_IDRAC8_CredentialInjection(t *testing.T) {
	// Create a BMC that echoes back received auth headers
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/check", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		sessionCookie := ""
		for _, c := range r.Cookies() {
			if c.Name == "-http-session-" {
				sessionCookie = c.Value
			}
		}
		st2 := r.Header.Get("ST2")
		fmt.Fprintf(w, `{"session_cookie":%q,"st2":%q}`, sessionCookie, st2)
	})
	bmc := httptest.NewTLSServer(mux)
	defer bmc.Close()

	srv := newTestDellServer(t, bmc, "dell_idrac8")
	entry := getOrCreateProxy(&srv.Config.Servers[0], "test-dell")
	entry.setBMCCredentials(&models.BMCCredentials{
		SessionCookie: "idrac8-session-abc",
		CSRFToken:     "idrac8-st2-xyz",
	})

	req := httptest.NewRequest("GET", "/__bmc/test-dell/api/check", nil)
	w := httptest.NewRecorder()
	srv.HandleBMCProxy(w, req)

	var result map[string]string
	json.NewDecoder(w.Body).Decode(&result)

	if result["session_cookie"] != "idrac8-session-abc" {
		t.Errorf("session_cookie = %q, want %q", result["session_cookie"], "idrac8-session-abc")
	}
	if result["st2"] != "idrac8-st2-xyz" {
		t.Errorf("st2 = %q, want %q", result["st2"], "idrac8-st2-xyz")
	}
}

func TestHandleBMCProxy_IDRAC9_CredentialInjection(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/check", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		sessionCookie := ""
		for _, c := range r.Cookies() {
			if c.Name == "-http-session-" {
				sessionCookie = c.Value
			}
		}
		xsrf := r.Header.Get("XSRF-TOKEN")
		fmt.Fprintf(w, `{"session_cookie":%q,"xsrf":%q}`, sessionCookie, xsrf)
	})
	bmc := httptest.NewTLSServer(mux)
	defer bmc.Close()

	srv := newTestDellServer(t, bmc, "dell_idrac9")
	entry := getOrCreateProxy(&srv.Config.Servers[0], "test-dell")
	entry.setBMCCredentials(&models.BMCCredentials{
		SessionCookie: "idrac9-session-abc",
		CSRFToken:     "idrac9-xsrf-xyz",
	})

	req := httptest.NewRequest("GET", "/__bmc/test-dell/api/check", nil)
	w := httptest.NewRecorder()
	srv.HandleBMCProxy(w, req)

	var result map[string]string
	json.NewDecoder(w.Body).Decode(&result)

	if result["session_cookie"] != "idrac9-session-abc" {
		t.Errorf("session_cookie = %q, want %q", result["session_cookie"], "idrac9-session-abc")
	}
	if result["xsrf"] != "idrac9-xsrf-xyz" {
		t.Errorf("xsrf = %q, want %q", result["xsrf"], "idrac9-xsrf-xyz")
	}
}

func TestRewriteLocationForBMC(t *testing.T) {
	tests := []struct {
		name      string
		loc       string
		bmcOrigin string
		server    string
		want      string
	}{
		{"absolute URL", "http://10.0.0.1:80/page/login.html", "http://10.0.0.1:80", "srv1", "/__bmc/srv1/page/login.html"},
		{"root-relative", "/page/login.html", "http://10.0.0.1:80", "srv1", "/__bmc/srv1/page/login.html"},
		{"relative (no leading slash)", "page/login.html", "http://10.0.0.1:80", "srv1", "page/login.html"},
		{"external URL", "https://example.com/foo", "http://10.0.0.1:80", "srv1", "https://example.com/foo"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rewriteLocationForBMC(tt.loc, tt.bmcOrigin, tt.server)
			if got != tt.want {
				t.Errorf("rewriteLocationForBMC(%q) = %q, want %q", tt.loc, got, tt.want)
			}
		})
	}
}
