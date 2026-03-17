package api

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zackpollard/kvm-switcher/internal/models"
)

// ---------------------------------------------------------------------------
// 1. TestIntegration_MegaRAC_SessionThenProxy
// ---------------------------------------------------------------------------

func TestIntegration_MegaRAC_SessionThenProxy(t *testing.T) {
	const serverName = "integ-megarac"

	var logoutHit atomic.Int32

	mux := http.NewServeMux()

	// POST /rpc/WEBSES/create.asp → AMI JS session response
	mux.HandleFunc("POST /rpc/WEBSES/create.asp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `{ 'SESSION_COOKIE' : 'megarac-sess-integ' , 'BMC_IP_ADDR' : '127.0.0.1' , 'CSRFTOKEN' : 'megarac-csrf-integ' , HAPI_STATUS:0 }`)
	})

	// GET /rpc/getrole.asp → role info
	mux.HandleFunc("GET /rpc/getrole.asp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `{ 'CURUSERNAME' : 'admin' , 'CURPRIV' : 4 , 'EXTENDED_PRIV' : 255 , HAPI_STATUS:0 }`)
	})

	// GET /rpc/hoststatus.asp → echo back received SessionCookie and CSRFTOKEN
	mux.HandleFunc("GET /rpc/hoststatus.asp", func(w http.ResponseWriter, r *http.Request) {
		sessionCookie := ""
		for _, c := range r.Cookies() {
			if c.Name == "SessionCookie" {
				sessionCookie = c.Value
			}
		}
		csrfToken := r.Header.Get("CSRFTOKEN")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"session_cookie":%q,"csrf_token":%q}`, sessionCookie, csrfToken)
	})

	// GET /rpc/WEBSES/logout.asp → record if hit
	mux.HandleFunc("GET /rpc/WEBSES/logout.asp", func(w http.ResponseWriter, r *http.Request) {
		logoutHit.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"WEBSES":{"SESSID":"LoggedOut"}}`)
	})

	bmc := httptest.NewServer(mux)
	t.Cleanup(bmc.Close)

	host, portStr, _ := strings.Cut(strings.TrimPrefix(bmc.URL, "http://"), ":")
	port := 80
	fmt.Sscanf(portStr, "%d", &port)

	t.Setenv("INTEG_MEGARAC_PASS", "testpass")

	cfg := &models.AppConfig{
		Servers: []models.ServerConfig{
			{Name: serverName, BMCIP: host, BMCPort: port, BoardType: "ami_megarac", Username: "admin", CredentialEnv: "INTEG_MEGARAC_PASS"},
		},
		Settings: models.Settings{MaxConcurrentSessions: 4, Runtime: "docker"},
	}
	bmcProxies.Delete(serverName)
	t.Cleanup(func() { bmcProxies.Delete(serverName) })
	srv := newServerCore(cfg, &mockContainerManager{})

	// --- Step 1: CreateIPMISession ---
	apiMux := http.NewServeMux()
	apiMux.HandleFunc("POST /api/ipmi-session/{name}", srv.CreateIPMISession)
	apiServer := httptest.NewServer(apiMux)
	t.Cleanup(apiServer.Close)

	resp, err := http.Post(apiServer.URL+"/api/ipmi-session/"+serverName, "application/json", nil)
	if err != nil {
		t.Fatalf("CreateIPMISession request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("CreateIPMISession status = %d, want 200; body: %s", resp.StatusCode, body)
	}

	var sessResp map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&sessResp); err != nil {
		t.Fatalf("failed to decode session response: %v", err)
	}
	if sessResp["session_cookie"] != "megarac-sess-integ" {
		t.Errorf("session_cookie = %q, want %q", sessResp["session_cookie"], "megarac-sess-integ")
	}
	if sessResp["csrf_token"] != "megarac-csrf-integ" {
		t.Errorf("csrf_token = %q, want %q", sessResp["csrf_token"], "megarac-csrf-integ")
	}

	// --- Step 2: Proxy GET /rpc/hoststatus.asp → verify creds injected ---
	req := httptest.NewRequest("GET", "/__bmc/"+serverName+"/rpc/hoststatus.asp", nil)
	w := httptest.NewRecorder()
	srv.HandleBMCProxy(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("proxy hoststatus status = %d, want 200", w.Code)
	}
	var hostStatus map[string]string
	if err := json.NewDecoder(w.Body).Decode(&hostStatus); err != nil {
		t.Fatalf("failed to decode hoststatus response: %v", err)
	}
	if hostStatus["session_cookie"] != "megarac-sess-integ" {
		t.Errorf("injected session_cookie = %q, want %q", hostStatus["session_cookie"], "megarac-sess-integ")
	}
	if hostStatus["csrf_token"] != "megarac-csrf-integ" {
		t.Errorf("injected csrf_token = %q, want %q", hostStatus["csrf_token"], "megarac-csrf-integ")
	}

	// Verify X-KVM-AutoLogin header
	if w.Header().Get("X-KVM-AutoLogin") != "true" {
		t.Errorf("X-KVM-AutoLogin = %q, want %q", w.Header().Get("X-KVM-AutoLogin"), "true")
	}

	// --- Step 3: Proxy GET logout.asp → verify intercepted, mock NOT hit ---
	logoutHit.Store(0)
	req = httptest.NewRequest("GET", "/__bmc/"+serverName+"/rpc/WEBSES/logout.asp", nil)
	w = httptest.NewRecorder()
	srv.HandleBMCProxy(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("proxy logout status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Disconnected") {
		t.Errorf("expected fake disconnect response, got: %s", body)
	}
	if logoutHit.Load() != 0 {
		t.Error("logout.asp should NOT have been forwarded to mock BMC")
	}
}

// ---------------------------------------------------------------------------
// 2. TestIntegration_IDRAC8_SessionThenProxy
// ---------------------------------------------------------------------------

func TestIntegration_IDRAC8_SessionThenProxy(t *testing.T) {
	const serverName = "integ-idrac8"

	var logoutHit atomic.Int32

	mux := http.NewServeMux()

	// POST /data/login → XML with auth tokens + Set-Cookie
	mux.HandleFunc("POST /data/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		http.SetCookie(w, &http.Cookie{Name: "-http-session-", Value: "idrac8-sess-integ"})
		fmt.Fprint(w, `<?xml version="1.0"?><root><status>ok</status><authResult>0</authResult><forwardUrl>index.html?ST1=idrac8-st1-integ,ST2=idrac8-st2-integ</forwardUrl></root>`)
	})

	// POST /data/logout → track if called
	mux.HandleFunc("POST /data/logout", func(w http.ResponseWriter, r *http.Request) {
		logoutHit.Add(1)
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprint(w, `<?xml version="1.0"?><root><status>ok</status></root>`)
	})

	// GET /check → echo received -http-session- cookie and ST2 header
	mux.HandleFunc("GET /check", func(w http.ResponseWriter, r *http.Request) {
		sessionCookie := ""
		for _, c := range r.Cookies() {
			if c.Name == "-http-session-" {
				sessionCookie = c.Value
			}
		}
		st2 := r.Header.Get("ST2")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"session_cookie":%q,"st2":%q}`, sessionCookie, st2)
	})

	// GET /session → JSON for X_Language test (firmware omits X_Language)
	mux.HandleFunc("GET /session", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Note: deliberately no X_Language header
		fmt.Fprint(w, `{"aimGetIntProp":{"scl_int_enabled":0,"status":"OK"}}`)
	})

	// GET /rpc/WEBSES/logout.asp → for completeness (should be on the AMI path)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/start.html", http.StatusFound)
	})

	bmc := httptest.NewTLSServer(mux)
	t.Cleanup(bmc.Close)

	u, err := url.Parse(bmc.URL)
	if err != nil {
		t.Fatalf("failed to parse BMC URL: %v", err)
	}
	host := u.Hostname()
	port := 443
	if p := u.Port(); p != "" {
		fmt.Sscanf(p, "%d", &port)
	}

	t.Setenv("INTEG_IDRAC8_PASS", "testpass")

	cfg := &models.AppConfig{
		Servers: []models.ServerConfig{
			{Name: serverName, BMCIP: host, BMCPort: port, BoardType: "dell_idrac8", Username: "root", CredentialEnv: "INTEG_IDRAC8_PASS"},
		},
		Settings: models.Settings{MaxConcurrentSessions: 4, Runtime: "docker"},
	}
	bmcProxies.Delete(serverName)
	t.Cleanup(func() { bmcProxies.Delete(serverName) })
	srv := newServerCore(cfg, &mockContainerManager{})

	// --- Step 1: CreateIPMISession ---
	apiMux := http.NewServeMux()
	apiMux.HandleFunc("POST /api/ipmi-session/{name}", srv.CreateIPMISession)
	apiServer := httptest.NewServer(apiMux)
	t.Cleanup(apiServer.Close)

	resp, err := http.Post(apiServer.URL+"/api/ipmi-session/"+serverName, "application/json", nil)
	if err != nil {
		t.Fatalf("CreateIPMISession request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("CreateIPMISession status = %d, want 200; body: %s", resp.StatusCode, respBody)
	}

	var sessResp map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&sessResp); err != nil {
		t.Fatalf("failed to decode session response: %v", err)
	}
	if sessResp["session_cookie"] != "idrac8-sess-integ" {
		t.Errorf("session_cookie = %q, want %q", sessResp["session_cookie"], "idrac8-sess-integ")
	}
	if sessResp["csrf_token"] != "idrac8-st2-integ" {
		t.Errorf("csrf_token = %q, want %q", sessResp["csrf_token"], "idrac8-st2-integ")
	}

	// --- Step 2: GET / through proxy → verify 302 redirect with ST1/ST2 (login bypass) ---
	proxyTS := httptest.NewServer(http.HandlerFunc(srv.HandleBMCProxy))
	t.Cleanup(proxyTS.Close)

	noRedirectClient := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	rootResp, err := noRedirectClient.Get(proxyTS.URL + "/__bmc/" + serverName + "/")
	if err != nil {
		t.Fatalf("proxy root request failed: %v", err)
	}
	rootResp.Body.Close()

	if rootResp.StatusCode != http.StatusFound {
		t.Fatalf("proxy root status = %d, want 302", rootResp.StatusCode)
	}
	loc := rootResp.Header.Get("Location")
	if !strings.Contains(loc, "ST1=idrac8-st1-integ") || !strings.Contains(loc, "ST2=idrac8-st2-integ") {
		t.Errorf("Location = %q, want to contain ST1 and ST2 tokens", loc)
	}

	// --- Step 3: POST /data/login through proxy → verify intercepted with cached tokens ---
	loginReq, err := http.NewRequest("POST", proxyTS.URL+"/__bmc/"+serverName+"/data/login",
		strings.NewReader("user=root&password=test"))
	if err != nil {
		t.Fatalf("failed to create login request: %v", err)
	}
	loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginResp, err := noRedirectClient.Do(loginReq)
	if err != nil {
		t.Fatalf("proxy login request failed: %v", err)
	}
	loginBody, _ := io.ReadAll(loginResp.Body)
	loginResp.Body.Close()

	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("proxy login status = %d, want 200; body: %s", loginResp.StatusCode, loginBody)
	}
	if !strings.Contains(string(loginBody), "<authResult>0</authResult>") {
		t.Errorf("login body should contain authResult 0, got: %s", loginBody)
	}
	if !strings.Contains(string(loginBody), "ST1=idrac8-st1-integ") {
		t.Errorf("login body should contain cached ST1, got: %s", loginBody)
	}

	// --- Step 4: POST /data/logout through proxy → verify intercepted ---
	logoutHit.Store(0)
	logoutReq, err := http.NewRequest("POST", proxyTS.URL+"/__bmc/"+serverName+"/data/logout", nil)
	if err != nil {
		t.Fatalf("failed to create logout request: %v", err)
	}
	logoutResp, err := noRedirectClient.Do(logoutReq)
	if err != nil {
		t.Fatalf("proxy logout request failed: %v", err)
	}
	logoutBody, _ := io.ReadAll(logoutResp.Body)
	logoutResp.Body.Close()

	if logoutResp.StatusCode != http.StatusOK {
		t.Fatalf("proxy logout status = %d, want 200", logoutResp.StatusCode)
	}
	if !strings.Contains(string(logoutBody), "<status>ok</status>") {
		t.Errorf("logout body should contain fake OK, got: %s", logoutBody)
	}
	if logoutHit.Load() != 0 {
		t.Error("POST /data/logout should NOT have been forwarded to mock BMC")
	}

	// --- Step 5: GET /check → verify -http-session- cookie and ST2 header injected ---
	req := httptest.NewRequest("GET", "/__bmc/"+serverName+"/check", nil)
	w := httptest.NewRecorder()
	srv.HandleBMCProxy(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("proxy check status = %d, want 200", w.Code)
	}
	var checkResult map[string]string
	if err := json.NewDecoder(w.Body).Decode(&checkResult); err != nil {
		t.Fatalf("failed to decode check response: %v", err)
	}
	if checkResult["session_cookie"] != "idrac8-sess-integ" {
		t.Errorf("injected session_cookie = %q, want %q", checkResult["session_cookie"], "idrac8-sess-integ")
	}
	if checkResult["st2"] != "idrac8-st2-integ" {
		t.Errorf("injected st2 = %q, want %q", checkResult["st2"], "idrac8-st2-integ")
	}

	// --- Step 6: GET /session → verify X_Language: en header injected ---
	req = httptest.NewRequest("GET", "/__bmc/"+serverName+"/session?aimGetIntProp=scl_int_enabled", nil)
	w = httptest.NewRecorder()
	srv.HandleBMCProxy(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("proxy session status = %d, want 200", w.Code)
	}
	xlang := w.Header().Get("X_Language")
	if xlang != "en" {
		t.Errorf("X_Language = %q, want %q", xlang, "en")
	}
}

// ---------------------------------------------------------------------------
// 3. TestIntegration_NanoKVM_SessionThenProxy
// ---------------------------------------------------------------------------

func TestIntegration_NanoKVM_SessionThenProxy(t *testing.T) {
	const serverName = "integ-nanokvm"

	mux := http.NewServeMux()

	// POST /api/auth/login → JWT token
	mux.HandleFunc("POST /api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":0,"data":{"token":"nano-jwt-test"}}`)
	})

	// POST /api/auth/logout → OK
	mux.HandleFunc("POST /api/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":0}`)
	})

	// GET / → HTML
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body>NanoKVM Dashboard</body></html>`)
	})

	// GET /check → echo cookies
	mux.HandleFunc("GET /check", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		cookies := make(map[string]string)
		for _, c := range r.Cookies() {
			cookies[c.Name] = c.Value
		}
		json.NewEncoder(w).Encode(cookies)
	})

	bmc := httptest.NewServer(mux)
	t.Cleanup(bmc.Close)

	host, portStr, _ := strings.Cut(strings.TrimPrefix(bmc.URL, "http://"), ":")
	port := 80
	fmt.Sscanf(portStr, "%d", &port)

	t.Setenv("INTEG_NANOKVM_PASS", "testpass")

	cfg := &models.AppConfig{
		Servers: []models.ServerConfig{
			{Name: serverName, BMCIP: host, BMCPort: port, BoardType: "nanokvm", Username: "admin", CredentialEnv: "INTEG_NANOKVM_PASS"},
		},
		Settings: models.Settings{MaxConcurrentSessions: 4, Runtime: "docker"},
	}
	bmcProxies.Delete(serverName)
	t.Cleanup(func() { bmcProxies.Delete(serverName) })
	srv := newServerCore(cfg, &mockContainerManager{})

	// --- Step 1: CreateIPMISession ---
	apiMux := http.NewServeMux()
	apiMux.HandleFunc("POST /api/ipmi-session/{name}", srv.CreateIPMISession)
	apiServer := httptest.NewServer(apiMux)
	t.Cleanup(apiServer.Close)

	resp, err := http.Post(apiServer.URL+"/api/ipmi-session/"+serverName, "application/json", nil)
	if err != nil {
		t.Fatalf("CreateIPMISession request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("CreateIPMISession status = %d, want 200; body: %s", resp.StatusCode, respBody)
	}

	var sessResp map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&sessResp); err != nil {
		t.Fatalf("failed to decode session response: %v", err)
	}
	if sessResp["session_cookie"] != "nano-jwt-test" {
		t.Errorf("session_cookie = %q, want %q", sessResp["session_cookie"], "nano-jwt-test")
	}

	// --- Step 2: GET /check through proxy → verify nano-kvm-token cookie injected ---
	req := httptest.NewRequest("GET", "/__bmc/"+serverName+"/check", nil)
	w := httptest.NewRecorder()
	srv.HandleBMCProxy(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("proxy check status = %d, want 200", w.Code)
	}
	var cookies map[string]string
	if err := json.NewDecoder(w.Body).Decode(&cookies); err != nil {
		t.Fatalf("failed to decode check response: %v", err)
	}
	if cookies["nano-kvm-token"] != "nano-jwt-test" {
		t.Errorf("nano-kvm-token = %q, want %q", cookies["nano-kvm-token"], "nano-jwt-test")
	}

	// --- Step 3: GET / through proxy → verify X-KVM-NanoToken header present ---
	req = httptest.NewRequest("GET", "/__bmc/"+serverName+"/", nil)
	w = httptest.NewRecorder()
	srv.HandleBMCProxy(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("proxy root status = %d, want 200", w.Code)
	}
	if w.Header().Get("X-KVM-NanoToken") != "nano-jwt-test" {
		t.Errorf("X-KVM-NanoToken = %q, want %q", w.Header().Get("X-KVM-NanoToken"), "nano-jwt-test")
	}
	if w.Header().Get("X-KVM-AutoLogin") != "true" {
		t.Errorf("X-KVM-AutoLogin = %q, want %q", w.Header().Get("X-KVM-AutoLogin"), "true")
	}
}

// ---------------------------------------------------------------------------
// 4. TestIntegration_APC_SessionThenProxy
// ---------------------------------------------------------------------------

func TestIntegration_APC_SessionThenProxy(t *testing.T) {
	const serverName = "integ-apc"

	mux := http.NewServeMux()

	// GET / → 303 to /home.htm
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/home.htm", http.StatusSeeOther)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
	})

	// GET /home.htm → 303 to /NMC/pre123/logon.htm
	mux.HandleFunc("GET /home.htm", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/NMC/pre123/logon.htm", http.StatusSeeOther)
	})

	// GET /NMC/pre123/logon.htm → login form page
	mux.HandleFunc("GET /NMC/pre123/logon.htm", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body>Login Form</body></html>`)
	})

	// POST /NMC/pre123/Forms/login1 → 303 to /NMC/auth456/
	mux.HandleFunc("POST /NMC/pre123/Forms/login1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/NMC/auth456/")
		w.WriteHeader(http.StatusSeeOther)
	})

	// GET /NMC/auth456/home.htm → dashboard HTML
	mux.HandleFunc("GET /NMC/auth456/home.htm", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body>APC Dashboard</body></html>`)
	})

	// GET /NMC/auth456/check → echo received request path
	mux.HandleFunc("GET /NMC/auth456/check", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
	})

	// Catch-all for /NMC/auth456/ paths
	mux.HandleFunc("GET /NMC/auth456/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
	})

	bmc := httptest.NewServer(mux)
	t.Cleanup(bmc.Close)

	host, portStr, _ := strings.Cut(strings.TrimPrefix(bmc.URL, "http://"), ":")
	port := 80
	fmt.Sscanf(portStr, "%d", &port)

	t.Setenv("INTEG_APC_PASS", "testpass")

	cfg := &models.AppConfig{
		Servers: []models.ServerConfig{
			{Name: serverName, BMCIP: host, BMCPort: port, BoardType: "apc_ups", Username: "apc", CredentialEnv: "INTEG_APC_PASS"},
		},
		Settings: models.Settings{MaxConcurrentSessions: 4, Runtime: "docker"},
	}
	bmcProxies.Delete(serverName)
	t.Cleanup(func() { bmcProxies.Delete(serverName) })
	srv := newServerCore(cfg, &mockContainerManager{})

	// --- Step 1: CreateIPMISession ---
	apiMux := http.NewServeMux()
	apiMux.HandleFunc("POST /api/ipmi-session/{name}", srv.CreateIPMISession)
	apiServer := httptest.NewServer(apiMux)
	t.Cleanup(apiServer.Close)

	resp, err := http.Post(apiServer.URL+"/api/ipmi-session/"+serverName, "application/json", nil)
	if err != nil {
		t.Fatalf("CreateIPMISession request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("CreateIPMISession status = %d, want 200; body: %s", resp.StatusCode, respBody)
	}

	// Verify the proxy entry has nmc_path set
	entry := getOrCreateProxy(&cfg.Servers[0], serverName)
	creds := entry.getBMCCredentials()
	if creds == nil {
		t.Fatal("expected BMC credentials to be set after CreateIPMISession")
	}
	if creds.Extra == nil || creds.Extra["nmc_path"] != "/NMC/auth456" {
		t.Errorf("nmc_path = %q, want %q", creds.Extra["nmc_path"], "/NMC/auth456")
	}

	// --- Step 2: GET / through proxy → verify 302 redirect to home.htm (login bypass) ---
	proxyTS := httptest.NewServer(http.HandlerFunc(srv.HandleBMCProxy))
	t.Cleanup(proxyTS.Close)

	noRedirectClient := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	rootResp, err := noRedirectClient.Get(proxyTS.URL + "/__bmc/" + serverName + "/")
	if err != nil {
		t.Fatalf("proxy root request failed: %v", err)
	}
	rootResp.Body.Close()

	if rootResp.StatusCode != http.StatusFound {
		t.Fatalf("proxy root status = %d, want 302", rootResp.StatusCode)
	}
	loc := rootResp.Header.Get("Location")
	if !strings.HasSuffix(loc, "/home.htm") {
		t.Errorf("Location = %q, want suffix /home.htm", loc)
	}

	// --- Step 3: GET /check through proxy → verify mock receives /NMC/auth456/check ---
	req := httptest.NewRequest("GET", "/__bmc/"+serverName+"/check", nil)
	w := httptest.NewRecorder()
	srv.HandleBMCProxy(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("proxy check status = %d, want 200", w.Code)
	}
	var checkResult map[string]string
	if err := json.NewDecoder(w.Body).Decode(&checkResult); err != nil {
		t.Fatalf("failed to decode check response: %v", err)
	}
	if checkResult["path"] != "/NMC/auth456/check" {
		t.Errorf("path = %q, want %q", checkResult["path"], "/NMC/auth456/check")
	}

	// --- Step 4: GET /NMC/oldtoken/page through proxy → verify token replaced ---
	req = httptest.NewRequest("GET", "/__bmc/"+serverName+"/NMC/oldtoken/home.htm", nil)
	w = httptest.NewRecorder()
	srv.HandleBMCProxy(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("proxy old-token status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "APC Dashboard") {
		t.Errorf("expected APC Dashboard content, got: %s", body)
	}
}

// ---------------------------------------------------------------------------
// 5. TestIntegration_GzipDecompression
// ---------------------------------------------------------------------------

func TestIntegration_GzipDecompression(t *testing.T) {
	const serverName = "integ-gzip"

	htmlContent := `<html><body>Compressed Content</body></html>`

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		if _, err := gz.Write([]byte(htmlContent)); err != nil {
			t.Errorf("gzip write error: %v", err)
			return
		}
		if err := gz.Close(); err != nil {
			t.Errorf("gzip close error: %v", err)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(buf.Bytes())
	})

	bmc := httptest.NewServer(mux)
	t.Cleanup(bmc.Close)

	host, portStr, _ := strings.Cut(strings.TrimPrefix(bmc.URL, "http://"), ":")
	port := 80
	fmt.Sscanf(portStr, "%d", &port)

	cfg := &models.AppConfig{
		Servers: []models.ServerConfig{
			{Name: serverName, BMCIP: host, BMCPort: port, BoardType: "ami_megarac", Username: "admin", CredentialEnv: "PASS"},
		},
		Settings: models.Settings{MaxConcurrentSessions: 4, Runtime: "docker"},
	}
	bmcProxies.Delete(serverName)
	t.Cleanup(func() { bmcProxies.Delete(serverName) })
	srv := newServerCore(cfg, &mockContainerManager{})

	req := httptest.NewRequest("GET", "/__bmc/"+serverName+"/", nil)
	w := httptest.NewRecorder()
	srv.HandleBMCProxy(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	// Verify Content-Encoding removed
	if ce := w.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("Content-Encoding = %q, want empty (should be decompressed)", ce)
	}

	// Verify body is decompressed
	body := w.Body.String()
	if !strings.Contains(body, "Compressed Content") {
		t.Errorf("body should contain decompressed HTML, got: %s", body)
	}
}

// ---------------------------------------------------------------------------
// 6. TestIntegration_HeaderRemoval
// ---------------------------------------------------------------------------

func TestIntegration_HeaderRemoval(t *testing.T) {
	const serverName = "integ-headers"

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Security-Policy", "default-src 'self'")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Disposition", "attachment; filename=test.html")
		fmt.Fprint(w, `<html><body>Headers Test</body></html>`)
	})

	bmc := httptest.NewServer(mux)
	t.Cleanup(bmc.Close)

	host, portStr, _ := strings.Cut(strings.TrimPrefix(bmc.URL, "http://"), ":")
	port := 80
	fmt.Sscanf(portStr, "%d", &port)

	cfg := &models.AppConfig{
		Servers: []models.ServerConfig{
			{Name: serverName, BMCIP: host, BMCPort: port, BoardType: "ami_megarac", Username: "admin", CredentialEnv: "PASS"},
		},
		Settings: models.Settings{MaxConcurrentSessions: 4, Runtime: "docker"},
	}
	bmcProxies.Delete(serverName)
	t.Cleanup(func() { bmcProxies.Delete(serverName) })
	srv := newServerCore(cfg, &mockContainerManager{})

	req := httptest.NewRequest("GET", "/__bmc/"+serverName+"/", nil)
	w := httptest.NewRecorder()
	srv.HandleBMCProxy(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	// Verify removed headers
	if v := w.Header().Get("Content-Security-Policy"); v != "" {
		t.Errorf("Content-Security-Policy should be removed, got %q", v)
	}
	if v := w.Header().Get("X-Frame-Options"); v != "" {
		t.Errorf("X-Frame-Options should be removed, got %q", v)
	}
	if v := w.Header().Get("Content-Disposition"); v != "" {
		t.Errorf("Content-Disposition should be removed, got %q", v)
	}

	// Verify Content-Type preserved
	if ct := w.Header().Get("Content-Type"); ct != "text/html" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/html")
	}
}

// ---------------------------------------------------------------------------
// 7. TestIntegration_BMCUnreachable_SessionCreation
// ---------------------------------------------------------------------------

func TestIntegration_BMCUnreachable_SessionCreation(t *testing.T) {
	const serverName = "integ-unreachable-sess"

	t.Setenv("INTEG_UNREACH_PASS", "testpass")

	cfg := &models.AppConfig{
		Servers: []models.ServerConfig{
			{Name: serverName, BMCIP: "127.0.0.1", BMCPort: 1, BoardType: "ami_megarac", Username: "admin", CredentialEnv: "INTEG_UNREACH_PASS"},
		},
		Settings: models.Settings{MaxConcurrentSessions: 4, Runtime: "docker"},
	}
	bmcProxies.Delete(serverName)
	t.Cleanup(func() { bmcProxies.Delete(serverName) })
	srv := newServerCore(cfg, &mockContainerManager{})

	apiMux := http.NewServeMux()
	apiMux.HandleFunc("POST /api/ipmi-session/{name}", srv.CreateIPMISession)
	apiServer := httptest.NewServer(apiMux)
	t.Cleanup(apiServer.Close)

	resp, err := http.Post(apiServer.URL+"/api/ipmi-session/"+serverName, "application/json", nil)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}

	var errResp map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	if !strings.Contains(errResp["error"], "BMC authentication failed") {
		t.Errorf("error = %q, want to contain 'BMC authentication failed'", errResp["error"])
	}
}

// ---------------------------------------------------------------------------
// 8. TestIntegration_BMCUnreachable_Proxy502
// ---------------------------------------------------------------------------

func TestIntegration_BMCUnreachable_Proxy502(t *testing.T) {
	const serverName = "integ-unreachable-proxy"

	cfg := &models.AppConfig{
		Servers: []models.ServerConfig{
			{Name: serverName, BMCIP: "127.0.0.1", BMCPort: 1, BoardType: "ami_megarac", Username: "admin", CredentialEnv: "PASS"},
		},
		Settings: models.Settings{MaxConcurrentSessions: 4, Runtime: "docker"},
	}
	bmcProxies.Delete(serverName)
	t.Cleanup(func() { bmcProxies.Delete(serverName) })
	srv := newServerCore(cfg, &mockContainerManager{})

	req := httptest.NewRequest("GET", "/__bmc/"+serverName+"/page", nil)
	w := httptest.NewRecorder()
	srv.HandleBMCProxy(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "BMC unreachable") {
		t.Errorf("body = %q, want to contain 'BMC unreachable'", body)
	}
}

// ---------------------------------------------------------------------------
// 9. TestIntegration_PollStatuses_WithMockBMCs
// ---------------------------------------------------------------------------

func TestIntegration_PollStatuses_WithMockBMCs(t *testing.T) {
	const megaracName = "integ-poll-megarac"
	const nanokvmName = "integ-poll-nanokvm"

	// --- Mock MegaRAC BMC ---
	megaracMux := http.NewServeMux()
	megaracMux.HandleFunc("GET /rpc/hoststatus.asp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `{ 'JF_STATE' : 1 }`)
	})
	megaracMux.HandleFunc("GET /rpc/getfruinfo.asp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `{ 'PI_ProductName' : 'Test Server X1' }`)
	})
	megaracMux.HandleFunc("GET /rpc/getallsensors.asp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `{ 'SensorName' : 'CPU1 Temp', 'SensorReading' : 42000 }`)
	})
	megaracBMC := httptest.NewServer(megaracMux)
	t.Cleanup(megaracBMC.Close)

	// --- Mock NanoKVM BMC ---
	nanokvmMux := http.NewServeMux()
	nanokvmMux.HandleFunc("GET /api/vm/info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":0,"data":{"application":"2.3.6","image":"v1.4.2"}}`)
	})
	nanokvmMux.HandleFunc("GET /api/vm/gpio", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":0,"data":{"pwr":true}}`)
	})
	nanokvmBMC := httptest.NewServer(nanokvmMux)
	t.Cleanup(nanokvmBMC.Close)

	// Parse hosts/ports
	mHost, mPortStr, _ := strings.Cut(strings.TrimPrefix(megaracBMC.URL, "http://"), ":")
	mPort := 80
	fmt.Sscanf(mPortStr, "%d", &mPort)

	nHost, nPortStr, _ := strings.Cut(strings.TrimPrefix(nanokvmBMC.URL, "http://"), ":")
	nPort := 80
	fmt.Sscanf(nPortStr, "%d", &nPort)

	cfg := &models.AppConfig{
		Servers: []models.ServerConfig{
			{Name: megaracName, BMCIP: mHost, BMCPort: mPort, BoardType: "ami_megarac", Username: "admin", CredentialEnv: "PASS"},
			{Name: nanokvmName, BMCIP: nHost, BMCPort: nPort, BoardType: "nanokvm", Username: "admin", CredentialEnv: "PASS"},
		},
		Settings: models.Settings{MaxConcurrentSessions: 4, Runtime: "docker"},
	}

	bmcProxies.Delete(megaracName)
	bmcProxies.Delete(nanokvmName)
	t.Cleanup(func() {
		bmcProxies.Delete(megaracName)
		bmcProxies.Delete(nanokvmName)
	})

	srv := newServerCore(cfg, &mockContainerManager{})

	// Set up proxy entries with credentials
	megaracEntry := getOrCreateProxy(&cfg.Servers[0], megaracName)
	megaracEntry.setBMCCredentials(&models.BMCCredentials{
		SessionCookie: "poll-megarac-sess",
		CSRFToken:     "poll-megarac-csrf",
	})

	nanokvmEntry := getOrCreateProxy(&cfg.Servers[1], nanokvmName)
	nanokvmEntry.setBMCCredentials(&models.BMCCredentials{
		SessionCookie: "poll-nanokvm-jwt",
	})

	// --- Call PollStatuses ---
	PollStatuses(cfg.Servers, srv.StatusCache)

	// --- Call GetServerStatuses → verify JSON response ---
	req := httptest.NewRequest("GET", "/api/server-statuses", nil)
	w := httptest.NewRecorder()
	srv.GetServerStatuses(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GetServerStatuses status = %d, want 200", w.Code)
	}

	var statuses map[string]*DeviceStatus
	if err := json.NewDecoder(w.Body).Decode(&statuses); err != nil {
		t.Fatalf("failed to decode statuses response: %v", err)
	}

	// MegaRAC status
	mStatus, ok := statuses[megaracName]
	if !ok {
		t.Fatalf("missing status for %s", megaracName)
	}
	if !mStatus.Online {
		t.Errorf("%s should be online", megaracName)
	}
	if mStatus.PowerState != "on" {
		t.Errorf("%s power_state = %q, want %q", megaracName, mStatus.PowerState, "on")
	}

	// NanoKVM status
	nStatus, ok := statuses[nanokvmName]
	if !ok {
		t.Fatalf("missing status for %s", nanokvmName)
	}
	if !nStatus.Online {
		t.Errorf("%s should be online", nanokvmName)
	}
	if nStatus.PowerState != "on" {
		t.Errorf("%s power_state = %q, want %q", nanokvmName, nStatus.PowerState, "on")
	}
}

// ---------------------------------------------------------------------------
// 10. TestIntegration_AutoLoginHeader_NoCreds
// ---------------------------------------------------------------------------

func TestIntegration_AutoLoginHeader_NoCreds(t *testing.T) {
	const serverName = "integ-no-autologin"

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body>No Creds BMC</body></html>`)
	})

	bmc := httptest.NewServer(mux)
	t.Cleanup(bmc.Close)

	host, portStr, _ := strings.Cut(strings.TrimPrefix(bmc.URL, "http://"), ":")
	port := 80
	fmt.Sscanf(portStr, "%d", &port)

	cfg := &models.AppConfig{
		Servers: []models.ServerConfig{
			{Name: serverName, BMCIP: host, BMCPort: port, BoardType: "ami_megarac", Username: "admin", CredentialEnv: "PASS"},
		},
		Settings: models.Settings{MaxConcurrentSessions: 4, Runtime: "docker"},
	}
	bmcProxies.Delete(serverName)
	t.Cleanup(func() { bmcProxies.Delete(serverName) })
	srv := newServerCore(cfg, &mockContainerManager{})

	// No credentials set — just proxy through
	req := httptest.NewRequest("GET", "/__bmc/"+serverName+"/", nil)
	w := httptest.NewRecorder()
	srv.HandleBMCProxy(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	// X-KVM-AutoLogin should NOT be present
	if v := w.Header().Get("X-KVM-AutoLogin"); v != "" {
		t.Errorf("X-KVM-AutoLogin = %q, want empty (no credentials set)", v)
	}
}
