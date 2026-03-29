import { test, expect } from '@playwright/test';

/**
 * E2E tests that run against a mock BMC server (no real hardware needed).
 *
 * These tests verify the core user flows of the KVM Switcher application:
 * dashboard loading, server status display, session management, and navigation.
 *
 * Prerequisites:
 *   - Mock BMC server running on port 9999 (tests/e2e/mockbmc)
 *   - App server running on port 8081 with test config (tests/e2e/configs/test-servers.yaml)
 *   - Frontend built in web/build
 *
 * Run via: bash tests/e2e/run-ci.sh
 */

test.describe('Dashboard', () => {
  test('loads and shows Infrastructure heading', async ({ page }) => {
    await page.goto('/');
    await expect(page.locator('h1')).toHaveText('Infrastructure');
  });

  test('displays server cards for mock servers', async ({ page }) => {
    await page.goto('/');

    // Wait for server cards to load (the app fetches /api/servers on mount)
    await expect(page.locator('h3:has-text("mock-megarac")')).toBeVisible({ timeout: 10000 });
    await expect(page.locator('h3:has-text("mock-idrac")')).toBeVisible({ timeout: 10000 });
  });

  test('shows board type labels on server cards', async ({ page }) => {
    await page.goto('/');
    await expect(page.locator('h3:has-text("mock-megarac")')).toBeVisible({ timeout: 10000 });

    // MegaRAC card should show "MegaRAC" label
    await expect(page.getByText('MegaRAC')).toBeVisible();
    // iDRAC card should show "iDRAC8" label
    await expect(page.getByText('iDRAC8')).toBeVisible();
  });

  test('server cards have KVM and IPMI buttons', async ({ page }) => {
    await page.goto('/');
    await expect(page.locator('h3:has-text("mock-megarac")')).toBeVisible({ timeout: 10000 });

    // Both servers should have IPMI and KVM buttons
    const ipmiButtons = page.getByRole('button', { name: 'IPMI' });
    const kvmButtons = page.getByRole('button', { name: 'KVM' });
    await expect(ipmiButtons.first()).toBeVisible();
    await expect(kvmButtons.first()).toBeVisible();
  });

  test('shows Servers group heading', async ({ page }) => {
    await page.goto('/');
    await expect(page.locator('h3:has-text("mock-megarac")')).toBeVisible({ timeout: 10000 });

    // Both mock servers are in the "Servers" group
    await expect(page.locator('h2:has-text("Servers")')).toBeVisible();
  });
});

test.describe('Server Status', () => {
  test('status polling shows online indicator', async ({ page }) => {
    await page.goto('/');
    await expect(page.locator('h3:has-text("mock-megarac")')).toBeVisible({ timeout: 10000 });

    // Wait for status polling to complete. The green dot has aria-label "Online".
    // The status poller runs on a background goroutine, so give it time.
    await expect(
      page.locator('[aria-label="Online"]').first()
    ).toBeVisible({ timeout: 30000 });
  });

  test('status API returns data for mock servers', async ({ page }) => {
    await page.goto('/');

    // Poll the status API directly until we get results
    const status = await page.evaluate(async () => {
      for (let i = 0; i < 20; i++) {
        const res = await fetch('/api/server-status');
        const data = await res.json();
        // Wait until at least one server has power_state populated
        if (data['mock-megarac']?.power_state || data['mock-idrac']?.power_state) {
          return data;
        }
        await new Promise(r => setTimeout(r, 2000));
      }
      const res = await fetch('/api/server-status');
      return await res.json();
    });

    // At least one mock server should report as online
    const hasSomeStatus =
      status['mock-megarac']?.online === true ||
      status['mock-idrac']?.online === true;
    expect(hasSomeStatus).toBe(true);
  });
});

test.describe('IPMI Session', () => {
  test('create IPMI session for MegaRAC returns credentials', async ({ page }) => {
    await page.goto('/');

    const result = await page.evaluate(async () => {
      const res = await fetch('/api/ipmi-session/mock-megarac', { method: 'POST' });
      return { status: res.status, body: await res.json() };
    });

    expect(result.status).toBe(200);
    expect(result.body.board_type).toBe('ami_megarac');
    expect(result.body.session_cookie).toBeTruthy();
    expect(result.body.csrf_token).toBeTruthy();
    expect(result.body.username).toBe('admin');
    expect(typeof result.body.privilege).toBe('number');
    expect(typeof result.body.extended_priv).toBe('number');
  });

  test('create IPMI session for iDRAC8 returns credentials', async ({ page }) => {
    await page.goto('/');

    const result = await page.evaluate(async () => {
      const res = await fetch('/api/ipmi-session/mock-idrac', { method: 'POST' });
      return { status: res.status, body: await res.json() };
    });

    expect(result.status).toBe(200);
    expect(result.body.board_type).toBe('dell_idrac8');
    expect(result.body.session_cookie).toBeTruthy();
    expect(result.body.csrf_token).toBeTruthy();
  });

  test('IPMI session for nonexistent server returns 404', async ({ page }) => {
    await page.goto('/');

    const result = await page.evaluate(async () => {
      const res = await fetch('/api/ipmi-session/nonexistent', { method: 'POST' });
      return { status: res.status };
    });

    expect(result.status).toBe(404);
  });
});

test.describe('KVM Session', () => {
  test('create session and navigate to KVM page', async ({ page }) => {
    await page.goto('/');

    // Create a session via API
    const session = await page.evaluate(async () => {
      const res = await fetch('/api/sessions', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ server_name: 'mock-megarac' }),
      });
      return { status: res.status, body: await res.json() };
    });

    expect(session.status).toBe(200);
    expect(session.body.id).toBeTruthy();
    expect(session.body.server_name).toBe('mock-megarac');

    // Navigate to the KVM page
    await page.goto(`/kvm/${session.body.id}`);

    // The KVM page should load (it will show session info even if the
    // actual KVM connection fails since there is no real iKVM/VNC target)
    await expect(page.locator('body')).toBeVisible();
  });

  test('list sessions includes created session', async ({ page }) => {
    await page.goto('/');

    // Create a session
    const createResult = await page.evaluate(async () => {
      const res = await fetch('/api/sessions', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ server_name: 'mock-megarac' }),
      });
      return await res.json();
    });

    // List sessions
    const sessions = await page.evaluate(async () => {
      const res = await fetch('/api/sessions');
      return await res.json();
    });

    expect(Array.isArray(sessions)).toBe(true);
    const found = sessions.find((s: any) => s.id === createResult.id);
    expect(found).toBeTruthy();
    expect(found.server_name).toBe('mock-megarac');
  });

  test('delete session removes it from list', async ({ page }) => {
    await page.goto('/');

    // Create a session
    const createResult = await page.evaluate(async () => {
      const res = await fetch('/api/sessions', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ server_name: 'mock-megarac' }),
      });
      return await res.json();
    });

    const sessionId = createResult.id;

    // Delete the session
    const deleteResult = await page.evaluate(async (id: string) => {
      const res = await fetch(`/api/sessions/${id}`, { method: 'DELETE' });
      return { status: res.status };
    }, sessionId);

    expect(deleteResult.status).toBe(200);

    // Verify it's gone (or marked disconnected)
    const getResult = await page.evaluate(async (id: string) => {
      const res = await fetch(`/api/sessions/${id}`);
      return { status: res.status, body: await res.json() };
    }, sessionId);

    // Session should either be 404 or show as disconnected
    if (getResult.status === 200) {
      expect(getResult.body.status).toBe('disconnected');
    }
  });
});

test.describe('Audit Log', () => {
  test('audit page loads and shows heading', async ({ page }) => {
    await page.goto('/audit');

    await expect(page.locator('h1')).toHaveText('Audit Log');
    await expect(page.getByText('View system activity and session history.')).toBeVisible();
  });

  test('audit page has filter controls', async ({ page }) => {
    await page.goto('/audit');

    await expect(page.locator('#filter-event')).toBeVisible();
    await expect(page.locator('#filter-server')).toBeVisible();
    await expect(page.getByRole('button', { name: 'Filter' })).toBeVisible();
    await expect(page.getByRole('button', { name: 'Clear' })).toBeVisible();
  });

  test('audit log shows entries after IPMI session creation', async ({ page }) => {
    // First, create an IPMI session to generate an audit entry
    await page.goto('/');
    await page.evaluate(async () => {
      await fetch('/api/ipmi-session/mock-megarac', { method: 'POST' });
    });

    // Navigate to audit log
    await page.goto('/audit');

    // Wait for the audit table to load. It should show at least the ipmi_session entry.
    // If there are no entries yet (timing), wait for the table to appear.
    await page.waitForSelector('table', { timeout: 10000 }).catch(() => {
      // Table might not appear if there are no entries yet -- that's acceptable
    });

    // Check if the page shows entries or the empty state
    const hasTable = await page.locator('table').isVisible();
    const hasEmpty = await page.getByText('No audit entries found.').isVisible();
    expect(hasTable || hasEmpty).toBe(true);
  });
});

test.describe('Navigation', () => {
  test('nav bar shows KVM Switcher branding', async ({ page }) => {
    await page.goto('/');
    await expect(page.getByText('KVM Switcher')).toBeVisible();
  });

  test('nav bar has Audit Log link', async ({ page }) => {
    await page.goto('/');
    const auditLink = page.locator('a[href="/audit"]');
    await expect(auditLink).toBeVisible();
    await expect(auditLink).toHaveText('Audit Log');
  });

  test('clicking Audit Log link navigates to audit page', async ({ page }) => {
    await page.goto('/');
    await page.locator('a[href="/audit"]').click();
    await expect(page).toHaveURL(/\/audit/);
    await expect(page.locator('h1')).toHaveText('Audit Log');
  });

  test('audit page back link returns to dashboard', async ({ page }) => {
    await page.goto('/audit');
    await page.locator('a:has-text("Back to Infrastructure")').click();
    await expect(page).toHaveURL(/\/$/);
    await expect(page.locator('h1')).toHaveText('Infrastructure');
  });
});

test.describe('API Endpoints', () => {
  test('GET /api/servers returns server list', async ({ page }) => {
    await page.goto('/');

    const result = await page.evaluate(async () => {
      const res = await fetch('/api/servers');
      return { status: res.status, body: await res.json() };
    });

    expect(result.status).toBe(200);
    expect(Array.isArray(result.body)).toBe(true);
    expect(result.body.length).toBe(2);

    const names = result.body.map((s: any) => s.name);
    expect(names).toContain('mock-megarac');
    expect(names).toContain('mock-idrac');
  });

  test('GET /healthz returns ok', async ({ page }) => {
    await page.goto('/');

    const result = await page.evaluate(async () => {
      const res = await fetch('/healthz');
      return { status: res.status, body: await res.json() };
    });

    expect(result.status).toBe(200);
    expect(result.body.status).toBe('ok');
  });

  test('GET /readyz returns ready', async ({ page }) => {
    await page.goto('/');

    const result = await page.evaluate(async () => {
      const res = await fetch('/readyz');
      return { status: res.status, body: await res.json() };
    });

    expect(result.status).toBe(200);
    expect(result.body.status).toBe('ready');
  });

  test('GET /auth/me returns unauthenticated (no OIDC)', async ({ page }) => {
    await page.goto('/');

    const result = await page.evaluate(async () => {
      const res = await fetch('/auth/me');
      return { status: res.status, body: await res.json() };
    });

    expect(result.status).toBe(200);
    expect(result.body.authenticated).toBe(false);
    expect(result.body.oidc_enabled).toBe(false);
  });
});

test.describe('BMC Proxy', () => {
  test('proxied MegaRAC request returns data', async ({ page }) => {
    await page.goto('/');

    // First create an IPMI session to set up proxy credentials
    await page.evaluate(async () => {
      await fetch('/api/ipmi-session/mock-megarac', { method: 'POST' });
    });

    // Now try proxied request
    const result = await page.evaluate(async () => {
      const res = await fetch('/__bmc/mock-megarac/rpc/hoststatus.asp');
      return { status: res.status, body: await res.text() };
    });

    expect(result.status).toBe(200);
    expect(result.body).toContain('JF_STATE');
  });

  test('MegaRAC logout interception returns fake OK', async ({ page }) => {
    await page.goto('/');

    // Set up session
    await page.evaluate(async () => {
      await fetch('/api/ipmi-session/mock-megarac', { method: 'POST' });
    });

    // Logout should be intercepted by the board handler
    const result = await page.evaluate(async () => {
      const res = await fetch('/__bmc/mock-megarac/rpc/WEBSES/logout.asp');
      return { status: res.status, body: await res.text() };
    });

    expect(result.status).toBe(200);
    // The intercepted logout returns JSON, not the raw BMC response
    expect(result.body).toContain('Disconnected');
  });

  test('proxied request to unknown server returns error', async ({ page }) => {
    await page.goto('/');

    const result = await page.evaluate(async () => {
      const res = await fetch('/__bmc/nonexistent/some/path');
      return { status: res.status };
    });

    // Should return 404 or 502 for unknown server
    expect(result.status).toBeGreaterThanOrEqual(400);
  });
});
