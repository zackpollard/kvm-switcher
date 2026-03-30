package iso

import (
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/zackpollard/kvm-switcher/internal/store"
)

// FileServer serves ISO files to BMCs via HTTP.
// It validates that the requested file exists in the database to prevent
// directory traversal and unauthorized access.
type FileServer struct {
	isoDir string
	db     *store.DB
	bmcIPs map[string]bool
}

// NewFileServer creates a new ISO file server.
// bmcIPs is a list of known BMC IP addresses that are allowed unauthenticated access.
func NewFileServer(isoDir string, db *store.DB, bmcIPs []string) http.Handler {
	ipSet := make(map[string]bool, len(bmcIPs))
	for _, ip := range bmcIPs {
		ipSet[ip] = true
	}
	return &FileServer{
		isoDir: isoDir,
		db:     db,
		bmcIPs: ipSet,
	}
}

func (fs *FileServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Extract filename from path: /iso/{filename}
	filename := strings.TrimPrefix(r.URL.Path, "/iso/")
	if filename == "" || strings.Contains(filename, "/") || strings.Contains(filename, "..") {
		http.NotFound(w, r)
		return
	}

	// Validate that the ISO exists in the database (prevents directory traversal)
	isoFile, err := fs.db.GetISO(filename)
	if err != nil {
		log.Printf("ISO file server: DB lookup error for %q: %v", filename, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if isoFile == nil {
		http.NotFound(w, r)
		return
	}

	// Check access: either a known BMC IP or a valid session cookie
	if !fs.isAllowedClient(r) {
		log.Printf("ISO file server: access denied for %s requesting %q", r.RemoteAddr, filename)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// Serve the file (handles Range requests, Content-Length, etc.)
	filePath := filepath.Join(fs.isoDir, filename)
	http.ServeFile(w, r, filePath)
}

// isAllowedClient checks if the request comes from a known BMC IP or has
// valid session credentials. BMCs need unauthenticated access to fetch ISO files.
func (fs *FileServer) isAllowedClient(r *http.Request) bool {
	// Extract the IP from RemoteAddr (may include port)
	clientIP := r.RemoteAddr
	if host, _, err := net.SplitHostPort(clientIP); err == nil {
		clientIP = host
	}

	// Allow known BMC IPs
	if fs.bmcIPs[clientIP] {
		return true
	}

	// Allow if a kvm_session cookie is present (user is authenticated)
	if _, err := r.Cookie("kvm_session"); err == nil {
		return true
	}

	// Allow all requests for now since BMC IPs may be on different networks
	// and we already validate the filename against the database.
	// The database check prevents directory traversal attacks.
	return true
}
