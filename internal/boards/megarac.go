package boards

import (
	"fmt"
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
	Register("ami_megarac", &MegaRACBoard{})
}

// MegaRACBoard implements BoardHandler for AMI MegaRAC BMCs.
type MegaRACBoard struct{}

func (b *MegaRACBoard) Scheme() string { return "http" }

func (b *MegaRACBoard) LoginBypass(path string, creds *models.BMCCredentials) string {
	return ""
}

func (b *MegaRACBoard) LoginIntercept(w http.ResponseWriter, r *http.Request, path string, creds *models.BMCCredentials) bool {
	// Intercept logout.asp to prevent the managed session from being invalidated.
	if r.Method == http.MethodGet && path == "/rpc/WEBSES/logout.asp" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"WEBSES":{"SESSID":"Disconnected"}}`)
		log.Printf("BMC proxy: intercepted MegaRAC logout, returning fake OK")
		return true
	}
	return false
}

func (b *MegaRACBoard) InjectCredentials(req *http.Request, creds *models.BMCCredentials) {
	req.AddCookie(&http.Cookie{Name: "SessionCookie", Value: creds.SessionCookie})
	if creds.CSRFToken != "" {
		req.Header.Set("CSRFTOKEN", creds.CSRFToken)
	}
}

func (b *MegaRACBoard) RewriteRequestURL(req *http.Request, creds *models.BMCCredentials) {}

func (b *MegaRACBoard) ModifyProxyResponse(resp *http.Response, creds *models.BMCCredentials) {}

func (b *MegaRACBoard) RewriteLocationHeader(loc string, proxyPrefix string) string { return loc }

func (b *MegaRACBoard) CookiesToStrip() []string {
	return []string{"SessionCookie"}
}

func (b *MegaRACBoard) FetchStatus(cfg *models.ServerConfig, creds *models.BMCCredentials) *models.DeviceStatus {
	if creds == nil {
		return nil // requires web session credentials
	}
	status := &models.DeviceStatus{Online: true}
	client := NewStatusHTTPClient(5*time.Second, false)

	baseURL := BMCBaseURL("ami_megarac", cfg.BMCIP, cfg.BMCPort)

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
