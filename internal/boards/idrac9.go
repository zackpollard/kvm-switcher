package boards

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/zackpollard/kvm-switcher/internal/models"
	"github.com/zackpollard/kvm-switcher/internal/tlsutil"
)

func init() {
	Register("dell_idrac9", &IDRAC9Board{})
}

// IDRAC9Board implements BoardHandler for Dell iDRAC9 (14G+ servers).
type IDRAC9Board struct{}

func (b *IDRAC9Board) Scheme() string { return "https" }

func (b *IDRAC9Board) LoginBypass(path string, creds *models.BMCCredentials) string {
	// iDRAC9's Angular SPA requires the login API call to set client-side
	// state, so we can't skip directly to the dashboard. Handled by the SW.
	return ""
}

func (b *IDRAC9Board) LoginIntercept(w http.ResponseWriter, r *http.Request, path string, creds *models.BMCCredentials) bool {
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
	return false
}

func (b *IDRAC9Board) InjectCredentials(req *http.Request, creds *models.BMCCredentials) {
	req.AddCookie(&http.Cookie{Name: "-http-session-", Value: creds.SessionCookie})
	if creds.CSRFToken != "" {
		req.Header.Set("XSRF-TOKEN", creds.CSRFToken)
	}
}

func (b *IDRAC9Board) RewriteRequestURL(req *http.Request, creds *models.BMCCredentials) {}

func (b *IDRAC9Board) ModifyProxyResponse(resp *http.Response, creds *models.BMCCredentials) {}

func (b *IDRAC9Board) RewriteLocationHeader(loc string, proxyPrefix string) string { return loc }

func (b *IDRAC9Board) CookiesToStrip() []string {
	return []string{"-http-session-"}
}

func (b *IDRAC9Board) FetchStatus(cfg *models.ServerConfig, creds *models.BMCCredentials) *models.DeviceStatus {
	if creds == nil {
		return nil // requires web session credentials
	}
	status := &models.DeviceStatus{Online: true}
	client := NewStatusHTTPClient(5*time.Second, tlsutil.SkipVerify(cfg))

	baseURL := BMCBaseURL("dell_idrac9", cfg.BMCIP, cfg.BMCPort)

	makeReq := func(url string) (*http.Request, error) {
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-Auth-Token", creds.CSRFToken)
		req.AddCookie(&http.Cookie{Name: "-http-session-", Value: creds.SessionCookie})
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
