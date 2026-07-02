/**
 * Sandbox-mode toggle for the yunhou backend.
 *
 * The backend operator (start.sh / deployment) is responsible for setting
 *   PAYPAL_ENV=sandbox
 *   PAYPAL_WEBHOOK_ID_SANDBOX=<id from PayPal dashboard>
 *   PAYPAL_API_BASE_SANDBOX=https://api-m.sandbox.paypal.com
 *
 * This helper validates those env vars exist when an L3 test starts. If
 * any are missing the test is skipped rather than wasted in CI.
 */

import { SandboxEnv } from './env';

export function assertSandboxBackend(env: SandboxEnv): void {
  const required = [
    'PAYPAL_ENV',
    'PAYPAL_WEBHOOK_ID_SANDBOX',
    'PAYPAL_API_BASE_SANDBOX',
  ];
  for (const k of required) {
    if (!process.env[k]) {
      throw new Error(
        `Sandbox webhook verification requires backend env ${k} to be set`,
      );
    }
  }
  if (process.env.PAYPAL_ENV !== 'sandbox') {
    throw new Error(`PAYPAL_ENV must be sandbox for L3 tests; got ${process.env.PAYPAL_ENV}`);
  }
  if (!process.env.PAYPAL_API_BASE_SANDBOX!.includes('sandbox')) {
    throw new Error('PAYPAL_API_BASE_SANDBOX must point to api-m.sandbox.paypal.com');
  }
  // env.baseUrl should not point at a tunnel — we want direct-local for
  // API calls, and tunnel for webhooks only.
  if (env.baseUrl.includes('ngrok') || env.baseUrl.includes('localhost.run')) {
    throw new Error(`BACKEND_URL must NOT be the tunnel; got ${env.baseUrl}`);
  }
}
