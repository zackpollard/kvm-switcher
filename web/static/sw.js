// Service Worker for IPMI BMC proxy
// Intercepts requests from BMC pages and rewrites them to /__bmc/{name}/...

const clientServerMap = new Map(); // clientId -> serverName

// Last known server name — used as fallback when clientId and Referer
// both fail (e.g., after BMC JS navigates the top frame to "/index.html").
let lastActiveServer = null;

self.addEventListener('install', () => {
	self.skipWaiting();
});

self.addEventListener('activate', (event) => {
	event.waitUntil(self.clients.claim());
});

// Extract server name from a /ipmi/{name}/... path
function extractServerName(path) {
	const m = path.match(/^\/ipmi\/([^/]+)(\/.*)?$/);
	return m ? m[1] : null;
}

// App routes that should never be proxied to a BMC
function isAppRoute(path) {
	return (
		path.startsWith('/api/') ||
		path.startsWith('/auth/') ||
		path.startsWith('/ws/') ||
		path.startsWith('/_app/') ||
		path === '/sw.js' ||
		path.startsWith('/__bmc/')
	);
}

self.addEventListener('fetch', (event) => {
	const url = new URL(event.request.url);

	// Only handle same-origin requests
	if (url.origin !== self.location.origin) return;

	const path = url.pathname;

	// Passthrough: app traffic and internal proxy path
	if (isAppRoute(path)) return;

	// /ipmi/{name}/... -> extract name, track client, rewrite to /__bmc/{name}/...
	const name = extractServerName(path);
	if (name) {
		const rest = path.slice('/ipmi/'.length + name.length) || '/';
		// Map BOTH the initiating client and the resulting client (for navigations).
		// When the BMC does top.location = "/page/login.html", a new client is
		// created — we need it mapped so subsequent AJAX from that page resolves
		// to the correct server even if another tab changes lastActiveServer.
		if (event.clientId) clientServerMap.set(event.clientId, name);
		if (event.resultingClientId) clientServerMap.set(event.resultingClientId, name);
		lastActiveServer = name;
		event.respondWith(proxyToBMC(event.request, name, rest + url.search));
		return;
	}

	// Resolve which BMC server this request belongs to
	let serverName = resolveServer(event);

	if (serverName) {
		event.respondWith(proxyToBMC(event.request, serverName, path + url.search));
		return;
	}

	// Not mapped — pass through (regular app page)
});

// Map a resolved server name to both the initiating and resulting clients.
// For navigations (e.g., BMC JS doing top.location = "/page/login.html"),
// the resultingClientId is the NEW client that loads the page — we must map
// it so subsequent AJAX from that page routes to the correct server.
function trackClient(event, name) {
	if (event.clientId) clientServerMap.set(event.clientId, name);
	if (event.resultingClientId) clientServerMap.set(event.resultingClientId, name);
}

// Determine which BMC server a non-/ipmi/ request belongs to.
function resolveServer(event) {
	const clientId = event.clientId;

	// 1. Check client -> server map
	if (clientId) {
		const name = clientServerMap.get(clientId);
		if (name) {
			// Also map resultingClientId for navigations so the new page
			// inherits the server mapping from the old page.
			if (event.resultingClientId) clientServerMap.set(event.resultingClientId, name);
			return name;
		}
	}

	// 2. Check Referer for /ipmi/{name}/... pattern
	const referer = event.request.referrer;
	if (referer) {
		try {
			const refUrl = new URL(referer);
			if (refUrl.origin === self.location.origin) {
				const name = extractServerName(refUrl.pathname);
				if (name) {
					trackClient(event, name);
					return name;
				}

				// Referer is a non-/ipmi/ BMC page (e.g., /page/login.html after
				// the BMC redirected the top frame). Check if the referrer's client
				// is mapped, or fall through to lastActiveServer.
				// If referer is a non-app path, it's likely a BMC sub-page.
				if (!isAppRoute(refUrl.pathname) && refUrl.pathname !== '/') {
					if (lastActiveServer) {
						trackClient(event, lastActiveServer);
						return lastActiveServer;
					}
				}
			}
		} catch (e) {
			// invalid referer, ignore
		}
	}

	// 3. Fallback: if we're loading a non-app resource and have a lastActiveServer,
	//    it's almost certainly a BMC sub-resource. The main app only loads from
	//    /api/, /_app/, /auth/, /ws/ — all of which are excluded above.
	//    Paths like /index.html, /page/login.html, /rpc/... are BMC content.
	if (lastActiveServer) {
		// Don't intercept the root SPA page itself or known SvelteKit routes
		const path = new URL(event.request.url).pathname;
		if (path === '/' || path.startsWith('/kvm/')) {
			return null;
		}
		trackClient(event, lastActiveServer);
		return lastActiveServer;
	}

	return null;
}

async function proxyToBMC(request, name, path) {
	try {
		const bmcUrl = '/__bmc/' + name + path;

		// Forward request headers and body.
		// For navigation requests (mode=navigate), use minimal headers to
		// avoid Chrome rejecting the respondWith() response. For subresource
		// requests (XHR/fetch), forward all headers — BMC endpoints require
		// a CSRFTOKEN header for authenticated requests.
		const opts = {
			method: request.method,
			credentials: 'same-origin',
			redirect: 'follow'
		};

		if (request.mode !== 'navigate') {
			// Forward all headers from the original request
			opts.headers = new Headers(request.headers);
		}

		if (request.method !== 'GET' && request.method !== 'HEAD') {
			// Read body fully (not as stream) to avoid needing duplex: 'half'
			opts.body = await request.arrayBuffer();
		}

		const resp = await fetch(bmcUrl, opts);

		// Build a clean response — navigation responses from respondWith()
		// can fail if the original has Set-Cookie headers or is flagged as
		// redirected. Constructing a fresh Response avoids these issues.
		const headers = new Headers();
		for (const [key, value] of resp.headers) {
			// Browsers block Set-Cookie in SW responses; skip to avoid errors
			if (key.toLowerCase() !== 'set-cookie') {
				headers.set(key, value);
			}
		}

		return new Response(resp.body, {
			status: resp.status,
			statusText: resp.statusText,
			headers
		});
	} catch (err) {
		// Return a proper error response instead of rejecting the promise.
		// A rejected respondWith() causes the browser to fall back to network,
		// which would serve the SPA index.html and potentially cause a reload loop.
		return new Response('BMC unreachable: ' + err.message, {
			status: 502,
			statusText: 'Bad Gateway',
			headers: { 'Content-Type': 'text/plain' }
		});
	}
}
