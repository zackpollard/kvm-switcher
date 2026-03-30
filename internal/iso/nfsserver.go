package iso

import (
	"fmt"
	"log"
	"net"
	"os"

	nfs "github.com/willscott/go-nfs"
	nfshelper "github.com/willscott/go-nfs/helpers"

	"github.com/go-git/go-billy/v5/osfs"
)

// NFSServer wraps a go-nfs listener serving ISO files read-only.
type NFSServer struct {
	listener net.Listener
	isoDir   string
}

// StartNFSServer creates and starts an NFS v3 server serving files from isoDir.
func StartNFSServer(isoDir string, port int) (*NFSServer, error) {
	// Verify directory exists
	info, err := os.Stat(isoDir)
	if err != nil {
		return nil, fmt.Errorf("ISO directory %q: %w", isoDir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("ISO path %q is not a directory", isoDir)
	}

	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, fmt.Errorf("failed to listen on port %d: %w", port, err)
	}

	// Create a read-only billy filesystem backed by the ISO directory
	bfs := osfs.New(isoDir)
	handler := nfshelper.NewNullAuthHandler(bfs)
	cacheHelper := nfshelper.NewCachingHandler(handler, 1024)

	log.Printf("NFS server starting on port %d, serving %s", port, isoDir)

	go func() {
		if err := nfs.Serve(listener, cacheHelper); err != nil {
			// Don't log if the listener was closed intentionally
			if !isClosedError(err) {
				log.Printf("NFS server error: %v", err)
			}
		}
	}()

	return &NFSServer{
		listener: listener,
		isoDir:   isoDir,
	}, nil
}

// Shutdown gracefully stops the NFS server.
func (s *NFSServer) Shutdown() error {
	log.Println("NFS server shutting down")
	return s.listener.Close()
}

// isClosedError checks if the error is due to a closed listener.
func isClosedError(err error) bool {
	if err == nil {
		return false
	}
	return err.Error() == "accept tcp: use of closed network connection" ||
		err.Error() == "use of closed network connection"
}
