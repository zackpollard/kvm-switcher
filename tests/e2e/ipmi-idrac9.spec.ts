import { test, expect } from '@playwright/test';
import { registerServiceWorker, createIPMISession, navigateToIPMI } from './helpers';

/**
 * Dell iDRAC9 IPMI web interface integration tests.
 *
 * Prerequisites:
 *   - Server running at KVM_BASE_URL (default http://localhost:8080)
 *   - BMC_PASSWORD_DELL env var set
 *   - iDRAC9 server (misty) reachable from the server
 *
 * Run: cd tests/e2e && npx playwright test ipmi-idrac9
 */

const SERVER = 'misty';

test.describe('iDRAC9 IPMI', () => {
  test('auto-login reaches dashboard without user interaction', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    const session = await createIPMISession(page, SERVER);
    expect(session.board_type).toBe('dell_idrac9');
    expect(session.session_cookie).toBeTruthy();

    // Navigate and wait for auto-login + Angular dashboard to load
    const ipmiPage = await navigateToIPMI(context, SERVER, 30000);

    const state = await ipmiPage.evaluate(() => ({
      title: document.title,
      url: window.location.href,
      bodyText: document.body.innerText?.substring(0, 500),
    }));

    expect(state.title).toContain('Dashboard');
    expect(state.url).toContain('restgui/index.html');
    expect(state.bodyText).toContain('Dashboard');
  });

  test('login form is hidden during auto-login', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    await createIPMISession(page, SERVER);

    const ipmiPage = await navigateToIPMI(context, SERVER, 5000);

    // The login form should be hidden by the injected CSS
    const state = await ipmiPage.evaluate(() => {
      const form = document.querySelector('form');
      if (!form) return { hasForm: false };
      const style = window.getComputedStyle(form);
      return {
        hasForm: true,
        formVisible: style.visibility !== 'hidden',
      };
    });

    // If the form exists, it should be hidden
    if (state.hasForm) {
      expect(state.formVisible).toBe(false);
    }
  });

  test('login interception returns cached session', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    await createIPMISession(page, SERVER);

    const loginResp = await page.evaluate(async (server: string) => {
      const res = await fetch(`/__bmc/${server}/sysmgmt/2015/bmc/session`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ UserName: 'root', Password: 'calvin' }),
      });
      return {
        status: res.status,
        xsrfToken: res.headers.get('xsrf-token'),
        body: await res.text(),
      };
    }, SERVER);

    expect(loginResp.status).toBe(201);
    expect(loginResp.body).toContain('"authResult":0');
    expect(loginResp.xsrfToken).toBeTruthy();
  });

  test('dashboard shows system information', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    await createIPMISession(page, SERVER);

    const ipmiPage = await navigateToIPMI(context, SERVER, 30000);

    const dashboard = await ipmiPage.evaluate(() => ({
      title: document.title,
      bodyText: document.body.innerText || '',
    }));

    expect(dashboard.title).toContain('Dashboard');
    expect(dashboard.bodyText).toContain('PowerEdge');
  });
});
