import { test, expect } from '@playwright/test';
import { registerServiceWorker, createIPMISession, navigateToIPMI } from './helpers';

/**
 * Dell iDRAC8 IPMI web interface integration tests.
 *
 * Prerequisites:
 *   - Server running at KVM_BASE_URL (default http://localhost:8080)
 *   - BMC_PASSWORD_DELL env var set
 *   - iDRAC8 server (yucca-2) reachable from the server
 *
 * Run: cd tests/e2e && npx playwright test ipmi-idrac8
 */

const SERVER = 'yucca-2';

test.describe('iDRAC8 IPMI', () => {
  test('auto-login reaches dashboard without user interaction', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    const session = await createIPMISession(page, SERVER);
    expect(session.board_type).toBe('dell_idrac8');
    expect(session.session_cookie).toBeTruthy();

    // Navigate and wait — auto-login should happen automatically
    const ipmiPage = await navigateToIPMI(context, SERVER, 30000);

    const state = await ipmiPage.evaluate(() => ({
      title: document.title,
      url: window.location.href,
      hasFrames: document.querySelectorAll('frame, iframe').length,
    }));

    expect(state.title).toContain('iDRAC');
    expect(state.url).toContain('index.html');
    expect(state.url).toContain('ST1=');
    expect(state.hasFrames).toBeGreaterThan(0);
  });

  test('login page is bypassed (no login form shown)', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    await createIPMISession(page, SERVER);

    const ipmiPage = await navigateToIPMI(context, SERVER, 5000);

    const state = await ipmiPage.evaluate(() => ({
      url: window.location.href,
      hasLoginForm: !!document.querySelector('input[name="user"]'),
    }));

    // With login bypass, should never see the login form
    expect(state.hasLoginForm).toBe(false);
    // Should be on dashboard (index.html) or in redirect transition
    expect(state.url.includes('index.html') || state.url.includes('ipmi/')).toBe(true);
  });

  test('login interception returns cached credentials', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    await createIPMISession(page, SERVER);

    const loginResp = await page.evaluate(async (server: string) => {
      const res = await fetch(`/__bmc/${server}/data/login`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: 'user=root&password=calvin',
      });
      return {
        status: res.status,
        body: await res.text(),
      };
    }, SERVER);

    expect(loginResp.status).toBe(200);
    expect(loginResp.body).toContain('<authResult>0</authResult>');
    expect(loginResp.body).toContain('ST1=');
    expect(loginResp.body).toContain('ST2=');
  });

  test('logout interception preserves session', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    await createIPMISession(page, SERVER);

    const logoutResp = await page.evaluate(async (server: string) => {
      const res = await fetch(`/__bmc/${server}/data/logout`, { method: 'POST' });
      return { status: res.status, body: await res.text() };
    }, SERVER);

    expect(logoutResp.status).toBe(200);
    expect(logoutResp.body).toContain('<status>ok</status>');

    // Session should still work
    const sessionResp = await page.evaluate(async (server: string) => {
      const res = await fetch(`/__bmc/${server}/session?aimGetIntProp=scl_int_enabled`);
      return { status: res.status, xLanguage: res.headers.get('x_language') };
    }, SERVER);

    expect(sessionResp.status).toBe(200);
    expect(sessionResp.xLanguage).toContain('en');
  });

  test('X_Language header is injected', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    await createIPMISession(page, SERVER);

    const resp = await page.evaluate(async (server: string) => {
      const res = await fetch(`/__bmc/${server}/session?aimGetIntProp=scl_int_enabled`);
      return {
        status: res.status,
        xLanguage: res.headers.get('x_language'),
      };
    }, SERVER);

    expect(resp.status).toBe(200);
    expect(resp.xLanguage).toContain('en');
  });
});
