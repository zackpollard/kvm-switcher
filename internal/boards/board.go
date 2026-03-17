package boards

import (
	"net/http"

	"github.com/zackpollard/kvm-switcher/internal/models"
)

// BoardHandler defines the proxy behavior and status fetching for a BMC board type.
type BoardHandler interface {
	// Scheme returns the URL scheme ("http" or "https") for this board type.
	Scheme() string

	// LoginBypass checks if the given GET path should be redirected to the dashboard
	// when cached credentials exist. Returns the redirect URL, or "" if no bypass.
	LoginBypass(path string, creds *models.BMCCredentials) string

	// LoginIntercept checks if the request should be intercepted and answered with
	// cached credentials instead of being forwarded to the BMC. Returns true if handled.
	LoginIntercept(w http.ResponseWriter, r *http.Request, path string, creds *models.BMCCredentials) bool

	// InjectCredentials adds board-type-specific auth to an outgoing proxy request.
	InjectCredentials(req *http.Request, creds *models.BMCCredentials)

	// RewriteRequestURL modifies the proxy request URL (e.g. prepending session tokens).
	RewriteRequestURL(req *http.Request, creds *models.BMCCredentials)

	// ModifyProxyResponse applies board-type-specific response modifications.
	ModifyProxyResponse(resp *http.Response, creds *models.BMCCredentials)

	// RewriteLocationHeader adjusts the Location header after generic rewriting.
	// proxyPrefix is "/__bmc/{name}".
	RewriteLocationHeader(loc string, proxyPrefix string) string

	// FetchStatus fetches detailed device status using the given credentials.
	// cfg is needed for board types that use Basic Auth (e.g. iDRAC8).
	FetchStatus(cfg *models.ServerConfig, creds *models.BMCCredentials) *models.DeviceStatus

	// CookiesToStrip returns cookie names that should be stripped from client requests
	// before proxying (in addition to the default kvm_session, kvm_oauth_state).
	CookiesToStrip() []string
}

// registry maps board types to their handler implementations.
var registry = map[string]BoardHandler{}

// Register adds a board handler for a board type.
func Register(boardType string, handler BoardHandler) {
	registry[boardType] = handler
}

// Get returns the board handler for a given board type.
func Get(boardType string) (BoardHandler, bool) {
	h, ok := registry[boardType]
	return h, ok
}
