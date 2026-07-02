/**
 * PayPal sandbox REST client. Wraps the OAuth2 client_credentials token
 * dance + a thin fetch wrapper. No SDK — the SDK adds bloat and we only
 * need a handful of REST calls.
 */

import { SandboxEnv } from './env';

export interface PayPalAccessToken {
  access_token: string;
  expires_in: number;
  issued_at: string; // heuristic; PayPal returns absolute exp
}

let cachedToken: { token: string; expiresAt: number } | null = null;

/** Get an OAuth2 access_token for sandbox REST. Cached until ~60s before exp. */
export async function getAccessToken(env: SandboxEnv): Promise<string> {
  if (cachedToken && cachedToken.expiresAt - 60_000 > Date.now()) {
    return cachedToken.token;
  }
  const basic = Buffer.from(`${env.clientId}:${env.secret}`).toString('base64');
  const res = await fetch(`${env.apiBase}/v1/oauth2/token`, {
    method: 'POST',
    headers: {
      Authorization: `Basic ${basic}`,
      'Content-Type': 'application/x-www-form-urlencoded',
    },
    body: 'grant_type=client_credentials',
  });
  if (!res.ok) {
    throw new Error(`PayPal token: ${res.status} ${await res.text()}`);
  }
  const json = (await res.json()) as PayPalAccessToken;
  // PayPal returns expires_in seconds. Use 80% of that to be safe across clock skew.
  const expiresAt = Date.now() + json.expires_in * 800;
  cachedToken = { token: json.access_token, expiresAt };
  return json.access_token;
}

export interface SandboxOrder {
  id: string;
  status: 'CREATED' | 'SAVED' | 'APPROVED' | 'VOIDED' | 'COMPLETED';
  purchase_units: Array<{
    custom_id?: string;
    reference_id?: string;
    amount: { currency_code: string; value: string };
  }>;
}

/** Create a sandbox Order in CREATED state — buyer hasn't approved yet. */
export async function createSandboxOrder(
  env: SandboxEnv,
  opts: { customId: string; amountUsd: string; description?: string },
): Promise<SandboxOrder> {
  const token = await getAccessToken(env);
  const res = await fetch(`${env.apiBase}/v2/checkout/orders`, {
    method: 'POST',
    headers: {
      Authorization: `Bearer ${token}`,
      'Content-Type': 'application/json',
      Prefer: 'return=representation',
    },
    body: JSON.stringify({
      intent: 'CAPTURE',
      purchase_units: [
        {
          custom_id: opts.customId,
          reference_id: opts.customId,
          description: opts.description ?? 'E2E test plan',
          amount: { currency_code: 'USD', value: opts.amountUsd },
        },
      ],
    }),
  });
  if (!res.ok) {
    throw new Error(`CreateOrder: ${res.status} ${await res.text()}`);
  }
  return (await res.json()) as SandboxOrder;
}

export interface SandboxSubscription {
  id: string;
  status: 'APPROVAL_PENDING' | 'APPROVED' | 'ACTIVE' | 'SUSPENDED' | 'CANCELLED' | 'EXPIRED';
  custom_id?: string;
}

/** Create a sandbox Subscription attached to the configured plan_id. */
export async function createSandboxSubscription(
  env: SandboxEnv,
  opts: { customId: string; planId?: string; returnUrls: { success: string; cancel: string } },
): Promise<SandboxSubscription> {
  const token = await getAccessToken(env);
  const planId = opts.planId ?? env.planId;
  const res = await fetch(`${env.apiBase}/v1/billing/subscriptions`, {
    method: 'POST',
    headers: {
      Authorization: `Bearer ${token}`,
      'Content-Type': 'application/json',
      Prefer: 'return=representation',
    },
    body: JSON.stringify({
      plan_id: planId,
      custom_id: opts.customId,
      application_context: {
        brand_name: 'yunhou-users E2E',
        shipping_preference: 'NO_SHIPPING',
        user_action: 'SUBSCRIBE_NOW',
        return_url: opts.returnUrls.success,
        cancel_url: opts.returnUrls.cancel,
      },
    }),
  });
  if (!res.ok) {
    throw new Error(`CreateSubscription: ${res.status} ${await res.text()}`);
  }
  return (await res.json()) as SandboxSubscription;
}

/** Sandbox simulator trigger — fire a synthetic webhook via PayPal's
 *  developer dashboard API. Used when the buyer-approval step can't be
 *  driven headless (e.g. timeout), so test logs prove the webhook path
 *  works end-to-end. */
export async function fireSimulatedWebhook(
  env: SandboxEnv,
  eventType: string,
  resourceBody: Record<string, unknown>,
  webhookId: string,
): Promise<void> {
  const token = await getAccessToken(env);
  const res = await fetch(
    `${env.apiBase}/v1/notifications/simulate-webhook-event`,
    {
      method: 'POST',
      headers: {
        Authorization: `Bearer ${token}`,
        'Content-Type': 'application/json',
      },
      body: JSON.stringify({
        webhook_id: webhookId,
        event_type: eventType,
        resource: resourceBody,
        // PayPal signs with their cert; the verifier decodes via the public
        // API so a simulated event is normally accepted as SUCCESS.
      }),
    },
  );
  if (!res.ok) {
    throw new Error(`SimulateWebhook: ${res.status} ${await res.text()}`);
  }
}

/** Issue a sandbox refund against a captured sale — used by refund path tests. */
export async function refundCapture(
  env: SandboxEnv,
  captureId: string,
  amountUsd?: string,
): Promise<{ id: string; status: string }> {
  const token = await getAccessToken(env);
  const body: Record<string, unknown> = {};
  if (amountUsd) {
    body.amount = { currency_code: 'USD', value: amountUsd };
  }
  const res = await fetch(
    `${env.apiBase}/v2/payments/captures/${captureId}/refund`,
    {
      method: 'POST',
      headers: {
        Authorization: `Bearer ${token}`,
        'Content-Type': 'application/json',
        Prefer: 'return=representation',
      },
      body: JSON.stringify(body),
    },
  );
  if (!res.ok) {
    throw new Error(`Refund: ${res.status} ${await res.text()}`);
  }
  return await res.json();
}

/** Get sandbox sale details (for renewal tests). */
export async function getCapture(env: SandboxEnv, captureId: string): Promise<{
  id: string;
  status: string;
  amount: { currency_code: string; value: string };
  custom_id?: string;
}> {
  const token = await getAccessToken(env);
  const res = await fetch(`${env.apiBase}/v2/payments/captures/${captureId}`, {
    headers: { Authorization: `Bearer ${token}` },
  });
  if (!res.ok) {
    throw new Error(`GetCapture: ${res.status} ${await res.text()}`);
  }
  return (await res.json()) as any;
}
