package api

import (
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/zackpollard/kvm-switcher/internal/models"
	kvmoidc "github.com/zackpollard/kvm-switcher/internal/oidc"
)

// htmlAttrRe matches href, src, action, and formaction HTML attributes with absolute paths.
// It captures the attribute name and delimiter to rewrite only well-known URL attributes.
var htmlAttrRe = regexp.MustCompile(`(?i)((?:href|src|action|formaction)\s*=\s*["'])\s*/([^/])`)

// cssURLRe matches CSS url() values with absolute paths.
var cssURLRe = regexp.MustCompile(`(?i)(url\(\s*["']?)\s*/([^/])`)

// jsStringLiteralRe matches JavaScript quoted string literals containing absolute paths.
// This handles inline <script> blocks in HTML where variables like var x = "/page/" appear,
// as well as document.write() calls that create elements with absolute src paths.
// It only matches double-quoted and single-quoted strings (not regex literals like /pattern/).
var jsStringLiteralRe = regexp.MustCompile(`(["'])/([^/"'])`)


// ipmiProxyTransport is shared across all IPMI proxy requests.
// It skips TLS verification since BMCs commonly use self-signed certificates.
var ipmiProxyTransport = &http.Transport{
	TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
}

// HandleIPMIProxy reverse-proxies HTTP requests to a BMC web interface.
// Requests to /ipmi/{name}/... are forwarded to http://bmc_ip:bmc_port/...
// with response content rewritten so absolute paths route back through the proxy.
func (s *Server) HandleIPMIProxy(w http.ResponseWriter, r *http.Request) {
	// Parse /ipmi/{name}/...
	trimmed := strings.TrimPrefix(r.URL.Path, "/ipmi/")
	if trimmed == "" || trimmed == r.URL.Path {
		writeError(w, http.StatusBadRequest, "server name required")
		return
	}

	serverName, targetPath, _ := strings.Cut(trimmed, "/")
	targetPath = "/" + targetPath

	// Find server config
	var serverCfg *models.ServerConfig
	for i := range s.Config.Servers {
		if s.Config.Servers[i].Name == serverName {
			serverCfg = &s.Config.Servers[i]
			break
		}
	}
	if serverCfg == nil {
		writeError(w, http.StatusNotFound, "server not found")
		return
	}

	// OIDC access check
	if s.Config.OIDC.Enabled {
		user := kvmoidc.UserFromContext(r.Context())
		if !kvmoidc.UserCanAccessServer(&s.Config.OIDC, user, serverName) {
			writeError(w, http.StatusForbidden, "access denied")
			return
		}
	}

	proxyPrefix := "/ipmi/" + serverName
	bmcHost := net.JoinHostPort(serverCfg.BMCIP, strconv.Itoa(serverCfg.BMCPort))
	bmcOrigin := "http://" + bmcHost
	// BMC may generate URLs without port when using port 80
	bmcOriginNoPort := "http://" + serverCfg.BMCIP

	target, _ := url.Parse(bmcOrigin)

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.URL.Path = targetPath
			req.URL.RawQuery = r.URL.RawQuery
			req.Host = target.Host

			// Strip our auth cookies so they aren't forwarded to the BMC
			filterCookies(req, "kvm_session", "kvm_oauth_state")

			// Set Referer to BMC origin so the BMC accepts the request
			if ref := req.Header.Get("Referer"); ref != "" {
				req.Header.Set("Referer", rewriteReferer(ref, proxyPrefix, bmcOrigin))
			}

			// Rewrite Origin header for POST/PUT requests
			if origin := req.Header.Get("Origin"); origin != "" {
				req.Header.Set("Origin", bmcOrigin)
			}

			// Remove Accept-Encoding to simplify body rewriting (avoid gzip from BMC)
			req.Header.Del("Accept-Encoding")
		},
		ModifyResponse: func(resp *http.Response) error {
			// Rewrite Location header for redirects
			if loc := resp.Header.Get("Location"); loc != "" {
				resp.Header.Set("Location", rewriteURL(loc, proxyPrefix, bmcOrigin, bmcOriginNoPort))
			}

			// Rewrite Refresh header if present
			if refresh := resp.Header.Get("Refresh"); refresh != "" {
				resp.Header.Set("Refresh", rewriteRefreshHeader(refresh, proxyPrefix, bmcOrigin, bmcOriginNoPort))
			}

			// Rewrite Set-Cookie paths
			rewriteSetCookiePaths(resp, proxyPrefix)

			// Remove headers that would block the proxied UI
			resp.Header.Del("Content-Security-Policy")
			resp.Header.Del("X-Frame-Options")

			// Only rewrite HTML body content
			ct := strings.ToLower(resp.Header.Get("Content-Type"))
			if strings.Contains(ct, "text/html") {
				return rewriteHTMLBody(resp, proxyPrefix, bmcOrigin, bmcOriginNoPort)
			}

			return nil
		},
		Transport: ipmiProxyTransport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("IPMI proxy error for %s: %v", serverName, err)
			http.Error(w, "BMC unreachable: "+err.Error(), http.StatusBadGateway)
		},
	}

	proxy.ServeHTTP(w, r)
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

// rewriteURL rewrites absolute BMC URLs and paths to go through the proxy prefix.
func rewriteURL(loc, proxyPrefix, bmcOrigin, bmcOriginNoPort string) string {
	// Full BMC URL with port
	if strings.HasPrefix(loc, bmcOrigin) {
		return proxyPrefix + strings.TrimPrefix(loc, bmcOrigin)
	}
	// Full BMC URL without port
	if bmcOrigin != bmcOriginNoPort && strings.HasPrefix(loc, bmcOriginNoPort) {
		return proxyPrefix + strings.TrimPrefix(loc, bmcOriginNoPort)
	}
	// Absolute path not already prefixed
	if strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, proxyPrefix) {
		return proxyPrefix + loc
	}
	return loc
}

// rewriteRefreshHeader rewrites URLs in HTTP Refresh headers (e.g. "0;url=/page").
func rewriteRefreshHeader(refresh, proxyPrefix, bmcOrigin, bmcOriginNoPort string) string {
	parts := strings.SplitN(refresh, ";", 2)
	if len(parts) < 2 {
		return refresh
	}
	urlPart := strings.TrimSpace(parts[1])
	if strings.HasPrefix(strings.ToLower(urlPart), "url=") {
		rawURL := strings.TrimSpace(urlPart[4:])
		rewritten := rewriteURL(rawURL, proxyPrefix, bmcOrigin, bmcOriginNoPort)
		return parts[0] + ";url=" + rewritten
	}
	return refresh
}

// rewriteReferer converts the Referer header back to the BMC origin.
func rewriteReferer(ref, proxyPrefix, bmcOrigin string) string {
	u, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	if strings.HasPrefix(u.Path, proxyPrefix) {
		u.Path = strings.TrimPrefix(u.Path, proxyPrefix)
		if u.Path == "" {
			u.Path = "/"
		}
	}
	bmcURL, _ := url.Parse(bmcOrigin)
	u.Scheme = bmcURL.Scheme
	u.Host = bmcURL.Host
	return u.String()
}

// rewriteSetCookiePaths rewrites cookie Path directives to include the proxy prefix.
func rewriteSetCookiePaths(resp *http.Response, proxyPrefix string) {
	cookies := resp.Cookies()
	if len(cookies) == 0 {
		return
	}

	resp.Header.Del("Set-Cookie")
	for _, c := range cookies {
		if c.Path == "" || c.Path == "/" {
			c.Path = proxyPrefix + "/"
		} else if !strings.HasPrefix(c.Path, proxyPrefix) {
			c.Path = proxyPrefix + c.Path
		}
		resp.Header.Add("Set-Cookie", c.String())
	}
}

// isTextContent returns true if the Content-Type indicates text-based content
// that should have URL paths rewritten.
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

// urlRewriteScript returns a <script> tag that intercepts XHR, fetch, link clicks,
// and form submissions to rewrite absolute paths through the proxy prefix. This
// handles JavaScript-initiated requests without needing to rewrite JS source code.
func urlRewriteScript(proxyPrefix string) []byte {
	return []byte(`<script>(function(){` +
		`var P='` + proxyPrefix + `';` +
		`function rw(u){` +
		`if(typeof u!=='string')return u;` +
		`if(u.charAt(0)==='/'&&u.charAt(1)!=='/'&&u.indexOf(P)!==0)return P+u;` +
		`return u}` +

		// Intercept XMLHttpRequest.open
		`var XO=XMLHttpRequest.prototype.open;` +
		`XMLHttpRequest.prototype.open=function(m,u){` +
		`arguments[1]=rw(u);return XO.apply(this,arguments)};` +

		// Intercept fetch
		`if(window.fetch){var FF=window.fetch;` +
		`window.fetch=function(u,o){return FF.call(this,rw(u),o)}}` +

		// Intercept link clicks
		`document.addEventListener('click',function(e){` +
		`var a=e.target;while(a&&a.tagName!=='A')a=a.parentElement;` +
		`if(a&&a.href){var h=a.getAttribute('href');` +
		`if(h&&h.charAt(0)==='/'&&h.charAt(1)!=='/'&&h.indexOf(P)!==0)` +
		`a.setAttribute('href',P+h)}},true);` +

		// Intercept form submissions
		`document.addEventListener('submit',function(e){` +
		`var f=e.target;if(f&&f.action){` +
		`try{var u=new URL(f.action);` +
		`if(u.pathname.charAt(0)==='/'&&u.pathname.indexOf(P)!==0)` +
		`f.action=u.pathname=P+u.pathname}catch(x){}}},true);` +

		// Intercept window.location and top.location assignments via a timer
		// that checks and rewrites the current page if navigated outside the proxy
		// (This is a fallback; most navigations are caught by the other interceptors)

		`})()</script>`)
}

// readResponseBody reads and decompresses the response body.
func readResponseBody(resp *http.Response) ([]byte, error) {
	var body []byte
	var err error

	encoding := resp.Header.Get("Content-Encoding")
	switch encoding {
	case "gzip":
		reader, gerr := gzip.NewReader(resp.Body)
		if gerr != nil {
			return nil, gerr
		}
		body, err = io.ReadAll(reader)
		reader.Close()
	default:
		body, err = io.ReadAll(resp.Body)
	}
	resp.Body.Close()
	return body, err
}

// setResponseBody replaces the response body and updates headers.
func setResponseBody(resp *http.Response, body []byte) {
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	resp.Body = io.NopCloser(bytes.NewReader(body))
}

// rewriteHTMLBody rewrites HTML responses:
// 1. Replaces full BMC origin URLs with the proxy prefix
// 2. Rewrites href/src/action/formaction attributes with absolute paths
// 3. Rewrites CSS url() values with absolute paths
// 4. Injects a client-side script to intercept XHR, fetch, and navigation
func rewriteHTMLBody(resp *http.Response, proxyPrefix, bmcOrigin, bmcOriginNoPort string) error {
	body, err := readResponseBody(resp)
	if err != nil {
		return err
	}

	prefixBytes := []byte(proxyPrefix)

	// Replace full BMC origin URLs (exact string match, safe for any content)
	body = replaceBMCOrigins(body, prefixBytes, bmcOrigin, bmcOriginNoPort)

	// Rewrite HTML attributes: href="/...", src="/...", action="/...", formaction="/..."
	body = rewriteWithRegex(body, htmlAttrRe, prefixBytes)

	// Rewrite CSS url() values in inline styles
	body = rewriteWithRegex(body, cssURLRe, prefixBytes)

	// Rewrite JS string literals with absolute paths in inline <script> blocks.
	// This catches patterns like var gPageDir = "/page/"; and
	// document.write('<script src="/str/...">') that would otherwise bypass the proxy.
	body = rewriteWithRegex(body, jsStringLiteralRe, prefixBytes)

	// Inject URL rewriting script into <head> (or at start of body)
	script := urlRewriteScript(proxyPrefix)
	body = injectScript(body, script)

	setResponseBody(resp, body)
	return nil
}

// replaceBMCOrigins replaces full BMC origin URLs with the proxy prefix.
func replaceBMCOrigins(body, prefix []byte, bmcOrigin, bmcOriginNoPort string) []byte {
	body = bytes.ReplaceAll(body, []byte(bmcOrigin+"/"), append(append([]byte{}, prefix...), '/'))
	body = bytes.ReplaceAll(body, []byte(bmcOrigin), append([]byte{}, prefix...))
	if bmcOrigin != bmcOriginNoPort {
		body = bytes.ReplaceAll(body, []byte(bmcOriginNoPort+"/"), append(append([]byte{}, prefix...), '/'))
		body = bytes.ReplaceAll(body, []byte(bmcOriginNoPort), append([]byte{}, prefix...))
	}
	return body
}

// rewriteWithRegex applies a regex that matches a prefix group + "/" + next char,
// inserting the proxy prefix after the matched prefix group's slash.
// The regex must have two capture groups: (1) the prefix, (2) the char after "/".
func rewriteWithRegex(body []byte, re *regexp.Regexp, prefix []byte) []byte {
	ipmiSlash := make([]byte, len(prefix)+1)
	copy(ipmiSlash, prefix)
	ipmiSlash[len(prefix)] = '/'

	ipmiTag := []byte("ipmi/")

	indices := re.FindAllSubmatchIndex(body, -1)
	if len(indices) == 0 {
		return body
	}

	var result bytes.Buffer
	result.Grow(len(body) + len(indices)*len(prefix))

	lastEnd := 0
	for _, idx := range indices {
		_, fullEnd := idx[0], idx[1]
		// Group 1 end is where the "/" starts
		group1End := idx[3]
		// Group 2 start is the char after "/"
		group2Start, group2End := idx[4], idx[5]

		// Skip if already rewritten
		if bytes.HasPrefix(body[group2Start:], ipmiTag) {
			continue
		}

		result.Write(body[lastEnd:group1End])
		result.Write(ipmiSlash)
		result.Write(body[group2Start:group2End])
		lastEnd = fullEnd
	}

	result.Write(body[lastEnd:])
	return result.Bytes()
}

// injectScript inserts the script tag after <head> or at the start of the body.
func injectScript(body, script []byte) []byte {
	// Try to inject after <head> or <head ...>
	headRe := regexp.MustCompile(`(?i)<head[^>]*>`)
	loc := headRe.FindIndex(body)
	if loc != nil {
		var result bytes.Buffer
		result.Grow(len(body) + len(script))
		result.Write(body[:loc[1]])
		result.Write(script)
		result.Write(body[loc[1]:])
		return result.Bytes()
	}

	// Fallback: inject at the very beginning
	var result bytes.Buffer
	result.Grow(len(body) + len(script))
	result.Write(script)
	result.Write(body)
	return result.Bytes()
}
