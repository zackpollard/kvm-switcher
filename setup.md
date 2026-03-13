# Setup Guide

## Configuration

All configuration lives in a single YAML file (default: `configs/servers.yaml`). Copy the example to get started:

```bash
cp configs/servers.example.yaml configs/servers.yaml
```

### Servers

Each server entry defines a BMC/IPMI endpoint:

```yaml
servers:
  - name: "server-1"
    bmc_ip: "10.10.11.251"
    bmc_port: 80
    board_type: "ami_megarac"
    username: "admin"
    credential_env: "BMC_PASSWORD_SERVER1"
```

| Field | Required | Description | Default |
|-------|----------|-------------|---------|
| `name` | Yes | Display name (must be unique) | — |
| `bmc_ip` | Yes | BMC management IP address | — |
| `bmc_port` | No | BMC HTTP port | `80` |
| `board_type` | Yes | Authenticator plugin (e.g. `ami_megarac`) | — |
| `username` | Yes | BMC login username | — |
| `credential_env` | Yes | Environment variable containing the BMC password | — |

BMC passwords are never stored in the config file. Set them as environment variables before starting:

```bash
export BMC_PASSWORD_SERVER1=yourpassword
export BMC_PASSWORD_SERVER2=anotherpassword
```

### Settings

```yaml
settings:
  runtime: "docker"
  container_image: "kvm-switcher/jviewer:latest"
  max_concurrent_sessions: 4
  session_timeout_minutes: 60
  idle_timeout_minutes: 30
  listen_address: "0.0.0.0:8080"
```

| Field | Description | Default |
|-------|-------------|---------|
| `runtime` | Container runtime: `docker` or `kubernetes` | `docker` |
| `container_image` | Docker/OCI image for KVM session containers | `kvm-switcher/jviewer:latest` |
| `max_concurrent_sessions` | Maximum active KVM sessions at once | `4` |
| `session_timeout_minutes` | Hard limit on session duration | `60` |
| `idle_timeout_minutes` | Sessions with no activity are cleaned up after this | `30` |
| `listen_address` | Address and port for the HTTP server | `0.0.0.0:8080` |

#### Kubernetes settings

Only used when `runtime: "kubernetes"`:

| Field | Description | Default |
|-------|-------------|---------|
| `kube_namespace` | Namespace for KVM pods (must already exist) | `kvm-switcher` |
| `kube_config` | Path to kubeconfig file; leave empty for in-cluster auth | `""` |

---

## OIDC Authentication

KVM Switcher supports optional OIDC authentication with role-based access control. When enabled, users must log in through your identity provider and can only access servers their roles permit.

When OIDC is not enabled, the application runs without any authentication (suitable for trusted networks).

### Prerequisites

You need an OIDC-compatible identity provider. Common options:

- **Keycloak** — self-hosted, full-featured
- **Authentik** — self-hosted, modern UI
- **Authelia** — lightweight, self-hosted
- **Google Workspace** / **Azure AD** / **Okta** — managed services

Your IdP must support the Authorization Code flow and issue ID tokens with a claim containing the user's roles or groups.

### Step 1: Create an OIDC client in your IdP

Register a new application/client in your identity provider with these settings:

| Setting | Value |
|---------|-------|
| **Client type** | Confidential (server-side) |
| **Grant type** | Authorization Code |
| **Redirect URI** | `https://<your-kvm-switcher-host>/auth/callback` |
| **Scopes** | `openid`, `profile`, `email`, and whatever scope exposes group/role claims (often `groups`) |

Take note of:
- The **Issuer URL** (e.g. `https://auth.example.com/realms/myorg`)
- The **Client ID**
- The **Client Secret**

### Step 2: Identify the role claim

Find out which claim in the ID token contains the user's groups or roles. Common values:

| IdP | Typical claim |
|-----|---------------|
| Keycloak | `groups` or `realm_access.roles` |
| Authentik | `groups` |
| Authelia | `groups` |
| Azure AD | `groups` or `roles` |
| Google | `groups` (requires Google Groups configured) |
| Okta | `groups` |

You can decode a sample ID token at [jwt.io](https://jwt.io) to inspect the claims.

### Step 3: Configure OIDC in servers.yaml

Add the `oidc` section to your config:

```yaml
oidc:
  enabled: true
  issuer_url: "https://auth.example.com/realms/myorg"
  client_id: "kvm-switcher"
  client_secret_env: "OIDC_CLIENT_SECRET"
  redirect_url: "https://kvm.example.com/auth/callback"
  scopes: ["openid", "profile", "email", "groups"]
  role_claim: "groups"
  role_mappings:
    admin:
      servers: ["*"]
    ops-team:
      servers: ["server-1", "server-2"]
    dev-team:
      servers: ["server-1"]
```

| Field | Required | Description |
|-------|----------|-------------|
| `enabled` | Yes | Set to `true` to enable OIDC |
| `issuer_url` | Yes | OIDC issuer URL (must serve `/.well-known/openid-configuration`) |
| `client_id` | Yes | Client ID from your IdP |
| `client_secret_env` | Yes | Environment variable containing the client secret |
| `redirect_url` | Yes | Must match the redirect URI registered in your IdP |
| `scopes` | No | OAuth2 scopes to request (default: `openid`, `profile`, `email`) |
| `role_claim` | No | JWT claim containing roles/groups (default: `groups`) |
| `role_mappings` | Yes | Map of role names to server access lists |

### Step 4: Define role mappings

Role mappings control which OIDC roles can access which servers:

```yaml
role_mappings:
  admin:
    servers: ["*"]              # wildcard: access to all servers
  ops-team:
    servers: ["server-1", "server-2"]  # specific servers only
  dev-team:
    servers: ["server-1"]
```

- Use `"*"` to grant access to all servers
- Server names must match the `name` field in the `servers` list (validated at startup)
- A user with multiple roles gets the union of all their permissions
- Users with no matching roles see an empty server list

### Step 5: Set the client secret and start

```bash
export OIDC_CLIENT_SECRET=your-client-secret
export BMC_PASSWORD_SERVER1=yourpassword

./kvm-switcher -config configs/servers.yaml -web web/build
```

The application will discover the OIDC provider's endpoints on startup and fail fast if the issuer is unreachable.

### Auth flow

1. Unauthenticated users hitting the UI are redirected to `/auth/login`
2. `/auth/login` redirects to your IdP's authorization endpoint
3. After login, the IdP redirects back to `/auth/callback` with an authorization code
4. The backend exchanges the code for tokens, extracts roles from the ID token, and creates a session cookie
5. API requests (`/api/*`) and WebSocket connections (`/ws/*`) require a valid session
6. The user's roles determine which servers appear in the list and which sessions they can create
7. `/auth/logout` clears the session and redirects to the homepage

### Auth endpoints

| Endpoint | Description |
|----------|-------------|
| `GET /auth/login` | Redirects to OIDC provider |
| `GET /auth/callback` | Handles OIDC callback, creates session |
| `GET /auth/logout` | Clears session, redirects to `/` |
| `GET /auth/me` | Returns current user info (works without auth) |

### Example: Keycloak setup

1. Create a new realm or use an existing one
2. Go to **Clients** → **Create client**
   - Client ID: `kvm-switcher`
   - Client authentication: **On** (confidential)
   - Valid redirect URIs: `https://kvm.example.com/auth/callback`
3. Go to the **Credentials** tab, copy the client secret
4. Create groups matching your role mapping names (e.g. `admin`, `ops-team`)
5. Add the `groups` scope to the client if not included by default
6. Assign users to the appropriate groups

Config:

```yaml
oidc:
  enabled: true
  issuer_url: "https://keycloak.example.com/realms/myorg"
  client_id: "kvm-switcher"
  client_secret_env: "OIDC_CLIENT_SECRET"
  redirect_url: "https://kvm.example.com/auth/callback"
  scopes: ["openid", "profile", "email", "groups"]
  role_claim: "groups"
  role_mappings:
    admin:
      servers: ["*"]
    ops-team:
      servers: ["server-1", "server-2"]
```

### Example: Authentik setup

1. Go to **Applications** → **Create** with provider type **OAuth2/OIDC**
   - Client type: Confidential
   - Redirect URI: `https://kvm.example.com/auth/callback`
   - Scopes: `openid profile email groups`
2. Copy the Client ID and Client Secret from the provider
3. Create groups in Authentik matching your role mapping names
4. Assign users to groups

Config:

```yaml
oidc:
  enabled: true
  issuer_url: "https://authentik.example.com/application/o/kvm-switcher/"
  client_id: "your-client-id"
  client_secret_env: "OIDC_CLIENT_SECRET"
  redirect_url: "https://kvm.example.com/auth/callback"
  scopes: ["openid", "profile", "email", "groups"]
  role_claim: "groups"
  role_mappings:
    admin:
      servers: ["*"]
    ops-team:
      servers: ["server-1", "server-2"]
```

### Troubleshooting

| Issue | Solution |
|-------|----------|
| `Failed to initialize OIDC provider` | Check that `issuer_url` is reachable and serves `/.well-known/openid-configuration` |
| Login redirects but callback fails | Verify `redirect_url` matches exactly what's registered in the IdP |
| User logs in but sees no servers | Check that the user's groups/roles match entries in `role_mappings` and the `role_claim` is correct |
| `OIDC token exchange failed` | Verify `client_secret_env` is set and the secret is correct |
| `oidc: role "x" references unknown server "y"` | Server name in `role_mappings` doesn't match any entry in `servers` |
