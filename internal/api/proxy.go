package api

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"

	"github.com/zackpollard/kvm-switcher/internal/models"
	kvmoidc "github.com/zackpollard/kvm-switcher/internal/oidc"
)

// ipmiProxyTransport is shared across all IPMI proxy requests.
var ipmiProxyTransport = &http.Transport{
	TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
}

// IPMIProxyManager manages per-server reverse proxy listeners.
// Each BMC gets its own port so content is served at the root path,
// avoiding any response body rewriting.
type IPMIProxyManager struct {
	mu      sync.Mutex
	ports   map[string]int // server name -> allocated port
	servers map[string]*http.Server
	config  *models.AppConfig
	oidc    bool
}

// NewIPMIProxyManager creates the manager and starts a listener for each server.
func NewIPMIProxyManager(cfg *models.AppConfig) (*IPMIProxyManager, error) {
	m := &IPMIProxyManager{
		ports:   make(map[string]int),
		servers: make(map[string]*http.Server),
		config:  cfg,
		oidc:    cfg.OIDC.Enabled,
	}

	for i := range cfg.Servers {
		srv := &cfg.Servers[i]
		port, err := m.startProxy(srv)
		if err != nil {
			m.Close()
			return nil, fmt.Errorf("starting IPMI proxy for %s: %w", srv.Name, err)
		}
		log.Printf("IPMI proxy for %s (%s:%d) listening on port %d", srv.Name, srv.BMCIP, srv.BMCPort, port)
		m.ports[srv.Name] = port
	}

	return m, nil
}

// GetPort returns the local proxy port for a server, or 0 if not found.
func (m *IPMIProxyManager) GetPort(serverName string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ports[serverName]
}

// Close shuts down all proxy listeners.
func (m *IPMIProxyManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, srv := range m.servers {
		srv.Close()
		delete(m.servers, name)
	}
}

func (m *IPMIProxyManager) startProxy(serverCfg *models.ServerConfig) (int, error) {
	bmcOrigin := fmt.Sprintf("http://%s:%d", serverCfg.BMCIP, serverCfg.BMCPort)
	target, _ := url.Parse(bmcOrigin)

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host

			// Strip our auth cookies
			filterCookies(req, "kvm_session", "kvm_oauth_state")
		},
		ModifyResponse: func(resp *http.Response) error {
			// Rewrite Location header so redirects stay through the proxy
			if loc := resp.Header.Get("Location"); loc != "" {
				resp.Header.Set("Location", rewriteLocationToLocal(loc, bmcOrigin))
			}

			// Remove headers that block framing/embedding
			resp.Header.Del("Content-Security-Policy")
			resp.Header.Del("X-Frame-Options")
			return nil
		},
		Transport: ipmiProxyTransport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("IPMI proxy error for %s: %v", serverCfg.Name, err)
			http.Error(w, "BMC unreachable: "+err.Error(), http.StatusBadGateway)
		},
	}

	// Wrap with OIDC check if enabled
	var handler http.Handler = proxy
	if m.oidc {
		serverName := serverCfg.Name
		oidcCfg := &m.config.OIDC
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user := kvmoidc.UserFromContext(r.Context())
			if !kvmoidc.UserCanAccessServer(oidcCfg, user, serverName) {
				http.Error(w, "access denied", http.StatusForbidden)
				return
			}
			proxy.ServeHTTP(w, r)
		})
	}

	// Listen on ephemeral port
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}

	port := ln.Addr().(*net.TCPAddr).Port
	srv := &http.Server{Handler: handler}

	m.mu.Lock()
	m.servers[serverCfg.Name] = srv
	m.mu.Unlock()

	go srv.Serve(ln)

	return port, nil
}

// rewriteLocationToLocal strips the BMC origin from absolute Location headers,
// converting them to root-relative paths so the browser stays on the proxy port.
func rewriteLocationToLocal(loc, bmcOrigin string) string {
	if strings.HasPrefix(loc, bmcOrigin) {
		return strings.TrimPrefix(loc, bmcOrigin)
	}
	// Also handle without explicit port (BMC might omit :80)
	if idx := strings.Index(bmcOrigin, ":80"); idx > 0 {
		noPort := bmcOrigin[:idx]
		if strings.HasPrefix(loc, noPort) {
			return strings.TrimPrefix(loc, noPort)
		}
	}
	return loc
}

// filterCookies removes named cookies from the outgoing request.
func filterCookies(req *http.Request, names ...string) {
	cookies := req.Cookies()
	if len(cookies) == 0 {
		return
	}

	skip := make(map[string]bool, len(names))
	for _, n := range names {
		skip[n] = true
	}

	req.Header.Del("Cookie")
	for _, c := range cookies {
		if !skip[c.Name] {
			req.AddCookie(c)
		}
	}
}

// isTextContent returns true if the Content-Type indicates text-based content.
func isTextContent(ct string) bool {
	ct = strings.ToLower(ct)
	return strings.Contains(ct, "text/html") ||
		strings.Contains(ct, "text/javascript") ||
		strings.Contains(ct, "application/javascript") ||
		strings.Contains(ct, "text/css") ||
		strings.Contains(ct, "application/json") ||
		strings.Contains(ct, "text/xml") ||
		strings.Contains(ct, "application/xml")
}

// HandleIPMIProxy handles /api/ipmi-ports to return the proxy port mapping.
func (s *Server) HandleIPMIProxy(w http.ResponseWriter, r *http.Request) {
	// This is now unused for direct proxying - see IPMIProxyManager
	writeError(w, http.StatusNotFound, "use /api/ipmi-ports for proxy port mapping")
}
