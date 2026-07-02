/**
 * yunhou-users backend helper. Wraps the public REST API + DB-side
 * assertions. All assertions read directly from PG so we test both the
 * HTTP response and the DB state — webhook delivery is the part under
 * test so we MUST inspect the database, not just the SDK response.
 */

import { Pool } from 'pg';

export interface BackendClient {
  baseUrl: string;
  db: Pool;
  login(email: string, appId?: string): Promise<{ accessToken: string; userId: string }>;
  createOrder(token: string, planId: string, opts?: { expiresAt?: string }): Promise<string>;
  getOrder(token: string, orderId: string): Promise<{ id: string; status: string; amount: number }>;
  getSubscription(token: string): Promise<{ id: string; status: string; planId: string; expiresAt: string | null }>;
  assertOrderPaid(orderId: string): Promise<void>;
  assertSubActive(userId: string): Promise<void>;
  assertWebhookEventLogged(channel: string, eventId: string): Promise<void>;
  close(): Promise<void>;
}

export async function initBackend(opts: {
  baseUrl: string;
  dbUrl: string;
}): Promise<BackendClient> {
  const db = new Pool({ connectionString: opts.dbUrl, max: 3 });

  async function login(email: string, appId = 'yundian'): Promise<{ accessToken: string; userId: string }> {
    // E2E: yunhou's POST /auth/login takes GitHub provider_token. We use
    // the sandbox-test buyer email as the GitHub user — yunhou-side, this
    // hits the mockable OAuth provider. In the e2e-UI test, the mock provider
    // is wired to accept any token and map it to a user.
    const res = await fetch(`${opts.baseUrl}/auth/login`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        provider: 'github',
        provider_token: `paypal-e2e-${email}-${Date.now()}`,
        app_id: appId,
      }),
    });
    if (!res.ok) {
      throw new Error(`login: ${res.status} ${await res.text()}`);
    }
    const json = await res.json();
    return {
      accessToken: json.data.access_token,
      userId: json.data.user.id,
    };
  }

  async function createOrder(
    token: string,
    planId: string,
    opts2: { expiresAt?: string } = {},
  ): Promise<string> {
    const body: Record<string, unknown> = { plan_id: planId };
    if (opts2.expiresAt) {
      body.expires_at = opts2.expiresAt;
    }
    const res = await fetch(`${opts.baseUrl}/payments/orders`, {
      method: 'POST',
      headers: {
        Authorization: `Bearer ${token}`,
        'Content-Type': 'application/json',
      },
      body: JSON.stringify(body),
    });
    if (res.status !== 201) {
      throw new Error(`createOrder: ${res.status} ${await res.text()}`);
    }
    const json = await res.json();
    return json.data.id;
  }

  async function getOrder(token: string, orderId: string) {
    const res = await fetch(`${opts.baseUrl}/payments/orders/${orderId}`, {
      headers: { Authorization: `Bearer ${token}` },
    });
    if (!res.ok) {
      throw new Error(`getOrder: ${res.status}`);
    }
    return (await res.json()).data;
  }

  async function getSubscription(token: string) {
    const res = await fetch(`${opts.baseUrl}/user/subscriptions`, {
      headers: { Authorization: `Bearer ${token}` },
    });
    if (!res.ok) {
      throw new Error(`getSubscription: ${res.status}`);
    }
    const json = await res.json();
    return json.data[0];
  }

  async function assertOrderPaid(orderId: string) {
    const deadline = Date.now() + 30_000;
    while (Date.now() < deadline) {
      const r = await db.query<{ status: string }>(
        `SELECT status FROM orders WHERE id = $1`,
        [orderId],
      );
      if (r.rows[0]?.status === 'paid') return;
      await new Promise((r2) => setTimeout(r2, 500));
    }
    throw new Error(`Order ${orderId} did not flip to paid within 30s`);
  }

  async function assertSubActive(userId: string) {
    const r = await db.query<{ status: string }>(
      `SELECT status FROM subscriptions WHERE user_id = $1 AND status = 'active' LIMIT 1`,
      [userId],
    );
    if (r.rows.length === 0) {
      throw new Error(`No active subscription for user ${userId}`);
    }
  }

  async function assertWebhookEventLogged(channel: string, eventId: string) {
    const r = await db.query<{ processed_at: Date | null }>(
      `SELECT processed_at FROM webhook_events WHERE channel = $1 AND event_id = $2`,
      [channel, eventId],
    );
    if (r.rows.length === 0) {
      throw new Error(`webhook event ${channel}/${eventId} not logged`);
    }
    if (r.rows[0].processed_at === null) {
      throw new Error(`webhook event ${channel}/${eventId} unprocessed (no processed_at)`);
    }
  }

  return {
    baseUrl: opts.baseUrl,
    db,
    login,
    createOrder,
    getOrder,
    getSubscription,
    assertOrderPaid,
    assertSubActive,
    assertWebhookEventLogged,
    close: () => db.end(),
  };
}
