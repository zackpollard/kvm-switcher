// Package main implements a mock BMC server that simulates enough of the
// AMI MegaRAC and Dell iDRAC8 APIs for E2E tests to run without real hardware.
//
// It listens on two ports:
//   - HTTP (default 9999) for MegaRAC-style BMCs
//   - HTTPS (default 9998) for iDRAC8-style BMCs (self-signed cert generated at startup)
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"strings"
	"time"
)

func main() {
	httpPort := flag.Int("port", 9999, "HTTP port (MegaRAC)")
	httpsPort := flag.Int("tls-port", 9998, "HTTPS port (iDRAC8)")
	flag.Parse()

	mux := http.NewServeMux()
	registerHandlers(mux)

	// Start HTTP listener (MegaRAC)
	httpAddr := fmt.Sprintf(":%d", *httpPort)
	go func() {
		log.Printf("Mock BMC HTTP listening on %s", httpAddr)
		if err := http.ListenAndServe(httpAddr, mux); err != nil {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	// Start HTTPS listener (iDRAC8) with self-signed cert
	tlsCert, err := generateSelfSignedCert()
	if err != nil {
		log.Fatalf("Failed to generate TLS cert: %v", err)
	}

	httpsAddr := fmt.Sprintf(":%d", *httpsPort)
	tlsServer := &http.Server{
		Addr:    httpsAddr,
		Handler: mux,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{tlsCert},
		},
	}

	log.Printf("Mock BMC HTTPS listening on %s", httpsAddr)
	log.Fatal(tlsServer.ListenAndServeTLS("", ""))
}

func registerHandlers(mux *http.ServeMux) {
	// ── MegaRAC endpoints ──────────────────────────────────────────────

	// Login: POST /rpc/WEBSES/create.asp
	// The Go parser uses regexes like 'SESSION_COOKIE'\s*:\s*'([^']+)'
	mux.HandleFunc("/rpc/WEBSES/create.asp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `HAPI_STATUS:0
{ 'SESSION_COOKIE' : 'mocksession456' , 'BMC_IP_ADDR' : '127.0.0.1' , 'CSRFTOKEN' : 'mockcsrf123' }
`)
	})

	// Role info: GET /rpc/getrole.asp
	// Parser expects 'CURUSERNAME' : 'admin', 'CURPRIV' : 4, 'EXTENDED_PRIV' : 259
	mux.HandleFunc("/rpc/getrole.asp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `HAPI_STATUS:0
{ 'CURUSERNAME' : 'admin' , 'CURPRIV' : 4 , 'EXTENDED_PRIV' : 259 }
`)
	})

	// CSRF token: GET /rpc/WEBSES/getcsrftoken.asp
	mux.HandleFunc("/rpc/WEBSES/getcsrftoken.asp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "HAPI_STATUS:0\nCSRFToken='mockcsrf123'\n")
	})

	// Logout: GET /rpc/WEBSES/logout.asp
	mux.HandleFunc("/rpc/WEBSES/logout.asp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "HAPI_STATUS:0\n")
	})

	// Host status: GET /rpc/hoststatus.asp
	// Parser: JF_STATE['\s]*:\s*(\d+)
	mux.HandleFunc("/rpc/hoststatus.asp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "HAPI_STATUS:0\n{ 'JF_STATE' : 1 }\n")
	})

	// FRU info: GET /rpc/getfruinfo.asp
	// Parser: PI_ProductName['\s]*:\s*'([^']*)'
	mux.HandleFunc("/rpc/getfruinfo.asp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `HAPI_STATUS:0
{ 'PI_ProductName' : 'Mock MegaRAC Server' , 'BI_BoardProductName' : 'X99E-ITX/ac' , 'BI_BoardMfr' : 'ASRock Rack' }
`)
	})

	// Sensor data: GET /rpc/getallsensors.asp
	// Parser: 'SensorName'\s*:\s*'CPU1 Temp'[^}]*'SensorReading'\s*:\s*(\d+)
	mux.HandleFunc("/rpc/getallsensors.asp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `HAPI_STATUS:0
{ 'SensorName' : 'CPU1 Temp' , 'SensorReading' : 42000 , 'SensorUnit' : 'degrees C' }
`)
	})

	// ── iDRAC8 endpoints ───────────────────────────────────────────────

	// Login: POST /data/login
	// Parser expects: cookie -http-session-, XML with <authResult>0</authResult>
	// and <forwardUrl>index.html?ST1=abc,ST2=def</forwardUrl>
	mux.HandleFunc("/data/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:  "-http-session-",
			Value: "mock-idrac-session-abc123",
			Path:  "/",
		})
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<root>
  <authResult>0</authResult>
  <forwardUrl>index.html?ST1=mockst1token,ST2=mockst2token</forwardUrl>
  <status>ok</status>
</root>`)
	})

	// Logout: POST/GET /data/logout
	mux.HandleFunc("/data/logout", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><root><status>ok</status></root>`)
	})

	// ── iDRAC8 Redfish endpoints (used for status polling with Basic Auth) ──

	// System info
	mux.HandleFunc("/redfish/v1/Systems/System.Embedded.1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"PowerState": "On",
			"Model":      "PowerEdge R730xd (Mock)",
			"Status": map[string]string{
				"HealthRollup": "OK",
			},
		})
	})

	// Power consumption
	mux.HandleFunc("/redfish/v1/Chassis/System.Embedded.1/Power", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"PowerControl": []map[string]any{
				{"PowerConsumedWatts": 185.0},
			},
		})
	})

	// Thermal (inlet temperature)
	mux.HandleFunc("/redfish/v1/Chassis/System.Embedded.1/Thermal", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"Temperatures": []map[string]any{
				{
					"Name":                   "System Board Inlet Temp",
					"ReadingCelsius":         23.0,
					"UpperThresholdCritical": 47.0,
				},
			},
		})
	})

	// ── Catch-all for proxied requests ─────────────────────────────────

	// Serve a simple page for any path not explicitly handled.
	// This ensures the BMC proxy has something to return for HTML page requests.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Log unhandled requests for debugging
		if r.URL.Path != "/" && !strings.HasPrefix(r.URL.Path, "/favicon") {
			log.Printf("Mock BMC: unhandled %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>Mock BMC</title></head>
<body>
<h1>Mock BMC Dashboard</h1>
<p>Path: %s</p>
</body>
</html>`, r.URL.Path)
	})
}

// generateSelfSignedCert creates an in-memory self-signed TLS certificate for testing.
func generateSelfSignedCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generating key: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Mock BMC"},
		},
		NotBefore: time.Now().Add(-1 * time.Hour),
		NotAfter:  time.Now().Add(24 * time.Hour),
		KeyUsage:  x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("creating certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("marshaling key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	return tls.X509KeyPair(certPEM, keyPEM)
}
