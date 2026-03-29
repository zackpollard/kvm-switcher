package tlsutil

import (
	"crypto/tls"

	"github.com/zackpollard/kvm-switcher/internal/models"
)

// SkipVerify returns the effective TLS skip-verify setting for a server.
// If the server's TLSSkipVerify field is nil (not set in config), it defaults
// to true because most BMCs use self-signed certificates.
func SkipVerify(cfg *models.ServerConfig) bool {
	if cfg.TLSSkipVerify == nil {
		return true // default: skip (BMCs use self-signed certs)
	}
	return *cfg.TLSSkipVerify
}

// ConfigForServer returns a *tls.Config with InsecureSkipVerify set according
// to the given flag.
func ConfigForServer(skipVerify bool) *tls.Config {
	return &tls.Config{InsecureSkipVerify: skipVerify}
}
