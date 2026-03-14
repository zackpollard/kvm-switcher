// Service Worker for IPMI BMC proxy
// Intercepts requests from BMC pages and rewrites them to /__bmc/{name}/...

const clientServerMap = new Map(); // clientId -> serverName
const knownServers = new Set(); // server names confirmed via navigation

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
		// For navigation requests, the name is authoritative — add to known set.
		// For sub-resource requests, only trust the name if we've seen it before
		// in a navigation. This prevents false extractions like /ipmi/images/progress.gif
		// (from ../images/progress.gif relative paths) from being treated as server names.
		if (event.request.mode === 'navigate') {
			knownServers.add(name);
		}

		if (knownServers.has(name)) {
			const rest = path.slice('/ipmi/'.length + name.length) || '/';
			if (event.clientId) clientServerMap.set(event.clientId, name);
			if (event.resultingClientId) clientServerMap.set(event.resultingClientId, name);
			lastActiveServer = name;
			event.respondWith(proxyToBMC(event.request, name, rest + url.search));
			return;
		}

		// False extraction (e.g., /ipmi/images/progress.gif from a relative
		// ../images/progress.gif on a BMC page). Strip the /ipmi/ prefix to
		// recover the real BMC path and proxy via the active server.
		const bmcPath = '/' + path.slice('/ipmi/'.length);
		const serverName = resolveServer(event);
		if (serverName) {
			event.respondWith(proxyToBMC(event.request, serverName, bmcPath + url.search));
			return;
		}
	}

	// Resolve which BMC server this request belongs to
	let serverName = resolveServer(event);

	if (serverName) {
		// For navigation requests not already under /ipmi/{name}/, redirect
		// so the browser URL bar shows the correct prefixed path. This
		// prevents the BMC's JS navigations (e.g., top.location = "/login.html")
		// from losing the /ipmi/ prefix.
		if (event.request.mode === 'navigate') {
			const redirectUrl = '/ipmi/' + serverName + path + url.search;
			event.respondWith(Response.redirect(redirectUrl, 302));
			return;
		}
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
		const bmcPrefix = '/__bmc/' + name;
		const ipmiPrefix = '/ipmi/' + name;

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

		// If the fetch followed a redirect (resp.url differs from bmcUrl),
		// and this is a navigation request, send a redirect response to the
		// browser so the URL bar updates. This ensures relative paths in the
		// BMC HTML resolve correctly (e.g., iDRAC9 redirects / →
		// /restgui/start.html, and CSS paths must be relative to that path).
		if (request.mode === 'navigate' && resp.url) {
			const respUrl = new URL(resp.url);
			const respPath = respUrl.pathname;
			if (respPath.startsWith(bmcPrefix) && respPath !== bmcPrefix + path.split('?')[0]) {
				const newPath = ipmiPrefix + respPath.slice(bmcPrefix.length);
				return Response.redirect(newPath + respUrl.search, 302);
			}
		}

		// Build a clean response — navigation responses from respondWith()
		// can fail if the original has Set-Cookie headers or is flagged as
		// redirected. Constructing a fresh Response avoids these issues.
		const headers = new Headers();
		for (const [key, value] of resp.headers) {
			const lower = key.toLowerCase();
			// Set-Cookie: browsers block this in SW responses.
			// Content-Encoding: fetch() already decompressed the body, so
			//   keeping this header causes the browser to try to decompress
			//   again, producing corrupt data or triggering a file download.
			// Content-Length: invalid after decompression (size changed).
			if (lower === 'set-cookie' || lower === 'content-encoding' || lower === 'content-length') {
				continue;
			}
			headers.set(key, value);
		}

		// For navigation requests to login pages, inject an auto-login script
		// if the proxy signals that cached credentials are available.
		let body = resp.body;
		if (request.mode === 'navigate' && resp.headers.get('x-kvm-autologin') === 'true') {
			const ct = (resp.headers.get('content-type') || '').toLowerCase();
			if (ct.includes('text/html')) {
				const html = await resp.text();
				body = injectAutoLoginScript(html, path);
			}
		}

		return new Response(body, {
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

// Auto-login script for iDRAC8 login.html
// Waits for the login form to become visible (after AJAX loads locale),
// fills dummy credentials (intercepted by the proxy), and submits.
const IDRAC8_AUTO_LOGIN = `<script>
(function() {
	var t = setInterval(function() {
		var da = document.getElementById('dataarea');
		if (da && da.style.visibility === 'visible') {
			clearInterval(t);
			var u = document.querySelector('input[name="user"]');
			var p = document.querySelector('input[name="password"]');
			if (u && p) {
				u.value = 'root';
				p.value = 'auto';
				if (typeof frmSubmit === 'function') frmSubmit();
			}
		}
	}, 200);
	setTimeout(function() { clearInterval(t); }, 30000);
})();
</script>`;

// Auto-login script for iDRAC9 start.html (Angular)
// Waits for the Angular form to initialize, fills credentials, and clicks submit.
const IDRAC9_AUTO_LOGIN = `<script>
(function() {
	var t = setInterval(function() {
		var btn = document.querySelector('button[type="submit"]');
		var uInput = document.querySelector('input[name="username"]');
		if (btn && uInput && window.angular) {
			var form = document.querySelector('form');
			if (!form) return;
			var scope = angular.element(form).scope();
			if (scope && scope.config) {
				clearInterval(t);
				scope.$apply(function() {
					scope.config.username = 'root';
					scope.config.password = 'auto';
				});
				setTimeout(function() { btn.click(); }, 100);
			}
		}
	}, 200);
	setTimeout(function() { clearInterval(t); }, 30000);
})();
</script>`;

// Inject auto-login script into login page HTML based on URL path.
function injectAutoLoginScript(html, path) {
	// iDRAC8: login.html
	if (path.endsWith('/login.html') || path === '/login.html') {
		// Only inject if this looks like an iDRAC8 login page
		if (html.includes('frmSubmit') && html.includes('dataarea')) {
			return html.replace('</body>', IDRAC8_AUTO_LOGIN + '</body>');
		}
	}

	// iDRAC9: start.html (Angular login page)
	if (path.endsWith('/start.html') || path === '/start.html') {
		if (html.includes('angular') && html.includes('logincontroller')) {
			return html.replace('</body>', IDRAC9_AUTO_LOGIN + '</body>');
		}
	}

	return html;
}
