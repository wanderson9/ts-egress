export type Route = { domain: string; upstream: string; enabled: boolean };
export type Totals = {
  requests: number;
  received: number;
  sent: number;
  errors: number;
};
export type ServiceHealth = {
  host: { state: string; detail?: string };
  backend?: { state: string; detail?: string; checked?: string };
  certificate: { state: string; attempt?: string; detail?: string; expires?: string };
  lastError?: string;
  lastResponse?: string;
};
export type Client = Totals & { ip: string; lastSeen: number };
export type Activity = {
  activity: Totals & { clients: Client[]; limited: boolean };
  countries: Record<string, { country: string; flag: string }>;
  blockedIPs?: string[];
  blockingAvailable?: boolean;
};
export type Settings = {
  hostname: string;
  admins: string[];
  email: string;
  acceptTerms: boolean;
  connect: boolean;
  tailnetOnly?: boolean;
  geolocation?: boolean;
};
export type SetupStatus = {
  identity?: string;
  settings: Settings;
  settingsRevision: number;
  geolocation: boolean;
  tailnet: {
    state: string;
    authURL?: string;
    adminURL?: string;
    error?: string;
    magicDNS: boolean;
    https: boolean;
    peers: { name: string; ips: string[]; online: boolean }[];
  };
};
export async function api<T>(
  path: string,
  method = 'GET',
  body?: unknown,
  headers: Record<string, string> = {},
): Promise<T> {
  const response = await fetch(path, {
    method,
    credentials: 'same-origin',
    headers:
      method === 'GET'
        ? headers
        : { 'Content-Type': 'application/json', 'X-TS-Egress': '1', ...headers },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!response.ok) {
    if (response.status === 401) window.location.assign('/');
    throw new Error((await response.text()).trim() || `Request failed (${response.status})`);
  }
  return response.status === 204 || response.status === 202 ? (undefined as T) : response.json();
}
export function safeHTTPS(value?: string): string | undefined {
  try {
    const url = new URL(value || '');
    return url.protocol === 'https:' ? url.href : undefined;
  } catch {
    return undefined;
  }
}
