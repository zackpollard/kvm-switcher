# Production Deployment

## Prerequisites

- **Go 1.22+** and **Node.js 22+** (build from source only)
- **Docker** (container deployment)
- Network access from the KVM Switcher host to all BMC management interfaces (typically a dedicated management VLAN)
- Firewall rules permitting outbound traffic to BMC ports: HTTP (80), HTTPS (443), VNC (5901), and iKVM/IVTP (7578)

## Building from source

```bash
make build
```

This runs `npm install && npm run build` in `web/`, then `go build -o kvm-switcher ./cmd/server/`. The output is a single `kvm-switcher` binary that serves both the API and the SvelteKit frontend from `web/build/`.

Run it directly:

```bash
./kvm-switcher -config configs/servers.yaml -web web/build
```

### Command-line flags

| Flag | Default | Description |
|------|---------|-------------|
| `-config` | `configs/servers.yaml` | Path to YAML configuration file |
| `-web` | `web/build` | Path to frontend static files directory |

## Docker deployment

Multi-arch images (linux/amd64, linux/arm64) are published to GHCR on every push to `main` that includes a conventional-commit version bump.

```
ghcr.io/zackpollard/kvm-switcher:latest
ghcr.io/zackpollard/kvm-switcher:<version>    # e.g. 1.2.3
```

### docker run

```bash
docker run -d \
  --name kvm-switcher \
  --restart unless-stopped \
  -p 8080:8080 \
  -v $(pwd)/configs/servers.yaml:/app/configs/servers.yaml:ro \
  -v kvm-data:/app/data \
  --env-file .env \
  -e KVM_METRICS_ENABLED=true \
  ghcr.io/zackpollard/kvm-switcher:latest
```

### docker-compose.yml

Minimal production compose file (without observability stack):

```yaml
services:
  kvm-switcher:
    image: ghcr.io/zackpollard/kvm-switcher:latest
    restart: unless-stopped
    ports:
      - "8080:8080"
    volumes:
      - ./configs/servers.yaml:/app/configs/servers.yaml:ro
      - kvm-data:/app/data
    env_file:
      - .env
    environment:
      - KVM_METRICS_ENABLED=true

volumes:
  kvm-data:
```

With Prometheus and Grafana (mirrors the repo's `docker-compose.yml`):

```yaml
services:
  kvm-switcher:
    image: ghcr.io/zackpollard/kvm-switcher:latest
    restart: unless-stopped
    ports:
      - "8080:8080"
    volumes:
      - ./configs/servers.yaml:/app/configs/servers.yaml:ro
      - kvm-data:/app/data
    env_file:
      - .env
    environment:
      - KVM_METRICS_ENABLED=true

  prometheus:
    image: prom/prometheus:latest
    restart: unless-stopped
    ports:
      - "9090:9090"
    volumes:
      - ./monitoring/prometheus/prometheus.yml:/etc/prometheus/prometheus.yml:ro
      - prometheus-data:/prometheus

  grafana:
    image: grafana/grafana:latest
    restart: unless-stopped
    ports:
      - "3000:3000"
    environment:
      - GF_SECURITY_ADMIN_USER=${GRAFANA_ADMIN_USER:-admin}
      - GF_SECURITY_ADMIN_PASSWORD=${GRAFANA_ADMIN_PASSWORD:-admin}
    volumes:
      - ./monitoring/grafana/provisioning:/etc/grafana/provisioning:ro
      - ./monitoring/grafana/dashboards:/var/lib/grafana/dashboards:ro
      - grafana-data:/var/lib/grafana

volumes:
  kvm-data:
  prometheus-data:
  grafana-data:
```

### Volume mounts

| Mount | Purpose |
|-------|---------|
| `configs/servers.yaml` (read-only) | Server and settings configuration |
| `data/` (persistent volume) | SQLite database (audit log, persistent sessions) |
| `monitoring/` (read-only, optional) | Prometheus and Grafana provisioning files |

## Configuration

All configuration lives in a single YAML file (default: `configs/servers.yaml`). See `configs/servers.example.yaml` for a complete annotated example.

### Server entries

```yaml
servers:
  - name: "server-1"              # Unique display name (required)
    bmc_ip: "10.10.11.251"        # BMC management IP (required)
    bmc_port: 80                  # BMC web port (default: 80, or 443 for Dell iDRAC)
    board_type: "ami_megarac"     # Board type (required, see below)
    username: "admin"             # BMC login username (required)
    credential_env: "BMC_PASSWORD_SERVER1"  # Env var holding the password (required)
    tls_skip_verify: true         # Skip TLS cert verification (default: true)
```

**Supported board types:**

| Board type | KVM method | Default port | Notes |
|-----------|-----------|-------------|-------|
| `ami_megarac` | Native iKVM (IVTP protocol) | 80 | ASRock Rack, Supermicro X11/X12 with AMI BMC |
| `dell_idrac9` | HTML5 VNC WebSocket proxy | 443 | Dell 14th gen+ (R640, R740, etc.) |
| `dell_idrac8` | VNC TCP proxy | 443 | Dell 13th gen (R730xd, etc.) |
| `nanokvm` | Native web UI proxy (MJPEG/H264) | 80 | Sipeed NanoKVM |
| `apc_ups` | Web proxy with login bypass | 80 | APC UPS/PDU with Network Management Card 2 |

### Settings

```yaml
settings:
  listen_address: "0.0.0.0:8080"       # Bind address (default: "0.0.0.0:8080")
  max_concurrent_sessions: 4            # Max active KVM sessions (default: 4)
  session_timeout_minutes: 60           # Absolute session lifetime (default: 60)
  idle_timeout_minutes: 30              # Idle timeout before auto-disconnect (default: 30)
  cors_origins: ["https://kvm.example.com"]  # Allowed CORS origins (default: ["*"])
  rate_limit_rpm: 60                    # Per-IP rate limit for mutation endpoints (default: 60)
  bmc_proxy_rate_limit_rpm: 300         # Per-IP rate limit for BMC proxy requests (default: 300)
  db_path: "data/kvm-switcher.db"       # SQLite database path (default: "data/kvm-switcher.db")
  audit_log: true                       # Enable audit logging (default: true)
  audit_retention_days: 90              # Days to retain audit entries (default: 90)
  metrics_enabled: true                 # Expose /metrics for Prometheus (default: false)
  bmc_creds_ttl_minutes: 120            # TTL for cached BMC credentials (default: 120)
```

### OIDC authentication

When enabled, all API and WebSocket routes require authentication. Users are mapped to servers via role claims.

```yaml
oidc:
  enabled: true
  issuer_url: "https://auth.example.com/realms/myorg"
  client_id: "kvm-switcher"
  client_secret_env: "OIDC_CLIENT_SECRET"     # Env var holding the client secret
  redirect_url: "https://kvm.example.com/auth/callback"
  scopes: ["openid", "profile", "email", "groups"]
  role_claim: "groups"                         # JWT claim containing roles/groups
  role_mappings:
    admin:
      servers: ["*"]                           # Wildcard: access to all servers
    ops-team:
      servers: ["server-1", "dell-14g", "ups-1"]
    dev-team:
      servers: ["server-1"]
```

**Validation rules:**
- `issuer_url`, `client_id`, `client_secret_env`, and `redirect_url` are all required when `enabled: true`
- At least one `role_mapping` must be defined
- Mapped server names must match entries in `servers:` (except `"*"` wildcard)

## Environment variables

### Settings overrides

All settings can be configured in YAML and selectively overridden via environment variables. Env vars take precedence.

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `KVM_CORS_ORIGINS` | comma-separated | `*` | Allowed CORS origins |
| `KVM_RATE_LIMIT_RPM` | int | `60` | Per-IP rate limit for mutation endpoints |
| `KVM_BMC_PROXY_RATE_LIMIT_RPM` | int | `300` | Per-IP rate limit for BMC proxy |
| `KVM_DB_PATH` | string | `data/kvm-switcher.db` | SQLite database file path |
| `KVM_AUDIT_LOG` | `true`/`false` | `true` | Enable audit logging |
| `KVM_AUDIT_RETENTION_DAYS` | int | `90` | Days to retain audit log entries |
| `KVM_METRICS_ENABLED` | `true`/`false` | `false` | Expose Prometheus /metrics endpoint |
| `KVM_BMC_CREDS_TTL_MINUTES` | int | `120` | Cached BMC credential lifetime |

### Credential env vars

BMC passwords and the OIDC client secret are never stored in the config file. Each server's `credential_env` field names the environment variable holding its password. Common patterns:

```bash
# BMC passwords (names match credential_env in servers.yaml)
BMC_PASSWORD_SERVER1=changeme
BMC_PASSWORD_DELL=changeme
NANOKVM_PASSWORD=changeme
APC_PASSWORD=changeme

# OIDC client secret (name matches client_secret_env in oidc config)
OIDC_CLIENT_SECRET=your-client-secret
```

Use a `.env` file (with `--env-file` or `env_file:` in compose) or inject via your secrets manager. Never commit credential values to version control.

## Reverse proxy setup

KVM Switcher should sit behind a reverse proxy for TLS termination. WebSocket upgrade headers must be forwarded for `/ws/kvm/*` paths (iKVM/VNC streams).

### nginx example

```nginx
upstream kvm {
    server 127.0.0.1:8080;
}

server {
    listen 443 ssl http2;
    server_name kvm.example.com;

    ssl_certificate     /etc/ssl/certs/kvm.example.com.pem;
    ssl_certificate_key /etc/ssl/private/kvm.example.com.key;

    # WebSocket paths — must proxy upgrade headers
    location /ws/ {
        proxy_pass http://kvm;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
    }

    # API and frontend
    location / {
        proxy_pass http://kvm;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }

    # Cache static frontend assets (JS, CSS, images)
    location /assets/ {
        proxy_pass http://kvm;
        proxy_cache_valid 200 7d;
        add_header Cache-Control "public, max-age=604800, immutable";
    }
}
```

Key points:
- `proxy_read_timeout` and `proxy_send_timeout` must be set high (3600s+) on WebSocket locations to prevent idle disconnects — the application handles its own idle timeout
- The server sets `WriteTimeout: 0` internally for WebSocket connections, so the reverse proxy is the effective timeout boundary
- If using Cloudflare or similar CDN, ensure WebSockets are enabled for the domain
- The `/sw.js` service worker is served with `Cache-Control: no-cache` by the application — do not override this at the proxy layer

## Observability

### Prometheus metrics

Enable with `KVM_METRICS_ENABLED=true` (or `metrics_enabled: true` in YAML). Metrics are exposed at `GET /metrics` (unauthenticated, even when OIDC is enabled).

Prometheus scrape config:

```yaml
scrape_configs:
  - job_name: "kvm-switcher"
    metrics_path: /metrics
    static_configs:
      - targets: ["kvm-switcher:8080"]
```

### Grafana dashboard

A pre-built Grafana dashboard is included at `monitoring/grafana/dashboards/kvm-switcher.json` (and `dev/grafana/dashboards/kvm-switcher.json`). The `docker-compose.yml` at the repo root provisions it automatically via Grafana's provisioning system.

To import manually: Grafana > Dashboards > Import > upload the JSON file.

### Health endpoints

| Endpoint | Auth | Description |
|----------|------|-------------|
| `GET /healthz` | No | Liveness probe — always returns `{"status":"ok"}` |
| `GET /readyz` | No | Readiness probe — checks SQLite database connectivity |

## Security checklist

- [ ] **CORS origins** — set `cors_origins` (or `KVM_CORS_ORIGINS`) to your exact domain(s). The default `["*"]` is not suitable for production.
- [ ] **TLS** — terminate TLS at a reverse proxy. All BMC credentials flow through the application; do not expose port 8080 directly to untrusted networks.
- [ ] **OIDC** — enable OIDC authentication for multi-user environments. Map roles to servers to enforce least-privilege access.
- [ ] **OIDC redirect URL** — set `redirect_url` to your public HTTPS URL (`https://kvm.example.com/auth/callback`), not `localhost`.
- [ ] **Credential injection** — supply BMC passwords and OIDC secrets via environment variables or a secrets manager. Never put passwords in `servers.yaml`.
- [ ] **Audit logging** — enabled by default. Set `audit_retention_days` based on your compliance requirements.
- [ ] **Rate limiting** — the defaults (60 RPM for mutations, 300 RPM for BMC proxy) are reasonable. Adjust if needed for your user count.
- [ ] **Database path** — ensure the `data/` directory (or `KVM_DB_PATH` target) is on a persistent volume in container deployments.
- [ ] **BMC network isolation** — the KVM Switcher host should be the only ingress point to the management VLAN. Do not expose BMC interfaces to the general network.

## Troubleshooting

### BMC unreachable

The application polls BMC status in the background. If a server shows as offline:
- Verify network connectivity from the KVM Switcher host to the BMC IP and port
- Check that `tls_skip_verify` is set appropriately (BMCs almost always use self-signed certificates)
- For Dell iDRAC, confirm port 443 is reachable and also port 5901 for VNC sessions
- For AMI MegaRAC, the iKVM protocol uses port 7578 (IVTP) in addition to the web port

### Max concurrent sessions reached

The `max_concurrent_sessions` limit is global across all servers. When hit, new session creation returns an error. Either increase the limit or delete idle sessions via the UI or `DELETE /api/sessions/{id}`.

### Service worker cache issues

The frontend uses a service worker for offline support. If users see stale content after an upgrade:
- The server serves `/sw.js` with `Cache-Control: no-cache` to ensure the browser checks for updates
- A hard refresh (Ctrl+Shift+R) forces a full reload
- In development, disable the service worker in browser DevTools > Application > Service Workers

### ipmitool not found (BMC cold reset)

The BMC cold reset feature (`bmc_reset` power action) requires `ipmitool` to be installed on the host. It is not included in the Docker image. If you need this feature in Docker, extend the image:

```dockerfile
FROM ghcr.io/zackpollard/kvm-switcher:latest
RUN apk add --no-cache ipmitool
```

### Session stuck in "starting" state

This typically means the BMC login succeeded but the KVM connection handshake failed. Common causes:
- Another user has an active KVM session on the BMC (most BMCs allow only one)
- The BMC's KVM service is unresponsive — try a BMC web interface reset through the IPMI panel
- For AMI MegaRAC, the IVTP port (7578) may be blocked by a firewall

### Database errors on startup

If the application fails to start with a database error:
- Ensure the directory for `db_path` exists and is writable
- In Docker, verify the data volume is mounted correctly
- If the database is corrupted, remove it and restart — sessions are transient and the audit log will start fresh
