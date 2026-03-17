# KVM Switcher

A self-hosted web panel for managing server infrastructure — providing browser-based KVM access, IPMI web interfaces, and live device monitoring for servers, UPS, and PDU devices.

## Features

- **KVM Console** — Browser-based remote console for AMI MegaRAC (via Docker/JViewer), Dell iDRAC8 (VNC), Dell iDRAC9 (HTML5), and Sipeed NanoKVM
- **IPMI Web Proxy** — Access BMC management interfaces through a single URL with automatic login
- **APC NMC2 Proxy** — Access APC UPS and PDU management panels with automatic login
- **Live Dashboard** — Real-time status monitoring with power state, health, temperatures, load, and battery stats
- **Auto-Login** — Sessions are created automatically on startup; login pages are bypassed or hidden
- **NanoKVM Update Checks** — Compares installed app version against latest GitHub release
- **OIDC Authentication** — Optional role-based access control via OpenID Connect

## Supported Devices

| Type | KVM | IPMI/Panel | Status Polling |
|------|-----|------------|----------------|
| AMI MegaRAC | Container (JViewer + noVNC) | Web proxy with auto-login | Power state, CPU temp |
| Dell iDRAC9 | HTML5 via VNC proxy | Web proxy with auto-login | Power, model, health, watts, temp (Redfish) |
| Dell iDRAC8 | VNC proxy | Web proxy with login bypass | Power, model, health, watts, temp (Redfish) |
| Sipeed NanoKVM | Native web UI proxy | N/A (KVM is the UI) | Power LED, app/firmware version, update check |
| APC UPS (NMC2) | N/A | Web proxy with login bypass | Battery %, runtime, load, voltage, temp |
| APC PDU (NMC2) | N/A | Web proxy with login bypass | Load (watts/amps), voltage, model |

Adding a new device type requires implementing the `BMCAuthenticator` interface in `internal/auth/`.

## Architecture

```
Browser ──HTTP/WS──▶ Go Backend
                         │
                    ┌────┴─────────────────────┐
                    │                          │
               REST API                   BMC Proxy
               WS Proxy              (reverse proxy per device)
               Status Poller          Session Manager
               SvelteKit SPA         (auto-login, credential injection)
                    │                          │
                    ▼                          ▼
              Dashboard UI              BMC/KVM Devices
              noVNC canvas              (IPMI, iDRAC, NanoKVM, APC)
```

- **Service Worker** routes BMC web traffic through the proxy, handles auto-login script injection, and manages NanoKVM API/WebSocket routing
- **Session Manager** creates BMC sessions on startup and renews stale ones automatically
- **Status Poller** fetches device stats every 30 seconds using board-type-specific APIs (Redfish, IPMI RPC, HTML scraping)

## Prerequisites

- **Go** 1.21+
- **Node.js** 18+
- **Docker** with BuildKit support (only needed for AMI MegaRAC KVM)
- Network access to BMC/device management interfaces

## Quick Start

### 1. Build the Docker image (AMI MegaRAC only)

The JViewer container must be built for `linux/amd64` — the BMC's native JNI libraries are x86_64 only.

```bash
make build-docker
```

### 2. Configure devices

Edit `configs/servers.yaml` (see `configs/servers.example.yaml` for all options):

```yaml
servers:
  - name: "server-1"
    bmc_ip: "10.10.11.251"
    bmc_port: 80
    board_type: "ami_megarac"
    username: "admin"
    credential_env: "BMC_PASSWORD_1"

  - name: "my-nanokvm"
    bmc_ip: "10.10.11.246"
    bmc_port: 80
    board_type: "nanokvm"
    username: "admin"
    credential_env: "NANOKVM_PASSWORD"

  - name: "ups-1"
    bmc_ip: "10.10.50.226"
    bmc_port: 80
    board_type: "apc_ups"
    username: "apc"
    credential_env: "APC_PASSWORD"

settings:
  max_concurrent_sessions: 4
  session_timeout_minutes: 60
  idle_timeout_minutes: 30
  listen_address: "0.0.0.0:8080"
```

Passwords are read from environment variables specified by `credential_env`.

### 3. Build and run

```bash
# Build frontend and backend
make build

# Set passwords
export BMC_PASSWORD_1=yourpassword
export NANOKVM_PASSWORD=admin
export APC_PASSWORD=apc

# Run
./kvm-switcher -config configs/servers.yaml -web web/build
```

The dashboard is available at `http://localhost:8080`.

### Development mode

```bash
# Terminal 1: Frontend with hot reload
make dev-frontend

# Terminal 2: Backend
make dev-backend
```

## Configuration

### Server entry

| Field | Description |
|-------|-------------|
| `name` | Display name for the device |
| `bmc_ip` | Device management IP address |
| `bmc_port` | HTTP port (default: 80, or 443 for iDRAC) |
| `board_type` | Device type (see supported types below) |
| `username` | Login username |
| `credential_env` | Environment variable containing the password |

### Board types

| Value | Device |
|-------|--------|
| `ami_megarac` | AMI MegaRAC BMC (ASRock Rack, etc.) |
| `dell_idrac9` | Dell iDRAC9 (14th gen+, e.g. R640) |
| `dell_idrac8` | Dell iDRAC8 (13th gen, e.g. R730xd) |
| `nanokvm` | Sipeed NanoKVM |
| `apc_ups` | APC Network Management Card 2 (UPS and PDU) |

### Settings

| Field | Description | Default |
|-------|-------------|---------|
| `max_concurrent_sessions` | Maximum active KVM sessions | 4 |
| `session_timeout_minutes` | Max session duration | 60 |
| `idle_timeout_minutes` | Idle session cleanup threshold | 30 |
| `container_image` | Docker image for JViewer containers | `kvm-switcher/jviewer:latest` |
| `listen_address` | HTTP listen address | `0.0.0.0:8080` |

## API

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/servers` | List configured devices |
| `GET` | `/api/server-status` | Get live status for all devices |
| `POST` | `/api/ipmi-session/{name}` | Create/refresh a BMC web session |
| `POST` | `/api/sessions` | Start a KVM session |
| `GET` | `/api/sessions` | List active KVM sessions |
| `GET` | `/api/sessions/{id}` | Get KVM session status |
| `DELETE` | `/api/sessions/{id}` | Terminate a KVM session |
| `GET` | `/ws/kvm/{id}` | WebSocket proxy to KVM container |

## Project Structure

```
cmd/server/          Go backend entry point
internal/
  api/               REST API, WebSocket proxy, BMC reverse proxy, status poller
  auth/              Device authenticators (MegaRAC, iDRAC8/9, NanoKVM, APC)
  config/            YAML config loader
  container/         Container runtime interface
  docker/            Docker container lifecycle management
  kubernetes/        Kubernetes pod lifecycle management
  models/            Shared types and session store
  oidc/              OpenID Connect authentication
web/
  src/               SvelteKit frontend (TypeScript, Tailwind CSS)
  static/sw.js       Service Worker for BMC proxy routing
docker/jviewer/      JViewer container image (Dockerfile + entrypoint)
configs/             Server configuration
tests/e2e/           Playwright end-to-end tests
```
