/**
 * @happy — full subscribe flow with real PayPal sandbox popup.
 *
 * What this exercises:
 *   - yunhou POST /payments/orders returns an order_id
 *   - Frontend PayPal JS SDK opens the sandbox buyer login → approve popup
 *   - Sandbox buyer signs in with PAYPAL_BUYER_ACCOUNT_* env-supplied creds
 *   - PayPal sends PAYMENT.CAPTURE.COMPLETED + BILLING.SUBSCRIPTION.CREATED
 *     webhooks to our ngrok-tunneled backend
 *   - Backend matches by custom_id, flips orders.status='paid', activates sub
 *   - Frontend polls /payments/orders/:id and sees the flip
 *
 * Pre-reqs (handled by sandbox runner):
 *   - PAYPAL_SANDBOX_CLIENT_ID / SECRET / BUYER_ACCOUNT_* exported
 *   - Backend running at BACKEND_URL with PAYPAL_ENV=sandbox
 *   - ngrok running, WEBHOOK_BASE_URL -> ngrok URL (test reads via admin API)
 *   - Webhook URL registered in PayPal dashboard against ngrok URL
 *   - Sandbox Plan pre-created (test reuses the configured PAYPAL_TEST_PLAN_ID)
 */

import { test, expect } from '@playwright/test';
import { loadSandboxEnv } from '../helpers/env';
import { initBackend } from '../helpers/backend';
import { assertSandboxBackend } from '../helpers/sandbox-mode';
import { discoverTunnel } from '../helpers/ngrok';
import { CheckoutPage } from '../pages/checkout-page';
import { PayPalPopupPage } from '../pages/paypal-popup-page';
import { createSandboxSubscription } from '../helpers/paypal-sandbox';

test.describe('@happy Sandbox subscription flow', () => {
  test('buyer approves in popup → order paid + sub active + webhook logged @happy', async ({
    page,
    baseURL,
  }) => {
    test.setTimeout(180_000); // PayPal sandbox can be slow; allow 3 min
    const env = loadSandboxEnv();
    assertSandboxBackend(env);

    // Discover tunnel. Backend uses the public URL as the public-facing
    // base when constructing sandbox verify-webhook-signature calls — we
    // pass it through to the backend via env if not already set.
    const tunnel = await discoverTunnel(8080);
    if (!process.env.WEBHOOK_BASE_URL) {
      process.env.WEBHOOK_BASE_URL = tunnel.publicUrl;
    }

    // 1) Backend lifecycle: connect to DB + create buyer session.
    const backend = await initBackend({
      baseUrl: baseURL ?? 'http://localhost:8080',
      dbUrl: process.env.E2E_DATABASE_URL ?? 'postgres://postgres@localhost/yunhou_users?sslmode=disable',
    });
    await backend.db.query('DELETE FROM webhook_events WHERE channel = $1', ['paypal']);
    await backend.db.query('DELETE FROM subscriptions WHERE user_id NOT IN (SELECT id FROM users LIMIT 1)');

    try {
      const { accessToken, userId } = await backend.login(env.buyerEmail);

      // Create the order via API first so the page can mount the SDK
      // against a real orderId. Without this, the page would have no
      // order to subscribe to and the webhook would never arrive.
      const orderId = await backend.createOrder(accessToken, 'monthly');

      // 2) Mount a minimal checkout HTML page that wires PayPal SDK.
      await page.route('**/checkout.html', async (route) => {
        await route.fulfill({
          status: 200,
          contentType: 'text/html',
          body: checkoutHtml({
            clientId: env.clientId,
            paypalPlanId: env.planId,
            orderId,
            accessToken,
          }),
        });
      });

      const checkout = new CheckoutPage(page);
      await checkout.goto();
      await checkout.expectLoaded();

      // 3) Frontend hits yunhou → POST /payments/orders then PayPal SDK
      //    opens sandbox popup. We do this by intercepting the popup.
      const popupPromise = page.waitForEvent('popup', { timeout: 30_000 });
      await checkout.clickPaypal();
      const popup = await popupPromise;
      const paypal = new PayPalPopupPage(popup, env);

      // 4) Sandbox buyer signs in (or is already logged in) and approves.
      await paypal.loginIfNeeded(env);
      await paypal.approve();

      // 5) Approval signal: onApprove in the parent page stamps the URL
      //    with ?subscription_id=...&order_id=... — that, not the popup
      //    close, is the reliable signal (sandbox occasionally keeps the
      //    popup open on a spinner or an interstitial after approval).
      await expect(page).toHaveURL(/\?/, { timeout: 90_000 });
      await popup.waitForEvent('close', { timeout: 15_000 }).catch(() => {
        // Popup lingering after a successful approval is harmless.
      });

      // 7) Read DB state directly. We deliberately do NOT trust SDK output.
      //    useOrderId is captured at top of test so we don't race another
      //    test's most-recent order.
      await backend.assertOrderPaid(orderId);
      await backend.assertSubActive(userId);

      // 8) Webhook event log — match on resource.custom_id (our orderId),
      //    NOT raw_payload.id: that's PayPal's own event id. BILLING.
      //    SUBSCRIPTION.* events echo the custom_id the checkout page set
      //    at subscription creation; PAYMENT.SALE.* renewals don't.
      //    raw_payload is stored as a JSON *string* (wrapRawPayload), so
      //    unwrap via #>> '{}' and re-parse before key lookup.
      const dump = await backend.db.query<{ event_id: string; event_type: string }>(
        `SELECT event_id, event_type FROM webhook_events WHERE channel = 'paypal'`,
      );
      console.log('paypal webhook_events received:', dump.rows);
      const events = await backend.db.query<{ event_id: string }>(
        `SELECT event_id FROM webhook_events
          WHERE channel = 'paypal'
            AND ((raw_payload #>> '{}')::jsonb -> 'resource' ->> 'custom_id') = $1
          ORDER BY received_at DESC LIMIT 1`,
        [orderId],
      );
      expect(events.rows[0]?.event_id).toBeDefined();

      // 9) UI assertion
      await checkout.expectOrderPaid();
      await checkout.expectSubscriptionActive();
    } finally {
      await backend.close();
    }
  });
});

/** Minimal HTML that loads PayPal JS SDK and renders a subscription
 *  button. The order + buyer session are created by the test via the
 *  backend API and injected here (the production frontend does the same
 *  through its BFF). custom_id binds the PayPal subscription to our
 *  order so the webhook handler can flip orders.status='paid'. */
function checkoutHtml(opts: {
  clientId: string;
  paypalPlanId: string;
  orderId: string;
  accessToken: string;
}): string {
  return `
<!doctype html>
<html>
<head>
  <meta charset="utf-8" />
  <title>Yunhou Checkout</title>
  <script src="https://www.sandbox.paypal.com/sdk/js?client-id=${encodeURIComponent(opts.clientId)}&vault=true&intent=subscription"></script>
</head>
<body>
  <h1>Checkout</h1>
  <label>email <input id="user-email" type="email" /></label>
  <div id="paypal-button-container"></div>
  <div>order: <span id="order-status">pending</span></div>
  <div>sub: <span id="sub-status">inactive</span></div>
  <script>
    const ORDER_ID = ${JSON.stringify(opts.orderId)};
    const TOKEN = ${JSON.stringify(opts.accessToken)};
    paypal.Buttons({
      createSubscription: (data, actions) => actions.subscription.create({
        plan_id: ${JSON.stringify(opts.paypalPlanId)},
        custom_id: ORDER_ID,
        application_context: {
          brand_name: 'yunhou L3',
          shipping_preference: 'NO_SHIPPING',
          user_action: 'SUBSCRIBE_NOW',
        },
      }),
      onApprove: async (data) => {
        // The test asserts the landing URL carries query params (mirrors
        // PayPal's return_url contract); stamp them without navigating.
        const q = '?subscription_id=' + encodeURIComponent(data.subscriptionID || '') +
                  '&order_id=' + encodeURIComponent(ORDER_ID);
        history.replaceState(null, '', window.location.pathname + q);
        // Reflect backend truth in the page: poll the order until the
        // webhook flips it to paid, then read the subscription.
        const deadline = Date.now() + 30000;
        while (Date.now() < deadline) {
          const r = await fetch('/payments/orders/' + ORDER_ID, {
            headers: { Authorization: 'Bearer ' + TOKEN },
          });
          if (r.ok) {
            const j = await r.json();
            document.getElementById('order-status').textContent = j.data.status;
            if (j.data.status === 'paid') break;
          }
          await new Promise((r2) => setTimeout(r2, 1000));
        }
        const rs = await fetch('/user/subscriptions', {
          headers: { Authorization: 'Bearer ' + TOKEN },
        });
        if (rs.ok) {
          const js = await rs.json();
          if (js.data && js.data[0]) {
            document.getElementById('sub-status').textContent = js.data[0].status;
          }
        }
      },
    }).render('#paypal-button-container');
  </script>
</body>
</html>`;
}

// Suppress unused-import lint warnings for the SDK helper we'll use in
// subsequent test files. Keeps this file compiling standalone.
void createSandboxSubscription;
