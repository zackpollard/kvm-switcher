package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/zackpollard/kvm-switcher/internal/models"
)

func init() {
	Register("dell_idrac9", &IDRAC9Authenticator{})
}

// IDRAC9Authenticator handles authentication for Dell iDRAC9 (14G+ servers).
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

	// Configure VNC on the iDRAC9 and set a known password via Redfish.
	// iDRAC9's proprietary WSS protocol isn't compatible with noVNC,
	// so we use the standard VNC server instead.
	vncPassword, err := a.configureVNC(ctx, host, port, username, password)
	if err != nil {
		_ = a.logoutSession(ctx, baseURL, sess)
		return nil, nil, fmt.Errorf("iDRAC9 VNC setup: %w", err)
	}

	log.Printf("iDRAC9 %s: authenticated, VNC target %s:5901", host, host)

	creds := &models.BMCCredentials{
		SessionCookie: sess.SessionCookie,
		CSRFToken:     sess.XSRFToken,
	}
	connectInfo := &models.KVMConnectInfo{
		Mode:        models.KVMModeVNC,
		TargetAddr:  fmt.Sprintf("%s:5901", host),
		VNCPassword: vncPassword,
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

	if resp.StatusCode == http.StatusServiceUnavailable {
		// HTTP 503 usually means session limit reached. Try clearing stale
		// sessions via Redfish and retry once.
		log.Printf("iDRAC9 %s: login returned 503 (session limit), clearing stale sessions...", baseURL)
		a.clearStaleSessions(ctx, baseURL, username, password)

		// Retry login
		req2, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
		if err != nil {
			return nil, err
		}
		req2.Header.Set("user", fmt.Sprintf("%q", username))
		req2.Header.Set("password", fmt.Sprintf("%q", password))
		resp, err = idracHTTPClient.Do(req2)
		if err != nil {
			return nil, fmt.Errorf("sending login retry request: %w", err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
	}

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

// configureVNC enables the VNC server on iDRAC9 via Redfish and sets a known password.
// Returns the VNC password to use for connection.
// iDRAC9 requires VNC passwords to meet complexity requirements (upper, lower, digit, special).
func (a *IDRAC9Authenticator) configureVNC(ctx context.Context, host string, port int, username, password string) (string, error) {
	redfishURL := fmt.Sprintf("https://%s:%d/redfish/v1/Managers/iDRAC.Embedded.1/Attributes", host, port)

	// Use the BMC password with complexity suffix to meet iDRAC9's VNC password policy.
	vncPassword := password + "1!"

	attrs := map[string]any{
		"Attributes": map[string]string{
			"VNCServer.1.Enable":   "Enabled",
			"VNCServer.1.Password": vncPassword,
		},
	}
	body, err := json.Marshal(attrs)
	if err != nil {
		return "", fmt.Errorf("marshaling VNC config: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, redfishURL, strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(username, password)
	req.Header.Set("Content-Type", "application/json")

	resp, err := idracHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("sending Redfish PATCH: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Redfish VNC config failed (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	return vncPassword, nil
}

// clearStaleSessions removes all GUI sessions via the Redfish API.
// This is used when login fails with HTTP 503 (session limit reached).
func (a *IDRAC9Authenticator) clearStaleSessions(ctx context.Context, baseURL, username, password string) {
	sessURL := baseURL + "/redfish/v1/SessionService/Sessions"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sessURL, nil)
	if err != nil {
		return
	}
	req.SetBasicAuth(username, password)

	resp, err := idracHTTPClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	var result struct {
		Members []struct {
			ODataID string `json:"@odata.id"`
		} `json:"Members"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return
	}

	for _, m := range result.Members {
		delURL := baseURL + m.ODataID
		delReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, delURL, nil)
		if err != nil {
			continue
		}
		delReq.SetBasicAuth(username, password)
		delResp, err := idracHTTPClient.Do(delReq)
		if err != nil {
			continue
		}
		delResp.Body.Close()
		log.Printf("iDRAC9: cleared session %s (HTTP %d)", m.ODataID, delResp.StatusCode)
	}
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
