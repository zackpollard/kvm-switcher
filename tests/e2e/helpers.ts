import { type Page, type BrowserContext, expect } from '@playwright/test';

/**
 * Register the service worker by loading the app root and waiting for activation.
 */
export async function registerServiceWorker(page: Page): Promise<void> {
  await page.goto('/', { waitUntil: 'networkidle' });
  // Wait for SW to become active rather than using a fixed timeout
  await page.waitForFunction(async () => {
    const reg = await navigator.serviceWorker.getRegistration();
    return !!reg?.active;
  }, { timeout: 10000 });
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
 * Uses condition-based waiting instead of fixed timeouts where possible.
 * Returns the new page.
 */
export async function navigateToIPMI(context: BrowserContext, serverName: string, waitMs = 12000): Promise<Page> {
  const ipmiPage = await context.newPage();
  await ipmiPage.goto(`/ipmi/${serverName}/`, { waitUntil: 'domcontentloaded', timeout: 30000 });
  // Wait for the page to have a non-empty title or for the specified fallback time.
  // BMC pages set their title after JS/AJAX completes.
  await ipmiPage.waitForFunction(
    () => document.title.length > 0,
    { timeout: waitMs }
  ).catch(() => {
    // Fallback: if title never sets (some pages don't), just wait
  });
  return ipmiPage;
}

/**
 * Wait for the IPMI dashboard to fully load by checking for a title keyword.
 */
export async function waitForDashboard(page: Page, titleContains: string, timeout = 60000): Promise<void> {
  await page.waitForFunction(
    (keyword: string) => document.title.includes(keyword),
    titleContains,
    { timeout }
  );
}
