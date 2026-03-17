package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/zackpollard/kvm-switcher/internal/boards"
	_ "github.com/zackpollard/kvm-switcher/internal/boards" // Register board handlers
	"github.com/zackpollard/kvm-switcher/internal/models"
)

// ---------------------------------------------------------------------------
// StatusCache tests
// ---------------------------------------------------------------------------

func TestStatusCache_SetAndGet(t *testing.T) {
	cache := NewStatusCache()
	want := &DeviceStatus{
		Online:       true,
		PowerState:   "on",
		Model:        "TestModel",
		Health:       "ok",
		LoadWatts:    120.5,
		TemperatureC: 42.0,
		AppVersion:   "v1.0",
		ImageVersion: "v2.0",
		UpdateAvail:  true,
	}
	cache.Set("server1", want)

	got, ok := cache.Get("server1")
	if !ok {
		t.Fatal("expected ok=true for existing key")
	}
	if got.Online != want.Online {
		t.Errorf("Online = %v, want %v", got.Online, want.Online)
	}
	if got.PowerState != want.PowerState {
		t.Errorf("PowerState = %q, want %q", got.PowerState, want.PowerState)
	}
	if got.Model != want.Model {
		t.Errorf("Model = %q, want %q", got.Model, want.Model)
	}
	if got.Health != want.Health {
		t.Errorf("Health = %q, want %q", got.Health, want.Health)
	}
	if got.LoadWatts != want.LoadWatts {
		t.Errorf("LoadWatts = %v, want %v", got.LoadWatts, want.LoadWatts)
	}
	if got.TemperatureC != want.TemperatureC {
		t.Errorf("TemperatureC = %v, want %v", got.TemperatureC, want.TemperatureC)
	}
	if got.AppVersion != want.AppVersion {
		t.Errorf("AppVersion = %q, want %q", got.AppVersion, want.AppVersion)
	}
	if got.ImageVersion != want.ImageVersion {
		t.Errorf("ImageVersion = %q, want %q", got.ImageVersion, want.ImageVersion)
	}
	if got.UpdateAvail != want.UpdateAvail {
		t.Errorf("UpdateAvail = %v, want %v", got.UpdateAvail, want.UpdateAvail)
	}
}

func TestStatusCache_GetMissing(t *testing.T) {
	cache := NewStatusCache()

	got, ok := cache.Get("nonexistent")
	if ok {
		t.Error("expected ok=false for missing key")
	}
	if got != nil {
		t.Errorf("expected nil status, got %+v", got)
	}
}

func TestStatusCache_GetAll_ReturnsCopy(t *testing.T) {
	cache := NewStatusCache()
	cache.Set("a", &DeviceStatus{Online: true, Model: "Original"})
	cache.Set("b", &DeviceStatus{Online: false, Model: "Other"})

	all := cache.GetAll()

	// Verify correct number of entries
	if len(all) != 2 {
		t.Fatalf("GetAll returned %d entries, want 2", len(all))
	}

	// Modify the returned map and values
	all["a"].Model = "Modified"
	delete(all, "b")

	// Original cache should be unchanged
	orig, ok := cache.Get("a")
	if !ok {
		t.Fatal("key 'a' should still exist in cache")
	}
	if orig.Model != "Original" {
		t.Errorf("original Model = %q, want %q (mutation leaked)", orig.Model, "Original")
	}

	_, ok = cache.Get("b")
	if !ok {
		t.Error("key 'b' should still exist in cache after deleting from copy")
	}
}

func TestStatusCache_Overwrite(t *testing.T) {
	cache := NewStatusCache()
	cache.Set("srv", &DeviceStatus{Online: true, PowerState: "on", Model: "First"})
	cache.Set("srv", &DeviceStatus{Online: false, PowerState: "off", Model: "Second"})

	got, ok := cache.Get("srv")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got.Online != false {
		t.Errorf("Online = %v, want false", got.Online)
	}
	if got.PowerState != "off" {
		t.Errorf("PowerState = %q, want %q", got.PowerState, "off")
	}
	if got.Model != "Second" {
		t.Errorf("Model = %q, want %q", got.Model, "Second")
	}
}

// ---------------------------------------------------------------------------
// bmcBaseURL tests
// ---------------------------------------------------------------------------

func TestBmcBaseURL_DefaultPorts(t *testing.T) {
	// HTTP board types get port 80 when bmcPort=0
	got := bmcBaseURL("ami_megarac", "10.0.0.1", 0)
	want := "http://10.0.0.1:80"
	if got != want {
		t.Errorf("ami_megarac port=0: got %q, want %q", got, want)
	}

	// HTTPS board types get port 443 when bmcPort=0
	got = bmcBaseURL("dell_idrac9", "10.0.0.2", 0)
	want = "https://10.0.0.2:443"
	if got != want {
		t.Errorf("dell_idrac9 port=0: got %q, want %q", got, want)
	}

	got = bmcBaseURL("dell_idrac8", "10.0.0.3", 0)
	want = "https://10.0.0.3:443"
	if got != want {
		t.Errorf("dell_idrac8 port=0: got %q, want %q", got, want)
	}
}

func TestBmcBaseURL_CustomPort(t *testing.T) {
	got := bmcBaseURL("ami_megarac", "10.0.0.1", 8080)
	want := "http://10.0.0.1:8080"
	if got != want {
		t.Errorf("custom port: got %q, want %q", got, want)
	}

	got = bmcBaseURL("dell_idrac9", "10.0.0.2", 9443)
	want = "https://10.0.0.2:9443"
	if got != want {
		t.Errorf("custom port: got %q, want %q", got, want)
	}
}

func TestBmcBaseURL_Schemes(t *testing.T) {
	tests := []struct {
		boardType  string
		wantScheme string
	}{
		{"dell_idrac8", "https"},
		{"dell_idrac9", "https"},
		{"ami_megarac", "http"},
		{"nanokvm", "http"},
		{"apc_ups", "http"},
	}
	for _, tt := range tests {
		t.Run(tt.boardType, func(t *testing.T) {
			got := bmcBaseURL(tt.boardType, "1.2.3.4", 1234)
			wantPrefix := tt.wantScheme + "://"
			if got[:len(wantPrefix)] != wantPrefix {
				t.Errorf("bmcBaseURL(%q) = %q, want scheme %q", tt.boardType, got, tt.wantScheme)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// inferContentType tests
// ---------------------------------------------------------------------------

func TestInferContentType(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/page/login.html", "text/html"},
		{"/page/status.htm", "text/html"},
		{"/js/app.js", "application/javascript"},
		{"/js/app.jsesp", "application/javascript"},
		{"/css/style.css", "text/css"},
		{"/data/config.json", "application/json"},
		{"/images/logo.png", "image/png"},
		{"/images/spinner.gif", "image/gif"},
		{"/images/photo.jpg", "image/jpeg"},
		{"/images/photo.jpeg", "image/jpeg"},
		{"/icons/icon.svg", "image/svg+xml"},
		{"/data/config.xml", "text/xml"},
		{"/unknown/file.unknown", "text/html"},
		{"/no-extension", "text/html"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := inferContentType(tt.path)
			if got != tt.want {
				t.Errorf("inferContentType(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// filterCookies tests
// ---------------------------------------------------------------------------

func TestFilterCookies_RemovesNamed(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "A", Value: "1"})
	req.AddCookie(&http.Cookie{Name: "B", Value: "2"})
	req.AddCookie(&http.Cookie{Name: "C", Value: "3"})
	req.AddCookie(&http.Cookie{Name: "D", Value: "4"})

	filterCookies(req, "B", "D")

	cookies := req.Cookies()
	names := make(map[string]bool)
	for _, c := range cookies {
		names[c.Name] = true
	}

	if names["B"] {
		t.Error("cookie B should have been removed")
	}
	if names["D"] {
		t.Error("cookie D should have been removed")
	}
	if !names["A"] {
		t.Error("cookie A should remain")
	}
	if !names["C"] {
		t.Error("cookie C should remain")
	}
	if len(cookies) != 2 {
		t.Errorf("expected 2 remaining cookies, got %d", len(cookies))
	}
}

func TestFilterCookies_NoCookies(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)

	// Should not panic
	filterCookies(req, "A", "B")

	cookies := req.Cookies()
	if len(cookies) != 0 {
		t.Errorf("expected 0 cookies, got %d", len(cookies))
	}
}

func TestFilterCookies_AllFiltered(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "X", Value: "1"})
	req.AddCookie(&http.Cookie{Name: "Y", Value: "2"})

	filterCookies(req, "X", "Y")

	cookies := req.Cookies()
	if len(cookies) != 0 {
		t.Errorf("expected 0 cookies after filtering all, got %d", len(cookies))
	}
	if req.Header.Get("Cookie") != "" {
		t.Errorf("Cookie header should be empty, got %q", req.Header.Get("Cookie"))
	}
}

// ---------------------------------------------------------------------------
// fetchMegaRACStatus tests (mock HTTP server)
// ---------------------------------------------------------------------------

func newMockMegaRAC(t *testing.T, powerState string, model string, cpuTempMillideg string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("GET /rpc/hoststatus.asp", func(w http.ResponseWriter, r *http.Request) {
		// Verify SessionCookie is present
		if c, err := r.Cookie("SessionCookie"); err != nil || c.Value == "" {
			t.Error("expected SessionCookie on hoststatus request")
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, "'JF_STATE' : %s,\n'JF_STATUS' : 0", powerState)
	})

	mux.HandleFunc("GET /rpc/getfruinfo.asp", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("SessionCookie"); err != nil || c.Value == "" {
			t.Error("expected SessionCookie on getfruinfo request")
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, "'PI_ProductName' : '%s',\n'BI_BoardProductName' : 'Board'", model)
	})

	mux.HandleFunc("GET /rpc/getallsensors.asp", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("SessionCookie"); err != nil || c.Value == "" {
			t.Error("expected SessionCookie on getallsensors request")
		}
		w.Header().Set("Content-Type", "text/html")
		if cpuTempMillideg != "" {
			fmt.Fprintf(w, "{ 'SensorName' : 'CPU1 Temp', 'SensorReading' : %s, 'SensorUnit' : 'degree C' }", cpuTempMillideg)
		} else {
			fmt.Fprint(w, "{ 'SensorName' : 'Fan1', 'SensorReading' : 3000 }")
		}
	})

	return httptest.NewServer(mux)
}

func fetchBoardStatus(t *testing.T, boardType string, host string, port int, creds *models.BMCCredentials) *DeviceStatus {
	t.Helper()
	handler, ok := boards.Get(boardType)
	if !ok {
		t.Fatalf("no board handler for %q", boardType)
	}
	cfg := &models.ServerConfig{BMCIP: host, BMCPort: port, BoardType: boardType}
	return handler.FetchStatus(cfg, creds)
}

func TestFetchMegaRACStatus_PowerOn(t *testing.T) {
	srv := newMockMegaRAC(t, "1", "SuperServer X11", "36000")
	defer srv.Close()

	host, port := parseHostPort(t, srv.URL)
	creds := &models.BMCCredentials{
		SessionCookie: "test-session",
		CSRFToken:     "test-csrf",
	}

	status := fetchBoardStatus(t, "ami_megarac", host, port, creds)

	if !status.Online {
		t.Error("expected Online=true")
	}
	if status.PowerState != "on" {
		t.Errorf("PowerState = %q, want %q", status.PowerState, "on")
	}
	if status.Model != "SuperServer X11" {
		t.Errorf("Model = %q, want %q", status.Model, "SuperServer X11")
	}
	if status.TemperatureC != 36.0 {
		t.Errorf("TemperatureC = %v, want 36.0", status.TemperatureC)
	}
}

func TestFetchMegaRACStatus_PowerOff(t *testing.T) {
	srv := newMockMegaRAC(t, "0", "", "")
	defer srv.Close()

	host, port := parseHostPort(t, srv.URL)
	creds := &models.BMCCredentials{
		SessionCookie: "test-session",
		CSRFToken:     "test-csrf",
	}

	status := fetchBoardStatus(t, "ami_megarac", host, port, creds)

	if !status.Online {
		t.Error("expected Online=true")
	}
	if status.PowerState != "off" {
		t.Errorf("PowerState = %q, want %q", status.PowerState, "off")
	}
}

func TestFetchMegaRACStatus_Unreachable(t *testing.T) {
	creds := &models.BMCCredentials{
		SessionCookie: "test-session",
		CSRFToken:     "test-csrf",
	}

	// Use an address that will immediately refuse connection
	status := fetchBoardStatus(t, "ami_megarac", "127.0.0.1", 1, creds)

	if status.Online {
		t.Error("expected Online=false for unreachable host")
	}
}

// ---------------------------------------------------------------------------
// fetchNanoKVMStatus tests (mock HTTP server)
// ---------------------------------------------------------------------------

func newMockNanoKVM(t *testing.T, appVersion, imageVersion string, pwrState bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/vm/info", func(w http.ResponseWriter, r *http.Request) {
		// Verify nano-kvm-token cookie is present
		if c, err := r.Cookie("nano-kvm-token"); err != nil || c.Value == "" {
			t.Error("expected nano-kvm-token cookie on /api/vm/info request")
		}
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]interface{}{
			"code": 0,
			"data": map[string]interface{}{
				"application": appVersion,
				"image":       imageVersion,
				"mdns":        "nanokvm",
			},
		}
		json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("GET /api/vm/gpio", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("nano-kvm-token"); err != nil || c.Value == "" {
			t.Error("expected nano-kvm-token cookie on /api/vm/gpio request")
		}
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]interface{}{
			"code": 0,
			"data": map[string]interface{}{
				"pwr": pwrState,
			},
		}
		json.NewEncoder(w).Encode(resp)
	})

	return httptest.NewServer(mux)
}

func TestFetchNanoKVMStatus_Success(t *testing.T) {
	srv := newMockNanoKVM(t, "2.3.6", "v1.4.0", true)
	defer srv.Close()

	host, port := parseHostPort(t, srv.URL)
	creds := &models.BMCCredentials{
		SessionCookie: "test-nano-token",
	}

	status := fetchBoardStatus(t, "nanokvm", host, port, creds)

	if !status.Online {
		t.Error("expected Online=true")
	}
	if status.Model != "NanoKVM" {
		t.Errorf("Model = %q, want %q", status.Model, "NanoKVM")
	}
	if status.AppVersion != "v2.3.6" {
		t.Errorf("AppVersion = %q, want %q", status.AppVersion, "v2.3.6")
	}
	if status.ImageVersion != "v1.4.0" {
		t.Errorf("ImageVersion = %q, want %q", status.ImageVersion, "v1.4.0")
	}
	if status.PowerState != "on" {
		t.Errorf("PowerState = %q, want %q", status.PowerState, "on")
	}
}

func TestFetchNanoKVMStatus_PowerOff(t *testing.T) {
	srv := newMockNanoKVM(t, "2.3.6", "v1.4.0", false)
	defer srv.Close()

	host, port := parseHostPort(t, srv.URL)
	creds := &models.BMCCredentials{
		SessionCookie: "test-nano-token",
	}

	status := fetchBoardStatus(t, "nanokvm", host, port, creds)

	if !status.Online {
		t.Error("expected Online=true")
	}
	if status.PowerState != "off" {
		t.Errorf("PowerState = %q, want %q", status.PowerState, "off")
	}
}

// ---------------------------------------------------------------------------
// fetchAPCStatus tests (mock HTTP server)
// ---------------------------------------------------------------------------

func newMockAPCPDU(t *testing.T, nmcPath string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	// UPS endpoint returns something that does not look like a UPS
	mux.HandleFunc("GET "+nmcPath+"/upstat.htm", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		// Return a page without UPS markers so the PDU path is taken
		fmt.Fprint(w, `<html><body>Not a UPS</body></html>`)
	})

	mux.HandleFunc("GET "+nmcPath+"/home.htm", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body>
			<div class="dataLabel">Device Load</div>
			<div class="dataValue"> 1.14 kW</div>
			<img alt="Current Load Value is 5.9 A" src="gauge.png">
		</body></html>`)
	})

	mux.HandleFunc("GET "+nmcPath+"/phstat.htm", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body>Phase L1 Voltage: 230.5 V nominal</body></html>`)
	})

	mux.HandleFunc("GET "+nmcPath+"/aboutpdu.htm", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body>
			<div class="dataLabel">Model Number</div>
			<div class="dataValue"> AP8681 </div>
		</body></html>`)
	})

	return httptest.NewServer(mux)
}

func TestFetchAPCStatus_PDU(t *testing.T) {
	nmcPath := "/nmc"
	srv := newMockAPCPDU(t, nmcPath)
	defer srv.Close()

	host, port := parseHostPort(t, srv.URL)
	creds := &models.BMCCredentials{
		Extra: map[string]string{"nmc_path": nmcPath},
	}

	status := fetchBoardStatus(t, "apc_ups", host, port, creds)

	if !status.Online {
		t.Error("expected Online=true")
	}
	if status.PowerState != "on" {
		t.Errorf("PowerState = %q, want %q", status.PowerState, "on")
	}
	if status.Model != "AP8681" {
		t.Errorf("Model = %q, want %q", status.Model, "AP8681")
	}
	if status.LoadWatts != 1140.0 {
		t.Errorf("LoadWatts = %v, want 1140.0", status.LoadWatts)
	}
	if status.LoadAmps != 5.9 {
		t.Errorf("LoadAmps = %v, want 5.9", status.LoadAmps)
	}
	if status.Voltage != 230.5 {
		t.Errorf("Voltage = %v, want 230.5", status.Voltage)
	}
}

func newMockAPCUPS(t *testing.T, nmcPath string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("GET "+nmcPath+"/upstat.htm", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body>
			<div class="dataLabel"><span>Runtime Remaining</span></div>
			<div class="dataValue"> 18 min</div>
			<div class="dataLabel"><span id="langCapacity">Capacity</span></div>
			<div class="dataValue"> 100.0&nbsp;%</div>
			<div class="dataLabel"><span>Load Power</span></div>
			<div class="dataValue"> 23.0&nbsp;%</div>
			<div class="dataLabel"><span>Load Current</span></div>
			<div class="dataValue"> 5.36&nbsp;Amps</div>
			<div class="dataLabel"><span>Internal Temperature</span></div>
			<div class="dataValue"> 22.9&deg;C</div>
			<div class="dataLabel"><span>Input Voltage</span></div>
			<th><td> 216.0 VAC</td></th>
		</body></html>`)
	})

	mux.HandleFunc("GET "+nmcPath+"/upabout.htm", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body>
			<div class="dataLabel"><span id="langModel">Model</span></div>
			<div class="dataValue"> Smart-UPS 3000 </div>
		</body></html>`)
	})

	return httptest.NewServer(mux)
}

func TestFetchAPCStatus_UPS(t *testing.T) {
	nmcPath := "/nmc"
	srv := newMockAPCUPS(t, nmcPath)
	defer srv.Close()

	host, port := parseHostPort(t, srv.URL)
	creds := &models.BMCCredentials{
		Extra: map[string]string{"nmc_path": nmcPath},
	}

	status := fetchBoardStatus(t, "apc_ups", host, port, creds)

	if !status.Online {
		t.Error("expected Online=true")
	}
	if status.PowerState != "on" {
		t.Errorf("PowerState = %q, want %q", status.PowerState, "on")
	}
	if status.Model != "Smart-UPS 3000" {
		t.Errorf("Model = %q, want %q", status.Model, "Smart-UPS 3000")
	}
	if status.RuntimeMin != 18.0 {
		t.Errorf("RuntimeMin = %v, want 18.0", status.RuntimeMin)
	}
	if status.BatteryPct != 100.0 {
		t.Errorf("BatteryPct = %v, want 100.0", status.BatteryPct)
	}
	if status.LoadPct != 23.0 {
		t.Errorf("LoadPct = %v, want 23.0", status.LoadPct)
	}
	if status.LoadAmps != 5.36 {
		t.Errorf("LoadAmps = %v, want 5.36", status.LoadAmps)
	}
	if status.TemperatureC != 22.9 {
		t.Errorf("TemperatureC = %v, want 22.9", status.TemperatureC)
	}
	if status.Voltage != 216.0 {
		t.Errorf("Voltage = %v, want 216.0", status.Voltage)
	}
}

// ---------------------------------------------------------------------------
// Helper: parse host and port from a test server URL
// ---------------------------------------------------------------------------

func parseHostPort(t *testing.T, rawURL string) (string, int) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("failed to parse URL %q: %v", rawURL, err)
	}
	host := u.Hostname()
	portStr := u.Port()
	if portStr == "" {
		if u.Scheme == "https" {
			return host, 443
		}
		return host, 80
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("failed to parse port from %q: %v", rawURL, err)
	}
	return host, port
}
