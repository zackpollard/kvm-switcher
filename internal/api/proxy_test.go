package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
		for _, c := range r.Cookies() {
			if c.Name == "kvm_session" {
				hasKVMSession = true
			}
		}
		fmt.Fprintf(w, `{"has_kvm_session":%v}`, hasKVMSession)
	})

	return httptest.NewServer(mux)
}

func TestIPMIProxyManager_StartsListeners(t *testing.T) {
	bmc := newMockBMC(t)
	defer bmc.Close()

	host, portStr, _ := strings.Cut(strings.TrimPrefix(bmc.URL, "http://"), ":")
	port := 80
	fmt.Sscanf(portStr, "%d", &port)

	cfg := &models.AppConfig{
		Servers: []models.ServerConfig{
			{Name: "test-bmc", BMCIP: host, BMCPort: port, BoardType: "ami_megarac", Username: "admin", CredentialEnv: "PASS"},
		},
	}

	mgr, err := NewIPMIProxyManager(cfg)
	if err != nil {
		t.Fatalf("NewIPMIProxyManager() error: %v", err)
	}
	defer mgr.Close()

	p := mgr.GetPort("test-bmc")
	if p == 0 {
		t.Fatal("expected non-zero port for test-bmc")
	}

	// Request through the proxy
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", p))
	if err != nil {
		t.Fatalf("GET / error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestIPMIProxyManager_NoContentRewriting(t *testing.T) {
	bmc := newMockBMC(t)
	defer bmc.Close()

	host, portStr, _ := strings.Cut(strings.TrimPrefix(bmc.URL, "http://"), ":")
	port := 80
	fmt.Sscanf(portStr, "%d", &port)

	cfg := &models.AppConfig{
		Servers: []models.ServerConfig{
			{Name: "test-bmc", BMCIP: host, BMCPort: port, BoardType: "ami_megarac", Username: "admin", CredentialEnv: "PASS"},
		},
	}

	mgr, err := NewIPMIProxyManager(cfg)
	if err != nil {
		t.Fatalf("NewIPMIProxyManager() error: %v", err)
	}
	defer mgr.Close()

	p := mgr.GetPort("test-bmc")

	// HTML content should pass through UNMODIFIED
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", p))
	if err != nil {
		t.Fatalf("GET / error: %v", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	body := string(bodyBytes)

	if !strings.Contains(body, `<title>BMC</title>`) {
		t.Errorf("HTML content was modified.\nbody: %s", body)
	}
	// No proxy prefix should appear in the content
	if strings.Contains(body, "ipmi") {
		t.Errorf("proxy prefix found in content, should be unmodified.\nbody: %s", body)
	}
}

func TestIPMIProxyManager_JSONPassthrough(t *testing.T) {
	bmc := newMockBMC(t)
	defer bmc.Close()

	host, portStr, _ := strings.Cut(strings.TrimPrefix(bmc.URL, "http://"), ":")
	port := 80
	fmt.Sscanf(portStr, "%d", &port)

	cfg := &models.AppConfig{
		Servers: []models.ServerConfig{
			{Name: "test-bmc", BMCIP: host, BMCPort: port, BoardType: "ami_megarac", Username: "admin", CredentialEnv: "PASS"},
		},
	}

	mgr, err := NewIPMIProxyManager(cfg)
	if err != nil {
		t.Fatalf("NewIPMIProxyManager() error: %v", err)
	}
	defer mgr.Close()

	p := mgr.GetPort("test-bmc")

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/rpc/status", p))
	if err != nil {
		t.Fatalf("GET /rpc/status error: %v", err)
	}
	defer resp.Body.Close()

	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)
	if result["status"] != "ok" {
		t.Errorf("JSON response corrupted: %v", result)
	}
}

func TestIPMIProxyManager_CookieStripping(t *testing.T) {
	bmc := newMockBMC(t)
	defer bmc.Close()

	host, portStr, _ := strings.Cut(strings.TrimPrefix(bmc.URL, "http://"), ":")
	port := 80
	fmt.Sscanf(portStr, "%d", &port)

	cfg := &models.AppConfig{
		Servers: []models.ServerConfig{
			{Name: "test-bmc", BMCIP: host, BMCPort: port, BoardType: "ami_megarac", Username: "admin", CredentialEnv: "PASS"},
		},
	}

	mgr, err := NewIPMIProxyManager(cfg)
	if err != nil {
		t.Fatalf("NewIPMIProxyManager() error: %v", err)
	}
	defer mgr.Close()

	p := mgr.GetPort("test-bmc")

	req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/api/check-cookies", p), nil)
	req.AddCookie(&http.Cookie{Name: "kvm_session", Value: "should-strip"})
	req.AddCookie(&http.Cookie{Name: "BMCSession", Value: "should-keep"})

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request error: %v", err)
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	if result["has_kvm_session"] == true {
		t.Error("kvm_session cookie should have been stripped")
	}
}

func TestIPMIProxyManager_UnknownServer(t *testing.T) {
	cfg := &models.AppConfig{
		Servers: []models.ServerConfig{},
	}

	mgr, err := NewIPMIProxyManager(cfg)
	if err != nil {
		t.Fatalf("NewIPMIProxyManager() error: %v", err)
	}
	defer mgr.Close()

	if p := mgr.GetPort("nonexistent"); p != 0 {
		t.Errorf("expected 0 for unknown server, got %d", p)
	}
}

func TestIPMIPorts_Endpoint(t *testing.T) {
	bmc := newMockBMC(t)
	defer bmc.Close()

	host, portStr, _ := strings.Cut(strings.TrimPrefix(bmc.URL, "http://"), ":")
	port := 80
	fmt.Sscanf(portStr, "%d", &port)

	cfg := &models.AppConfig{
		Servers: []models.ServerConfig{
			{Name: "test-bmc", BMCIP: host, BMCPort: port, BoardType: "ami_megarac", Username: "admin", CredentialEnv: "PASS"},
		},
		Settings: models.Settings{MaxConcurrentSessions: 4, Runtime: "docker"},
	}

	mgr, err := NewIPMIProxyManager(cfg)
	if err != nil {
		t.Fatalf("NewIPMIProxyManager() error: %v", err)
	}
	defer mgr.Close()

	srv := NewServer(cfg, &mockContainerManager{})
	srv.IPMIProxies = mgr

	req := httptest.NewRequest("GET", "/api/ipmi-ports", nil)
	w := httptest.NewRecorder()
	srv.IPMIPorts(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var ports map[string]int
	json.NewDecoder(w.Body).Decode(&ports)

	if ports["test-bmc"] == 0 {
		t.Error("expected non-zero port for test-bmc in response")
	}
}
