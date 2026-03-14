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
  test('login page renders with form visible', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    await createIPMISession(page, SERVER);

    const ipmiPage = await navigateToIPMI(context, SERVER);

    const state = await ipmiPage.evaluate(() => ({
      title: document.title,
      dataareaVisible: document.getElementById('dataarea')?.style?.visibility,
      initImgHidden: document.getElementById('initImg')?.style?.display === 'none',
      hasUserInput: !!document.querySelector('input[name="user"]'),
      hasPasswordInput: !!document.querySelector('input[type="password"]'),
      lang: (window as unknown as Record<string, unknown>).lang,
    }));

    expect(state.dataareaVisible).toBe('visible');
    expect(state.initImgHidden).toBe(true);
    expect(state.hasUserInput).toBe(true);
    expect(state.hasPasswordInput).toBe(true);
    expect(state.lang).toBe('en');
    // Title may take extra time to populate via AJAX; check if present
    if (state.title) {
      expect(state.title).toContain('iDRAC8');
    }
  });

  test('auto-login reaches dashboard after form submission', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    const session = await createIPMISession(page, SERVER);
    expect(session.board_type).toBe('dell_idrac8');
    expect(session.session_cookie).toBeTruthy();

    const ipmiPage = await navigateToIPMI(context, SERVER);

    // Wait for login form to appear
    const formVisible = await ipmiPage.evaluate(() =>
      document.getElementById('dataarea')?.style?.visibility === 'visible'
    );
    expect(formVisible).toBe(true);

    // Fill and submit login form
    await ipmiPage.fill('input[name="user"]', 'root');
    await ipmiPage.fill('input[name="password"]', 'calvin');

    const submitBtn = await ipmiPage.$('#btnOK');
    expect(submitBtn).not.toBeNull();
    await submitBtn!.click();

    // Wait for navigation to dashboard
    await ipmiPage.waitForTimeout(15000);

    const afterLogin = await ipmiPage.evaluate(() => ({
      title: document.title,
      url: window.location.href,
      hasFrames: document.querySelectorAll('frame, iframe').length,
    }));

    expect(afterLogin.title).toContain('Summary');
    expect(afterLogin.url).toContain('index.html');
    expect(afterLogin.url).toContain('ST1=');
    expect(afterLogin.hasFrames).toBeGreaterThan(0);
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
        contentType: res.headers.get('content-type'),
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
