package api

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zackpollard/kvm-switcher/internal/models"
)

// DeviceStatus holds polled status information for a single device.
type DeviceStatus struct {
	Online       bool    `json:"online"`
	PowerState   string  `json:"power_state,omitempty"`   // "on", "off", ""
	Model        string  `json:"model,omitempty"`
	Health       string  `json:"health,omitempty"`         // "ok", "warning", "critical", ""
	LoadWatts    float64 `json:"load_watts,omitempty"`
	LoadPct      float64 `json:"load_pct,omitempty"`      // UPS: percentage of rated capacity
	LoadAmps     float64 `json:"load_amps,omitempty"`
	Voltage      float64 `json:"voltage,omitempty"`
	BatteryPct   float64 `json:"battery_pct,omitempty"`
	RuntimeMin   float64 `json:"runtime_min,omitempty"`
	TemperatureC float64 `json:"temperature_c,omitempty"`
}

// StatusCache stores device status per server with thread-safe access.
type StatusCache struct {
	mu      sync.RWMutex
	entries map[string]*DeviceStatus // server name -> status
}

// NewStatusCache creates a new StatusCache.
func NewStatusCache() *StatusCache {
	return &StatusCache{
		entries: make(map[string]*DeviceStatus),
	}
}

// Get returns the cached status for a server by name.
func (sc *StatusCache) Get(name string) (*DeviceStatus, bool) {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	s, ok := sc.entries[name]
	return s, ok
}

// Set stores the status for a server by name.
func (sc *StatusCache) Set(name string, status *DeviceStatus) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.entries[name] = status
}

// GetAll returns a copy of all cached statuses.
func (sc *StatusCache) GetAll() map[string]*DeviceStatus {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	result := make(map[string]*DeviceStatus, len(sc.entries))
	for k, v := range sc.entries {
		cp := *v
		result[k] = &cp
	}
	return result
}

// GetAllProxyEntries iterates the bmcProxies sync.Map and returns all entries.
func GetAllProxyEntries() map[string]*bmcProxyEntry {
	result := make(map[string]*bmcProxyEntry)
	bmcProxies.Range(func(key, value any) bool {
		result[key.(string)] = value.(*bmcProxyEntry)
		return true
	})
	return result
}

// newStatusHTTPClient creates an HTTP client with the given timeout and optional
// TLS InsecureSkipVerify for HTTPS BMCs.
// bmcBaseURL returns the base URL for a BMC, handling default ports.
func bmcBaseURL(boardType, bmcIP string, bmcPort int) string {
	scheme := bmcScheme(boardType)
	if bmcPort == 0 {
		if scheme == "https" {
			bmcPort = 443
		} else {
			bmcPort = 80
		}
	}
	return fmt.Sprintf("%s://%s:%d", scheme, bmcIP, bmcPort)
}

func newStatusHTTPClient(timeout time.Duration, insecureTLS bool) *http.Client {
	transport := &http.Transport{}
	if insecureTLS {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
}

// fetchMegaRACStatus fetches status from an AMI MegaRAC BMC.
func fetchMegaRACStatus(bmcIP string, bmcPort int, creds *models.BMCCredentials) *DeviceStatus {
	status := &DeviceStatus{Online: true}
	client := newStatusHTTPClient(5*time.Second, false)

	baseURL := bmcBaseURL("ami_megarac", bmcIP, bmcPort)

	// Fetch host power status
	req, err := http.NewRequest("GET", baseURL+"/rpc/hoststatus.asp", nil)
	if err != nil {
		status.Online = false
		return status
	}
	req.AddCookie(&http.Cookie{Name: "SessionCookie", Value: creds.SessionCookie})
	if creds.CSRFToken != "" {
		req.Header.Set("CSRFTOKEN", creds.CSRFToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		status.Online = false
		return status
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return status
	}
	bodyStr := string(body)

	// Parse JF_STATE from AMI's custom JS response: 'JF_STATE' : 1
	reState := regexp.MustCompile(`JF_STATE['\s]*:\s*(\d+)`)
	if m := reState.FindStringSubmatch(bodyStr); len(m) > 1 {
		if m[1] == "1" {
			status.PowerState = "on"
		} else {
			status.PowerState = "off"
		}
	}

	// Fetch FRU info for model name
	fruReq, err := http.NewRequest("GET", baseURL+"/rpc/getfruinfo.asp", nil)
	if err != nil {
		return status
	}
	fruReq.AddCookie(&http.Cookie{Name: "SessionCookie", Value: creds.SessionCookie})
	if creds.CSRFToken != "" {
		fruReq.Header.Set("CSRFTOKEN", creds.CSRFToken)
	}

	fruResp, err := client.Do(fruReq)
	if err != nil {
		return status
	}
	defer fruResp.Body.Close()

	fruBody, err := io.ReadAll(fruResp.Body)
	if err != nil {
		return status
	}
	fruStr := string(fruBody)

	// Try to extract model from PI_ProductName, BI_BoardProductName, or BI_BoardMfr
	var modelParts []string
	reProduct := regexp.MustCompile(`PI_ProductName['\s]*:\s*'([^']*)'`)
	reBoardProduct := regexp.MustCompile(`BI_BoardProductName['\s]*:\s*'([^']*)'`)
	reBoardMfr := regexp.MustCompile(`BI_BoardMfr['\s]*:\s*'([^']*)'`)

	if m := reProduct.FindStringSubmatch(fruStr); len(m) > 1 && strings.TrimSpace(m[1]) != "" {
		status.Model = strings.TrimSpace(m[1])
	} else {
		if m := reBoardMfr.FindStringSubmatch(fruStr); len(m) > 1 && strings.TrimSpace(m[1]) != "" {
			modelParts = append(modelParts, strings.TrimSpace(m[1]))
		}
		if m := reBoardProduct.FindStringSubmatch(fruStr); len(m) > 1 && strings.TrimSpace(m[1]) != "" {
			modelParts = append(modelParts, strings.TrimSpace(m[1]))
		}
		if len(modelParts) > 0 {
			status.Model = strings.Join(modelParts, " ")
		}
	}

	// Fetch sensor data for temperature
	sensReq, err := http.NewRequest("GET", baseURL+"/rpc/getallsensors.asp", nil)
	if err != nil {
		return status
	}
	sensReq.AddCookie(&http.Cookie{Name: "SessionCookie", Value: creds.SessionCookie})
	if creds.CSRFToken != "" {
		sensReq.Header.Set("CSRFTOKEN", creds.CSRFToken)
	}
	sensResp, err := client.Do(sensReq)
	if err != nil {
		return status
	}
	defer sensResp.Body.Close()
	sensBody, err := io.ReadAll(sensResp.Body)
	if err != nil {
		return status
	}
	sensStr := string(sensBody)

	// Sensors use millidegrees (36000 = 36°C). Extract CPU1 temp for the card display.
	reCPUTemp := regexp.MustCompile(`'SensorName'\s*:\s*'CPU1 Temp'[^}]*'SensorReading'\s*:\s*(\d+)`)
	if m := reCPUTemp.FindStringSubmatch(sensStr); len(m) > 1 {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil && v > 0 {
			status.TemperatureC = v / 1000
		}
	}

	return status
}

// redfishSystemResponse represents the JSON from /redfish/v1/Systems/System.Embedded.1.
type redfishSystemResponse struct {
	PowerState string `json:"PowerState"`
	Model      string `json:"Model"`
	Status     struct {
		HealthRollup string `json:"HealthRollup"`
	} `json:"Status"`
}

// redfishPowerResponse represents the JSON from /redfish/v1/Chassis/.../Power.
type redfishPowerResponse struct {
	PowerControl []struct {
		PowerConsumedWatts float64 `json:"PowerConsumedWatts"`
	} `json:"PowerControl"`
}

// redfishThermalResponse represents the JSON from /redfish/v1/Chassis/.../Thermal.
type redfishThermalResponse struct {
	Temperatures []struct {
		Name                   string  `json:"Name"`
		ReadingCelsius         float64 `json:"ReadingCelsius"`
		UpperThresholdCritical float64 `json:"UpperThresholdCritical"`
	} `json:"Temperatures"`
}

// fetchIDRAC9Status fetches status from a Dell iDRAC9 via Redfish.
func fetchIDRAC9Status(bmcIP string, bmcPort int, creds *models.BMCCredentials) *DeviceStatus {
	status := &DeviceStatus{Online: true}
	client := newStatusHTTPClient(5*time.Second, true)

	baseURL := bmcBaseURL("dell_idrac9", bmcIP, bmcPort)

	// Helper to create authenticated requests for iDRAC9
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
		var sysResp redfishSystemResponse
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
				var powerResp redfishPowerResponse
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
				var thermalResp redfishThermalResponse
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

// fetchIDRAC8Status fetches status from a Dell iDRAC8 via Redfish.
func fetchIDRAC8Status(cfg *models.ServerConfig, creds *models.BMCCredentials) *DeviceStatus {
	status := &DeviceStatus{Online: true}
	client := newStatusHTTPClient(20*time.Second, true) // iDRAC8 is slow

	baseURL := bmcBaseURL("dell_idrac8", cfg.BMCIP, cfg.BMCPort)

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
		var sysResp redfishSystemResponse
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
				var powerResp redfishPowerResponse
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
				var thermalResp redfishThermalResponse
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

// fetchAPCStatus fetches status from an APC UPS or PDU via its NMC web interface.
func fetchAPCStatus(bmcIP string, bmcPort int, creds *models.BMCCredentials) *DeviceStatus {
	status := &DeviceStatus{Online: true}
	client := newStatusHTTPClient(5*time.Second, false)

	nmcPath := ""
	if creds.Extra != nil {
		nmcPath = creds.Extra["nmc_path"]
	}

	baseURL := bmcBaseURL("apc_ups", bmcIP, bmcPort) + nmcPath

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
				// Check if this is actually a UPS page with useful data
				if strings.Contains(upsStr, "Runtime Remaining") || strings.Contains(upsStr, "langCapacity") {
					isUPS = true
					status.PowerState = "on"
					status.Model = "APC UPS"

					// Collapse whitespace for easier regex matching across HTML lines
					flat := regexp.MustCompile(`\s+`).ReplaceAllString(upsStr, " ")

					// Runtime Remaining: "Runtime Remaining</span> </div> <div class="dataValue"> 18min"
					if m := regexp.MustCompile(`Runtime Remaining.*?dataValue">\s*(\d+(?:\.\d+)?)\s*min`).FindStringSubmatch(flat); len(m) > 1 {
						if v, err := strconv.ParseFloat(m[1], 64); err == nil {
							status.RuntimeMin = v
						}
					}

					// Battery Capacity: "Capacity</span> </div> <div class="dataValue"> 100.0&nbsp;%"
					if m := regexp.MustCompile(`Capacity.*?dataValue">\s*(\d+(?:\.\d+)?)\s*(?:&nbsp;)?\s*%`).FindStringSubmatch(flat); len(m) > 1 {
						if v, err := strconv.ParseFloat(m[1], 64); err == nil {
							status.BatteryPct = v
						}
					}

					// Load Power: "Load Power</span> </div> <div class="dataValue"> 23.0&nbsp; ...%Watts"
					if m := regexp.MustCompile(`Load Power.*?dataValue">\s*(\d+(?:\.\d+)?)`).FindStringSubmatch(flat); len(m) > 1 {
						if v, err := strconv.ParseFloat(m[1], 64); err == nil {
							status.LoadPct = v
						}
					}

					// Load Current: "Load Current</span> </div> <div class="dataValue"> 5.36&nbsp;Amps"
					if m := regexp.MustCompile(`Load Current.*?dataValue">\s*(\d+(?:\.\d+)?)`).FindStringSubmatch(flat); len(m) > 1 {
						if v, err := strconv.ParseFloat(m[1], 64); err == nil {
							status.LoadAmps = v
						}
					}

					// Internal Temperature: "Internal Temperature</span> </div> <div class="dataValue"> 22.9&deg;C"
					if m := regexp.MustCompile(`Internal Temperature.*?dataValue">\s*(\d+(?:\.\d+)?)\s*(?:&deg;|°)`).FindStringSubmatch(flat); len(m) > 1 {
						if v, err := strconv.ParseFloat(m[1], 64); err == nil {
							status.TemperatureC = v
						}
					}

					// Input Voltage: "Input Voltage</span> </th> <td> 216.0 VAC"
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

		// Collapse whitespace for easier regex matching across HTML lines
		homeFlat := regexp.MustCompile(`\s+`).ReplaceAllString(homeStr, " ")

		// Parse Device Load in kW: "Device Load</div> <div class="dataValue"> 1.14 kW"
		if m := regexp.MustCompile(`Device Load.*?dataValue">\s*(\d+(?:\.\d+)?)\s*kW`).FindStringSubmatch(homeFlat); len(m) > 1 {
			if v, err := strconv.ParseFloat(m[1], 64); err == nil {
				status.LoadWatts = v * 1000
			}
		}

		// Parse Phase L1 load in amps from gauge: alt="Current Load Value is 5.9 A"
		if m := regexp.MustCompile(`Current Load Value is (\d+(?:\.\d+)?)\s*A`).FindStringSubmatch(homeFlat); len(m) > 1 {
			if v, err := strconv.ParseFloat(m[1], 64); err == nil {
				status.LoadAmps = v
			}
		}

		// Try to get voltage from phstat.htm
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

// checkDeviceOnline performs a simple HTTP HEAD request to see if a BMC is reachable.
func checkDeviceOnline(bmcIP string, bmcPort int, boardType string) bool {
	url := bmcBaseURL(boardType, bmcIP, bmcPort) + "/"

	client := newStatusHTTPClient(3*time.Second, true)
	req, err := http.NewRequest("HEAD", url, nil)
	if err != nil {
		return false
	}

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// fetchDeviceStatus fetches the status for a single server, dispatching to the
// appropriate board-type-specific fetcher.
func fetchDeviceStatus(cfg *models.ServerConfig) *DeviceStatus {
	// Check if we have cached BMC credentials from a proxy entry
	entries := GetAllProxyEntries()
	entry, hasEntry := entries[cfg.Name]

	var creds *models.BMCCredentials
	if hasEntry {
		creds = entry.getBMCCredentials()
	}

	// If we have credentials, fetch detailed status
	if creds != nil {
		switch cfg.BoardType {
		case "dell_idrac9":
			return fetchIDRAC9Status(cfg.BMCIP, cfg.BMCPort, creds)
		case "dell_idrac8":
			return fetchIDRAC8Status(cfg, creds)
		case "apc_ups":
			return fetchAPCStatus(cfg.BMCIP, cfg.BMCPort, creds)
		default:
			// AMI MegaRAC
			return fetchMegaRACStatus(cfg.BMCIP, cfg.BMCPort, creds)
		}
	}

	// iDRAC8 can use Basic Auth without a web session
	if cfg.BoardType == "dell_idrac8" {
		return fetchIDRAC8Status(cfg, nil)
	}

	// No credentials available — just check if device is reachable
	online := checkDeviceOnline(cfg.BMCIP, cfg.BMCPort, cfg.BoardType)
	return &DeviceStatus{Online: online}
}

// PollStatuses fetches status for all configured servers in parallel and updates the cache.
func PollStatuses(servers []models.ServerConfig, cache *StatusCache) {
	var wg sync.WaitGroup
	for i := range servers {
		wg.Add(1)
		go func(cfg *models.ServerConfig) {
			defer wg.Done()
			// Hard deadline per server to prevent a hung connection
			// from blocking the entire poll cycle.
			done := make(chan *DeviceStatus, 1)
			go func() {
				done <- fetchDeviceStatus(cfg)
			}()
			select {
			case status := <-done:
				cache.Set(cfg.Name, status)
			case <-time.After(30 * time.Second):
				cache.Set(cfg.Name, &DeviceStatus{Online: false})
			}
		}(&servers[i])
	}
	wg.Wait()
}

// StartStatusPoller starts a background goroutine that polls all server statuses
// every 30 seconds. It runs an initial poll immediately.
func StartStatusPoller(servers []models.ServerConfig, cache *StatusCache) {
	// Run initial poll
	go func() {
		log.Printf("Status poller: starting initial poll for %d servers", len(servers))
		PollStatuses(servers, cache)
		log.Printf("Status poller: initial poll complete")

		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			log.Printf("Status poller: tick, polling %d servers", len(servers))
			PollStatuses(servers, cache)
			log.Printf("Status poller: tick complete")
		}
	}()
}
