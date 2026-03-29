package auth

import (
	"crypto/tls"
	"net/http"
)

// Shared HTTP client for iDRAC (self-signed certs).
// TODO: Make TLS InsecureSkipVerify configurable per-server. Currently the
// Authenticator interface doesn't carry ServerConfig, so the per-server
// tls_skip_verify setting isn't available here. When the interface is
// extended to accept ServerConfig, use tlsutil.SkipVerify(cfg) instead.
var idracHTTPClient = &http.Client{
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	},
}
