package api

import (
	"crypto/tls"
	"fmt"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"

	"github.com/zackpollard/kvm-switcher/internal/models"
)

// bmcProxyEntry holds a reverse proxy and its cookie jar for a single BMC.
type bmcProxyEntry struct {
	proxy    *httputil.ReverseProxy
	jar      http.CookieJar
	mu       sync.RWMutex
	bmcCreds *models.BMCCredentials // pre-authenticated BMC session credentials
}

func (e *bmcProxyEntry) setBMCCredentials(creds *models.BMCCredentials) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.bmcCreds = creds
}

func (e *bmcProxyEntry) getBMCCredentials() *models.BMCCredentials {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.bmcCreds
}

// bmcProxies caches a proxy entry per server name.
var bmcProxies sync.Map // map[string]*bmcProxyEntry

// HandleBMCProxy handles /__bmc/{name}/{path...} — reverse-proxies to the BMC.
func (s *Server) HandleBMCProxy(w http.ResponseWriter, r *http.Request) {
	// Extract server name from path: /__bmc/{name}/...
	path := strings.TrimPrefix(r.URL.Path, "/__bmc/")
	slashIdx := strings.Index(path, "/")
	var name, remainder string
	if slashIdx < 0 {
		name = path
		remainder = "/"
	} else {
		name = path[:slashIdx]
		remainder = path[slashIdx:]
	}

	if name == "" {
		http.Error(w, "missing server name", http.StatusBadRequest)
		return
	}

	// Find server config
	var serverCfg *models.ServerConfig
	for i := range s.Config.Servers {
		if s.Config.Servers[i].Name == name {
			serverCfg = &s.Config.Servers[i]
			break
		}
	}
	if serverCfg == nil {
		http.Error(w, "unknown server", http.StatusNotFound)
		return
	}

	// Get or create cached proxy
	entry := getOrCreateProxy(serverCfg, name)

	// Rewrite the request path to the remainder
	r.URL.Path = remainder
	if r.URL.RawPath != "" {
		r.URL.RawPath = remainder
	}

	entry.proxy.ServeHTTP(w, r)
}

// getOrCreateProxy returns a cached proxy entry for the given server.
func getOrCreateProxy(serverCfg *models.ServerConfig, name string) *bmcProxyEntry {
	if v, ok := bmcProxies.Load(name); ok {
		return v.(*bmcProxyEntry)
	}

	bmcOrigin := fmt.Sprintf("http://%s:%d", serverCfg.BMCIP, serverCfg.BMCPort)
	target, _ := url.Parse(bmcOrigin)

	// Cookie jar stores BMC session cookies server-side.
	// Browsers can't set cookies from SW respondWith() responses,
	// so the backend must manage them.
	jar, _ := cookiejar.New(nil)

	// Create entry first so the proxy closures can reference it
	// for pre-authenticated BMC credentials.
	entry := &bmcProxyEntry{jar: jar}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host

			// Strip our auth cookies and browser-side BMC cookies
			// (the proxy manages BMC session cookies server-side)
			filterCookies(req, "kvm_session", "kvm_oauth_state", "SessionCookie")

			// Add stored BMC cookies from the jar
			for _, c := range jar.Cookies(req.URL) {
				req.AddCookie(c)
			}

			// Inject pre-authenticated BMC credentials if available.
			// This ensures all proxied requests carry the session cookie
			// and CSRF token, even if the browser/SW didn't send them.
			if creds := entry.getBMCCredentials(); creds != nil {
				req.AddCookie(&http.Cookie{Name: "SessionCookie", Value: creds.SessionCookie})
				if creds.CSRFToken != "" {
					// AMI MegaRAC checks the "CSRFTOKEN" header (no X- prefix)
					// on non-WEBSES endpoints. Set it so all proxied requests
					// pass CSRF validation even before the browser has the token.
					req.Header.Set("CSRFTOKEN", creds.CSRFToken)
				}
			}

		},
		ModifyResponse: func(resp *http.Response) error {
			// Store any Set-Cookie from the BMC in our jar
			if cookies := resp.Header["Set-Cookie"]; len(cookies) > 0 {
				jar.SetCookies(resp.Request.URL, resp.Cookies())
			}

			// Rewrite Location header so redirects stay through the proxy
			if loc := resp.Header.Get("Location"); loc != "" {
				resp.Header.Set("Location", rewriteLocationForBMC(loc, bmcOrigin, name))
			}

			// Remove headers that block framing/embedding
			resp.Header.Del("Content-Security-Policy")
			resp.Header.Del("X-Frame-Options")
			return nil
		},
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("BMC proxy error for %s: %v", name, err)
			http.Error(w, "BMC unreachable: "+err.Error(), http.StatusBadGateway)
		},
	}

	entry.proxy = proxy
	actual, _ := bmcProxies.LoadOrStore(name, entry)
	return actual.(*bmcProxyEntry)
}

// rewriteLocationForBMC converts BMC Location headers to /__bmc/{name}/... paths.
func rewriteLocationForBMC(loc, bmcOrigin, name string) string {
	prefix := "/__bmc/" + name

	// Absolute URL from BMC -> strip origin, add prefix
	if strings.HasPrefix(loc, bmcOrigin) {
		return prefix + strings.TrimPrefix(loc, bmcOrigin)
	}
	// Handle without explicit port (BMC might omit :80)
	if idx := strings.Index(bmcOrigin, ":80"); idx > 0 {
		noPort := bmcOrigin[:idx]
		if strings.HasPrefix(loc, noPort) {
			return prefix + strings.TrimPrefix(loc, noPort)
		}
	}

	// Root-relative path from BMC -> add prefix
	if strings.HasPrefix(loc, "/") {
		return prefix + loc
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
