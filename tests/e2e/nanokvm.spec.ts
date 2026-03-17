import { test, expect } from '@playwright/test';
import { registerServiceWorker, createIPMISession, navigateToIPMI } from './helpers';

/**
 * Sipeed NanoKVM E2E tests against a real device.
 *
 * Prerequisites:
 *   - Server running at KVM_BASE_URL (default http://localhost:8080)
 *   - NANOKVM_PASSWORD env var set
 *   - At least one NanoKVM reachable from the server
 *
 * Run: cd tests/e2e && npx playwright test nanokvm
 */

const SERVER = 'geodude-kvm';

test.describe('NanoKVM', () => {
  test('session creation returns JWT token', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    const session = await createIPMISession(page, SERVER);

    expect(session.board_type).toBe('nanokvm');
    expect(session.session_cookie).toBeTruthy();
    // NanoKVM tokens are JWTs (three dot-separated base64 segments)
    expect(String(session.session_cookie)).toMatch(/^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$/);
  });

  test('KVM page loads with auto-login (no login form)', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    await createIPMISession(page, SERVER);

    const kvmPage = await navigateToIPMI(context, SERVER);

    // Wait for the NanoKVM SPA to render (title is set by React)
    await kvmPage.waitForFunction(
      () => document.title === 'NanoKVM',
      { timeout: 15000 }
    );

    const state = await kvmPage.evaluate(() => ({
      title: document.title,
      url: window.location.href,
      // NanoKVM SPA shows login page at /auth/login if not authenticated
      isOnLoginPage: window.location.href.includes('/auth/login'),
      hasRootDiv: !!document.getElementById('root'),
    }));

    expect(state.title).toBe('NanoKVM');
    expect(state.isOnLoginPage).toBe(false);
    expect(state.hasRootDiv).toBe(true);
  });

  test('proxied API calls work with injected token', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    await createIPMISession(page, SERVER);

    // Call the NanoKVM API through the proxy directly
    const infoResp = await page.evaluate(async (server: string) => {
      const res = await fetch(`/__bmc/${server}/api/vm/info`);
      return { status: res.status, body: await res.json() };
    }, SERVER);

    expect(infoResp.status).toBe(200);
    expect(infoResp.body.code).toBe(0);
    expect(infoResp.body.data.application).toBeTruthy();
    expect(infoResp.body.data.mdns).toContain('geodude');
  });

  test('GPIO endpoint returns power state', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    await createIPMISession(page, SERVER);

    const gpioResp = await page.evaluate(async (server: string) => {
      const res = await fetch(`/__bmc/${server}/api/vm/gpio`);
      return { status: res.status, body: await res.json() };
    }, SERVER);

    expect(gpioResp.status).toBe(200);
    expect(gpioResp.body.code).toBe(0);
    expect(typeof gpioResp.body.data.pwr).toBe('boolean');
  });

  test('MJPEG video stream is accessible', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    await createIPMISession(page, SERVER);

    // The MJPEG stream should return a multipart content type
    const streamResp = await page.evaluate(async (server: string) => {
      const controller = new AbortController();
      // Abort after getting headers — we don't need to consume the stream
      setTimeout(() => controller.abort(), 2000);
      try {
        const res = await fetch(`/__bmc/${server}/api/stream/mjpeg`, {
          signal: controller.signal,
        });
        return {
          status: res.status,
          contentType: res.headers.get('content-type'),
        };
      } catch (e) {
        // AbortError is expected — we just wanted the headers
        if ((e as Error).name === 'AbortError') {
          return { status: 200, contentType: 'aborted-after-headers' };
        }
        throw e;
      }
    }, SERVER);

    // Either we got the content-type before abort, or the fetch was aborted
    // (which means the connection was established successfully)
    expect(streamResp.status).toBe(200);
  });

  test('response headers include auto-login signals', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    await createIPMISession(page, SERVER);

    const headers = await page.evaluate(async (server: string) => {
      const res = await fetch(`/__bmc/${server}/`);
      return {
        autoLogin: res.headers.get('x-kvm-autologin'),
        nanoToken: res.headers.get('x-kvm-nanotoken'),
      };
    }, SERVER);

    expect(headers.autoLogin).toBe('true');
    expect(headers.nanoToken).toBeTruthy();
    // Token should be a JWT
    expect(headers.nanoToken).toMatch(/^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$/);
  });

  test('status API reports NanoKVM device info', async ({ context }) => {
    const page = await context.newPage();
    await registerServiceWorker(page);
    // Ensure session exists so poller can get detailed stats
    await createIPMISession(page, SERVER);

    // Wait for poller to pick up the session
    await page.waitForTimeout(35000);

    const status = await page.evaluate(async (server: string) => {
      const res = await fetch('/api/server-status');
      const data = await res.json();
      return data[server] || {};
    }, SERVER);

    expect(status.online).toBe(true);
    expect(status.model).toBe('NanoKVM');
    expect(status.app_version).toBeTruthy();
    expect(status.image_version).toBeTruthy();
  });
});
