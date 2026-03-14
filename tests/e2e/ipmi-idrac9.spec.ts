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
  test('login page loads with Angular form', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    await createIPMISession(page, SERVER);

    const ipmiPage = await navigateToIPMI(context, SERVER, 10000);

    const state = await ipmiPage.evaluate(() => ({
      title: document.title,
      hasUsernameInput: !!document.querySelector('input[name="username"]'),
      hasPasswordInput: !!document.querySelector('input[name="password"]'),
      hasSubmitButton: !!document.querySelector('button[type="submit"]'),
      bodyText: document.body.innerText?.substring(0, 200),
    }));

    expect(state.title).toContain('iDRAC9');
    expect(state.hasUsernameInput).toBe(true);
    expect(state.hasPasswordInput).toBe(true);
    expect(state.hasSubmitButton).toBe(true);
    expect(state.bodyText).toContain('Log In');
  });

  test('auto-login reaches dashboard', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    const session = await createIPMISession(page, SERVER);
    expect(session.board_type).toBe('dell_idrac9');
    expect(session.session_cookie).toBeTruthy();

    const ipmiPage = await navigateToIPMI(context, SERVER, 8000);

    // Fill Angular form — use keyboard since password field is readonly initially
    await ipmiPage.click('input[name="username"]');
    await ipmiPage.keyboard.type('root');
    await ipmiPage.press('input[name="username"]', 'Tab');
    await ipmiPage.waitForTimeout(500);
    await ipmiPage.keyboard.type('calvin');
    await ipmiPage.click('button[type="submit"]');

    // Wait for Angular to process login and load dashboard
    await ipmiPage.waitForTimeout(15000);

    const afterLogin = await ipmiPage.evaluate(() => ({
      title: document.title,
      url: window.location.href,
      bodyText: document.body.innerText?.substring(0, 500),
    }));

    expect(afterLogin.title).toContain('Dashboard');
    expect(afterLogin.url).toContain('restgui/index.html');
    expect(afterLogin.bodyText).toContain('Dashboard');
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

    const ipmiPage = await navigateToIPMI(context, SERVER, 8000);

    // Login via Angular
    await ipmiPage.click('input[name="username"]');
    await ipmiPage.keyboard.type('root');
    await ipmiPage.press('input[name="username"]', 'Tab');
    await ipmiPage.waitForTimeout(500);
    await ipmiPage.keyboard.type('calvin');
    await ipmiPage.click('button[type="submit"]');
    await ipmiPage.waitForTimeout(15000);

    const dashboard = await ipmiPage.evaluate(() => ({
      title: document.title,
      bodyText: document.body.innerText || '',
    }));

    expect(dashboard.title).toContain('Dashboard');
    expect(dashboard.bodyText).toContain('PowerEdge');
  });
});
