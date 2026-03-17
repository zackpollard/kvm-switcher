package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/zackpollard/kvm-switcher/internal/models"
)

func TestAPCAuthenticate_ReturnsError(t *testing.T) {
	a := &APCAuthenticator{}
	_, _, err := a.Authenticate(context.Background(), "127.0.0.1", 80, "admin", "pass")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "does not support KVM") {
		t.Fatalf("expected error containing 'does not support KVM', got: %s", err.Error())
	}
}

// apcMockServer creates a test server that simulates the APC NMC2 login flow.
// The flow is:
//
//	GET /           → 303 → /home.htm
//	GET /home.htm   → 303 → /NMC/{preToken}/logon.htm
//	POST /NMC/{preToken}/Forms/login1 → 303 → Location: {loginRedirect}
func apcMockServer(preToken, loginRedirect string) *httptest.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/home.htm", http.StatusSeeOther)
	})

	mux.HandleFunc("/home.htm", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/NMC/"+preToken+"/logon.htm", http.StatusSeeOther)
	})

	mux.HandleFunc("/NMC/"+preToken+"/logon.htm", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/NMC/"+preToken+"/Forms/login1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Location", loginRedirect)
		w.WriteHeader(http.StatusSeeOther)
	})

	return httptest.NewServer(mux)
}

func TestAPCCreateWebSession_Success(t *testing.T) {
	srv := apcMockServer("pre123", "/NMC/auth456/")
	defer srv.Close()

	host, port := hostPort(t, srv)
	a := &APCAuthenticator{}
	creds, err := a.CreateWebSession(context.Background(), host, port, "admin", "pass")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds.Extra["nmc_path"] != "/NMC/auth456" {
		t.Fatalf("expected nmc_path '/NMC/auth456', got '%s'", creds.Extra["nmc_path"])
	}
}

func TestAPCCreateWebSession_TokenWithSubpath(t *testing.T) {
	srv := apcMockServer("pre123", "/NMC/authTOK/subpage.htm")
	defer srv.Close()

	host, port := hostPort(t, srv)
	a := &APCAuthenticator{}
	creds, err := a.CreateWebSession(context.Background(), host, port, "admin", "pass")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds.Extra["nmc_path"] != "/NMC/authTOK" {
		t.Fatalf("expected nmc_path '/NMC/authTOK', got '%s'", creds.Extra["nmc_path"])
	}
}

func TestAPCCreateWebSession_TokenUnchanged(t *testing.T) {
	// Login redirect returns the same token as the pre-auth token.
	srv := apcMockServer("pre123", "/NMC/pre123/")
	defer srv.Close()

	host, port := hostPort(t, srv)
	a := &APCAuthenticator{}
	_, err := a.CreateWebSession(context.Background(), host, port, "admin", "pass")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "session token unchanged") {
		t.Fatalf("expected error containing 'session token unchanged', got: %s", err.Error())
	}
}

func TestAPCCreateWebSession_UnexpectedLoginPage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/unexpected/path", http.StatusSeeOther)
	})
	mux.HandleFunc("/unexpected/path", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	host, port := hostPort(t, srv)
	a := &APCAuthenticator{}
	_, err := a.CreateWebSession(context.Background(), host, port, "admin", "pass")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unexpected login page path") {
		t.Fatalf("expected error containing 'unexpected login page path', got: %s", err.Error())
	}
}

func TestAPCCreateWebSession_NoRedirectAfterLogin(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/home.htm", http.StatusSeeOther)
	})
	mux.HandleFunc("/home.htm", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/NMC/pre123/logon.htm", http.StatusSeeOther)
	})
	mux.HandleFunc("/NMC/pre123/logon.htm", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/NMC/pre123/Forms/login1", func(w http.ResponseWriter, r *http.Request) {
		// Return 200 with no Location header
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	host, port := hostPort(t, srv)
	a := &APCAuthenticator{}
	_, err := a.CreateWebSession(context.Background(), host, port, "admin", "pass")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no redirect after login") {
		t.Fatalf("expected error containing 'no redirect after login', got: %s", err.Error())
	}
}

func TestAPCCreateWebSession_BadRedirectPath(t *testing.T) {
	srv := apcMockServer("pre123", "/bad/path")
	defer srv.Close()

	host, port := hostPort(t, srv)
	a := &APCAuthenticator{}
	_, err := a.CreateWebSession(context.Background(), host, port, "admin", "pass")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unexpected auth redirect path") {
		t.Fatalf("expected error containing 'unexpected auth redirect path', got: %s", err.Error())
	}
}

func TestAPCCreateWebSession_ServerDown(t *testing.T) {
	a := &APCAuthenticator{}
	_, err := a.CreateWebSession(context.Background(), "127.0.0.1", 1, "admin", "pass")
	if err == nil {
		t.Fatal("expected error for unreachable host, got nil")
	}
}

func TestAPCLogout_SendsRequest(t *testing.T) {
	var mu sync.Mutex
	var requests []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	host, port := hostPort(t, srv)
	a := &APCAuthenticator{}
	creds := &models.BMCCredentials{
		Extra: map[string]string{"nmc_path": "/NMC/tok"},
	}
	err := a.Logout(context.Background(), host, port, creds)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, req := range requests {
		if req == "GET /NMC/tok/logout.htm" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected GET /NMC/tok/logout.htm, got requests: %v", requests)
	}
}

func TestAPCLogout_EmptyNMCPath(t *testing.T) {
	requestMade := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestMade = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	host, port := hostPort(t, srv)
	a := &APCAuthenticator{}
	creds := &models.BMCCredentials{
		Extra: map[string]string{},
	}
	err := a.Logout(context.Background(), host, port, creds)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if requestMade {
		t.Fatal("expected no HTTP request to be made, but one was sent")
	}
}

func TestAPCLogout_NilExtra(t *testing.T) {
	requestMade := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestMade = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	host, port := hostPort(t, srv)
	a := &APCAuthenticator{}
	creds := &models.BMCCredentials{
		Extra: nil,
	}
	err := a.Logout(context.Background(), host, port, creds)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if requestMade {
		t.Fatal("expected no HTTP request to be made, but one was sent")
	}
}

// hostPort extracts the host and port from an httptest.Server.
func hostPort(t *testing.T, srv *httptest.Server) (string, int) {
	t.Helper()
	// srv.URL is like "http://127.0.0.1:PORT"
	u := srv.URL
	// Remove scheme
	u = strings.TrimPrefix(u, "http://")
	parts := strings.SplitN(u, ":", 2)
	if len(parts) != 2 {
		t.Fatalf("unexpected server URL format: %s", srv.URL)
	}
	host := parts[0]
	var port int
	for _, c := range parts[1] {
		port = port*10 + int(c-'0')
	}
	return host, port
}
