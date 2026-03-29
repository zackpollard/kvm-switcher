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
- [ ] Fix intermittent "404 no servers found" caused by stale SW cache (currently requires manual unregister + refresh)
- [ ] Add diagnostic logging for client mapping failures (behind a debug flag)

### Status Polling
- [ ] Add staleness indicators in the UI (e.g. "last updated 2m ago")
- [ ] Log circuit breaker recovery events (currently only failures are logged)
- [ ] Per-server configurable polling intervals
- [ ] Expose status fetch errors to the frontend instead of silently failing

### Error Handling
- [ ] Surface proxy errors to the user (currently logged server-side only)
- [ ] Better feedback when BMC web UI fails to load (instead of blank iframe)
- [ ] Retry logic for transient BMC authentication failures

## Code Quality

### Frontend Testing
- [ ] Auth flow tests (OIDC login/logout, role-based access)
- [ ] Session management tests (create, reconnect, timeout, disconnect)
- [ ] Error state tests (BMC unreachable, session expired, network loss)
- [ ] KVMViewer component tests
- [ ] Accessibility / responsive design tests

### Backend Testing
- [ ] Integration tests for iKVM bridge (VNC handshake, frame delivery)
- [x] VNC protocol rewriting tests (ServerInit rewrite, SetEncodings filter, CheckOrigin)
- [x] Proxy response rewriting tests (header stripping, auto-login injection, gzip decompression)
- [ ] Auth flow integration tests (full login -> session -> BMC proxy cycle)

### API Documentation
- [ ] OpenAPI/Swagger spec for all REST endpoints
- [ ] WebSocket protocol documentation (VNC proxy, iKVM bridge)
- [ ] Document the service worker routing rules

## Security

### Production Hardening
- [x] Configure restrictive CORS origins — WebSocket upgraders now check configured origins
- [ ] Remove InsecureSkipVerify from iDRAC9 WebSocket proxy (add proper CA trust)
- [ ] Add session cookie rotation on privilege changes
- [x] Document and enforce audit log retention/cleanup policy — 90-day default, hourly purge
- [x] Rate limit BMC proxy requests — 300 RPM default, mutation endpoints also protected

## Infrastructure

### CI/CD
- [ ] GitHub Actions workflow for automated testing on push
- [ ] Lint checks (golangci-lint, eslint)
- [ ] Build verification for both backend and frontend
- [ ] E2E test integration (Playwright against test BMCs or mocks)

### Docker / Deployment
- [ ] Multi-arch Docker image for the app itself (ARM + amd64)
- [ ] Clean up legacy JViewer Docker build targets from Makefile
- [ ] Document production deployment (reverse proxy, TLS termination, env vars)
- [ ] Helm chart or docker-compose production template
