import { test, expect } from '@playwright/test';
import { registerServiceWorker, createIPMISession, navigateToIPMI, waitForDashboard } from './helpers';

/**
 * APC NMC2 (UPS/PDU) E2E tests against real devices.
 *
 * Prerequisites:
 *   - Server running at KVM_BASE_URL (default http://localhost:8080)
 *   - APC_PASSWORD env var set
 *   - APC PDU (pdu-1) and/or UPS (ups-1) reachable from the server
 *
 * Run: cd tests/e2e && npx playwright test apc
 */

const PDU_SERVER = 'pdu-1';

test.describe('APC PDU', () => {
  test('session creation succeeds', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    const session = await createIPMISession(page, PDU_SERVER);

    expect(session.board_type).toBe('apc_ups');
    // APC uses URL-based auth — no session cookie or CSRF token
    // Just verify the session was created without error
  });

  test('panel loads with login bypass (no login form)', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    await createIPMISession(page, PDU_SERVER);

    const panelPage = await navigateToIPMI(context, PDU_SERVER);
    await waitForDashboard(panelPage, 'Network Management Card', 30000);

    const state = await panelPage.evaluate(() => ({
      title: document.title,
      url: window.location.href,
      // APC login page has a form with login_username field
      hasLoginForm: !!document.querySelector('input[name="login_username"]'),
      // Dashboard has the app-name div
      hasDashboard: !!document.querySelector('#app-name') || !!document.querySelector('.app-primary-name'),
    }));

    expect(state.title).toContain('Network Management Card');
    expect(state.hasLoginForm).toBe(false);
    expect(state.hasDashboard).toBe(true);
  });

  test('dashboard shows device information', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    await createIPMISession(page, PDU_SERVER);

    const panelPage = await navigateToIPMI(context, PDU_SERVER);
    await waitForDashboard(panelPage, 'Network Management Card', 30000);

    const info = await panelPage.evaluate(() => ({
      bodyText: document.body.innerText || '',
    }));

    // PDU dashboard should show load information
    expect(info.bodyText).toContain('Device Load');
  });

  test('navigation within panel works (sub-pages load)', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    await createIPMISession(page, PDU_SERVER);

    const panelPage = await navigateToIPMI(context, PDU_SERVER);
    await waitForDashboard(panelPage, 'Network Management Card', 30000);

    // Navigate to the about page (relative link within the APC UI)
    const aboutResp = await panelPage.evaluate(async (server: string) => {
      const res = await fetch(`/__bmc/${server}/aboutpdu.htm`);
      const text = await res.text();
      return {
        status: res.status,
        hasModel: text.includes('Model Number'),
      };
    }, PDU_SERVER);

    expect(aboutResp.status).toBe(200);
    expect(aboutResp.hasModel).toBe(true);
  });

  test('root URL redirects to dashboard (login bypass)', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    await createIPMISession(page, PDU_SERVER);

    // Direct proxy request to root — should redirect to home.htm
    const resp = await page.evaluate(async (server: string) => {
      const res = await fetch(`/__bmc/${server}/`, { redirect: 'manual' });
      return {
        status: res.status,
        location: res.headers.get('location'),
      };
    }, PDU_SERVER);

    expect(resp.status).toBe(302);
    expect(resp.location).toContain('home.htm');
  });

  test('status API reports PDU stats', async ({ page }) => {
    await page.goto('/');
    // Wait for session manager + status poller
    const status = await page.evaluate(async (server: string) => {
      for (let i = 0; i < 20; i++) {
        const res = await fetch('/api/server-status');
        const data = await res.json();
        if (data[server]?.load_watts) return data[server];
        await new Promise(r => setTimeout(r, 3000));
      }
      const res = await fetch('/api/server-status');
      return (await res.json())[server] || {};
    }, PDU_SERVER);

    expect(status.online).toBe(true);
    expect(status.power_state).toBe('on');
    expect(status.model).toBeTruthy();
    expect(status.load_watts).toBeGreaterThan(0);
    expect(status.voltage).toBeGreaterThan(0);
  });
});

test.describe('APC UPS', () => {
  const UPS_SERVER = 'ups-1';

  test('panel loads with login bypass', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    await createIPMISession(page, UPS_SERVER);

    const panelPage = await navigateToIPMI(context, UPS_SERVER);
    await waitForDashboard(panelPage, 'Network Management Card', 30000);

    const state = await panelPage.evaluate(() => ({
      title: document.title,
      hasLoginForm: !!document.querySelector('input[name="login_username"]'),
    }));

    expect(state.title).toContain('Network Management Card');
    expect(state.hasLoginForm).toBe(false);
  });

  test('status API reports UPS stats', async ({ page }) => {
    await page.goto('/');
    const status = await page.evaluate(async (server: string) => {
      for (let i = 0; i < 20; i++) {
        const res = await fetch('/api/server-status');
        const data = await res.json();
        if (data[server]?.battery_pct) return data[server];
        await new Promise(r => setTimeout(r, 3000));
      }
      const res = await fetch('/api/server-status');
      return (await res.json())[server] || {};
    }, UPS_SERVER);

    expect(status.online).toBe(true);
    expect(status.model).toBeTruthy();
    expect(status.battery_pct).toBeGreaterThan(0);
    expect(status.runtime_min).toBeGreaterThan(0);
    expect(status.voltage).toBeGreaterThan(0);
  });
});
