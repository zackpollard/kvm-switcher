import { test, expect } from '@playwright/test';
import { registerServiceWorker, createIPMISession, navigateToIPMI, waitForDashboard } from './helpers';

/**
 * APC NMC2 (UPS/PDU) E2E tests against real devices.
 *
 * Prerequisites:
 *   - Server running at KVM_BASE_URL (default http://localhost:8080)
 *   - APC_PASSWORD env var set
 *   - APC PDU (pdu-1) and/or UPS (ups-2) reachable from the server
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
      hasLoginForm: !!document.querySelector('input[name="login_username"]'),
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

    expect(info.bodyText).toContain('Device Load');
  });

  test('navigation within panel works (sub-pages load)', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    await createIPMISession(page, PDU_SERVER);

    const panelPage = await navigateToIPMI(context, PDU_SERVER);
    await waitForDashboard(panelPage, 'Network Management Card', 30000);

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

  test('login bypass redirects root to dashboard', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    await createIPMISession(page, PDU_SERVER);

    // Navigate to the IPMI root and verify we end up at the dashboard
    // (not the login page). The SW follows the redirect internally.
    const panelPage = await navigateToIPMI(context, PDU_SERVER);
    await waitForDashboard(panelPage, 'Network Management Card', 30000);

    const state = await panelPage.evaluate(() => ({
      title: document.title,
      hasLoginForm: !!document.querySelector('input[name="login_username"]'),
    }));

    // We should be on the dashboard, not the login page
    expect(state.title).toContain('Network Management Card');
    expect(state.hasLoginForm).toBe(false);
  });

  test('status API reports PDU stats', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    // Ensure session exists so poller can get detailed stats
    await createIPMISession(page, PDU_SERVER);

    // Wait for poller to pick up the session
    await page.waitForTimeout(35000);

    const status = await page.evaluate(async (server: string) => {
      const res = await fetch('/api/server-status');
      const data = await res.json();
      return data[server] || {};
    }, PDU_SERVER);

    expect(status.online).toBe(true);
    expect(status.power_state).toBe('on');
    expect(status.model).toBeTruthy();
    expect(status.load_watts).toBeGreaterThan(0);
    expect(status.voltage).toBeGreaterThan(0);
  });
});

test.describe('APC UPS', () => {
  const UPS_SERVER = 'ups-2';

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

  test('status API reports UPS stats', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    await createIPMISession(page, UPS_SERVER);

    await page.waitForTimeout(35000);

    const status = await page.evaluate(async (server: string) => {
      const res = await fetch('/api/server-status');
      const data = await res.json();
      return data[server] || {};
    }, UPS_SERVER);

    expect(status.online).toBe(true);
    expect(status.model).toBeTruthy();
  });
});
