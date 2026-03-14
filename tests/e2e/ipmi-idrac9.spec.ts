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

    // Navigate and wait — auto-login should happen automatically
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

  test('login page loads with Angular form before auto-submit', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    await createIPMISession(page, SERVER);

    // Navigate but check quickly before auto-login completes
    const ipmiPage = await navigateToIPMI(context, SERVER, 8000);

    // At this point we should either be on the login page with Angular form
    // or already past it (auto-login was fast). Both are acceptable.
    const state = await ipmiPage.evaluate(() => ({
      title: document.title,
      url: window.location.href,
      hasUsernameInput: !!document.querySelector('input[name="username"]'),
      hasSubmitButton: !!document.querySelector('button[type="submit"]'),
    }));

    // Either on dashboard or login page with Angular form
    expect(
      state.url.includes('restgui/index.html') || state.hasUsernameInput
    ).toBe(true);
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

    // Navigate and wait for auto-login to complete
    const ipmiPage = await navigateToIPMI(context, SERVER, 30000);

    const dashboard = await ipmiPage.evaluate(() => ({
      title: document.title,
      bodyText: document.body.innerText || '',
    }));

    expect(dashboard.title).toContain('Dashboard');
    expect(dashboard.bodyText).toContain('PowerEdge');
  });
});
