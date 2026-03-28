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
	_ "github.com/zackpollard/kvm-switcher/internal/auth"   // Register authenticators
	_ "github.com/zackpollard/kvm-switcher/internal/boards" // Register board handlers
	"github.com/zackpollard/kvm-switcher/internal/config"
	containermgr "github.com/zackpollard/kvm-switcher/internal/container"
	dockermgr "github.com/zackpollard/kvm-switcher/internal/docker"
	k8smgr "github.com/zackpollard/kvm-switcher/internal/kubernetes"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/zackpollard/kvm-switcher/internal/middleware"
	"github.com/zackpollard/kvm-switcher/internal/models"
	kvmoidc "github.com/zackpollard/kvm-switcher/internal/oidc"
	"github.com/zackpollard/kvm-switcher/internal/store"
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

	// Initialize container manager based on runtime.
	// When native_ikvm is enabled, Docker/K8s is optional since MegaRAC boards
	// use the native IVTP protocol instead of JViewer containers.
	var cm containermgr.Manager
	switch cfg.Settings.Runtime {
	case "kubernetes":
		log.Println("Using Kubernetes runtime")
		cm, err = k8smgr.NewManager(cfg.Settings.ContainerImage, cfg.Settings.KubeNamespace, cfg.Settings.KubeConfig)
	case "docker":
		log.Println("Using Docker runtime")
		cm, err = dockermgr.NewManager(cfg.Settings.ContainerImage)
	default:
		if !cfg.Settings.NativeIKVM {
			log.Fatalf("Unknown runtime: %s", cfg.Settings.Runtime)
		}
	}
	if err != nil {
		if cfg.Settings.NativeIKVM {
			log.Printf("Warning: container runtime unavailable (%v); MegaRAC boards will use native iKVM", err)
			cm = nil // Ensure cm is nil, not a nil-wrapped interface
		} else {
			log.Fatalf("Failed to initialize container runtime: %v", err)
		}
	}
	if cm != nil {
		defer cm.Close()
		// Clean up any orphaned containers from previous runs
		if err := cm.CleanupOrphans(context.Background()); err != nil {
			log.Printf("Warning: failed to cleanup orphans: %v", err)
		}
	}

	// Open SQLite database for audit logging and persistent sessions
	db, err := store.Open(cfg.Settings.DBPath)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close()
	log.Printf("Database opened at %s", cfg.Settings.DBPath)

	// Create API server with SQLite-backed session store
	sessionStore, err := store.NewSQLiteSessionStore(db)
	if err != nil {
		log.Fatalf("Failed to initialize session store: %v", err)
	}

	// Use DB as audit logger if audit logging is enabled
	var auditLogger models.AuditLogger
	if cfg.Settings.AuditLog != nil && *cfg.Settings.AuditLog {
		auditLogger = db
		log.Println("Audit logging enabled")
	}

	srv := api.NewServerWithStore(cfg, cm, sessionStore, auditLogger)

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

	// Prometheus metrics endpoint (unauthenticated, when enabled)
	if cfg.Settings.MetricsEnabled {
		mux.Handle("GET /metrics", promhttp.Handler())
		log.Println("Prometheus metrics enabled at /metrics")
	}

	// Health/readiness probes (unauthenticated)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		// Check DB is reachable
		if err := db.Ping(); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"status":"unavailable","reason":"database unreachable"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ready"}`))
	})

	// Rate limiter for mutation endpoints
	rateLimited := middleware.RateLimitMiddleware(cfg.Settings.RateLimitRPM)

	// Auth routes (always registered, but login redirects only work when OIDC is enabled)
	if oidcProvider != nil {
		mux.Handle("GET /auth/login", rateLimited(http.HandlerFunc(oidcProvider.HandleLogin)))
		mux.Handle("GET /auth/callback", rateLimited(http.HandlerFunc(oidcProvider.HandleCallback)))
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
	registerAPIRoutes := func(mux *http.ServeMux) {
		mux.HandleFunc("GET /api/servers", srv.ListServers)
		mux.Handle("POST /api/sessions", rateLimited(http.HandlerFunc(srv.CreateSession)))
		mux.HandleFunc("GET /api/sessions", srv.ListSessions)
		mux.HandleFunc("GET /api/sessions/{id}", srv.GetSession)
		mux.HandleFunc("PATCH /api/sessions/{id}/keepalive", srv.KeepAliveSession)
		mux.HandleFunc("DELETE /api/sessions/{id}", srv.DeleteSession)
		mux.Handle("POST /api/ipmi-session/{name}", rateLimited(http.HandlerFunc(srv.CreateIPMISession)))
		mux.HandleFunc("GET /api/server-status", srv.GetServerStatuses)
		mux.HandleFunc("GET /api/audit-log", srv.GetAuditLog)
		mux.HandleFunc("POST /api/sessions/{id}/power", srv.KVMPowerControl)
		mux.HandleFunc("POST /api/sessions/{id}/display-lock", srv.KVMDisplayLock)
		mux.HandleFunc("POST /api/sessions/{id}/reset-video", srv.KVMResetVideo)
		mux.HandleFunc("POST /api/sessions/{id}/mouse-mode", srv.KVMMouseMode)
		mux.HandleFunc("POST /api/sessions/{id}/keyboard-layout", srv.KVMKeyboardLayout)
		mux.HandleFunc("POST /api/sessions/{id}/ipmi", srv.KVMIPMICommand)
		mux.HandleFunc("GET /api/sessions/{id}/screenshot", srv.KVMScreenshot)
		mux.HandleFunc("/api/ws", srv.HandleNanoKVMWebSocket)
		mux.HandleFunc("/api/stream/h264", srv.HandleNanoKVMWebSocket)
		mux.HandleFunc("GET /ws/kvm/{id}", srv.HandleKVMWebSocket)
		mux.HandleFunc("/__bmc/", srv.HandleBMCProxy)
	}

	if oidcProvider != nil {
		apiMux := http.NewServeMux()
		registerAPIRoutes(apiMux)

		protected := oidcProvider.Middleware(apiMux)
		mux.Handle("/api/", protected)
		mux.Handle("/ws/", protected)
		mux.Handle("/__bmc/", protected)
	} else {
		registerAPIRoutes(mux)
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
	var handler http.Handler = mux
	if cfg.Settings.MetricsEnabled {
		handler = middleware.MetricsMiddleware()(handler)
	}
	handler = middleware.CORSMiddleware(cfg.Settings.CORSOrigins)(handler)

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
	go sessionCleanup(srv, &cfg.Settings)

	// Wait for shutdown signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down...")

	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Stop active sessions (skip disconnected/error sessions whose containers are already gone)
	for _, session := range srv.Sessions.List() {
		if session.ContainerID != "" && session.Status != models.SessionDisconnected && session.Status != models.SessionError {
			log.Printf("Stopping session %s container...", session.ID)
			if err := cm.StopContainer(ctx, session.ContainerID); err != nil {
				log.Printf("Warning: failed to clean up session %s: %v", session.ID, err)
			}
			session.ContainerID = ""
			session.Status = models.SessionDisconnected
			srv.Sessions.Set(session)
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
			// Service Worker script must never be cached by the browser
			// so updates are detected immediately.
			if r.URL.Path == "/sw.js" {
				w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
				w.Header().Set("Service-Worker-Allowed", "/")
			}
			fs.ServeHTTP(w, r)
			return
		}
		// Fall back to index.html for SPA routes
		http.ServeFile(w, r, dir+"/index.html")
	})
}

// sessionCleanup periodically checks for idle sessions, cleans them up,
// and removes stale BMC credentials.
func sessionCleanup(srv *api.Server, cfg *models.Settings) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		threshold := time.Now().Add(-time.Duration(cfg.IdleTimeoutMinutes) * time.Minute)

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

		// Clean up stale BMC credentials
		srv.CleanupStaleBMCCreds(cfg.BMCCredsTTLMinutes)
	}
}
