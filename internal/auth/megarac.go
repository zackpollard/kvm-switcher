package auth

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
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
	Register("ami_megarac", &MegaRACAuthenticator{})
}

// MegaRACAuthenticator implements BMCAuthenticator for AMI MegaRAC BMCs.
type MegaRACAuthenticator struct{}

// sessionResponse holds the parsed fields from /rpc/WEBSES/create.asp response.
type sessionResponse struct {
	SessionCookie string
	BMCIPAddr     string
	CSRFToken     string
}

// jnlpXML represents the JNLP XML structure for parsing.
type jnlpXML struct {
	XMLName xml.Name            `xml:"jnlp"`
	AppDesc jnlpApplicationDesc `xml:"application-desc"`
}

type jnlpApplicationDesc struct {
	Arguments []string `xml:"argument"`
}

// FetchKVMToken gets a KVM token using existing BMC credentials (no new web session).
// This allows reusing the session manager's web session for KVM without consuming
// an additional BMC session slot.
func (m *MegaRACAuthenticator) FetchKVMToken(ctx context.Context, host string, port int, existingCreds *models.BMCCredentials) (*models.KVMConnectInfo, error) {
	baseURL := fmt.Sprintf("http://%s:%d", host, port)
	sessResp := &sessionResponse{
		SessionCookie: existingCreds.SessionCookie,
		CSRFToken:     existingCreds.CSRFToken,
	}
	args, err := m.getJNLP(ctx, baseURL, host, sessResp)
	if err != nil {
		return nil, fmt.Errorf("getting JNLP: %w", err)
	}
	existingCreds.KVMToken = args.KVMToken
	existingCreds.WebCookie = args.WebCookie
	return &models.KVMConnectInfo{
		Mode:     models.KVMModeIKVM,
		IKVMArgs: args,
	}, nil
}

func (m *MegaRACAuthenticator) Authenticate(ctx context.Context, host string, port int, username, password string) (*models.BMCCredentials, *models.KVMConnectInfo, error) {
	baseURL := fmt.Sprintf("http://%s:%d", host, port)

	// Step 1: Create session
	sessResp, err := m.createSession(ctx, baseURL, username, password)
	if err != nil {
		return nil, nil, fmt.Errorf("creating BMC session: %w", err)
	}

	// Step 2: Get JNLP with KVM token
	args, err := m.getJNLP(ctx, baseURL, host, sessResp)
	if err != nil {
		// Try to logout on failure
		_ = m.logoutWithSession(ctx, baseURL, sessResp)
		return nil, nil, fmt.Errorf("getting JNLP: %w", err)
	}

	creds := &models.BMCCredentials{
		SessionCookie: sessResp.SessionCookie,
		CSRFToken:     sessResp.CSRFToken,
		KVMToken:      args.KVMToken,
		WebCookie:     args.WebCookie,
	}

	connectInfo := &models.KVMConnectInfo{
		Mode:     models.KVMModeIKVM,
		IKVMArgs: args,
	}

	return creds, connectInfo, nil
}

func (m *MegaRACAuthenticator) CreateWebSession(ctx context.Context, host string, port int, username, password string) (*models.BMCCredentials, error) {
	baseURL := fmt.Sprintf("http://%s:%d", host, port)

	sessResp, err := m.createSession(ctx, baseURL, username, password)
	if err != nil {
		return nil, fmt.Errorf("creating BMC session: %w", err)
	}

	creds := &models.BMCCredentials{
		SessionCookie: sessResp.SessionCookie,
		CSRFToken:     sessResp.CSRFToken,
	}

	// Fetch user role info — the BMC web UI reads Username/PNO/Extendedpriv
	// cookies to decide whether to render the dashboard or a blank page.
	role, err := m.getRole(ctx, baseURL, sessResp)
	if err != nil {
		log.Printf("Warning: failed to get BMC role info: %v", err)
	} else {
		creds.Username = role.username
		creds.Privilege = role.privilege
		creds.ExtendedPriv = role.extendedPriv
	}

	return creds, nil
}

func (m *MegaRACAuthenticator) Logout(ctx context.Context, host string, port int, creds *models.BMCCredentials) error {
	baseURL := fmt.Sprintf("http://%s:%d", host, port)
	sessResp := &sessionResponse{
		SessionCookie: creds.SessionCookie,
		CSRFToken:     creds.CSRFToken,
	}
	return m.logoutWithSession(ctx, baseURL, sessResp)
}

func (m *MegaRACAuthenticator) createSession(ctx context.Context, baseURL, username, password string) (*sessionResponse, error) {
	endpoint := baseURL + "/rpc/WEBSES/create.asp"

	form := url.Values{}
	form.Set("WEBVAR_USERNAME", username)
	form.Set("WEBVAR_PASSWORD", password)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	body, err := readBodyTolerant(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	return parseSessionResponse(string(body))
}

func (m *MegaRACAuthenticator) getJNLP(ctx context.Context, baseURL, host string, sess *sessionResponse) (*models.JViewerArgs, error) {
	endpoint := fmt.Sprintf("%s/Java/jviewer.jnlp?EXTRNIP=%s&JNLPSTR=JViewer", baseURL, url.QueryEscape(host))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Cookie", "SessionCookie="+sess.SessionCookie)
	req.Header.Set("X-CSRFTOKEN", sess.CSRFToken)
	req.Header.Set("Referer", baseURL+"/")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	body, err := readBodyTolerant(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	return parseJNLP(string(body))
}

func (m *MegaRACAuthenticator) logoutWithSession(ctx context.Context, baseURL string, sess *sessionResponse) error {
	endpoint := baseURL + "/rpc/WEBSES/logout.asp"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("creating logout request: %w", err)
	}
	req.Header.Set("Cookie", "SessionCookie="+sess.SessionCookie)
	req.Header.Set("X-CSRFTOKEN", sess.CSRFToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("sending logout request: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	return nil
}

// roleInfo holds parsed user privilege data from /rpc/getrole.asp.
type roleInfo struct {
	username     string
	privilege    int
	extendedPriv int
}

func (m *MegaRACAuthenticator) getRole(ctx context.Context, baseURL string, sess *sessionResponse) (*roleInfo, error) {
	endpoint := baseURL + "/rpc/getrole.asp"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Cookie", "SessionCookie="+sess.SessionCookie)
	req.Header.Set("X-CSRFTOKEN", sess.CSRFToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	body, err := readBodyTolerant(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	return parseRoleResponse(string(body))
}

func parseRoleResponse(body string) (*roleInfo, error) {
	statusRe := regexp.MustCompile(`HAPI_STATUS:(-?\d+)`)
	if m := statusRe.FindStringSubmatch(body); m != nil && m[1] != "0" {
		return nil, fmt.Errorf("getrole returned HAPI_STATUS:%s", m[1])
	}

	role := &roleInfo{}

	usernameRe := regexp.MustCompile(`'CURUSERNAME'\s*:\s*'([^']*)'`)
	if m := usernameRe.FindStringSubmatch(body); m != nil {
		role.username = m[1]
	}

	privRe := regexp.MustCompile(`'CURPRIV'\s*:\s*(\d+)`)
	if m := privRe.FindStringSubmatch(body); m != nil {
		fmt.Sscanf(m[1], "%d", &role.privilege)
	}

	extPrivRe := regexp.MustCompile(`'EXTENDED_PRIV'\s*:\s*(\d+)`)
	if m := extPrivRe.FindStringSubmatch(body); m != nil {
		fmt.Sscanf(m[1], "%d", &role.extendedPriv)
	}

	return role, nil
}

// readBodyTolerant reads an HTTP response body, tolerating premature connection closes.
// AMI MegaRAC BMCs use HTTP/1.0 and frequently close the connection before delivering
// all bytes advertised by Content-Length. This function returns whatever data was received
// as long as it's non-empty.
func readBodyTolerant(r io.Reader) ([]byte, error) {
	var buf bytes.Buffer
	_, err := io.Copy(&buf, r)
	if err != nil && buf.Len() > 0 {
		// Got some data before the connection was cut - use what we have
		log.Printf("BMC response truncated (got %d bytes): %v", buf.Len(), err)

		// Only tolerate unexpected EOF / connection reset errors
		if errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(err.Error(), "unexpected EOF") || strings.Contains(err.Error(), "connection reset") {
			return buf.Bytes(), nil
		}
	}
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// parseSessionResponse extracts SESSION_COOKIE, BMC_IP_ADDR, and CSRFTOKEN from the
// JavaScript-like response body of /rpc/WEBSES/create.asp.
func parseSessionResponse(body string) (*sessionResponse, error) {
	// Check HAPI_STATUS first — non-zero means the BMC rejected the request
	statusRe := regexp.MustCompile(`HAPI_STATUS:(-?\d+)`)
	if statusMatch := statusRe.FindStringSubmatch(body); statusMatch != nil {
		if statusMatch[1] != "0" {
			return nil, fmt.Errorf("BMC returned HAPI_STATUS:%s (login failed)", statusMatch[1])
		}
	}

	cookieRe := regexp.MustCompile(`'SESSION_COOKIE'\s*:\s*'([^']+)'`)
	bmcIPRe := regexp.MustCompile(`'BMC_IP_ADDR'\s*:\s*'([^']+)'`)
	csrfRe := regexp.MustCompile(`'CSRFTOKEN'\s*:\s*'([^']+)'`)

	cookieMatch := cookieRe.FindStringSubmatch(body)
	if cookieMatch == nil {
		return nil, fmt.Errorf("SESSION_COOKIE not found in response")
	}

	csrfMatch := csrfRe.FindStringSubmatch(body)
	if csrfMatch == nil {
		return nil, fmt.Errorf("CSRFTOKEN not found in response")
	}

	resp := &sessionResponse{
		SessionCookie: cookieMatch[1],
		CSRFToken:     csrfMatch[1],
	}

	if bmcIPMatch := bmcIPRe.FindStringSubmatch(body); bmcIPMatch != nil {
		resp.BMCIPAddr = bmcIPMatch[1]
	}

	return resp, nil
}

// parseJNLP extracts JViewer arguments from the JNLP XML response.
func parseJNLP(body string) (*models.JViewerArgs, error) {
	var jnlp jnlpXML
	if err := xml.Unmarshal([]byte(body), &jnlp); err != nil {
		return nil, fmt.Errorf("parsing JNLP XML: %w", err)
	}

	argMap := make(map[string]string)
	args := jnlp.AppDesc.Arguments
	for i := 0; i < len(args)-1; i += 2 {
		key := strings.TrimPrefix(args[i], "-")
		argMap[key] = args[i+1]
	}

	jviewerArgs := &models.JViewerArgs{
		Hostname:          argMap["hostname"],
		KVMToken:          argMap["kvmtoken"],
		KVMSecure:         argMap["kvmsecure"],
		KVMPort:           argMap["kvmport"],
		VMSecure:          argMap["vmsecure"],
		CDState:           argMap["cdstate"],
		FDState:           argMap["fdstate"],
		HDState:           argMap["hdstate"],
		CDNum:             argMap["cdnum"],
		FDNum:             argMap["fdnum"],
		HDNum:             argMap["hdnum"],
		ExtendedPriv:      argMap["extendedpriv"],
		Localization:      argMap["localization"],
		KeyboardLayout:    argMap["keyboardlayout"],
		WebSecurePort:     argMap["websecureport"],
		SinglePortEnabled: argMap["singleportenabled"],
		WebCookie:         argMap["webcookie"],
		OEMFeatures:       argMap["oemfeatures"],
	}

	if jviewerArgs.KVMToken == "" {
		return nil, fmt.Errorf("kvmtoken not found in JNLP arguments")
	}
	if jviewerArgs.Hostname == "" {
		return nil, fmt.Errorf("hostname not found in JNLP arguments")
	}

	return jviewerArgs, nil
}
