import { defineConfig, devices } from '@playwright/test';

/**
 * Playwright config for yunhou-users PayPal L3 integration tests.
 *
 * Two-tier baseURL pattern:
 *   - BACKEND_URL (used by API helpers like /auth/login, /payments/orders,
 *     /user/subscriptions): the **direct** backend URL — usually localhost.
 *   - WEBHOOK_BASE_URL: the **tunnel** URL PayPal sees (e.g. ngrok). This must
 *     be reachable from PayPal's webhook dispatchers. The backend reads this
 *     to discover its own public URL.
 *
 * When WEBHOOK_TUNNEL_PROVIDER=ngrok, the ngrok helper inspects the running
 * ngrok process and exposes its URL via WEBHOOK_BASE_URL automatically.
 */
export default defineConfig({
  testDir: './tests',
  fullyParallel: false, // PayPal sandbox serializes per-merchant; we avoid parallel brand risk.
  forbidOnly: process.env.CI === 'true',
  retries: process.env.CI ? 2 : 0,
  workers: 1,
  reporter: process.env.CI
    ? [['html', { open: 'never' }], ['github']]
    : [['list'], ['html', { open: 'never' }]],
  timeout: 90_000, // PayPal sandbox can be slow; allow 90s per test.
  expect: { timeout: 10_000 },

  use: {
    baseURL: process.env.BACKEND_URL ?? 'http://localhost:8080',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    video: 'retain-on-failure',
    actionTimeout: 15_000,
    navigationTimeout: 30_000,
  },

  projects: [
    {
      name: 'desktop-chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
});
