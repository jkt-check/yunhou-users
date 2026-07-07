/**
 * Environment helpers — all sandbox credentials live here so test files
 * only deal in high-level names (e.g. `Buyer.email`) and not env-var lookups.
 *
 * Required env vars (set these in your shell or .env.e2e):
 *   - PAYPAL_SANDBOX_CLIENT_ID
 *   - PAYPAL_SANDBOX_SECRET
 *   - PAYPAL_BUYER_ACCOUNT_EMAIL        (sandbox personal account)
 *   - PAYPAL_BUYER_ACCOUNT_PASSWORD
 *   - PAYPAL_TEST_PLAN_ID               (pre-created sandbox Plan, see README)
 *   - BACKEND_URL                       (default http://localhost:8080)
 *   - WEBHOOK_TUNNEL_PROVIDER           (ngrok | none; default ngrok)
 */

export interface SandboxEnv {
  clientId: string;
  secret: string;
  buyerEmail: string;
  buyerPassword: string;
  planId: string;
  baseUrl: string;
  apiBase: string; // PayPal sandbox REST base
  liveApiBase: string;
}

export function loadSandboxEnv(): SandboxEnv {
  const clientId = mustEnv('PAYPAL_SANDBOX_CLIENT_ID');
  const secret = mustEnv('PAYPAL_SANDBOX_SECRET');
  const buyerEmail = mustEnv('PAYPAL_BUYER_ACCOUNT_EMAIL');
  const buyerPassword = mustEnv('PAYPAL_BUYER_ACCOUNT_PASSWORD');
  const planId = mustEnv('PAYPAL_TEST_PLAN_ID');
  return {
    clientId,
    secret,
    buyerEmail,
    buyerPassword,
    planId,
    baseUrl: process.env.BACKEND_URL ?? 'http://localhost:8080',
    apiBase: 'https://api-m.sandbox.paypal.com',
    liveApiBase: 'https://api-m.paypal.com',
  };
}

function mustEnv(name: string): string {
  const v = process.env[name];
  if (!v) {
    throw new Error(`Missing required env var: ${name}`);
  }
  return v;
}

export const TEST_PLAN_AMOUNT_USD = '4.99'; // small to keep sandbox money predictable
export const SUBSCRIPTION_INTERVAL_DAYS = 30;
