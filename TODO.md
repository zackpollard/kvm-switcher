# TODO

## Features

### Virtual Media
- [ ] ISO mounting for iDRAC8/iDRAC9 via Redfish virtual media API
- [ ] Virtual media for MegaRAC (IVTP media commands exist but are unused)
- [ ] Frontend UI for uploading/selecting ISO images
- [ ] Progress indication for media mount operations

### Serial over LAN (SOL)
- [ ] Text-based console access alongside KVM
- [ ] Useful for headless servers or when video capture is broken
- [ ] xterm.js integration in frontend
- [ ] SOL via IPMI for MegaRAC/iDRAC, SSH for NanoKVM

### Multi-User KVM
- [ ] Show who else is connected to a KVM session
- [ ] Session takeover/sharing controls
- [ ] Concurrent viewer support (read-only observers)

## Reliability / UX

### Service Worker
- [ ] Refactor 3-layer fallback routing (clientId -> Referer -> lastActiveServer) into a cleaner state machine
- [x] Fix intermittent "404 no servers found" — clear lastActiveServer on app-route navigation, add / and /kvm/ to isAppRoute
- [x] Add diagnostic logging for client mapping failures (DEBUG flag, 8 console.debug calls)

### Status Polling
- [x] Add staleness indicators in the UI ("Updated Xs ago", yellow when stale >60s)
- [x] Log circuit breaker recovery events (open/half-open -> closed transitions)
- [x] Per-server configurable polling intervals (poll_interval_seconds, default 30)
- [x] Expose status fetch errors to the frontend (error field on DeviceStatus)

### Error Handling
- [x] Surface proxy errors to the user (styled HTML error page with retry button)
- [ ] Better feedback when BMC web UI fails to load (instead of blank iframe)
- [x] Retry logic for transient BMC authentication failures (3 attempts, 2s backoff)

## Code Quality

### Frontend Testing
- [x] Auth flow tests (OIDC integration: 401, RBAC filtering, /auth/me)
- [x] Session management tests (getSession, listSessions, keepAlive, delete, create, WebSocket URL)
- [x] Error state tests (API error paths: power control, session conflicts, IPMI errors)
- [x] KVMViewer component tests (10 tests: events, credentials, Ctrl+Alt+Del, desktop name)
- [x] Accessibility fixes (ARIA labels, roles, live regions, keyboard nav) and tests (11 tests)

### Backend Testing
- [x] iKVM bridge tests (lifecycle, screenshot BGRA→RGBA, commands, VNC message building, frame tracking)
- [x] VNC protocol rewriting tests (ServerInit rewrite, SetEncodings filter, CheckOrigin)
- [x] Proxy response rewriting tests (header stripping, auto-login injection, gzip decompression)
- [x] Auth flow integration tests (OIDC -> session -> RBAC with 5 sub-tests)

### API Documentation
- [x] OpenAPI/Swagger spec for all REST endpoints (api/openapi.yaml, 22 paths, 24 schemas)
- [x] WebSocket protocol documentation (docs/websocket-protocol.md)
- [x] Document the service worker routing rules (docs/service-worker.md)

## Security

### Production Hardening
- [x] Configure restrictive CORS origins — WebSocket upgraders now check configured origins
- [x] Make InsecureSkipVerify configurable per-server (tls_skip_verify field, default true)
- [x] Add session cookie rotation on privilege changes
- [x] Document and enforce audit log retention/cleanup policy — 90-day default, hourly purge
- [x] Rate limit BMC proxy requests — 300 RPM default, mutation endpoints also protected

## Infrastructure

### CI/CD
- [x] GitHub Actions workflow for automated testing on push (ci.yml + publish.yml)
- [x] Lint checks (gofmt, go vet, svelte-check)
- [x] Build verification for both backend and frontend
- [ ] E2E test integration (Playwright against test BMCs or mocks)

### Docker / Deployment
- [x] Multi-arch Docker image (ARM + amd64 via publish.yml with native runners)
- [x] Clean up legacy JViewer Docker build targets from Makefile
- [x] Document production deployment (docs/deployment.md: config, Docker, nginx, OIDC, security)
