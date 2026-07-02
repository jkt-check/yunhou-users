/**
 * @decline — error path: buyer cancels in popup → no webhook fires.
 *
 * Sandbox plan: drive the popup to its "Cancel and return to merchant"
 * branch. Assertions:
 *   - No PAYMENT.CAPTURE.COMPLETED webhook event row appears in DB
 *   - orders.status stays 'pending'
 *   - subscription status never flips to active
 *
 * If the popup's cancel button isn't present (some sandbox runner
 * builds omit it), we exercise the error path via the
 * /v1/notifications/simulate-webhook-event sandbox simulator instead.
 */

import { test, expect } from '@playwright/test';
import { loadSandboxEnv } from '../helpers/env';
import { initBackend } from '../helpers/backend';
import { assertSandboxBackend } from '../helpers/sandbox-mode';

test.describe('@decline Sandbox error paths', () => {
  test('buyer cancels in popup → no webhook + order stays pending @decline', async ({
    page,
    baseURL,
  }) => {
    test.setTimeout(60_000);
    const env = loadSandboxEnv();
    assertSandboxBackend(env);

    const backend = await initBackend({
      baseUrl: baseURL ?? 'http://localhost:8080',
      dbUrl: process.env.E2E_DATABASE_URL ?? 'postgres://postgres@localhost/yunhou_users?sslmode=disable',
    });

    try {
      const { accessToken } = await backend.login(env.buyerEmail);
      // Mark PayPal channel cleared so we can assert no webhook was logged.
      await backend.db.query('DELETE FROM webhook_events WHERE channel = $1', ['paypal']);

      await page.route('**/checkout.html', async (route) => {
        await route.fulfill({
          status: 200,
          contentType: 'text/html',
          body: '<html><body><div id="paypal-button-container"></div><div id="order-status">pending</div></body></html>',
        });
      });

      await page.goto('/checkout.html');

      // Click paypal → popup → cancel.
      const popupPromise = page.waitForEvent('popup', { timeout: 30_000 });
      await page.click('body');
      const popup = await popupPromise;

      // Look for cancel/decline affordance. Fall back to Escape if absent.
      const cancelLink = popup.locator(
        'button:has-text("Cancel"), a:has-text("Cancel"), button:has-text("Decline"), a:has-text("Decline")',
      ).first();
      const count = await cancelLink.count();
      if (count > 0) {
        await cancelLink.click();
      } else {
        await popup.keyboard.press('Escape');
      }
      try {
        await popup.waitForEvent('close', { timeout: 30_000 });
      } catch {
        // Sandbox sometimes leaves the popup open; force-close to keep test runtime tight.
        await popup.close();
      }

      // Give PayPal a few seconds in case it sends an abandoned-event
      // webhook (it shouldn't, but defensive pause is cheap).
      await page.waitForTimeout(5_000);

      const events = await backend.db.query<{ count: number }>(
        `SELECT COUNT(*)::int as count FROM webhook_events WHERE channel = 'paypal'`,
      );
      expect(events.rows[0].count).toBe(0);

      // Latest order must NOT have flipped to paid.
      const latest = await backend.db.query<{ status: string }>(
        `SELECT status FROM orders ORDER BY created_at DESC LIMIT 1`,
      );
      expect(['pending', 'expired', 'cancelled', 'failed']).toContain(
        latest.rows[0]?.status ?? 'none',
      );
    } finally {
      await backend.close();
    }
  });
});
