package auth

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/zackpollard/kvm-switcher/internal/models"
)

func init() {
	Register("apc_ups", &APCAuthenticator{})
}

// APCAuthenticator handles authentication for APC Network Management Card 2
// (NMC2) devices such as UPS and PDU management cards.
//
// APC NMC2 uses URL-based sessions: after login, all authenticated URLs
// contain a session token in the path (e.g., /NMC/{token}/home.htm).
type APCAuthenticator struct{}

func (a *APCAuthenticator) Authenticate(ctx context.Context, host string, port int, username, password string) (*models.BMCCredentials, *models.KVMConnectInfo, error) {
	return nil, nil, fmt.Errorf("APC UPS does not support KVM")
}

func (a *APCAuthenticator) CreateWebSession(ctx context.Context, host string, port int, username, password string) (*models.BMCCredentials, error) {
	baseURL := fmt.Sprintf("http://%s:%d", host, port)

	nmcPath, err := a.login(ctx, baseURL, username, password)
	if err != nil {
		return nil, fmt.Errorf("APC login: %w", err)
	}

	log.Printf("APC %s: authenticated, NMC path %s", host, nmcPath)

	return &models.BMCCredentials{
		Extra: map[string]string{"nmc_path": nmcPath},
	}, nil
}

func (a *APCAuthenticator) Logout(ctx context.Context, host string, port int, creds *models.BMCCredentials) error {
	if creds.Extra == nil || creds.Extra["nmc_path"] == "" {
		return nil
	}
	baseURL := fmt.Sprintf("http://%s:%d", host, port)
	logoutURL := baseURL + creds.Extra["nmc_path"] + "/logout.htm"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, logoutURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// login performs the APC NMC2 login flow:
//  1. GET / → follows redirects → arrives at /NMC/{pre_token}/logon.htm
//  2. POST /NMC/{pre_token}/Forms/login1 with credentials
//  3. Response redirects to /NMC/{auth_token}/ — extract auth_token
//
// Returns the authenticated NMC path (e.g., "/NMC/xYz123...").
func (a *APCAuthenticator) login(ctx context.Context, baseURL, username, password string) (string, error) {
	// Step 1: Follow redirects to get the login page URL with pre-auth token
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 5 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/", nil)
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("getting login page: %w", err)
	}
	resp.Body.Close()

	// The final URL should be /NMC/{pre_token}/logon.htm
	finalPath := resp.Request.URL.Path
	if !strings.Contains(finalPath, "/NMC/") || !strings.HasSuffix(finalPath, "/logon.htm") {
		return "", fmt.Errorf("unexpected login page path: %s", finalPath)
	}
	preTokenPath := strings.TrimSuffix(finalPath, "/logon.htm")

	// Step 2: POST login credentials
	loginURL := baseURL + preTokenPath + "/Forms/login1"
	form := url.Values{}
	form.Set("login_username", username)
	form.Set("login_password", password)
	form.Set("submit", "Log On")

	// Don't follow the redirect — we need to read the Location header
	noRedirectClient := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	loginReq, err := http.NewRequestWithContext(ctx, http.MethodPost, loginURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("creating login request: %w", err)
	}
	loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	loginResp, err := noRedirectClient.Do(loginReq)
	if err != nil {
		return "", fmt.Errorf("sending login: %w", err)
	}
	defer func() {
		io.Copy(io.Discard, loginResp.Body)
		loginResp.Body.Close()
	}()

	// Step 3: Extract authenticated session path from Location header
	location := loginResp.Header.Get("Location")
	if location == "" {
		return "", fmt.Errorf("no redirect after login (status %d)", loginResp.StatusCode)
	}

	locURL, err := url.Parse(location)
	if err != nil {
		return "", fmt.Errorf("parsing redirect URL: %w", err)
	}

	// Location is like http://host/NMC/{auth_token}/ or /NMC/{auth_token}/page.htm
	// Extract just /NMC/{token} (first two path segments).
	locPath := strings.TrimSuffix(locURL.Path, "/")
	if !strings.HasPrefix(locPath, "/NMC/") {
		return "", fmt.Errorf("unexpected auth redirect path: %s", locPath)
	}
	// /NMC/{token}/maybe/more → extract /NMC/{token}
	tokenPart := locPath[5:] // skip "/NMC/"
	if idx := strings.Index(tokenPart, "/"); idx >= 0 {
		tokenPart = tokenPart[:idx]
	}
	authPath := "/NMC/" + tokenPart

	// Verify the new token works
	if authPath == preTokenPath {
		return "", fmt.Errorf("login failed: session token unchanged (bad credentials?)")
	}

	return authPath, nil
}
