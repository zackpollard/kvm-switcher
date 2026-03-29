# Service Worker Routing Rules

**Source:** `web/static/sw.js`

## Purpose

The service worker intercepts HTTP requests from BMC web UI pages loaded inside the KVM Switcher app and routes them through the backend proxy. Without it, BMC pages that use absolute paths (e.g., `/page/login.html`, `/rpc/WEBSES/create`) would hit the app server directly instead of being proxied to the correct BMC.

The SW rewrites URLs from `/ipmi/{name}/...` to `/__bmc/{name}/...`, which is the backend's BMC reverse proxy endpoint.

## URL Rewriting

All BMC traffic flows through this rewrite:

```
Browser request:  /ipmi/{name}/page/login.html
    |
    v  (service worker)
Fetch to backend: /__bmc/{name}/page/login.html
    |
    v  (Go reverse proxy)
BMC request:      https://{bmc-ip}/page/login.html
```

The `/ipmi/{name}/` prefix is visible in the browser URL bar. The `/__bmc/{name}/` prefix is internal -- the backend proxy strips it and forwards to the real BMC.

## Server Resolution

When a request arrives, the SW must determine which BMC server it belongs to. Three strategies are tried in order:

### 1. clientId Map

```js
clientServerMap.get(event.clientId)
```

A `Map<clientId, serverName>` tracks which browser client (tab/frame) is associated with which server. Updated on every `/ipmi/{name}/` navigation and inherited by `resultingClientId` during page transitions.

### 2. Referer Header

If the clientId has no mapping, the `Referer` header is checked for `/ipmi/{name}/` patterns. If the referer is a non-app BMC path (e.g., `/page/login.html`), falls through to `lastActiveServer`.

### 3. lastActiveServer

A global variable holding the most recently accessed server name. Used as a last resort for requests that have lost their client mapping (e.g., after BMC JS navigates the top frame to `/index.html`).

Set on every `/ipmi/{name}/` request. Cleared on app-route navigations (`/`, `/kvm/`, etc.) to prevent stale routing.

## App Route Passthrough

These paths are never intercepted by the SW:

| Path | Reason |
|------|--------|
| `/` | SvelteKit app root |
| `/kvm/` | KVM viewer pages |
| `/api/` | App REST API |
| `/_app/` | SvelteKit static assets |
| `/auth/` | OIDC authentication |
| `/ws/` | WebSocket endpoints |
| `/sw.js` | Service worker itself |
| `/__bmc/` | Already-rewritten proxy requests |

When navigating to an app route, `lastActiveServer` is cleared to prevent subsequent requests (favicon, etc.) from being misrouted to a BMC.

## NanoKVM Special Case

NanoKVM devices use `/api/` and `/ws/` paths in their SPA, which would normally pass through as app routes. The SW detects NanoKVM servers during auto-login (when the backend response includes an `x-kvm-nanotoken` header) and adds them to `nanoKVMServers`.

For clients mapped to a NanoKVM server, `/api/*` and `/ws/*` requests are intercepted and proxied to the device via `/__bmc/{name}/api/...` instead of hitting the app's own API.

Additionally, the NanoKVM's JWT token is injected as a `document.cookie` (`nano-kvm-token=...`) so the NanoKVM SPA can find it.

## Auto-Login Injection

When the backend response includes `x-kvm-autologin: true` on a navigation to an HTML page, the SW injects a login script before `</body>`. Two BMC types are handled:

### iDRAC8 (`login.html`)

Detected by the presence of `frmSubmit` and `dataarea` in the HTML.

- Hides `#dataarea` via CSS.
- Polls for the login form to become visible (200ms interval, 30s timeout).
- Fills username/password with `"."` (dummy values -- the proxy intercepts the POST).
- Calls `frmSubmit()`.

### iDRAC9 (`start.html`)

Detected by the presence of `angular` and `logincontroller` in the HTML.

- Hides the form/login-container via CSS.
- Waits for Angular to initialize and the submit button to appear.
- Sets credentials via Angular's `scope.$apply()` so Angular's internal state is updated.
- Clicks the submit button after 100ms delay.

In both cases, dummy credentials are used because the backend proxy intercepts the login POST and substitutes real credentials before it reaches the BMC.

## Navigation Handling

BMC web UIs frequently navigate via JavaScript (e.g., `top.location = "/login.html"`), which loses the `/ipmi/{name}/` prefix. The SW handles this:

1. For navigation requests to non-`/ipmi/` paths that resolve to a server via `resolveServer()`, the SW returns a `302` redirect to `/ipmi/{name}/{original-path}` so the URL bar stays correct.

2. For fetch responses that followed a backend redirect (i.e., `resp.url` differs from the requested URL), the SW returns an HTML page with `location.replace()` pointing to the correct `/ipmi/{name}/` path. This JS-based redirect is used instead of `Response.redirect()` to work around Firefox issues with SW redirect responses on navigations.

3. Both `event.clientId` and `event.resultingClientId` are mapped to the server name during navigations, so the new page inherits the server association.

## Debug Logging

Set `const DEBUG = true;` at the top of `sw.js` to enable `console.debug` output. Logs cover:
- Server resolution decisions (clientId map hit, Referer extraction, lastActiveServer fallback)
- `/ipmi/` route matching and path rewriting
- NanoKVM `/api/` interception
- `lastActiveServer` clearing on app-route navigation
- Client-to-server mapping updates

The flag is `false` by default.

## Known Issues / Edge Cases

### lastActiveServer Persistence

`lastActiveServer` is a global variable that persists for the SW's lifetime. If a user accesses server A, then navigates to the app root, then opens a BMC page that uses absolute paths (without going through `/ipmi/{name}/`), the stale `lastActiveServer` could route requests to the wrong server. Mitigation: it is cleared on app-route navigations, but not on SW restart (service workers can be killed and restarted by the browser at any time, which resets the variable to `null`).

### False Server Name Extraction

Relative paths like `../images/progress.gif` on a BMC page resolve to `/ipmi/images/progress.gif`, where `images` looks like a server name. The SW mitigates this with a `knownServers` set -- only names seen in a `navigate`-mode request are trusted. Sub-resource requests to `/ipmi/{name}/` where `name` is not in `knownServers` are treated as false extractions: the `/ipmi/` prefix is stripped and the request is proxied via the resolved server instead.

### Set-Cookie Headers

Browsers block `Set-Cookie` headers in service worker responses. The SW strips them from proxied responses. BMC session cookies are managed by the backend proxy instead.

### Content-Encoding Mismatch

The Go backend decompresses gzip responses from BMCs, but `Content-Encoding` and `Content-Length` headers may still be present. The SW strips both to prevent the browser from attempting to decompress already-decompressed content.
