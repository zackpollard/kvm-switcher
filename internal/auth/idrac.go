package auth

import (
	"crypto/tls"
	"net/http"
)

// Shared HTTP client for iDRAC (self-signed certs).
var idracHTTPClient = &http.Client{
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	},
}
