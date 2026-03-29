package oidc

import "github.com/zackpollard/kvm-switcher/internal/models"

// NewTestProvider creates a Provider with pre-loaded sessions for use in
// cross-package integration tests. It bypasses real OIDC discovery.
func NewTestProvider(cfg *models.OIDCConfig, sessions map[string]*models.UserSession) *Provider {
	return &Provider{
		config:   cfg,
		sessions: sessions,
	}
}
