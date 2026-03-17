package boards

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/zackpollard/kvm-switcher/internal/models"
)

func init() {
	Register("nanokvm", &NanoKVMBoard{})
}

// NanoKVMBoard implements BoardHandler for Sipeed NanoKVM devices.
type NanoKVMBoard struct{}

func (b *NanoKVMBoard) Scheme() string { return "http" }

func (b *NanoKVMBoard) LoginBypass(path string, creds *models.BMCCredentials) string {
	// NanoKVM: the SPA checks for the nano-kvm-token cookie. The proxy
	// injects it, so the app skips the login page automatically.
	return ""
}

func (b *NanoKVMBoard) LoginIntercept(w http.ResponseWriter, r *http.Request, path string, creds *models.BMCCredentials) bool {
	return false
}

func (b *NanoKVMBoard) InjectCredentials(req *http.Request, creds *models.BMCCredentials) {
	req.AddCookie(&http.Cookie{Name: "nano-kvm-token", Value: creds.SessionCookie})
}

func (b *NanoKVMBoard) RewriteRequestURL(req *http.Request, creds *models.BMCCredentials) {}

func (b *NanoKVMBoard) ModifyProxyResponse(resp *http.Response, creds *models.BMCCredentials) {
	// Pass the JWT token so the SW can set it as a browser cookie
	if creds != nil && creds.SessionCookie != "" {
		resp.Header.Set("X-KVM-NanoToken", creds.SessionCookie)
	}
}

func (b *NanoKVMBoard) RewriteLocationHeader(loc string, proxyPrefix string) string { return loc }

func (b *NanoKVMBoard) CookiesToStrip() []string {
	return []string{"nano-kvm-token"}
}

// Cached latest NanoKVM release versions from GitHub.
var (
	nanoKVMLatestApp       string
	nanoKVMLatestImage     string
	nanoKVMLatestCheckTime time.Time
	nanoKVMLatestMu        sync.Mutex
	nanoKVMReleasesURL     = "https://api.github.com/repos/sipeed/NanoKVM/releases?per_page=20"
)

type nanoKVMVersions struct {
	App   string
	Image string
}

// getNanoKVMLatestVersions returns the latest NanoKVM app and image versions
// from GitHub releases, cached for 1 hour.
func getNanoKVMLatestVersions() nanoKVMVersions {
	nanoKVMLatestMu.Lock()
	defer nanoKVMLatestMu.Unlock()

	if time.Since(nanoKVMLatestCheckTime) < time.Hour && nanoKVMLatestApp != "" {
		return nanoKVMVersions{App: nanoKVMLatestApp, Image: nanoKVMLatestImage}
	}

	client := NewStatusHTTPClient(5*time.Second, false)
	req, err := http.NewRequest("GET", nanoKVMReleasesURL, nil)
	if err != nil {
		return nanoKVMVersions{App: nanoKVMLatestApp, Image: nanoKVMLatestImage}
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		return nanoKVMVersions{App: nanoKVMLatestApp, Image: nanoKVMLatestImage}
	}
	defer resp.Body.Close()

	var releases []struct {
		TagName    string `json:"tag_name"`
		Prerelease bool   `json:"prerelease"`
		Draft      bool   `json:"draft"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nanoKVMVersions{App: nanoKVMLatestApp, Image: nanoKVMLatestImage}
	}

	for _, r := range releases {
		if r.Prerelease || r.Draft {
			continue
		}
		if strings.HasPrefix(r.TagName, "v") && nanoKVMLatestImage == "" {
			nanoKVMLatestImage = r.TagName
		} else if !strings.HasPrefix(r.TagName, "v") && nanoKVMLatestApp == "" {
			nanoKVMLatestApp = r.TagName
		}
		if nanoKVMLatestApp != "" && nanoKVMLatestImage != "" {
			break
		}
	}

	nanoKVMLatestCheckTime = time.Now()
	log.Printf("NanoKVM latest versions: app=%s image=%s", nanoKVMLatestApp, nanoKVMLatestImage)

	return nanoKVMVersions{App: nanoKVMLatestApp, Image: nanoKVMLatestImage}
}

func (b *NanoKVMBoard) FetchStatus(cfg *models.ServerConfig, creds *models.BMCCredentials) *models.DeviceStatus {
	if creds == nil {
		return nil // requires web session credentials
	}
	status := &models.DeviceStatus{Online: true, Model: "NanoKVM"}
	client := NewStatusHTTPClient(5*time.Second, false)

	baseURL := BMCBaseURL("nanokvm", cfg.BMCIP, cfg.BMCPort)

	makeReq := func(url string) (*http.Request, error) {
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return nil, err
		}
		req.AddCookie(&http.Cookie{Name: "nano-kvm-token", Value: creds.SessionCookie})
		return req, nil
	}

	// Device info
	if req, err := makeReq(baseURL + "/api/vm/info"); err == nil {
		if resp, err := client.Do(req); err == nil {
			var infoResp struct {
				Code int `json:"code"`
				Data struct {
					MDNS        string `json:"mdns"`
					Image       string `json:"image"`
					Application string `json:"application"`
				} `json:"data"`
			}
			json.NewDecoder(resp.Body).Decode(&infoResp)
			resp.Body.Close()
			if infoResp.Code == 0 {
				status.Model = "NanoKVM"
				if infoResp.Data.Application != "" {
					status.AppVersion = "v" + infoResp.Data.Application
				}
				if infoResp.Data.Image != "" {
					status.ImageVersion = infoResp.Data.Image
				}
				latest := getNanoKVMLatestVersions()
				if latest.App != "" && infoResp.Data.Application != latest.App {
					status.UpdateAvail = true
				}
			}
		}
	}

	// GPIO state (power LED = host power state)
	if req, err := makeReq(baseURL + "/api/vm/gpio"); err == nil {
		if resp, err := client.Do(req); err == nil {
			var gpioResp struct {
				Code int `json:"code"`
				Data struct {
					Pwr bool `json:"pwr"`
				} `json:"data"`
			}
			json.NewDecoder(resp.Body).Decode(&gpioResp)
			resp.Body.Close()
			if gpioResp.Code == 0 {
				if gpioResp.Data.Pwr {
					status.PowerState = "on"
				} else {
					status.PowerState = "off"
				}
			}
		}
	}

	return status
}
