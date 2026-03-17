package auth

import (
	"context"
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

func init() {
	Register("dell_idrac8", &IDRAC8Authenticator{})
}

// IDRAC8Authenticator handles authentication for Dell iDRAC8 (13G servers).
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
	authResult, forwardURL, errorMsg := parseIDRAC8LoginResponse(string(body))

	if authResult != "0" {
		if errorMsg != "" {
			return nil, fmt.Errorf("login failed: authResult=%s (%s)", authResult, errorMsg)
		}
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

// parseIDRAC8LoginResponse extracts authResult, forwardUrl, and errorMsg from iDRAC8's XML response.
func parseIDRAC8LoginResponse(body string) (authResult, forwardURL, errorMsg string) {
	// Try XML parsing first (root element is <root>)
	type loginResp struct {
		XMLName    xml.Name `xml:"root"`
		AuthResult string   `xml:"authResult"`
		ForwardURL string   `xml:"forwardUrl"`
		ErrorMsg   string   `xml:"errorMsg"`
	}
	var xmlResp loginResp
	if err := xml.Unmarshal([]byte(body), &xmlResp); err == nil {
		return xmlResp.AuthResult, xmlResp.ForwardURL, xmlResp.ErrorMsg
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
	errRe := regexp.MustCompile(`<errorMsg>([^<]+)</errorMsg>`)
	if m := errRe.FindStringSubmatch(body); m != nil {
		errorMsg = m[1]
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
