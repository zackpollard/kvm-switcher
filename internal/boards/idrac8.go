package boards

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/zackpollard/kvm-switcher/internal/models"
)

func init() {
	Register("dell_idrac8", &IDRAC8Board{})
}

// IDRAC8Board implements BoardHandler for Dell iDRAC8 (13G servers).
type IDRAC8Board struct{}

func (b *IDRAC8Board) Scheme() string { return "https" }

func (b *IDRAC8Board) LoginBypass(path string, creds *models.BMCCredentials) string {
	if path == "/" || path == "/start.html" || path == "/login.html" {
		st1 := ""
		if creds.Extra != nil {
			st1 = creds.Extra["st1"]
		}
		redirectURL := fmt.Sprintf("index.html?ST1=%s,ST2=%s", st1, creds.CSRFToken)
		log.Printf("BMC proxy: bypassing iDRAC8 login, redirecting to dashboard")
		return redirectURL
	}
	return ""
}

func (b *IDRAC8Board) LoginIntercept(w http.ResponseWriter, r *http.Request, path string, creds *models.BMCCredentials) bool {
	// iDRAC8 pre-login logout: the login page POSTs to /data/logout
	// before submitting credentials (session fixation prevention).
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

	return false
}

func (b *IDRAC8Board) InjectCredentials(req *http.Request, creds *models.BMCCredentials) {
	req.AddCookie(&http.Cookie{Name: "-http-session-", Value: creds.SessionCookie})
	if creds.CSRFToken != "" {
		req.Header.Set("ST2", creds.CSRFToken)
	}
}

func (b *IDRAC8Board) RewriteRequestURL(req *http.Request, creds *models.BMCCredentials) {}

func (b *IDRAC8Board) ModifyProxyResponse(resp *http.Response, creds *models.BMCCredentials) {
	// iDRAC8 sets Content-Type: application/x-gzip on some responses.
	if resp.Header.Get("Content-Type") == "application/x-gzip" {
		resp.Header.Set("Content-Type", InferContentType(resp.Request.URL.Path))
	}

	// iDRAC8 serves .jsesp (embedded JS) files as text/html.
	if strings.HasSuffix(resp.Request.URL.Path, ".jsesp") {
		resp.Header.Set("Content-Type", "application/javascript")
	}

	// iDRAC8 firmware omits the X_Language header on /session API responses.
	if resp.Header.Get("X_Language") == "" {
		resp.Header.Set("X_Language", "en")
	}
}

func (b *IDRAC8Board) RewriteLocationHeader(loc string, proxyPrefix string) string { return loc }

func (b *IDRAC8Board) CookiesToStrip() []string {
	return []string{"-http-session-"}
}

func (b *IDRAC8Board) FetchStatus(cfg *models.ServerConfig, creds *models.BMCCredentials) *models.DeviceStatus {
	status := &models.DeviceStatus{Online: true}
	client := NewStatusHTTPClient(20*time.Second, true) // iDRAC8 is slow

	baseURL := BMCBaseURL("dell_idrac8", cfg.BMCIP, cfg.BMCPort)

	// iDRAC8 Redfish uses Basic Auth (not session cookies like the web UI)
	password := ""
	if cfg.CredentialEnv != "" {
		password = os.Getenv(cfg.CredentialEnv)
	}

	makeReq := func(url string) (*http.Request, error) {
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return nil, err
		}
		req.SetBasicAuth(cfg.Username, password)
		return req, nil
	}

	// Fetch system info (power state, model, health)
	req, err := makeReq(baseURL + "/redfish/v1/Systems/System.Embedded.1")
	if err != nil {
		status.Online = false
		return status
	}

	resp, err := client.Do(req)
	if err != nil {
		status.Online = false
		return status
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		var sysResp RedfishSystemResponse
		if err := json.NewDecoder(resp.Body).Decode(&sysResp); err == nil {
			status.PowerState = strings.ToLower(sysResp.PowerState)
			status.Model = sysResp.Model
			status.Health = strings.ToLower(sysResp.Status.HealthRollup)
		}
	}

	// Fetch power consumption
	req, err = makeReq(baseURL + "/redfish/v1/Chassis/System.Embedded.1/Power")
	if err == nil {
		resp, err := client.Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var powerResp RedfishPowerResponse
				if err := json.NewDecoder(resp.Body).Decode(&powerResp); err == nil {
					if len(powerResp.PowerControl) > 0 {
						status.LoadWatts = powerResp.PowerControl[0].PowerConsumedWatts
					}
				}
			}
		}
	}

	// Fetch thermal (inlet temperature)
	req, err = makeReq(baseURL + "/redfish/v1/Chassis/System.Embedded.1/Thermal")
	if err == nil {
		resp, err := client.Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var thermalResp RedfishThermalResponse
				if err := json.NewDecoder(resp.Body).Decode(&thermalResp); err == nil {
					for _, t := range thermalResp.Temperatures {
						if strings.Contains(strings.ToLower(t.Name), "inlet") {
							status.TemperatureC = t.ReadingCelsius
							break
						}
					}
				}
			}
		}
	}

	return status
}

// InferContentType returns a MIME type based on the URL path extension.
// Used when the BMC sends a generic Content-Type like application/x-gzip.
func InferContentType(path string) string {
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
