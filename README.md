# KVM Switcher

A self-hosted web panel that provides browser-based KVM access to servers with IPMI/BMC interfaces, replacing Java Web Start (JNLP) clients.

## How It Works

```
Browser ──HTTP/WS──▶ Go Backend ──Docker SDK──▶ Container per session
                         │                          │
                     REST API                  JViewer (Java 8)
                     WS Proxy                  Xvfb + x11vnc
                     SvelteKit SPA             websockify
                         │                          │
                         │                     BMC KVM stream
                         ▼                          │
                    noVNC canvas ◀──websocket──────────
```

Each KVM session runs in an isolated Docker container:
1. The Go backend authenticates with the BMC and obtains a KVM token
2. A Docker container starts with JViewer connected to the BMC's KVM stream
3. Xvfb provides a virtual display, x11vnc captures it, websockify bridges to WebSocket
4. noVNC in the browser renders the KVM console with full keyboard/mouse support
5. Sessions auto-reconnect on timeout or socket failure

## Supported BMC Types

| Type | Status |
|------|--------|
| AMI MegaRAC | Supported |
| Supermicro | Planned |
| Dell iDRAC | Planned |
| HP iLO | Planned |

Adding a new vendor requires implementing the `BMCAuthenticator` interface in `internal/auth/`.

## Prerequisites

- **Go** 1.21+
- **Node.js** 18+
- **Docker** with BuildKit support
- Network access to BMC management interfaces

## Quick Start

### 1. Build the Docker image

The JViewer container must be built for `linux/amd64` — the BMC's native JNI libraries are x86_64 only.

```bash
make build-docker
```

### 2. Configure servers

Edit `configs/servers.yaml`:

```yaml
servers:
  - name: "server-1"
    bmc_ip: "10.10.11.251"
    bmc_port: 80
    board_type: "ami_megarac"
    username: "admin"
    credential_env: "BMC_PASSWORD_SERVER1"

settings:
  max_concurrent_sessions: 4
  session_timeout_minutes: 60
  idle_timeout_minutes: 30
  docker_image: "kvm-switcher/jviewer:latest"
  listen_address: "0.0.0.0:8080"
```

BMC passwords are read from environment variables specified by `credential_env`.

### 3. Build and run

```bash
# Build frontend and backend
make build

# Set BMC passwords
export BMC_PASSWORD_SERVER1=yourpassword

# Run
./kvm-switcher -config configs/servers.yaml -web web/build
```

The panel is available at `http://localhost:8080`.

### Development mode

Run the frontend dev server and Go backend separately:

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
| `name` | Display name for the server |
| `bmc_ip` | BMC management IP address |
| `bmc_port` | BMC HTTP port (default: 80) |
| `board_type` | Authenticator type (e.g. `ami_megarac`) |
| `username` | BMC login username |
| `credential_env` | Environment variable containing the BMC password |

### Settings

| Field | Description | Default |
|-------|-------------|---------|
| `max_concurrent_sessions` | Maximum active KVM sessions | 4 |
| `session_timeout_minutes` | Max session duration | 60 |
| `idle_timeout_minutes` | Idle session cleanup threshold | 30 |
| `docker_image` | Docker image for KVM containers | `kvm-switcher/jviewer:latest` |
| `listen_address` | HTTP listen address | `0.0.0.0:8080` |

## API

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/servers` | List configured servers |
| `POST` | `/api/sessions` | Start a KVM session |
| `GET` | `/api/sessions` | List active sessions |
| `GET` | `/api/sessions/{id}` | Get session status |
| `DELETE` | `/api/sessions/{id}` | Terminate a session |
| `GET` | `/ws/kvm/{id}` | WebSocket proxy to KVM container |

## Project Structure

```
cmd/server/          Go backend entry point
internal/
  api/               REST API handlers, WebSocket proxy
  auth/              BMC authenticator interface + implementations
  config/            YAML config loader
  docker/            Docker container lifecycle management
  models/            Shared types and session store
web/                 SvelteKit frontend (TypeScript, Tailwind CSS)
docker/jviewer/      JViewer container image (Dockerfile + entrypoint)
configs/             Server configuration
```

## Auto-Reconnect

The JViewer container monitors for error/timeout popup dialogs. When a known dialog is detected (e.g. "Socket Failure"), the container exits and the frontend automatically creates a new session. The list of dialogs that trigger reconnection is in `docker/jviewer/entrypoint.sh`.
