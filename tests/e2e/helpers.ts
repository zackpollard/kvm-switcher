import { type Page, type BrowserContext } from '@playwright/test';

/**
 * Register the service worker by loading the app root and waiting for activation.
 */
export async function registerServiceWorker(page: Page): Promise<void> {
  await page.goto('/', { waitUntil: 'networkidle' });
  await page.waitForTimeout(2000);
  await page.evaluate(async () => {
    const reg = await navigator.serviceWorker.getRegistration();
    if (!reg?.active) throw new Error('Service worker not active');
  });
}

/**
 * Create an IPMI session for the given server via the API.
 * Returns the session response body.
 */
export async function createIPMISession(page: Page, serverName: string): Promise<Record<string, unknown>> {
  const result = await page.evaluate(async (name: string) => {
    const res = await fetch(`/api/ipmi-session/${name}`, { method: 'POST' });
    return { status: res.status, body: await res.json() };
  }, serverName);

  if (result.status !== 200) {
    throw new Error(`Failed to create IPMI session: HTTP ${result.status} - ${JSON.stringify(result.body)}`);
  }

  return result.body as Record<string, unknown>;
}

/**
 * Navigate to the IPMI page for a server and wait for it to settle.
 * Returns the new page.
 */
export async function navigateToIPMI(context: BrowserContext, serverName: string, waitMs = 12000): Promise<Page> {
  const ipmiPage = await context.newPage();
  await ipmiPage.goto(`/ipmi/${serverName}/`, { waitUntil: 'domcontentloaded', timeout: 30000 });
  await ipmiPage.waitForTimeout(waitMs);
  return ipmiPage;
}
