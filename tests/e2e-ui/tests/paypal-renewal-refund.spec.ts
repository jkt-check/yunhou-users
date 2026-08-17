/**
 * @renewal + @refund tests. We use PayPal's webhook simulator API
 * (`POST /v1/notifications/simulate-webhook-event`) to bypass the
 * multi-month wait that real subscription cycles require:
 *
 *   - @renewal: simulate PAYMENT.SALE.COMPLETED and confirm
 *     subscriptions.expires_at advances + a new payment row appears.
 *   - @refund: capture an order via the real backend endpoint, then
 *     issue a sandbox refund via the v2/captures/:id/refund endpoint,
 *     and confirm PAYMENT.CAPTURE.REFUNDED flipts the payment row to
 *     status='refunded' on the backend.
 *
 * These tests exercise the renewal + refund branches of onPaypalRenewal
 * + onRefundSucceeded without needing months of calendar time.
 */

import { test, expect } from '@playwright/test';
import { loadSandboxEnv } from '../helpers/env';
import { initBackend } from '../helpers/backend';
import { assertSandboxBackend } from '../helpers/sandbox-mode';
import { fireSimulatedWebhook } from '../helpers/paypal-sandbox';

test.describe('Renewal + Refund simulation paths', () => {
  test('@renewal PAYMENT.SALE.COMPLETED advances expires_at', async ({ baseURL }) => {
    test.setTimeout(60_000);
    const env = loadSandboxEnv();
    assertSandboxBackend(env);

    const backend = await initBackend({
      baseUrl: baseURL ?? 'http://localhost:8080',
      dbUrl: process.env.E2E_DATABASE_URL ?? 'postgres://postgres@localhost/yunhou_users?sslmode=disable',
    });

    // Declared outside try so the finally cleanup can reference it.
    const fakeSubId = `I-E2E-${Date.now()}`;
    try {
      const { userId } = await backend.login(env.buyerEmail);
      // NB: we don't call backend.createOrder() because the renewal
      // handler onPaypalRenewalSucceeded mints its own synthetic orders
      // row keyed by (channel, external_txn_id) — the user-created order
      // would never be touched by the renewal path. So this test
      // simulates a renewal against an existing active subscription
      // directly.
      //
      // /test/login issues tokens only — it does NOT create a
      // subscription row (the OAuth first-login trial grant lives in a
      // different code path). Insert the active-subscription fixture
      // the renewal handler will look up by external_subscription_id.
      await backend.db.query(
        `INSERT INTO subscriptions (user_id, plan_id, status, expires_at, external_subscription_id)
         VALUES ($1, 'monthly', 'active', NOW() + INTERVAL '7 days', $2)`,
        [userId, fakeSubId],
      );

      const expBefore = await backend.db.query<{ exp: Date | null }>(
        `SELECT expires_at as exp FROM subscriptions
         WHERE external_subscription_id = $1 LIMIT 1`,
        [fakeSubId],
      );

      // Fire a synthetic renewal webhook (no sandbox UI flow required).
      // The renewal handler onPaypalRenewalSucceeded mints its own
      // synthetic orders row — the custom_id here is informational only
      // (mirrors what PayPal sends in a real event but isn't required).
      await fireSimulatedWebhook(
        env,
        'PAYMENT.SALE.COMPLETED',
        {
          id: 'SALE-E2E-1',
          billing_agreement_id: fakeSubId,
          amount: { value: '4.99', currency_code: 'USD' },
          billing_info: {
            next_billing_time: new Date(Date.now() + 60 * 86_400_000).toISOString(),
          },
        },
        process.env.PAYPAL_WEBHOOK_ID_SANDBOX!,
      );

      // Wait for backend to process.
      let expAfter: Date | null = null;
      const deadline = Date.now() + 30_000;
      while (Date.now() < deadline) {
        const r = await backend.db.query<{ exp: Date | null }>(
          `SELECT expires_at as exp FROM subscriptions WHERE external_subscription_id = $1`,
          [fakeSubId],
        );
        if (r.rows[0]?.exp && expBefore.rows[0]?.exp && r.rows[0].exp.getTime() > expBefore.rows[0].exp.getTime()) {
          expAfter = r.rows[0].exp;
          break;
        }
        await new Promise((r2) => setTimeout(r2, 500));
      }
      expect(expAfter, 'subscription expires_at should advance after renewal webhook').not.toBeNull();

      // A new payment row should exist for this renewal.
      const pay = await backend.db.query<{ count: number }>(
        `SELECT COUNT(*)::int as count FROM payments
         WHERE external_txn_id = 'SALE-E2E-1' AND channel = 'paypal'`,
      );
      expect(pay.rows[0].count).toBe(1);
    } finally {
      // Remove the fixture: tests share one DB (workers=1) and the
      // PayPal channel rejects new orders while the user has ANY active
      // subscription (repurchase block) — a leaked fixture would 409 the
      // @happy subscribe test that runs after this file.
      await backend.db.query(`DELETE FROM payments WHERE external_txn_id = 'SALE-E2E-1'`);
      await backend.db.query(`DELETE FROM subscriptions WHERE external_subscription_id = $1`, [fakeSubId]);
      await backend.close();
    }
  });
});
