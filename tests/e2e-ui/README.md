# L3 PayPal Integration Tests

Playwright + real PayPal **sandbox** + yunhou-users backend, end-to-end
through the actual buyer popup and real webhook delivery. The most
expensive tier in the test pyramid but the only one that catches SDK
shape changes and PayPal sandbox behavior.

## When to run

- Before shipping a PayPal-touching PR to production.
- After any change to PayPal's webhook event schemas (rare but happens).
- After changes to `parsePaypal` or `onPaypalRenewalSucceeded`.

For unit-level PayPal checks, run `go test ./internal/middleware/...` and
`go test ./tests/e2e/...` — those are fast and don't need a sandbox.

## Setup (one-time)

1. **PayPal sandbox accounts** at https://developer.paypal.com/dashboard/
   - Sandbox Business account
   - Sandbox Personal (buyer) account
   - Create a REST app → grab `Client ID` + `Secret`
   - **Pre-create a subscription Plan** (Catalog → Products → Plans):
     save the `plan_id` (e.g. `P-5AB12345CD1234567`) as `PAYPAL_TEST_PLAN_ID`

2. **Local infrastructure**:
   - Docker or local: PostgreSQL 16
   - `ngrok` (or `cloudflared`, `localtunnel`)
   - Node.js 20+

3. **Webhook registration** in PayPal dashboard:
   - URL: `https://<ngrok-id>.ngrok.io/webhooks/payment/paypal`
   - Events: `PAYMENT.CAPTURE.*`, `PAYMENT.SALE.*`, `BILLING.SUBSCRIPTION.*`
   - Save the webhook ID as `PAYPAL_WEBHOOK_ID_SANDBOX`

4. **Backend env** when starting yunhou:
   ```bash
   export PAYPAL_ENV=sandbox
   export PAYPAL_WEBHOOK_ID_SANDBOX=<id from dashboard>
   export PAYPAL_API_BASE_SANDBOX=https://api-m.sandbox.paypal.com
   export PAYPAL_SANDBOX_WEBHOOK_ID=$PAYPAL_WEBHOOK_ID_SANDBOX
   ```

5. **Test env** (in `tests/e2e-ui/.env.e2e`, loaded by your shell):
   ```
   PAYPAL_SANDBOX_CLIENT_ID=<from dashboard>
   PAYPAL_SANDBOX_SECRET=<from dashboard>
   PAYPAL_BUYER_ACCOUNT_EMAIL=<sandbox buyer email>
   PAYPAL_BUYER_ACCOUNT_PASSWORD=<sandbox buyer password>
   PAYPAL_TEST_PLAN_ID=P-xxx...
   BACKEND_URL=http://localhost:8080
   E2E_DATABASE_URL=postgres://postgres@localhost/yunhou_users?sslmode=disable
   ```

## Run

```bash
# 1. Bring up infrastructure (PG + ngrok + backend in separate terminals)
brew services start postgresql@16
ngrok http 8080
# (in another terminal) go run ./cmd/server

# 2. Install + run the integration suite
cd tests/e2e-ui
npm install
npx playwright install chromium
npm run all       # everything
npm run paypal:happy       # single tag
```

## Test taxonomy

| Tag | File | What it covers |
|---|---|---|
| `@happy`  | `paypal-subscribe.spec.ts` | Buyer logs in + approves → CAPTURE.COMPLETED + SUBSCRIPTION.CREATED fire → order paid + sub active |
| `@decline`| `paypal-decline.spec.ts` | Buyer cancels in popup → no webhook → order stays pending |
| `@renewal`| `paypal-renewal-refund.spec.ts` | Sandbox simulator fires PAYMENT.SALE.COMPLETED → subscription.expires_at advances |
| (future) `@refund` | (extension) | PayPal `/v2/payments/captures/:id/refund` → CAPTURE.REFUNDED → payments.status='refunded' |

CI: `@happy` is the gate. `@renewal` is run manually monthly.

## How it works

1. Test boots a backend (`go run ./cmd/server`) listening on :8080.
2. ngrok exposes :8080 on `https://abc.ngrok.io`. PayPal webhook URL
   registered against this public URL.
3. `setupE2E` helpers log in via `POST /test/login` (dev-only JWT mint endpoint;
   requires `PAYPAL_L3_E2E_MODE=1`), then create a Yunhou order via
   `POST /payments/orders`.
4. The test mounts a minimal checkout HTML that loads the PayPal JS SDK
   against the sandbox endpoint and renders the Buttons.
5. Buyer popup: Playwright drives the iframe/popup flow with the
   configured sandbox buyer credentials.
6. PayPal dispatches webhooks → ngrok → yunhou backend → DB.
7. Assertion reads PG directly so we trust DB state, not SDK claims.

## Sandbox quirks to know

- **Sandbox is slow.** First subscription create can take 5-10 seconds.
  Renewal simulators are faster (under 3s typical).
- **Buyer is required.** Without `PAYPAL_BUYER_ACCOUNT_*` env vars, the
  popup will hang on the email field. Tests with missing creds are
  skipped automatically.
- **Webhook IDs are environment-scoped.** Don't reuse the same webhook
  ID across sandbox accounts.
- **Some sandbox builds omit the cancel button** in the popup. Decline
  tests fall back to `Escape` keyboard event in that case.

## CI gating

- L3 is **NOT** in the default PR CI. It's nightly/manual.
- PR CI runs `go test ./internal/...` and `go test ./tests/e2e/...`.
- Nightly L3 job runs against the sandbox runner with secrets injected.

Add it later when you've validated the suite is stable enough to gate
releases (target: <5% flakiness over a week of nightly runs).

## Adding new tests

Each `.spec.ts` file should:

1. Call `loadSandboxEnv()` and `assertSandboxBackend(env)` first.
2. Initialize `initBackend({...})` and `close()` it in a finally.
3. Use `loadSandboxEnv()` to access PayPal client_id + buyer creds.
4. Drive the popup via `CheckoutPage` and `PayPalPopupPage`.
5. Use `fireSimulatedWebhook()` for any webhook you can't trigger from
   the UI in a reasonable test runtime.

Tag the test with `@smoke` if it's a CI gate candidate, or another tag
otherwise — `npm run paypal:<tag>` runs only that tag.
