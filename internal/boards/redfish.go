package boards

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"time"
)

// Redfish response types shared by iDRAC8 and iDRAC9.

// RedfishSystemResponse represents the JSON from /redfish/v1/Systems/System.Embedded.1.
type RedfishSystemResponse struct {
	PowerState string `json:"PowerState"`
	Model      string `json:"Model"`
	Status     struct {
		HealthRollup string `json:"HealthRollup"`
	} `json:"Status"`
}

// RedfishPowerResponse represents the JSON from /redfish/v1/Chassis/.../Power.
type RedfishPowerResponse struct {
	PowerControl []struct {
		PowerConsumedWatts float64 `json:"PowerConsumedWatts"`
	} `json:"PowerControl"`
}

// RedfishThermalResponse represents the JSON from /redfish/v1/Chassis/.../Thermal.
type RedfishThermalResponse struct {
	Temperatures []struct {
		Name                   string  `json:"Name"`
		ReadingCelsius         float64 `json:"ReadingCelsius"`
		UpperThresholdCritical float64 `json:"UpperThresholdCritical"`
	} `json:"Temperatures"`
}

// BMCBaseURL returns the base URL for a BMC, handling default ports.
func BMCBaseURL(boardType, bmcIP string, bmcPort int) string {
	scheme := "http"
	if h, ok := Get(boardType); ok {
		scheme = h.Scheme()
	}
	if bmcPort == 0 {
		if scheme == "https" {
			bmcPort = 443
		} else {
			bmcPort = 80
		}
	}
	return fmt.Sprintf("%s://%s:%d", scheme, bmcIP, bmcPort)
}

// NewStatusHTTPClient creates an HTTP client with the given timeout and optional
// TLS InsecureSkipVerify for HTTPS BMCs.
func NewStatusHTTPClient(timeout time.Duration, insecureTLS bool) *http.Client {
	transport := &http.Transport{}
	if insecureTLS {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
}
