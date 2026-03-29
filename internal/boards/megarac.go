package boards

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zackpollard/kvm-switcher/internal/models"
	"github.com/zackpollard/kvm-switcher/internal/tlsutil"
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
	client := NewStatusHTTPClient(5*time.Second, tlsutil.SkipVerify(cfg))

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

// --- VirtualMediaHandler implementation ---

// parseMegaRACMediaURL extracts server, path, filename, and share type from an NFS or CIFS URL.
func parseMegaRACMediaURL(imageURL string) (ipAddr, srcPath, imgName string, shrType int, err error) {
	u, err := url.Parse(imageURL)
	if err != nil {
		return "", "", "", 0, fmt.Errorf("invalid URL: %w", err)
	}
	switch u.Scheme {
	case "nfs":
		dir, file := path.Split(u.Path)
		if file == "" {
			return "", "", "", 0, fmt.Errorf("NFS URL must include a filename (e.g., nfs://server/path/image.iso)")
		}
		return u.Host, dir, file, 0, nil
	case "cifs", "smb":
		dir, file := path.Split(u.Path)
		if file == "" {
			return "", "", "", 0, fmt.Errorf("CIFS URL must include a filename (e.g., cifs://server/share/image.iso)")
		}
		return u.Host, dir, file, 1, nil
	default:
		return "", "", "", 0, fmt.Errorf("MegaRAC requires NFS or CIFS URL (e.g., nfs://server/path/image.iso)")
	}
}

// megaracDoRequest performs an authenticated HTTP request to the MegaRAC BMC and returns the response body.
func (b *MegaRACBoard) megaracDoRequest(client *http.Client, method, url string, body string, creds *models.BMCCredentials) (string, error) {
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return "", fmt.Errorf("request creation failed: %w", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.AddCookie(&http.Cookie{Name: "SessionCookie", Value: creds.SessionCookie})
	if creds.CSRFToken != "" {
		req.Header.Set("CSRFTOKEN", creds.CSRFToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return string(respBody), nil
}

// megaracCheckHAPIStatus checks the HAPI_STATUS in a MegaRAC JS response. Returns nil if status is 0 (success).
func megaracCheckHAPIStatus(body string) error {
	re := regexp.MustCompile(`HAPI_STATUS\s*:\s*(\d+)`)
	m := re.FindStringSubmatch(body)
	if len(m) < 2 {
		return fmt.Errorf("could not parse HAPI_STATUS from response")
	}
	if m[1] != "0" {
		return fmt.Errorf("MegaRAC returned error status %s", m[1])
	}
	return nil
}

func (b *MegaRACBoard) GetMediaStatus(cfg *models.ServerConfig, creds *models.BMCCredentials) (*models.VirtualMediaStatus, error) {
	if creds == nil {
		return nil, fmt.Errorf("no BMC credentials available")
	}

	baseURL := BMCBaseURL("ami_megarac", cfg.BMCIP, cfg.BMCPort)
	client := NewStatusHTTPClient(30*time.Second, tlsutil.SkipVerify(cfg))

	body, err := b.megaracDoRequest(client, "GET", baseURL+"/rpc/getrmediacfg.asp", "", creds)
	if err != nil {
		return nil, fmt.Errorf("failed to get remote media config: %w", err)
	}

	// Find the CD/DVD slot. The response contains multiple slots (Floppy, CD/DVD, Harddisk).
	// Look for the IMG_TYPE that matches "CD/DVD" and check its START_FLAG.
	// Response format: 'IMG_TYPE' : 'CD/DVD', ... 'START_FLAG' : 1, 'STATUS_FLAG' : 0, ...
	// We need to find the block for CD/DVD and extract its fields.

	status := &models.VirtualMediaStatus{
		MediaType: "CD/DVD",
	}

	// Find the CD/DVD entry by locating IMG_TYPE='CD/DVD' and reading nearby fields.
	// MegaRAC returns entries as JS object arrays, each entry having these fields.
	reCDBlock := regexp.MustCompile(`(?s)'IMG_TYPE'\s*:\s*'CD/DVD'[^}]*`)
	cdBlock := reCDBlock.FindString(body)
	if cdBlock == "" {
		// No CD/DVD slot found - return empty status
		return status, nil
	}

	reStartFlag := regexp.MustCompile(`'START_FLAG'\s*:\s*(\d+)`)
	if m := reStartFlag.FindStringSubmatch(cdBlock); len(m) > 1 && m[1] != "0" {
		status.Inserted = true
	}

	reImgName := regexp.MustCompile(`'IMG_NAME'\s*:\s*'([^']*)'`)
	if m := reImgName.FindStringSubmatch(cdBlock); len(m) > 1 && m[1] != "" {
		status.Image = m[1]
	}

	// Also include IP_ADDR and SRC_PATH in the image description if present
	reIPAddr := regexp.MustCompile(`'IP_ADDR'\s*:\s*'([^']*)'`)
	reSrcPath := regexp.MustCompile(`'SRC_PATH'\s*:\s*'([^']*)'`)
	if status.Image != "" {
		ipAddr := ""
		srcPath := ""
		if m := reIPAddr.FindStringSubmatch(cdBlock); len(m) > 1 {
			ipAddr = m[1]
		}
		if m := reSrcPath.FindStringSubmatch(cdBlock); len(m) > 1 {
			srcPath = m[1]
		}
		if ipAddr != "" {
			status.Image = ipAddr + srcPath + status.Image
		}
	}

	return status, nil
}

func (b *MegaRACBoard) MountMedia(cfg *models.ServerConfig, creds *models.BMCCredentials, imageURL string) error {
	if creds == nil {
		return fmt.Errorf("no BMC credentials available")
	}

	ipAddr, srcPath, imgName, shrType, err := parseMegaRACMediaURL(imageURL)
	if err != nil {
		return err
	}

	baseURL := BMCBaseURL("ami_megarac", cfg.BMCIP, cfg.BMCPort)
	client := NewStatusHTTPClient(30*time.Second, tlsutil.SkipVerify(cfg))

	// Step 1: Set the media image configuration
	setParams := fmt.Sprintf(
		"MEDIA_TYPE=1&IMAGE_OPER=0&IMAGE_TYPE=CD/DVD&IMAGE_NAME=%s&IP_ADDR=%s&SRC_PATH=%s&SHR_TYPE=%d",
		url.QueryEscape(imgName),
		url.QueryEscape(ipAddr),
		url.QueryEscape(srcPath),
		shrType,
	)

	body, err := b.megaracDoRequest(client, "POST", baseURL+"/rpc/setmediaimage.asp", setParams, creds)
	if err != nil {
		return fmt.Errorf("failed to set media image: %w", err)
	}
	if err := megaracCheckHAPIStatus(body); err != nil {
		return fmt.Errorf("set media image failed: %w", err)
	}

	// Step 2: Start the redirection
	startParams := "MEDIA_TYPE=1&IMAGE_TYPE=CD/DVD&START_BIT=1"
	body, err = b.megaracDoRequest(client, "POST", baseURL+"/rpc/startredirection.asp", startParams, creds)
	if err != nil {
		return fmt.Errorf("failed to start redirection: %w", err)
	}
	if err := megaracCheckHAPIStatus(body); err != nil {
		return fmt.Errorf("start redirection failed: %w", err)
	}

	log.Printf("MegaRAC virtual media: mounted %s from %s%s (share type %d)", imgName, ipAddr, srcPath, shrType)
	return nil
}

func (b *MegaRACBoard) EjectMedia(cfg *models.ServerConfig, creds *models.BMCCredentials) error {
	if creds == nil {
		return fmt.Errorf("no BMC credentials available")
	}

	baseURL := BMCBaseURL("ami_megarac", cfg.BMCIP, cfg.BMCPort)
	client := NewStatusHTTPClient(30*time.Second, tlsutil.SkipVerify(cfg))

	// Stop the redirection
	stopParams := "MEDIA_TYPE=1&IMAGE_TYPE=CD/DVD&START_BIT=0"
	body, err := b.megaracDoRequest(client, "POST", baseURL+"/rpc/startredirection.asp", stopParams, creds)
	if err != nil {
		return fmt.Errorf("failed to stop redirection: %w", err)
	}
	if err := megaracCheckHAPIStatus(body); err != nil {
		return fmt.Errorf("stop redirection failed: %w", err)
	}

	log.Printf("MegaRAC virtual media: ejected CD/DVD")
	return nil
}
