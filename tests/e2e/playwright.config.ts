import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: '.',
  timeout: 120_000,
  retries: 0,
  workers: 1, // serial execution — BMCs have low session limits
  use: {
    baseURL: process.env.KVM_BASE_URL || 'http://localhost:8080',
    headless: true,
  },
  projects: [
    {
      name: 'chromium',
      use: { browserName: 'chromium' },
    },
  ],
});
