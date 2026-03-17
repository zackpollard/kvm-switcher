import { test, expect } from '@playwright/test';
import { registerServiceWorker, createIPMISession, navigateToIPMI, waitForDashboard } from './helpers';

/**
 * AMI MegaRAC IPMI web interface E2E tests against a real device.
 *
 * Prerequisites:
 *   - Server running at KVM_BASE_URL (default http://localhost:8080)
 *   - BMC_PASSWORD_BROCK env var set
 *   - MegaRAC server (brock) reachable from the server
 *
 * Run: cd tests/e2e && npx playwright test ipmi-megarac
 */

const SERVER = 'brock';

test.describe('MegaRAC IPMI', () => {
  test('session creation returns credentials', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    const session = await createIPMISession(page, SERVER);

    expect(session.board_type).toBe('ami_megarac');
    expect(session.session_cookie).toBeTruthy();
    expect(session.csrf_token).toBeTruthy();
    expect(session.username).toBeTruthy();
    expect(typeof session.privilege).toBe('number');
    expect(typeof session.extended_priv).toBe('number');
  });

  test('auto-login reaches dashboard without user interaction', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    const session = await createIPMISession(page, SERVER);

    // Set BMC cookies that the MegaRAC web UI expects
    await page.evaluate((s) => {
      document.cookie = `SessionCookie=${s.session_cookie};path=/`;
      document.cookie = `CSRFTOKEN=${s.csrf_token};path=/`;
      document.cookie = `Username=${s.username};path=/`;
      document.cookie = `PNO=${s.privilege};path=/`;
      document.cookie = `Extendedpriv=${s.extended_priv};path=/`;
      document.cookie = 'SessionExpired=;expires=Thu, 01 Jan 1970 00:00:00 GMT;path=/';
    }, session);

    const ipmiPage = await navigateToIPMI(context, SERVER);
    await waitForDashboard(ipmiPage, 'Megarac', 60000);

    const state = await ipmiPage.evaluate(() => ({
      title: document.title,
      url: window.location.href,
      // MegaRAC uses a frameset — check for frames
      hasFrames: document.querySelectorAll('frame, frameset').length > 0,
    }));

    expect(state.title).toBeTruthy();
    expect(state.hasFrames).toBe(true);
  });

  test('logout interception preserves session', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    await createIPMISession(page, SERVER);

    // Call logout through proxy — should be intercepted
    const logoutResp = await page.evaluate(async (server: string) => {
      const res = await fetch(`/__bmc/${server}/rpc/WEBSES/logout.asp`);
      return { status: res.status, body: await res.text() };
    }, SERVER);

    expect(logoutResp.status).toBe(200);

    // Session should still work — CSRF token endpoint should respond
    const csrfResp = await page.evaluate(async (server: string) => {
      const res = await fetch(`/__bmc/${server}/rpc/WEBSES/getcsrftoken.asp`);
      const text = await res.text();
      return {
        status: res.status,
        hasToken: text.includes('CSRFToken'),
        hapiStatus: text.match(/HAPI_STATUS:(\d+)/)?.[1],
      };
    }, SERVER);

    expect(csrfResp.status).toBe(200);
    expect(csrfResp.hasToken).toBe(true);
    expect(csrfResp.hapiStatus).toBe('0');
  });

  test('proxied API calls carry correct credentials', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    await createIPMISession(page, SERVER);

    const hostStatus = await page.evaluate(async (server: string) => {
      const res = await fetch(`/__bmc/${server}/rpc/hoststatus.asp`);
      const text = await res.text();
      return {
        status: res.status,
        hasJFState: text.includes('JF_STATE'),
        hapiStatus: text.match(/HAPI_STATUS:(\d+)/)?.[1],
      };
    }, SERVER);

    expect(hostStatus.status).toBe(200);
    expect(hostStatus.hasJFState).toBe(true);
    expect(hostStatus.hapiStatus).toBe('0');
  });

  test('status API reports MegaRAC device info', async ({ page }) => {
    await page.goto('/');
    const status = await page.evaluate(async (server: string) => {
      for (let i = 0; i < 20; i++) {
        const res = await fetch('/api/server-status');
        const data = await res.json();
        if (data[server]?.power_state) return data[server];
        await new Promise(r => setTimeout(r, 3000));
      }
      const res = await fetch('/api/server-status');
      return (await res.json())[server] || {};
    }, SERVER);

    expect(status.online).toBe(true);
    expect(status.power_state).toBe('on');
    expect(status.model).toBeTruthy();
    expect(status.temperature_c).toBeGreaterThan(0);
  });
});
