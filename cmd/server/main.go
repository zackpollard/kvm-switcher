package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/zackpollard/kvm-switcher/internal/api"
	_ "github.com/zackpollard/kvm-switcher/internal/auth"   // Register authenticators
	_ "github.com/zackpollard/kvm-switcher/internal/boards" // Register board handlers
	"github.com/zackpollard/kvm-switcher/internal/config"
	"github.com/zackpollard/kvm-switcher/internal/iso"
	"github.com/zackpollard/kvm-switcher/internal/middleware"
	"github.com/zackpollard/kvm-switcher/internal/models"
	kvmoidc "github.com/zackpollard/kvm-switcher/internal/oidc"
	"github.com/zackpollard/kvm-switcher/internal/store"
)

// @title KVM Switcher API
// @version 1.0
// @description Web-based KVM panel for managing remote server consoles via BMC/IPMI. Supports AMI MegaRAC (native iKVM), Dell iDRAC (VNC/WebSocket), and NanoKVM devices. Optional OIDC authentication with role-based access control.
// @host localhost:8080
// @BasePath /
// @securityDefinitions.apikey CookieAuth
// @in cookie
// @name kvm_session
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

	// Ensure ISO directory exists
	if err := os.MkdirAll(cfg.Settings.ISODir, 0755); err != nil {
		log.Fatalf("Failed to create ISO directory %s: %v", cfg.Settings.ISODir, err)
	}
	log.Printf("ISO library directory: %s", cfg.Settings.ISODir)

	// Auto-detect ISO serve address from ListenAddress if not explicitly set
	if cfg.Settings.ISOServeAddress == "" {
		host, _, err := net.SplitHostPort(cfg.Settings.ListenAddress)
		if err == nil && host != "" && host != "0.0.0.0" && host != "::" {
			cfg.Settings.ISOServeAddress = host
		}
		// If still empty, log a warning
		if cfg.Settings.ISOServeAddress == "" {
			log.Println("WARNING: iso_serve_address not configured and could not auto-detect. Local ISO mount will require explicit configuration.")
		} else {
			log.Printf("ISO serve address auto-detected: %s", cfg.Settings.ISOServeAddress)
		}
	}

	srv := api.NewServerWithStore(cfg, sessionStore, auditLogger, db)

	// Set up OIDC provider if enabled
	var oidcProvider *kvmoidc.Provider
	if cfg.OIDC.Enabled {
		log.Println("OIDC authentication enabled")
		oidcProvider, err = kvmoidc.NewProvider(context.Background(), &cfg.OIDC)
		if err != nil {
			log.Fatalf("Failed to initialize OIDC provider: %v", err)
		}
		oidcProvider.AuditDB = auditLogger
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
	bmcRateLimited := middleware.RateLimitMiddleware(cfg.Settings.BMCProxyRateLimitRPM)

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
		mux.Handle("DELETE /api/sessions/{id}", rateLimited(http.HandlerFunc(srv.DeleteSession)))
		mux.Handle("POST /api/ipmi-session/{name}", rateLimited(http.HandlerFunc(srv.CreateIPMISession)))
		mux.HandleFunc("GET /api/server-status", srv.GetServerStatuses)
		mux.HandleFunc("GET /api/audit-log", srv.GetAuditLog)
		mux.Handle("POST /api/sessions/{id}/power", rateLimited(http.HandlerFunc(srv.KVMPowerControl)))
		mux.Handle("POST /api/sessions/{id}/display-lock", rateLimited(http.HandlerFunc(srv.KVMDisplayLock)))
		mux.Handle("POST /api/sessions/{id}/reset-video", rateLimited(http.HandlerFunc(srv.KVMResetVideo)))
		mux.Handle("POST /api/sessions/{id}/mouse-mode", rateLimited(http.HandlerFunc(srv.KVMMouseMode)))
		mux.Handle("POST /api/sessions/{id}/keyboard-layout", rateLimited(http.HandlerFunc(srv.KVMKeyboardLayout)))
		mux.Handle("POST /api/sessions/{id}/ipmi", rateLimited(http.HandlerFunc(srv.KVMIPMICommand)))
		mux.HandleFunc("GET /api/sessions/{id}/screenshot", srv.KVMScreenshot)
		mux.Handle("POST /api/sessions/{id}/virtual-media/mount", rateLimited(http.HandlerFunc(srv.VirtualMediaMount)))
		mux.Handle("POST /api/sessions/{id}/virtual-media/eject", rateLimited(http.HandlerFunc(srv.VirtualMediaEject)))
		mux.HandleFunc("GET /api/sessions/{id}/virtual-media", srv.VirtualMediaStatus)
		mux.HandleFunc("/api/ws", srv.HandleNanoKVMWebSocket)
		mux.HandleFunc("/api/stream/h264", srv.HandleNanoKVMWebSocket)
		mux.HandleFunc("GET /ws/kvm/{id}", srv.HandleKVMWebSocket)
		mux.Handle("/__bmc/", bmcRateLimited(http.HandlerFunc(srv.HandleBMCProxy)))

		// ISO library management routes
		mux.HandleFunc("GET /api/isos", srv.ListISOs)
		mux.Handle("POST /api/isos", rateLimited(http.HandlerFunc(srv.UploadISO)))
		mux.Handle("DELETE /api/isos/{name}", rateLimited(http.HandlerFunc(srv.DeleteISO)))
		mux.HandleFunc("GET /api/isos/{name}/download", srv.DownloadISO)
		mux.Handle("POST /api/sessions/{id}/virtual-media/mount-local", rateLimited(http.HandlerFunc(srv.MountLocalISO)))
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

	// ISO file server — OUTSIDE OIDC middleware (BMCs need unauthenticated access)
	bmcIPs := make([]string, 0, len(cfg.Servers))
	for _, s := range cfg.Servers {
		bmcIPs = append(bmcIPs, s.BMCIP)
	}
	isoFileServer := iso.NewFileServer(cfg.Settings.ISODir, db, bmcIPs)
	mux.Handle("/iso/", isoFileServer)

	// Start NFS server if enabled
	var nfsServer *iso.NFSServer
	if cfg.Settings.NFSEnabled != nil && *cfg.Settings.NFSEnabled {
		var nfsErr error
		nfsServer, nfsErr = iso.StartNFSServer(cfg.Settings.ISODir, cfg.Settings.NFSPort)
		if nfsErr != nil {
			log.Printf("WARNING: Failed to start NFS server: %v", nfsErr)
		} else {
			log.Printf("NFS server started on port %d", cfg.Settings.NFSPort)
		}
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
	go sessionCleanup(srv, &cfg.Settings, db)

	// Wait for shutdown signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down...")

	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Stop active iKVM bridges
	for _, session := range srv.Sessions.List() {
		if session.Status == models.SessionConnected || session.Status == models.SessionStarting {
			srv.StopIKVMBridge(session.ID)
			session.Status = models.SessionDisconnected
			srv.Sessions.Set(session)
		}
	}

	// Stop NFS server if running
	if nfsServer != nil {
		if err := nfsServer.Shutdown(); err != nil {
			log.Printf("NFS server shutdown error: %v", err)
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
func sessionCleanup(srv *api.Server, cfg *models.Settings, db *store.DB) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		threshold := time.Now().Add(-time.Duration(cfg.IdleTimeoutMinutes) * time.Minute)

		for _, session := range srv.Sessions.List() {
			if session.Status == models.SessionConnected && session.LastActivity.Before(threshold) {
				log.Printf("Session %s: idle timeout, cleaning up", session.ID)
				srv.StopIKVMBridge(session.ID)
				session.Status = models.SessionDisconnected
				srv.Sessions.Set(session)
			}
		}

		// Clean up stale BMC credentials
		srv.CleanupStaleBMCCreds(cfg.BMCCredsTTLMinutes)

		// Purge old audit log entries (run hourly, not every minute)
		if time.Now().Minute() == 0 {
			cutoff := time.Now().AddDate(0, 0, -cfg.AuditRetentionDays)
			if n, err := db.PurgeOldAuditEntries(cutoff); err != nil {
				log.Printf("Audit purge error: %v", err)
			} else if n > 0 {
				log.Printf("Audit purge: removed %d old entries", n)
			}
		}
	}
}
