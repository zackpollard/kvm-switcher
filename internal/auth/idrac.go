package auth

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/zackpollard/kvm-switcher/internal/models"
)

// Shared HTTP client for iDRAC (self-signed certs).
var idracHTTPClient = &http.Client{
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	},
}

func init() {
	Register("dell_idrac9", &IDRAC9Authenticator{})
	Register("dell_idrac8", &IDRAC8Authenticator{})
}

// ---------------------------------------------------------------------------
// iDRAC9 (14G+ Dell servers, e.g. R640)
// ---------------------------------------------------------------------------

type IDRAC9Authenticator struct{}

// idrac9Session holds parsed iDRAC9 login response data.
type idrac9Session struct {
	SessionCookie string // -http-session- cookie value
	XSRFToken     string // XSRF-TOKEN header
}

func (a *IDRAC9Authenticator) Authenticate(ctx context.Context, host string, port int, username, password string) (*models.BMCCredentials, *models.KVMConnectInfo, error) {
	baseURL := fmt.Sprintf("https://%s:%d", host, port)

	sess, err := a.login(ctx, baseURL, username, password)
	if err != nil {
		return nil, nil, fmt.Errorf("iDRAC9 login: %w", err)
	}

	// Get virtual console launch info to obtain VCSID token.
	wssURL, err := a.getVConsoleURL(ctx, baseURL, host, port, sess)
	if err != nil {
		_ = a.logoutSession(ctx, baseURL, sess)
		return nil, nil, fmt.Errorf("iDRAC9 vconsole: %w", err)
	}

	creds := &models.BMCCredentials{
		SessionCookie: sess.SessionCookie,
		CSRFToken:     sess.XSRFToken,
	}
	connectInfo := &models.KVMConnectInfo{
		Mode:      models.KVMModeWebSocket,
		TargetURL: wssURL,
	}

	return creds, connectInfo, nil
}

func (a *IDRAC9Authenticator) CreateWebSession(ctx context.Context, host string, port int, username, password string) (*models.BMCCredentials, error) {
	baseURL := fmt.Sprintf("https://%s:%d", host, port)

	sess, err := a.login(ctx, baseURL, username, password)
	if err != nil {
		return nil, fmt.Errorf("iDRAC9 login: %w", err)
	}

	return &models.BMCCredentials{
		SessionCookie: sess.SessionCookie,
		CSRFToken:     sess.XSRFToken,
	}, nil
}

func (a *IDRAC9Authenticator) Logout(ctx context.Context, host string, port int, creds *models.BMCCredentials) error {
	baseURL := fmt.Sprintf("https://%s:%d", host, port)
	sess := &idrac9Session{
		SessionCookie: creds.SessionCookie,
		XSRFToken:     creds.CSRFToken,
	}
	return a.logoutSession(ctx, baseURL, sess)
}

// login authenticates with iDRAC9 via POST /sysmgmt/2015/bmc/session.
// Credentials are sent as HTTP headers (not body).
func (a *IDRAC9Authenticator) login(ctx context.Context, baseURL, username, password string) (*idrac9Session, error) {
	endpoint := baseURL + "/sysmgmt/2015/bmc/session"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("user", fmt.Sprintf("%q", username))
	req.Header.Set("password", fmt.Sprintf("%q", password))

	resp, err := idracHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending login request: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("login returned HTTP %d", resp.StatusCode)
	}

	sess := &idrac9Session{}

	// Extract -http-session- cookie
	for _, c := range resp.Cookies() {
		if c.Name == "-http-session-" {
			sess.SessionCookie = c.Value
			break
		}
	}
	if sess.SessionCookie == "" {
		return nil, fmt.Errorf("no -http-session- cookie in login response")
	}

	// Extract XSRF-TOKEN header
	sess.XSRFToken = resp.Header.Get("XSRF-TOKEN")
	if sess.XSRFToken == "" {
		return nil, fmt.Errorf("no XSRF-TOKEN header in login response")
	}

	return sess, nil
}

// getVConsoleURL fetches the virtual console launch URL and extracts the WSS address.
func (a *IDRAC9Authenticator) getVConsoleURL(ctx context.Context, baseURL, host string, port int, sess *idrac9Session) (string, error) {
	endpoint := baseURL + "/sysmgmt/2015/server/vconsole"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.AddCookie(&http.Cookie{Name: "-http-session-", Value: sess.SessionCookie})
	req.Header.Set("XSRF-TOKEN", sess.XSRFToken)

	resp, err := idracHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("sending vconsole request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading vconsole response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("vconsole returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	// Response is JSON with a "Location" field containing the viewer URL.
	// Parse URL params to get VCSID.
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parsing vconsole JSON: %w", err)
	}

	loc, _ := result["Location"].(string)
	if loc == "" {
		return "", fmt.Errorf("no Location in vconsole response: %s", string(body))
	}

	parsed, err := url.Parse(loc)
	if err != nil {
		return "", fmt.Errorf("parsing vconsole Location URL: %w", err)
	}

	vcsid := parsed.Query().Get("VCSID")
	if vcsid == "" {
		return "", fmt.Errorf("no VCSID in vconsole URL: %s", loc)
	}

	// Construct the WSS URL for the VNC-over-WebSocket endpoint.
	// iDRAC9 serves this on the same port (443).
	kvmPort := parsed.Query().Get("kvmport")
	if kvmPort == "" || kvmPort == "443" {
		kvmPort = fmt.Sprintf("%d", port)
	}

	wssURL := fmt.Sprintf("wss://%s:%s/vnc/vconsole?vck=%s", host, kvmPort, url.QueryEscape(vcsid))
	return wssURL, nil
}

func (a *IDRAC9Authenticator) logoutSession(ctx context.Context, baseURL string, sess *idrac9Session) error {
	endpoint := baseURL + "/sysmgmt/2015/bmc/session"

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return err
	}
	req.AddCookie(&http.Cookie{Name: "-http-session-", Value: sess.SessionCookie})
	req.Header.Set("XSRF-TOKEN", sess.XSRFToken)

	resp, err := idracHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("sending logout request: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	return nil
}

// ---------------------------------------------------------------------------
// iDRAC8 (13G Dell servers, e.g. R730xd)
// ---------------------------------------------------------------------------

type IDRAC8Authenticator struct{}

// idrac8Session holds parsed iDRAC8 login response data.
type idrac8Session struct {
	SessionCookie string // -http-session- cookie value
	ST1           string // session token 1 (URL param)
	ST2           string // session token 2 (HTTP header)
}

func (a *IDRAC8Authenticator) Authenticate(ctx context.Context, host string, port int, username, password string) (*models.BMCCredentials, *models.KVMConnectInfo, error) {
	baseURL := fmt.Sprintf("https://%s:%d", host, port)

	sess, err := a.login(ctx, baseURL, username, password)
	if err != nil {
		return nil, nil, fmt.Errorf("iDRAC8 login: %w", err)
	}

	log.Printf("iDRAC8 %s: authenticated, VNC target %s:5901", host, host)

	creds := &models.BMCCredentials{
		SessionCookie: sess.SessionCookie,
		CSRFToken:     sess.ST2,
		Extra:         map[string]string{"st1": sess.ST1},
	}
	connectInfo := &models.KVMConnectInfo{
		Mode:        models.KVMModeVNC,
		TargetAddr:  fmt.Sprintf("%s:5901", host),
		VNCPassword: password,
	}

	return creds, connectInfo, nil
}

func (a *IDRAC8Authenticator) CreateWebSession(ctx context.Context, host string, port int, username, password string) (*models.BMCCredentials, error) {
	baseURL := fmt.Sprintf("https://%s:%d", host, port)

	sess, err := a.login(ctx, baseURL, username, password)
	if err != nil {
		return nil, fmt.Errorf("iDRAC8 login: %w", err)
	}

	return &models.BMCCredentials{
		SessionCookie: sess.SessionCookie,
		CSRFToken:     sess.ST2,
		Extra:         map[string]string{"st1": sess.ST1},
	}, nil
}

func (a *IDRAC8Authenticator) Logout(ctx context.Context, host string, port int, creds *models.BMCCredentials) error {
	baseURL := fmt.Sprintf("https://%s:%d", host, port)
	st1 := ""
	if creds.Extra != nil {
		st1 = creds.Extra["st1"]
	}
	sess := &idrac8Session{
		SessionCookie: creds.SessionCookie,
		ST1:           st1,
		ST2:           creds.CSRFToken,
	}
	return a.logoutSession(ctx, baseURL, sess)
}

// login authenticates with iDRAC8 via POST /data/login.
func (a *IDRAC8Authenticator) login(ctx context.Context, baseURL, username, password string) (*idrac8Session, error) {
	endpoint := baseURL + "/data/login"

	// Build form body manually — iDRAC8 parses fields positionally and
	// requires "user" before "password". Go's url.Values.Encode() sorts
	// alphabetically, which puts "password" first and causes auth failure.
	formBody := "user=" + url.QueryEscape(username) + "&password=" + url.QueryEscape(password)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(formBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := idracHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending login request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading login response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("login returned HTTP %d", resp.StatusCode)
	}

	sess := &idrac8Session{}

	// Extract -http-session- cookie
	for _, c := range resp.Cookies() {
		if c.Name == "-http-session-" {
			sess.SessionCookie = c.Value
			break
		}
	}
	if sess.SessionCookie == "" {
		return nil, fmt.Errorf("no -http-session- cookie in login response")
	}

	// Parse XML response to extract authResult and forwardUrl with ST1/ST2.
	// Response format: <root>...<authResult>0</authResult><forwardUrl>index.html?ST1=abc,ST2=def</forwardUrl></root>
	// Note: ST1 and ST2 are comma-separated in the forwardUrl, not &-separated.
	authResult, forwardURL := parseIDRAC8LoginResponse(string(body))

	if authResult != "0" {
		return nil, fmt.Errorf("login failed: authResult=%s", authResult)
	}

	if forwardURL != "" {
		// iDRAC8 uses comma to separate ST1 and ST2 in the query string,
		// e.g. "index.html?ST1=abc,ST2=def". Replace comma with & so
		// url.Parse can extract both values.
		normalized := strings.Replace(forwardURL, ",ST2=", "&ST2=", 1)
		parsed, err := url.Parse(normalized)
		if err == nil {
			sess.ST1 = parsed.Query().Get("ST1")
			sess.ST2 = parsed.Query().Get("ST2")
		}
	}

	if sess.ST1 == "" || sess.ST2 == "" {
		return nil, fmt.Errorf("missing ST1/ST2 tokens in login response")
	}

	return sess, nil
}

// parseIDRAC8LoginResponse extracts authResult and forwardUrl from iDRAC8's XML response.
func parseIDRAC8LoginResponse(body string) (authResult, forwardURL string) {
	// Try XML parsing first (root element is <root>)
	type loginResp struct {
		XMLName    xml.Name `xml:"root"`
		AuthResult string   `xml:"authResult"`
		ForwardURL string   `xml:"forwardUrl"`
	}
	var xmlResp loginResp
	if err := xml.Unmarshal([]byte(body), &xmlResp); err == nil {
		return xmlResp.AuthResult, xmlResp.ForwardURL
	}

	// Fallback to regex
	authRe := regexp.MustCompile(`<authResult>(\d+)</authResult>`)
	if m := authRe.FindStringSubmatch(body); m != nil {
		authResult = m[1]
	}
	fwdRe := regexp.MustCompile(`<forwardUrl>([^<]+)</forwardUrl>`)
	if m := fwdRe.FindStringSubmatch(body); m != nil {
		forwardURL = m[1]
	}
	return
}

func (a *IDRAC8Authenticator) logoutSession(ctx context.Context, baseURL string, sess *idrac8Session) error {
	endpoint := baseURL + "/data/logout"
	if sess.ST1 != "" {
		endpoint += "?ST1=" + url.QueryEscape(sess.ST1)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.AddCookie(&http.Cookie{Name: "-http-session-", Value: sess.SessionCookie})
	if sess.ST2 != "" {
		req.Header.Set("ST2", sess.ST2)
	}

	resp, err := idracHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("sending logout request: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	return nil
}
