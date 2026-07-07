/**
 * ngrok tunnel helper. The yunhou backend must be reachable from PayPal's
 * webhook dispatchers; ngrok exposes the locally-running backend on a
 * public URL.
 *
 * Assumes `ngrok` is on $PATH. The sandbox CI runner pre-installs it.
 * Falls back to a direct URL if WEBHOOK_BASE_URL is set externally.
 */

const DEFAULT_TUNNEL_PROVIDER = process.env.WEBHOOK_TUNNEL_PROVIDER ?? 'ngrok';
const NGROK_API = 'http://127.0.0.1:4040';

export interface TunnelInfo {
  publicUrl: string;
  /** Default backend port. The helper maps `publicUrl -> backend port`
   * by overwriting the host:port component in the public URL. */
  backendPort: number;
}

export async function discoverTunnel(
  backendPort: number,
): Promise<TunnelInfo> {
  if (DEFAULT_TUNNEL_PROVIDER === 'none') {
    const u = process.env.WEBHOOK_BASE_URL;
    if (!u) {
      throw new Error('WEBHOOK_TUNNEL_PROVIDER=none but WEBHOOK_BASE_URL unset');
    }
    return { publicUrl: u, backendPort };
  }

  // Default: ngrok. Hit the local admin API to read the assigned URL.
  const res = await fetch(`${NGROK_API}/api/tunnels`);
  if (!res.ok) {
    throw new Error(
      `ngrok admin API: ${res.status}; is ngrok running? Try 'ngrok http <port>' separately.`,
    );
  }
  const json = (await res.json()) as { tunnels: Array<{ public_url: string }> };
  const t = json.tunnels.find((x) => x.public_url.startsWith('https://')) ?? json.tunnels[0];
  if (!t) {
    throw new Error('No ngrok tunnels running');
  }
  return { publicUrl: t.public_url, backendPort };
}

/** Rewrites a public-tunnel URL to point at a specific backend port.
 *  E.g. https://abc.ngrok.io -> http://127.0.0.1:8080 — useful when the
 *  helper needs to call the backend directly but the env only carries the
 *  tunnel URL. */
export function tunnelToLocal(tunnel: TunnelInfo, portOverride?: number): string {
  if (tunnel.publicUrl.includes('ngrok')) {
    return `http://127.0.0.1:${portOverride ?? tunnel.backendPort}`;
  }
  return tunnel.publicUrl.replace(/:\d+/, `:${portOverride ?? tunnel.backendPort}`);
}
