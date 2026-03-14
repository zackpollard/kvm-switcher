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

	"github.com/zackpollard/kvm-switcher/internal/models"
)

// bmcProxyEntry holds a reverse proxy and its cookie jar for a single BMC.
type bmcProxyEntry struct {
	proxy     *httputil.ReverseProxy
	jar       http.CookieJar
	mu        sync.RWMutex
	bmcCreds  *models.BMCCredentials // pre-authenticated BMC session credentials
	boardType string
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

	// Intercept login requests and return cached credentials instead of
	// creating a new BMC session. This prevents session buildup and
	// provides auto-login functionality.
	if handled := handleLoginIntercept(w, r, remainder, entry); handled {
		return
	}

	// Rewrite the request path to the remainder
	r.URL.Path = remainder
	if r.URL.RawPath != "" {
		r.URL.RawPath = remainder
	}

	entry.proxy.ServeHTTP(w, r)
}

// handleLoginIntercept checks if the request is a BMC login and returns cached
// credentials instead of forwarding to the BMC. Returns true if handled.
func handleLoginIntercept(w http.ResponseWriter, r *http.Request, path string, entry *bmcProxyEntry) bool {
	creds := entry.getBMCCredentials()
	if creds == nil {
		return false
	}

	switch entry.boardType {
	case "dell_idrac9":
		// iDRAC9 login: POST /sysmgmt/2015/bmc/session
		if r.Method == http.MethodPost && path == "/sysmgmt/2015/bmc/session" {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("XSRF-TOKEN", creds.CSRFToken)
			http.SetCookie(w, &http.Cookie{
				Name:     "-http-session-",
				Value:    creds.SessionCookie,
				Path:     "/",
				Secure:   true,
				HttpOnly: true,
			})
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"authResult":0}`)
			log.Printf("BMC proxy: intercepted iDRAC9 login, returning cached session")
			return true
		}

	case "dell_idrac8":
		// iDRAC8 pre-login logout: the login page POSTs to /data/logout
		// before submitting credentials (session fixation prevention).
		// Intercept this to prevent it from invalidating our managed session.
		if r.Method == http.MethodPost && path == "/data/logout" {
			w.Header().Set("Content-Type", "text/xml")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><root><status>ok</status></root>`)
			log.Printf("BMC proxy: intercepted iDRAC8 pre-login logout, returning fake OK")
			return true
		}

		// iDRAC8 login: POST /data/login
		if r.Method == http.MethodPost && path == "/data/login" {
			st1 := ""
			if creds.Extra != nil {
				st1 = creds.Extra["st1"]
			}
			w.Header().Set("Content-Type", "text/xml")
			http.SetCookie(w, &http.Cookie{
				Name:     "-http-session-",
				Value:    creds.SessionCookie,
				Path:     "/",
				Secure:   true,
				HttpOnly: true,
			})
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?> <root> <status>ok</status> <authResult>0</authResult> <forwardUrl>index.html?ST1=%s,ST2=%s</forwardUrl> </root>`, st1, creds.CSRFToken)
			log.Printf("BMC proxy: intercepted iDRAC8 login, returning cached session")
			return true
		}
	}

	return false
}

// bmcScheme returns the URL scheme for a given board type.
func bmcScheme(boardType string) string {
	switch boardType {
	case "dell_idrac8", "dell_idrac9":
		return "https"
	default:
		return "http"
	}
}

// getOrCreateProxy returns a cached proxy entry for the given server.
func getOrCreateProxy(serverCfg *models.ServerConfig, name string) *bmcProxyEntry {
	if v, ok := bmcProxies.Load(name); ok {
		return v.(*bmcProxyEntry)
	}

	scheme := bmcScheme(serverCfg.BoardType)
	bmcOrigin := fmt.Sprintf("%s://%s:%d", scheme, serverCfg.BMCIP, serverCfg.BMCPort)
	target, _ := url.Parse(bmcOrigin)

	// Cookie jar stores BMC session cookies server-side.
	jar, _ := cookiejar.New(nil)

	entry := &bmcProxyEntry{jar: jar, boardType: serverCfg.BoardType}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host

			// Some BMCs (iDRAC9) require Accept-Encoding to serve static files.
			// Ensure it's always set so CSS/JS/images load correctly.
			if req.Header.Get("Accept-Encoding") == "" {
				req.Header.Set("Accept-Encoding", "gzip, deflate")
			}

			// Strip our auth cookies and browser-side BMC cookies
			filterCookies(req, "kvm_session", "kvm_oauth_state", "SessionCookie", "-http-session-")

			// Add stored BMC cookies from the jar
			for _, c := range jar.Cookies(req.URL) {
				req.AddCookie(c)
			}

			// Inject pre-authenticated BMC credentials (board-type-specific)
			if creds := entry.getBMCCredentials(); creds != nil {
				injectBMCCredentials(req, entry.boardType, creds)
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

			// iDRAC8 sets Content-Type: application/x-gzip on some responses
			// (typically 302 redirects). Fix to the correct MIME type.
			if resp.Header.Get("Content-Type") == "application/x-gzip" {
				resp.Header.Set("Content-Type", inferContentType(resp.Request.URL.Path))
			}

			// Decompress gzip responses at the proxy level so the service
			// worker receives plain content. This avoids browser-specific
			// differences in how SWs handle Content-Encoding (Firefox may
			// not transparently decompress in SW fetch, causing downloads).
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

			// iDRAC8 serves .jsesp (embedded JS) files as text/html. Chrome's strict
			// MIME type checking blocks script execution for non-JS Content-Types.
			if strings.HasSuffix(resp.Request.URL.Path, ".jsesp") {
				resp.Header.Set("Content-Type", "application/javascript")
			}

			// iDRAC8 firmware omits the X_Language header on /session API
			// responses. The login page JS reads this header to determine the
			// locale and silently fails (null.substring throws in a try/catch),
			// preventing loadLocale() from ever being called and leaving the
			// login form hidden. Inject the header so the page renders.
			if serverCfg.BoardType == "dell_idrac8" && resp.Header.Get("X_Language") == "" {
				resp.Header.Set("X_Language", "en")
			}

			// Signal to the service worker that auto-login is available.
			// The SW uses this to inject auto-submit scripts into login pages.
			if entry.getBMCCredentials() != nil {
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

// injectBMCCredentials adds board-type-specific auth to an outgoing proxy request.
func injectBMCCredentials(req *http.Request, boardType string, creds *models.BMCCredentials) {
	switch boardType {
	case "dell_idrac9":
		// iDRAC9: -http-session- cookie + XSRF-TOKEN header
		req.AddCookie(&http.Cookie{Name: "-http-session-", Value: creds.SessionCookie})
		if creds.CSRFToken != "" {
			req.Header.Set("XSRF-TOKEN", creds.CSRFToken)
		}

	case "dell_idrac8":
		// iDRAC8: -http-session- cookie + ST2 header.
		// ST1 is NOT injected here — it's a URL parameter managed by the
		// browser-side JS after login. Injecting it server-side corrupts
		// query strings on unauthenticated API calls.
		req.AddCookie(&http.Cookie{Name: "-http-session-", Value: creds.SessionCookie})
		if creds.CSRFToken != "" {
			req.Header.Set("ST2", creds.CSRFToken)
		}

	default:
		// AMI MegaRAC: SessionCookie cookie + CSRFTOKEN header
		req.AddCookie(&http.Cookie{Name: "SessionCookie", Value: creds.SessionCookie})
		if creds.CSRFToken != "" {
			req.Header.Set("CSRFTOKEN", creds.CSRFToken)
		}
	}
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
	switch {
	case strings.HasSuffix(path, ".html"), strings.HasSuffix(path, ".htm"):
		return "text/html"
	case strings.HasSuffix(path, ".js"), strings.HasSuffix(path, ".jsesp"):
		return "application/javascript"
	case strings.HasSuffix(path, ".css"):
		return "text/css"
	case strings.HasSuffix(path, ".json"):
		return "application/json"
	case strings.HasSuffix(path, ".png"):
		return "image/png"
	case strings.HasSuffix(path, ".gif"):
		return "image/gif"
	case strings.HasSuffix(path, ".jpg"), strings.HasSuffix(path, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(path, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(path, ".xml"):
		return "text/xml"
	default:
		return "text/html"
	}
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
