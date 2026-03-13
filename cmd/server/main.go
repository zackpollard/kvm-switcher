package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zackpollard/kvm-switcher/internal/api"
	_ "github.com/zackpollard/kvm-switcher/internal/auth" // Register authenticators
	"github.com/zackpollard/kvm-switcher/internal/config"
	containermgr "github.com/zackpollard/kvm-switcher/internal/container"
	dockermgr "github.com/zackpollard/kvm-switcher/internal/docker"
	k8smgr "github.com/zackpollard/kvm-switcher/internal/kubernetes"
	"github.com/zackpollard/kvm-switcher/internal/models"
	kvmoidc "github.com/zackpollard/kvm-switcher/internal/oidc"
)

func main() {
	configPath := flag.String("config", "configs/servers.yaml", "Path to configuration file")
	webDir := flag.String("web", "web/build", "Path to frontend static files")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("KVM Switcher starting...")

	// Load configuration
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	log.Printf("Loaded %d server(s) from config", len(cfg.Servers))

	// Initialize container manager based on runtime
	var cm containermgr.Manager
	switch cfg.Settings.Runtime {
	case "kubernetes":
		log.Println("Using Kubernetes runtime")
		cm, err = k8smgr.NewManager(cfg.Settings.ContainerImage, cfg.Settings.KubeNamespace, cfg.Settings.KubeConfig)
	case "docker":
		log.Println("Using Docker runtime")
		cm, err = dockermgr.NewManager(cfg.Settings.ContainerImage)
	default:
		log.Fatalf("Unknown runtime: %s", cfg.Settings.Runtime)
	}
	if err != nil {
		log.Fatalf("Failed to initialize container runtime: %v", err)
	}
	defer cm.Close()

	// Clean up any orphaned containers from previous runs
	if err := cm.CleanupOrphans(context.Background()); err != nil {
		log.Printf("Warning: failed to cleanup orphans: %v", err)
	}

	// Create API server
	srv := api.NewServer(cfg, cm)

	// Start IPMI proxy listeners (one per server, zero content rewriting)
	ipmiProxies, err := api.NewIPMIProxyManager(cfg)
	if err != nil {
		log.Fatalf("Failed to start IPMI proxies: %v", err)
	}
	defer ipmiProxies.Close()
	srv.IPMIProxies = ipmiProxies

	// Set up OIDC provider if enabled
	var oidcProvider *kvmoidc.Provider
	if cfg.OIDC.Enabled {
		log.Println("OIDC authentication enabled")
		oidcProvider, err = kvmoidc.NewProvider(context.Background(), &cfg.OIDC)
		if err != nil {
			log.Fatalf("Failed to initialize OIDC provider: %v", err)
		}
		log.Printf("OIDC configured with issuer: %s", cfg.OIDC.IssuerURL)
	}

	// Set up routes
	mux := http.NewServeMux()

	// Auth routes (always registered, but login redirects only work when OIDC is enabled)
	if oidcProvider != nil {
		mux.HandleFunc("GET /auth/login", oidcProvider.HandleLogin)
		mux.HandleFunc("GET /auth/callback", oidcProvider.HandleCallback)
		mux.HandleFunc("GET /auth/logout", oidcProvider.HandleLogout)
	}
	// /auth/me is always available - returns auth status
	if oidcProvider != nil {
		mux.HandleFunc("GET /auth/me", oidcProvider.HandleMe)
	} else {
		mux.HandleFunc("GET /auth/me", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"authenticated":false,"oidc_enabled":false}`))
		})
	}

	// API routes - wrap with OIDC middleware if enabled
	if oidcProvider != nil {
		apiMux := http.NewServeMux()
		apiMux.HandleFunc("GET /api/servers", srv.ListServers)
		apiMux.HandleFunc("POST /api/sessions", srv.CreateSession)
		apiMux.HandleFunc("GET /api/sessions", srv.ListSessions)
		apiMux.HandleFunc("GET /api/sessions/{id}", srv.GetSession)
		apiMux.HandleFunc("DELETE /api/sessions/{id}", srv.DeleteSession)
		apiMux.HandleFunc("GET /api/ipmi-ports", srv.IPMIPorts)
		apiMux.HandleFunc("GET /ws/kvm/{id}", srv.HandleKVMWebSocket)

		protected := oidcProvider.Middleware(apiMux)
		mux.Handle("/api/", protected)
		mux.Handle("/ws/", protected)
	} else {
		mux.HandleFunc("GET /api/servers", srv.ListServers)
		mux.HandleFunc("POST /api/sessions", srv.CreateSession)
		mux.HandleFunc("GET /api/sessions", srv.ListSessions)
		mux.HandleFunc("GET /api/sessions/{id}", srv.GetSession)
		mux.HandleFunc("DELETE /api/sessions/{id}", srv.DeleteSession)
		mux.HandleFunc("GET /api/ipmi-ports", srv.IPMIPorts)
		mux.HandleFunc("GET /ws/kvm/{id}", srv.HandleKVMWebSocket)
	}

	// Serve frontend static files
	if _, err := os.Stat(*webDir); err == nil {
		log.Printf("Serving frontend from %s", *webDir)
		fs := http.FileServer(http.Dir(*webDir))
		mux.Handle("/", spaHandler(fs, *webDir))
	} else {
		log.Printf("Frontend directory %s not found, serving API only", *webDir)
		mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"service":"kvm-switcher","status":"running"}`))
		})
	}

	// Add CORS middleware
	handler := corsMiddleware(mux)

	// Create HTTP server
	httpServer := &http.Server{
		Addr:         cfg.Settings.ListenAddress,
		Handler:      handler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0, // No timeout for WebSocket connections
		IdleTimeout:  60 * time.Second,
	}

	// Start server
	go func() {
		log.Printf("Listening on %s", cfg.Settings.ListenAddress)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Start session cleanup goroutine
	go sessionCleanup(srv, cfg.Settings.IdleTimeoutMinutes)

	// Wait for shutdown signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down...")

	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Stop all active sessions
	for _, session := range srv.Sessions.List() {
		if session.ContainerID != "" {
			log.Printf("Stopping session %s container...", session.ID)
			_ = cm.StopContainer(ctx, session.ContainerID)
		}
	}

	if err := httpServer.Shutdown(ctx); err != nil {
		log.Fatalf("Server shutdown error: %v", err)
	}

	log.Println("KVM Switcher stopped.")
}

// spaHandler serves static files and falls back to index.html for SPA routing.
func spaHandler(fs http.Handler, dir string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Try to serve the file directly
		path := dir + r.URL.Path
		if _, err := os.Stat(path); err == nil {
			fs.ServeHTTP(w, r)
			return
		}
		// Fall back to index.html for SPA routes
		http.ServeFile(w, r, dir+"/index.html")
	})
}

// corsMiddleware adds CORS headers for development.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// sessionCleanup periodically checks for idle sessions and cleans them up.
func sessionCleanup(srv *api.Server, idleTimeoutMinutes int) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		threshold := time.Now().Add(-time.Duration(idleTimeoutMinutes) * time.Minute)

		for _, session := range srv.Sessions.List() {
			if session.Status == models.SessionConnected && session.LastActivity.Before(threshold) {
				log.Printf("Session %s: idle timeout, cleaning up", session.ID)
				if session.ContainerID != "" {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					_ = srv.Container.StopContainer(ctx, session.ContainerID)
					cancel()
				}
				session.Status = models.SessionDisconnected
				srv.Sessions.Set(session)
			}
		}
	}
}
