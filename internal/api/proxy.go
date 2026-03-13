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

// pathRewriteRe matches a delimiter (quote, paren, or equals) followed by a forward slash
// and captures both the delimiter and the character after the slash. We use a replacement
// function to skip protocol-relative URLs (//) and already-rewritten paths (/ipmi/).
var pathRewriteRe = regexp.MustCompile(`(["'(=])\s*/([^/])`)

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

			// Remove Accept-Encoding to simplify body rewriting (avoid gzip from BMC)
			req.Header.Del("Accept-Encoding")
		},
		ModifyResponse: func(resp *http.Response) error {
			// Rewrite Location header for redirects
			if loc := resp.Header.Get("Location"); loc != "" {
				resp.Header.Set("Location", rewriteURL(loc, proxyPrefix, bmcOrigin, bmcOriginNoPort))
			}

			// Rewrite Set-Cookie paths
			rewriteSetCookiePaths(resp, proxyPrefix)

			// Remove headers that would block the proxied UI
			resp.Header.Del("Content-Security-Policy")
			resp.Header.Del("X-Frame-Options")

			// Rewrite body for text content types
			if isTextContent(resp.Header.Get("Content-Type")) {
				return rewriteResponseBody(resp, proxyPrefix, bmcOrigin, bmcOriginNoPort)
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

// rewriteResponseBody reads the response body, rewrites absolute paths to go through
// the proxy prefix, and replaces the body with the rewritten content.
func rewriteResponseBody(resp *http.Response, proxyPrefix, bmcOrigin, bmcOriginNoPort string) error {
	var body []byte
	var err error

	encoding := resp.Header.Get("Content-Encoding")
	switch encoding {
	case "gzip":
		reader, gerr := gzip.NewReader(resp.Body)
		if gerr != nil {
			return gerr
		}
		body, err = io.ReadAll(reader)
		reader.Close()
	default:
		body, err = io.ReadAll(resp.Body)
	}
	resp.Body.Close()
	if err != nil {
		return err
	}

	prefixBytes := []byte(proxyPrefix)

	// Replace full BMC URLs first (with port, then without)
	body = bytes.ReplaceAll(body, []byte(bmcOrigin+"/"), append(prefixBytes, '/'))
	body = bytes.ReplaceAll(body, []byte(bmcOrigin), prefixBytes)
	if bmcOrigin != bmcOriginNoPort {
		body = bytes.ReplaceAll(body, []byte(bmcOriginNoPort+"/"), append(prefixBytes, '/'))
		body = bytes.ReplaceAll(body, []byte(bmcOriginNoPort), prefixBytes)
	}

	// Rewrite absolute paths in attribute values, JS strings, and CSS url()
	body = rewriteAbsolutePaths(body, prefixBytes)

	// Clear encoding headers and set new content length
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	resp.Body = io.NopCloser(bytes.NewReader(body))

	return nil
}

// rewriteAbsolutePaths finds delimiter+/+char patterns and inserts the proxy prefix,
// skipping already-rewritten paths that start with /ipmi/.
func rewriteAbsolutePaths(body, prefix []byte) []byte {
	ipmiSlash := make([]byte, len(prefix)+1)
	copy(ipmiSlash, prefix)
	ipmiSlash[len(prefix)] = '/'

	ipmiTag := []byte("ipmi/")

	indices := pathRewriteRe.FindAllIndex(body, -1)
	if len(indices) == 0 {
		return body
	}

	var result bytes.Buffer
	result.Grow(len(body) + len(indices)*len(prefix))

	lastEnd := 0
	for _, idx := range indices {
		start, end := idx[0], idx[1]

		// Find the slash position within the match
		slashPos := start + bytes.IndexByte(body[start:end], '/')
		charAfterSlash := slashPos + 1

		// Skip if the content after / starts with "ipmi/" (already rewritten)
		if bytes.HasPrefix(body[charAfterSlash:], ipmiTag) {
			continue
		}

		// Write everything before the slash, then the proxy prefix, then continue
		result.Write(body[lastEnd:slashPos])
		result.Write(ipmiSlash)
		result.Write(body[charAfterSlash:end])
		lastEnd = end
	}

	result.Write(body[lastEnd:])
	return result.Bytes()
}
