package auth

import (
	"context"

	"github.com/zackpollard/kvm-switcher/internal/models"
)

// BMCAuthenticator defines the interface for authenticating with different BMC types.
type BMCAuthenticator interface {
	// Authenticate logs in to the BMC and retrieves a KVM session with tokens.
	Authenticate(ctx context.Context, host string, port int, username, password string) (*models.BMCCredentials, *models.JViewerArgs, error)

	// CreateWebSession creates a BMC web session (login only, no KVM/JNLP).
	// Returns credentials with SessionCookie and CSRFToken for web UI access.
	CreateWebSession(ctx context.Context, host string, port int, username, password string) (*models.BMCCredentials, error)

	// Logout ends the BMC session.
	Logout(ctx context.Context, host string, port int, creds *models.BMCCredentials) error
}

// Registry maps board types to their authenticator implementations.
var Registry = map[string]BMCAuthenticator{}

// Register adds an authenticator for a board type.
func Register(boardType string, auth BMCAuthenticator) {
	Registry[boardType] = auth
}

// Get returns the authenticator for a given board type.
func Get(boardType string) (BMCAuthenticator, bool) {
	auth, ok := Registry[boardType]
	return auth, ok
}
