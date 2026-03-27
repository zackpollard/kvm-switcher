package api

import (
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/zackpollard/kvm-switcher/internal/boards"
	"github.com/zackpollard/kvm-switcher/internal/models"
)

// bmcProxyEntry holds a reverse proxy and its cookie jar for a single BMC.
type bmcProxyEntry struct {
	proxy     *httputil.ReverseProxy
	jar       http.CookieJar
	mu        sync.RWMutex
	bmcCreds  *models.BMCCredentials // pre-authenticated BMC session credentials
	boardType string
	kvmActive bool // true when an iKVM bridge is using this entry's BMC session
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

	handler, _ := boards.Get(serverCfg.BoardType)

	// If we have cached credentials, bypass the login page entirely
	if r.Method == http.MethodGet && handler != nil {
		if creds := entry.getBMCCredentials(); creds != nil {
			if redirectURL := handler.LoginBypass(remainder, creds); redirectURL != "" {
				http.Redirect(w, r, redirectURL, http.StatusFound)
				return
			}
		}
	}

	// Intercept login POST requests and return cached credentials
	if handler != nil {
		if creds := entry.getBMCCredentials(); creds != nil {
			if handler.LoginIntercept(w, r, remainder, creds) {
				return
			}
		}
	}

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

	handler, _ := boards.Get(serverCfg.BoardType)

	scheme := "http"
	if handler != nil {
		scheme = handler.Scheme()
	}
	bmcOrigin := fmt.Sprintf("%s://%s:%d", scheme, serverCfg.BMCIP, serverCfg.BMCPort)
	target, err := url.Parse(bmcOrigin)
	if err != nil {
		log.Printf("BMC proxy: failed to parse origin URL %q: %v", bmcOrigin, err)
		target, _ = url.Parse("http://localhost") // fallback; should not happen
	}

	// Cookie jar stores BMC session cookies server-side.
	jar, err := cookiejar.New(nil)
	if err != nil {
		log.Printf("BMC proxy: failed to create cookie jar: %v", err)
	}

	entry := &bmcProxyEntry{jar: jar, boardType: serverCfg.BoardType}

	// Build the cookie strip list: defaults + board-specific
	cookieStripList := []string{"kvm_session", "kvm_oauth_state"}
	if handler != nil {
		cookieStripList = append(cookieStripList, handler.CookiesToStrip()...)
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host

			// Some BMCs (iDRAC9) require Accept-Encoding to serve static files.
			if req.Header.Get("Accept-Encoding") == "" {
				req.Header.Set("Accept-Encoding", "gzip, deflate")
			}

			// Strip our auth cookies and browser-side BMC cookies
			filterCookies(req, cookieStripList...)

			// Add stored BMC cookies from the jar
			for _, c := range jar.Cookies(req.URL) {
				req.AddCookie(c)
			}

			// Inject pre-authenticated BMC credentials (board-type-specific)
			if creds := entry.getBMCCredentials(); creds != nil && handler != nil {
				handler.InjectCredentials(req, creds)
				handler.RewriteRequestURL(req, creds)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			// Store any Set-Cookie from the BMC in our jar
			if cookies := resp.Header["Set-Cookie"]; len(cookies) > 0 {
				jar.SetCookies(resp.Request.URL, resp.Cookies())
			}

			// Rewrite Location header so redirects stay through the proxy
			if loc := resp.Header.Get("Location"); loc != "" {
				rewritten := rewriteLocationForBMC(loc, bmcOrigin, name)
				if handler != nil {
					rewritten = handler.RewriteLocationHeader(rewritten, "/__bmc/"+name)
				}
				resp.Header.Set("Location", rewritten)
			}

			// Decompress gzip responses at the proxy level so the service
			// worker receives plain content.
			if resp.Header.Get("Content-Encoding") == "gzip" {
				if reader, err := gzip.NewReader(resp.Body); err == nil {
					decompressed, readErr := io.ReadAll(reader)
					reader.Close()
					if readErr == nil {
						resp.Body = io.NopCloser(bytes.NewReader(decompressed))
						resp.Header.Del("Content-Encoding")
						resp.ContentLength = int64(len(decompressed))
						resp.Header.Set("Content-Length", strconv.Itoa(len(decompressed)))
					}
				}
			}

			// Board-type-specific response modifications (always applied)
			creds := entry.getBMCCredentials()
			if handler != nil {
				handler.ModifyProxyResponse(resp, creds)
			}

			// Signal to the service worker that auto-login is available.
			if creds != nil {
				resp.Header.Set("X-KVM-AutoLogin", "true")
			}

			// Remove headers that block framing/embedding or trigger downloads
			resp.Header.Del("Content-Security-Policy")
			resp.Header.Del("X-Frame-Options")
			resp.Header.Del("Content-Disposition")
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
	// Handle without explicit port (BMC might omit default port)
	if idx := strings.LastIndex(bmcOrigin, ":"); idx > 0 {
		portSuffix := bmcOrigin[idx:]
		if portSuffix == ":80" || portSuffix == ":443" {
			noPort := bmcOrigin[:idx]
			if strings.HasPrefix(loc, noPort+"/") {
				return prefix + strings.TrimPrefix(loc, noPort)
			}
		}
	}

	// Root-relative path from BMC -> add prefix
	if strings.HasPrefix(loc, "/") {
		return prefix + loc
	}

	return loc
}

// inferContentType returns a MIME type based on the URL path extension.
// Used when the BMC sends a generic Content-Type like application/x-gzip.
func inferContentType(path string) string {
	return boards.InferContentType(path)
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
