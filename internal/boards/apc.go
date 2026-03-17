package boards

import (
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zackpollard/kvm-switcher/internal/models"
)

func init() {
	Register("apc_ups", &APCBoard{})
}

// APCBoard implements BoardHandler for APC UPS/PDU devices.
type APCBoard struct{}

func (b *APCBoard) Scheme() string { return "http" }

func (b *APCBoard) LoginBypass(path string, creds *models.BMCCredentials) string {
	if path == "/" || strings.HasSuffix(path, "/logon.htm") {
		log.Printf("BMC proxy: bypassing APC login, redirecting to dashboard")
		return "home.htm"
	}
	return ""
}

func (b *APCBoard) LoginIntercept(w http.ResponseWriter, r *http.Request, path string, creds *models.BMCCredentials) bool {
	return false
}

func (b *APCBoard) InjectCredentials(req *http.Request, creds *models.BMCCredentials) {
	// APC NMC2: auth is URL-based (session token in path). No cookies
	// or headers needed — the Director prepends the NMC session path.
}

func (b *APCBoard) RewriteRequestURL(req *http.Request, creds *models.BMCCredentials) {
	// APC NMC2: session auth is URL-based. Prepend the NMC session path
	// to every request. If the path already has /NMC/{token}/ (from
	// absolute URLs in the HTML), strip the old token and replace.
	if nmcPath := creds.Extra["nmc_path"]; nmcPath != "" {
		p := req.URL.Path
		if strings.HasPrefix(p, "/NMC/") {
			// /NMC/{old_token}/rest → /rest
			if idx := strings.Index(p[5:], "/"); idx >= 0 {
				p = p[5+idx:]
			}
		}
		req.URL.Path = nmcPath + p
		if req.URL.RawPath != "" {
			req.URL.RawPath = nmcPath + p
		}
	}
}

func (b *APCBoard) ModifyProxyResponse(resp *http.Response, creds *models.BMCCredentials) {}

func (b *APCBoard) RewriteLocationHeader(loc string, proxyPrefix string) string {
	// Strip /NMC/{token}/ from rewritten paths so the client sees clean
	// paths (the proxy re-adds the token on the next request via the Director).
	after := strings.TrimPrefix(loc, proxyPrefix)
	if strings.HasPrefix(after, "/NMC/") {
		if idx := strings.Index(after[5:], "/"); idx >= 0 {
			return proxyPrefix + after[5+idx:]
		}
		return proxyPrefix + "/"
	}
	return loc
}

func (b *APCBoard) CookiesToStrip() []string {
	return nil
}

func (b *APCBoard) FetchStatus(cfg *models.ServerConfig, creds *models.BMCCredentials) *models.DeviceStatus {
	if creds == nil {
		return nil // requires web session credentials
	}
	status := &models.DeviceStatus{Online: true}
	client := NewStatusHTTPClient(5*time.Second, false)

	nmcPath := ""
	if creds.Extra != nil {
		nmcPath = creds.Extra["nmc_path"]
	}

	baseURL := BMCBaseURL("apc_ups", cfg.BMCIP, cfg.BMCPort) + nmcPath

	// Try UPS status page first
	isUPS := false
	upsReq, err := http.NewRequest("GET", baseURL+"/upstat.htm", nil)
	if err == nil {
		upsResp, err := client.Do(upsReq)
		if err == nil {
			defer upsResp.Body.Close()
			upsBody, err := io.ReadAll(upsResp.Body)
			if err == nil {
				upsStr := string(upsBody)
				if strings.Contains(upsStr, "Runtime Remaining") || strings.Contains(upsStr, "langCapacity") {
					isUPS = true
					status.PowerState = "on"
					status.Model = "APC UPS"

					flat := regexp.MustCompile(`\s+`).ReplaceAllString(upsStr, " ")

					if m := regexp.MustCompile(`Runtime Remaining.*?dataValue">\s*(\d+(?:\.\d+)?)\s*min`).FindStringSubmatch(flat); len(m) > 1 {
						if v, err := strconv.ParseFloat(m[1], 64); err == nil {
							status.RuntimeMin = v
						}
					}

					if m := regexp.MustCompile(`Capacity.*?dataValue">\s*(\d+(?:\.\d+)?)\s*(?:&nbsp;)?\s*%`).FindStringSubmatch(flat); len(m) > 1 {
						if v, err := strconv.ParseFloat(m[1], 64); err == nil {
							status.BatteryPct = v
						}
					}

					if m := regexp.MustCompile(`Load Power.*?dataValue">\s*(\d+(?:\.\d+)?)`).FindStringSubmatch(flat); len(m) > 1 {
						if v, err := strconv.ParseFloat(m[1], 64); err == nil {
							status.LoadPct = v
						}
					}

					if m := regexp.MustCompile(`Load Current.*?dataValue">\s*(\d+(?:\.\d+)?)`).FindStringSubmatch(flat); len(m) > 1 {
						if v, err := strconv.ParseFloat(m[1], 64); err == nil {
							status.LoadAmps = v
						}
					}

					if m := regexp.MustCompile(`Internal Temperature.*?dataValue">\s*(\d+(?:\.\d+)?)\s*(?:&deg;|°)`).FindStringSubmatch(flat); len(m) > 1 {
						if v, err := strconv.ParseFloat(m[1], 64); err == nil {
							status.TemperatureC = v
						}
					}

					if m := regexp.MustCompile(`Input Voltage.*?<td>\s*(\d+(?:\.\d+)?)\s*VAC`).FindStringSubmatch(flat); len(m) > 1 {
						if v, err := strconv.ParseFloat(m[1], 64); err == nil {
							status.Voltage = v
						}
					}
				}

				// Fetch actual model from upabout.htm
				if aboutReq, err := http.NewRequest("GET", baseURL+"/upabout.htm", nil); err == nil {
					if aboutResp, err := client.Do(aboutReq); err == nil {
						aboutBody, _ := io.ReadAll(aboutResp.Body)
						aboutResp.Body.Close()
						aboutFlat := regexp.MustCompile(`\s+`).ReplaceAllString(string(aboutBody), " ")
						if m := regexp.MustCompile(`(?:langModel|Model).*?dataValue">\s*([^<]+)`).FindStringSubmatch(aboutFlat); len(m) > 1 {
							if model := strings.TrimSpace(m[1]); model != "" {
								status.Model = model
							}
						}
					}
				}
			}
		}
	}

	// If not UPS, try PDU home page
	if !isUPS {
		homeReq, err := http.NewRequest("GET", baseURL+"/home.htm", nil)
		if err != nil {
			status.Online = false
			return status
		}

		homeResp, err := client.Do(homeReq)
		if err != nil {
			status.Online = false
			return status
		}
		defer homeResp.Body.Close()

		homeBody, err := io.ReadAll(homeResp.Body)
		if err != nil {
			return status
		}
		homeStr := string(homeBody)

		status.PowerState = "on"
		status.Model = "APC PDU"

		// Fetch actual model from aboutpdu.htm
		if aboutReq, err := http.NewRequest("GET", baseURL+"/aboutpdu.htm", nil); err == nil {
			if aboutResp, err := client.Do(aboutReq); err == nil {
				aboutBody, _ := io.ReadAll(aboutResp.Body)
				aboutResp.Body.Close()
				aboutFlat := regexp.MustCompile(`\s+`).ReplaceAllString(string(aboutBody), " ")
				if m := regexp.MustCompile(`Model\s*Number.*?dataValue">\s*([^<]+)`).FindStringSubmatch(aboutFlat); len(m) > 1 {
					if model := strings.TrimSpace(m[1]); model != "" {
						status.Model = model
					}
				}
			}
		}

		homeFlat := regexp.MustCompile(`\s+`).ReplaceAllString(homeStr, " ")

		if m := regexp.MustCompile(`Device Load.*?dataValue">\s*(\d+(?:\.\d+)?)\s*kW`).FindStringSubmatch(homeFlat); len(m) > 1 {
			if v, err := strconv.ParseFloat(m[1], 64); err == nil {
				status.LoadWatts = v * 1000
			}
		}

		if m := regexp.MustCompile(`Current Load Value is (\d+(?:\.\d+)?)\s*A`).FindStringSubmatch(homeFlat); len(m) > 1 {
			if v, err := strconv.ParseFloat(m[1], 64); err == nil {
				status.LoadAmps = v
			}
		}

		phReq, err := http.NewRequest("GET", baseURL+"/phstat.htm", nil)
		if err == nil {
			phResp, err := client.Do(phReq)
			if err == nil {
				defer phResp.Body.Close()
				phBody, err := io.ReadAll(phResp.Body)
				if err == nil {
					phStr := string(phBody)
					reVolts := regexp.MustCompile(`(\d+(?:\.\d+)?)\s*V\b`)
					if m := reVolts.FindStringSubmatch(phStr); len(m) > 1 {
						if v, err := strconv.ParseFloat(m[1], 64); err == nil {
							status.Voltage = v
						}
					}
				}
			}
		}
	}

	return status
}

