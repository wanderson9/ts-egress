// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Setup } from './Setup';
import { Dashboard } from './Dashboard';
import { Settings } from './Settings';
import { ClientDetails } from './Activity';
import { Footer } from './Footer';
import { safeHTTPS } from './api';
import { Confirm } from './components';

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});
// jsdom does not implement browser CSS.escape, used by Aria table navigation.
beforeEach(() => {
  vi.stubGlobal('CSS', {
    escape: (value: string) => value.replace(/[^a-zA-Z0-9_-]/g, (char) => `\\${char}`),
  });
});
function response(value: unknown, status = 200) {
  return new Response(status === 204 ? null : JSON.stringify(value), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}
describe('React Aria interfaces', () => {
  it('refreshes Settings observations without replacing unsaved drafts or revisions', async () => {
    vi.useFakeTimers();
    let state = 'connected';
    let fail = false;
    const fetchMock = vi.fn(async (input: string, init?: RequestInit) => {
      if (init?.method === 'PUT') return response(null, 204);
      if (input === '/api/geolocation') return response({ enabled: false });
      if (fail) throw new Error('Status lookup failed');
      if (input === '/api/blocks')
        return response({ ips: state === 'connected' ? [] : ['192.0.2.1'] });
      return response({
        settings: { hostname: 'ts-egress', admins: [], email: 'me@example.com', connect: true },
        settingsRevision: state === 'connected' ? 4 : 5,
        tailnet: { state, peers: [] },
      });
    });
    vi.stubGlobal('fetch', fetchMock);
    render(<Settings />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(screen.getByText('Connected')).toBeTruthy();
    fireEvent.change(screen.getByLabelText('Tailscale administrators'), {
      target: { value: 'draft@example.com' },
    });
    state = 'needs-login';
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5100);
    });
    expect(screen.queryByText('Connected')).toBeNull();
    expect(screen.getByText('Connection needs attention')).toBeTruthy();
    expect(screen.getByText('192.0.2.1')).toBeTruthy();
    expect((screen.getByLabelText('Tailscale administrators') as HTMLInputElement).value).toBe(
      'draft@example.com',
    );
    fail = true;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5100);
    });
    expect(screen.getByText('Connection status unavailable')).toBeTruthy();
    expect(screen.getByText(/Displayed list may be outdated/)).toBeTruthy();
    vi.useRealTimers();
    fail = false;
    await userEvent.setup().click(screen.getByRole('button', { name: 'Save settings' }));
    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/setup/settings',
        expect.objectContaining({ headers: expect.objectContaining({ 'If-Match': '4' }) }),
      ),
    );
  });
  it('confirms self-removal and reports a saved lockout without another status request', async () => {
    let statusRequests = 0;
    const fetchMock = vi.fn(async (input: string, init?: RequestInit) => {
      if (init?.method === 'PUT') return response(null, 204);
      if (input === '/api/geolocation') return response({ enabled: false });
      if (input === '/api/blocks') return response({ ips: [] });
      statusRequests++;
      return response({
        identity: 'me@example.com',
        settings: {
          hostname: 'ts-egress',
          admins: ['me@example.com'],
          email: 'me@example.com',
          acceptTerms: true,
          connect: true,
        },
        settingsRevision: 5,
        tailnet: { state: 'connected', peers: [] },
      });
    });
    vi.stubGlobal('fetch', fetchMock);
    const user = userEvent.setup();
    render(<Settings />);
    const admins = await screen.findByLabelText('Tailscale administrators');
    expect(
      (screen.getByLabelText('Initial ACME registration email') as HTMLInputElement).disabled,
    ).toBe(true);
    await user.clear(admins);
    await user.type(admins, 'other@example.com');
    await user.click(screen.getByRole('button', { name: 'Save settings' }));
    await screen.findByRole('dialog', { name: 'Remove your administrator access?' });
    expect(fetchMock.mock.calls.filter((c) => c[1]?.method === 'PUT')).toHaveLength(0);
    const before = statusRequests;
    await user.click(screen.getByRole('button', { name: 'Save and remove my access' }));
    await screen.findByText('Settings saved. Your administrator access was removed.');
    expect(statusRequests).toBe(before);
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/setup/settings',
      expect.objectContaining({
        headers: expect.objectContaining({ 'X-Confirm-Admin-Lockout': '1' }),
      }),
    );
  });
  it('does not present a failed block-list request as an empty list', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: string) => {
        if (input === '/api/blocks') throw new Error('Block storage unavailable');
        if (input === '/api/geolocation') return response({ enabled: false });
        return response({
          settings: { hostname: 'ts-egress', admins: [], email: 'me@example.com', connect: true },
          settingsRevision: 1,
          tailnet: { state: 'connected', peers: [] },
        });
      }),
    );
    render(<Settings />);
    await screen.findByText(/Blocked IP status unavailable/);
    expect(screen.queryByText('No blocked IPs.')).toBeNull();
  });
  it('displays rejected confirmation errors and permits retry', async () => {
    const action = vi
      .fn()
      .mockRejectedValueOnce(new Error('Settings changed; reload'))
      .mockResolvedValue(undefined);
    const user = userEvent.setup();
    render(
      <Confirm label="Save" title="Apply?" onConfirm={action}>
        Apply changes.
      </Confirm>,
    );
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await user.click(screen.getByRole('button', { name: 'Confirm' }));
    expect(await screen.findByRole('alert')).toHaveProperty(
      'textContent',
      'Settings changed; reload',
    );
    await user.click(screen.getByRole('button', { name: 'Confirm' }));
    await waitFor(() => expect(screen.queryByRole('alertdialog')).toBeNull());
    expect(action).toHaveBeenCalledTimes(2);
  });
  it('does not reuse saved backend health for unsaved routes', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        response({
          routes: [{ domain: 'app.example.com', upstream: 'http://server:3000', enabled: true }],
          revision: 1,
          health: {
            'app.example.com': {
              host: { state: 'Online' },
              backend: { state: 'Online' },
              certificate: { state: 'Valid' },
            },
          },
        }),
      ),
    );
    const user = userEvent.setup();
    render(<Dashboard />);
    await screen.findByRole('img', { name: 'Backend Online' });
    await user.click(screen.getByRole('checkbox', { name: 'Enable app.example.com' }));
    await user.click(screen.getByRole('checkbox', { name: 'Enable app.example.com' }));
    expect(screen.getByRole('img', { name: 'Backend unknown' })).toBeTruthy();
  });
  it('shows the running version and a confirmed update link', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => response({ version: 'v0.1', latest: 'v0.2', updateAvailable: true })),
    );
    render(<Footer />);
    const update = await screen.findByRole('link', { name: 'Update available: v0.2' });
    expect(update.getAttribute('href')).toBe('https://github.com/wanderson9/ts-egress/releases');
    expect(screen.getByRole('link', { name: 'ts-egress on GitHub' }).getAttribute('href')).toBe(
      'https://github.com/wanderson9/ts-egress',
    );
    expect(screen.getByRole('link', { name: 'ts-egress v0.1' })).toBeTruthy();
  });
  it('does not advertise an update when the release check fails', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => new Response(null, { status: 503 })),
    );
    render(<Footer />);
    expect(screen.getByRole('link', { name: 'ts-egress' })).toBeTruthy();
    expect(screen.queryByText(/Update available/)).toBeNull();
  });
  it('shows a separate backend service light and page title', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        response({
          routes: [{ domain: 'app.example.com', upstream: 'http://server:3000', enabled: true }],
          revision: 1,
          health: {
            'app.example.com': {
              host: { state: 'Online' },
              backend: {
                state: 'Error',
                detail: '503 Service Unavailable',
                checked: new Date().toISOString(),
              },
              certificate: { state: 'Valid' },
            },
          },
        }),
      ),
    );
    render(<Dashboard />);
    const light = await screen.findByRole('img', { name: 'Backend Error' });
    expect(light.className).toContain('offline');
    expect(light.getAttribute('title')).toContain('503 Service Unavailable');
    expect(screen.getByText('Online')).toBeTruthy();
    expect(document.title).toBe('Services · ts-egress');
  });
  it('shows waiting instead of opening blank sign-in tabs', async () => {
    const open = vi.fn();
    vi.stubGlobal('open', open);
    const fetchMock = vi.fn(async (input: string) =>
      input === '/api/setup/session'
        ? response({ claimed: true, authenticated: true, tailnet: false })
        : input === '/api/setup/reauthenticate'
          ? response({ authURL: '' })
          : response({
              settings: {
                hostname: 'ts-egress',
                admins: [],
                email: 'you@example.com',
                acceptTerms: true,
                connect: true,
                tailnetOnly: true,
              },
              settingsRevision: 1,
              tailnet: { state: 'needs-login', peers: [], magicDNS: true, https: true },
            }),
    );
    vi.stubGlobal('fetch', fetchMock);
    render(<Setup />);
    await userEvent
      .setup()
      .click(await screen.findByRole('button', { name: 'Sign in to Tailscale ↗' }));
    await screen.findByText('Preparing Tailscale sign-in link. Please wait…');
    expect(screen.queryByRole('button', { name: 'Sign in to Tailscale ↗' })).toBeNull();
    expect(open).not.toHaveBeenCalled();
    expect(fetchMock.mock.calls.filter((c) => c[0] === '/api/setup/reauthenticate')).toHaveLength(
      1,
    );
  });
  it('enables country lookups in Settings without using the wizard', async () => {
    const fetchMock = vi.fn(async (input: string, init?: RequestInit) =>
      init?.method === 'PUT'
        ? response(null, 204)
        : input === '/api/geolocation'
          ? response({ enabled: false })
          : input === '/api/blocks'
            ? response({ ips: [] })
            : response({
                settings: {
                  hostname: 'ts-egress',
                  admins: [],
                  email: 'you@example.com',
                  acceptTerms: true,
                  connect: true,
                },
                settingsRevision: 3,
                tailnet: { state: 'connected', peers: [] },
              }),
    );
    vi.stubGlobal('fetch', fetchMock);
    render(<Settings />);
    const user = userEvent.setup();
    await user.click(
      await screen.findByRole('checkbox', {
        name: 'Send selected public client IPs to ipwho.is for country lookup',
      }),
    );
    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/geolocation',
        expect.objectContaining({ method: 'PUT', body: JSON.stringify({ enabled: true }) }),
      ),
    );
    expect(screen.queryByText('Setup progress')).toBeNull();
    await user.type(
      screen.getByLabelText('Tailscale administrators'),
      'one@example.com,two@example.com',
    );
    await user.click(screen.getByRole('button', { name: 'Save settings' }));
    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/setup/settings',
        expect.objectContaining({
          headers: expect.objectContaining({ 'If-Match': '3' }),
          body: expect.stringContaining('"admins":["one@example.com","two@example.com"]'),
        }),
      ),
    );
  });
  it('ranks clients by total traffic and confirms gateway-wide blocking', async () => {
    const fetchMock = vi.fn(async (input: string, init?: RequestInit) =>
      init?.method === 'PUT'
        ? response(null, 204)
        : input === '/api/geolocation'
          ? response({ enabled: false })
          : response({
              activity: {
                requests: 101,
                received: 100,
                sent: 1000,
                errors: 0,
                clients: [
                  {
                    ip: '1.1.1.1',
                    requests: 100,
                    received: 10,
                    sent: 10,
                    errors: 0,
                    lastSeen: 1700000000,
                  },
                  {
                    ip: '8.8.8.8',
                    requests: 1,
                    received: 90,
                    sent: 990,
                    errors: 0,
                    lastSeen: 1700000000,
                  },
                ],
              },
              countries: {},
              blockingAvailable: true,
              blockedIPs: [],
            }),
    );
    vi.stubGlobal('fetch', fetchMock);
    render(<ClientDetails domain="app.example.com" />);
    await screen.findByText('8.8.8.8');
    const lookup = screen.getByRole('link', {
      name: 'Check 8.8.8.8 on AbuseIPDB (opens in a new tab)',
    });
    expect(lookup.getAttribute('href')).toBe('https://www.abuseipdb.com/check/8.8.8.8');
    expect(lookup.getAttribute('target')).toBe('_blank');
    expect(lookup.getAttribute('rel')).toBe('noopener noreferrer');
    const rows = screen.getAllByRole('row');
    expect(rows[1].textContent).toContain('8.8.8.8');
    const user = userEvent.setup();
    await user.click(screen.getAllByRole('button', { name: 'Block IP' })[0]);
    await screen.findByRole('alertdialog', { name: 'Block 8.8.8.8?' });
    await user.click(screen.getByRole('button', { name: 'Confirm' }));
    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/blocks',
        expect.objectContaining({
          method: 'PUT',
          body: JSON.stringify({ ip: '8.8.8.8', blocked: true }),
        }),
      ),
    );
    await screen.findByRole('button', { name: 'Unblock IP' });
  });
  it('keeps the settings draft revision across status refreshes', async () => {
    let revision = 1;
    const fetchMock = vi.fn(async (input: string, init?: RequestInit) => {
      if (input === '/api/setup/session')
        return response({ claimed: true, authenticated: true, tailnet: false });
      if (init?.method === 'PUT') return response(null, 204);
      return response({
        settings: {
          hostname: 'ts-egress',
          admins: [],
          email: 'you@example.com',
          acceptTerms: true,
          connect: true,
        },
        settingsRevision: revision,
        tailnet: { state: 'connecting', magicDNS: false, https: false, peers: [] },
      });
    });
    vi.stubGlobal('fetch', fetchMock);
    render(<Setup />);
    const user = userEvent.setup();
    await screen.findByRole('heading', { name: '2. Connect to Tailscale' });
    revision = 2;
    await user.click(screen.getByRole('button', { name: '← Back' }));
    await user.click(screen.getByRole('button', { name: 'Save & continue →' }));
    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/setup/settings',
        expect.objectContaining({ headers: expect.objectContaining({ 'If-Match': '1' }) }),
      ),
    );
  });
  it('retries failed connections from the sealed local console', async () => {
    const fetchMock = vi.fn(async (input: string) =>
      input === '/api/setup/session'
        ? response({ claimed: true, authenticated: true, tailnet: false })
        : input === '/api/setup/connect'
          ? response(null, 202)
          : response({
              settings: {
                hostname: 'ts-egress',
                admins: [],
                email: 'you@example.com',
                acceptTerms: true,
                connect: true,
                tailnetOnly: true,
              },
              settingsRevision: 1,
              tailnet: {
                state: 'error',
                error: 'listener failed',
                magicDNS: false,
                https: false,
                peers: [],
              },
            }),
    );
    vi.stubGlobal('fetch', fetchMock);
    render(<Setup />);
    await userEvent.setup().click(await screen.findByRole('button', { name: 'Retry connection' }));
    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/setup/connect',
        expect.objectContaining({ method: 'POST' }),
      ),
    );
    expect(
      screen.getByRole('link', { name: 'Change administrator password' }).getAttribute('href'),
    ).toBe('/password');
  });
  it('changes the sealed local password without redirecting to the tailnet', async () => {
    const assign = vi.fn();
    vi.stubGlobal('location', { ...window.location, pathname: '/password', search: '', assign });
    const fetchMock = vi.fn(async (input: string) =>
      input === '/api/setup/session'
        ? response({ claimed: true, authenticated: true, tailnet: false })
        : input === '/api/setup/password'
          ? response(null, 204)
          : response({
              settings: {
                hostname: 'ts-egress',
                admins: [],
                email: '',
                acceptTerms: true,
                connect: true,
                tailnetOnly: true,
              },
              settingsRevision: 1,
              tailnet: {
                state: 'connected',
                adminURL: 'https://egress.tail.ts.net',
                magicDNS: true,
                https: true,
                peers: [],
              },
            }),
    );
    vi.stubGlobal('fetch', fetchMock);
    render(<Setup />);
    const user = userEvent.setup();
    await user.type(
      await screen.findByLabelText('Current password'),
      'initial administrator password',
    );
    await user.type(screen.getByLabelText('New password'), 'replacement administrator password');
    await user.click(screen.getByRole('button', { name: 'Change password' }));
    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/setup/password',
        expect.objectContaining({ method: 'POST' }),
      ),
    );
    expect(assign).not.toHaveBeenCalled();
  });
  it('publishes with the revision from the freshly fetched route list', async () => {
    let revision = 1;
    const fetchMock = vi.fn(async (input: string, init?: RequestInit) => {
      if (input === '/api/setup/session')
        return response({ claimed: true, authenticated: true, tailnet: true });
      if (input === '/api/routes') return response(null, 204);
      if (input === '/api/status') return response({ routes: [], revision });
      if (input === '/api/geolocation') return response({ enabled: false });
      return response({
        settings: {
          hostname: 'ts-egress',
          admins: [],
          email: 'you@example.com',
          acceptTerms: true,
          connect: true,
        },
        settingsRevision: 1,
        tailnet: {
          state: 'connected',
          adminURL: 'https://egress.tail.ts.net',
          magicDNS: true,
          https: true,
          peers: [{ name: 'server', ips: ['100.100.1.2'], online: true }],
        },
      });
    });
    vi.stubGlobal('fetch', fetchMock);
    render(<Setup />);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Continue →' }));
    await user.selectOptions(screen.getByLabelText('Tailnet device'), 'server');
    await user.type(screen.getByLabelText('Public domain'), 'new.example.com');
    revision = 7;
    await user.click(screen.getByRole('button', { name: 'Publish & finish →' }));
    await user.click(screen.getByRole('button', { name: 'Confirm' }));
    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/routes',
        expect.objectContaining({ headers: expect.objectContaining({ 'If-Match': '7' }) }),
      ),
    );
  });
  it('keeps the editing revision when polling newer routes', async () => {
    let revision = 1;
    const fetchMock = vi.fn(async (_input: string, init?: RequestInit) => {
      if (init?.method === 'PUT') return response(null, 204);
      return response({
        revision,
        routes: [{ domain: 'app.example.com', upstream: 'http://server:3000', enabled: true }],
      });
    });
    vi.stubGlobal('fetch', fetchMock);
    render(<Dashboard />);
    const user = userEvent.setup();
    const toggle = await screen.findByRole('checkbox', { name: 'Enable app.example.com' });
    revision = 2;
    await user.click(toggle);
    // Becoming dirty restarts the status poll immediately.
    await waitFor(() => expect(fetchMock.mock.calls.length).toBeGreaterThan(1));
    await user.click(screen.getByRole('button', { name: 'Save changes' }));
    await user.click(screen.getByRole('button', { name: 'Confirm' }));
    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/routes',
        expect.objectContaining({
          method: 'PUT',
          headers: expect.objectContaining({ 'If-Match': '1' }),
        }),
      ),
    );
  });
  it('labels first-run authentication and claims an account', async () => {
    let claimed = false;
    const fetchMock = vi.fn(async (input: string) => {
      if (input === '/api/setup/session') return response({ claimed, authenticated: claimed });
      if (input === '/api/setup/claim') {
        claimed = true;
        return response(null, 204);
      }
      return response({
        settings: {
          hostname: 'ts-egress',
          admins: [],
          email: '',
          acceptTerms: false,
          connect: false,
        },
        tailnet: {
          state: 'not-connected',
          peers: [],
          magicDNS: false,
          https: false,
        },
      });
    });
    vi.stubGlobal('fetch', fetchMock);
    render(<Setup />);
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText('Administrator password'), 'a long test password');
    await user.type(
      screen.getByLabelText('Confirm administrator password'),
      'a different password',
    );
    await user.click(screen.getByRole('button', { name: 'Create administrator →' }));
    await screen.findByText('Passwords do not match.');
    expect(fetchMock.mock.calls.some((call) => call[0] === '/api/setup/claim')).toBe(false);
    await user.clear(screen.getByLabelText('Confirm administrator password'));
    await user.type(
      screen.getByLabelText('Confirm administrator password'),
      'a long test password',
    );
    await user.click(screen.getByRole('button', { name: 'Create administrator →' }));
    await screen.findByRole('heading', { name: '1. Configure this gateway' });
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/setup/claim',
      expect.objectContaining({
        method: 'POST',
        body: JSON.stringify({ password: 'a long test password' }),
      }),
    );
  });
  it('shows one setup page at a time and leaves device refresh under user control', async () => {
    const settings = {
      hostname: 'ts-egress',
      admins: ['you@example.com'],
      email: 'you@example.com',
      acceptTerms: true,
      connect: true,
    };
    const fetchMock = vi.fn(async (input: string) =>
      input === '/api/setup/session'
        ? response({ claimed: true, authenticated: true })
        : input === '/api/status'
          ? response({ routes: [], revision: 1 })
          : response({
              settings,
              tailnet: {
                state: 'connected',
                adminURL: 'https://egress.tail.ts.net',
                magicDNS: true,
                https: true,
                peers: [
                  {
                    name: 'server.tail.ts.net',
                    ips: ['100.100.1.2'],
                    online: true,
                  },
                  {
                    name: 'other.tail.ts.net',
                    ips: ['100.100.1.3'],
                    online: true,
                  },
                ],
              },
            }),
    );
    vi.stubGlobal('fetch', fetchMock);
    render(<Setup />);
    const user = userEvent.setup();
    await screen.findByRole('heading', { name: '2. Connect to Tailscale' });
    expect(screen.queryByLabelText('Public domain')).toBeNull();
    await user.click(screen.getByRole('button', { name: 'Continue →' }));
    await screen.findByRole('heading', { name: '3. Publish a service' });
    expect(screen.queryByRole('heading', { name: '2. Connect to Tailscale' })).toBeNull();
    await user.selectOptions(screen.getByLabelText('Tailnet device'), 'server.tail.ts.net');
    await user.type(screen.getByLabelText('Public domain'), 'app.example.com');
    await user.click(screen.getByRole('button', { name: 'Refresh devices' }));
    await waitFor(() =>
      expect((screen.getByLabelText('Tailnet device') as HTMLSelectElement).value).toBe(
        'server.tail.ts.net',
      ),
    );
    expect((screen.getByLabelText('Public domain') as HTMLInputElement).value).toBe(
      'app.example.com',
    );
    await user.click(screen.getByRole('button', { name: '← Back' }));
    await screen.findByRole('heading', { name: '2. Connect to Tailscale' });
  });
  it('supports keyboard route controls and accessible confirmation', async () => {
    const fetchMock = vi.fn(async (_input: string, init?: RequestInit) =>
      init?.method === 'PUT'
        ? response(null, 204)
        : response({
            routes: [
              {
                domain: 'app.example.com',
                upstream: 'http://server:3000',
                enabled: true,
              },
            ],
            requests: 42,
            failures: 0,
          }),
    );
    vi.stubGlobal('fetch', fetchMock);
    render(<Dashboard />);
    const user = userEvent.setup();
    const toggle = await screen.findByRole('checkbox', {
      name: 'Enable app.example.com',
    });
    toggle.focus();
    await user.keyboard(' ');
    expect((toggle as HTMLInputElement).checked).toBe(false);
    await user.click(screen.getByRole('button', { name: 'Save changes' }));
    const dialog = await screen.findByRole('alertdialog', {
      name: 'Apply service changes?',
    });
    expect(dialog).toBeTruthy();
    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(screen.queryByRole('alertdialog')).toBeNull());
    expect(fetchMock.mock.calls.filter((call) => call[1]?.method === 'PUT')).toHaveLength(0);
  });
  it('recovers route loading when the first status request fails', async () => {
    let requests = 0;
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => {
        requests++;
        if (requests === 1) return new Response('temporarily unavailable', { status: 503 });
        return response({
          routes: [{ domain: 'app.example.com', upstream: 'http://server:3000', enabled: true }],
        });
      }),
    );
    render(<Dashboard />);
    await userEvent.setup().click(screen.getByRole('button', { name: 'Refresh' }));
    await screen.findByRole('button', { name: 'Edit app.example.com' });
    expect(
      (screen.getByRole('button', { name: 'Add service' }) as HTMLButtonElement).disabled,
    ).toBe(false);
  });
  it('does not render unsafe enrollment links', () => {
    expect(safeHTTPS('javascript:alert(1)')).toBeUndefined();
    expect(safeHTTPS('http://example.com')).toBeUndefined();
    expect(safeHTTPS('https://login.tailscale.com/a')).toBe('https://login.tailscale.com/a');
  });
  it('distinguishes enabled routing from offline hosts and certificate errors', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        response({
          routes: [
            {
              domain: 'app.example.com',
              upstream: 'http://server:3000',
              enabled: true,
            },
          ],
          sites: {},
          health: {
            'app.example.com': {
              host: { state: 'Offline' },
              certificate: { state: 'Error', detail: 'DNS validation failed' },
            },
          },
        }),
      ),
    );
    render(<Dashboard />);
    await screen.findByText('Offline');
    expect(screen.getByText('Failed')).toBeTruthy();
    expect(screen.queryByText('Live')).toBeNull();
    expect(
      (
        screen.getByRole('checkbox', {
          name: 'Enable app.example.com',
        }) as HTMLInputElement
      ).checked,
    ).toBe(true);
  });
  it('opens certificate failure details and submits a retry', async () => {
    const fetchMock = vi.fn(async (input: string) =>
      input === '/api/certificate/retry'
        ? response(null, 202)
        : response({
            routes: [
              {
                domain: 'app.example.com',
                upstream: 'http://server:3000',
                enabled: true,
              },
            ],
            health: {
              'app.example.com': {
                host: { state: 'Online' },
                certificate: {
                  state: 'Failed',
                  detail: 'DNS validation failed',
                },
              },
            },
          }),
    );
    vi.stubGlobal('fetch', fetchMock);
    render(<Dashboard />);
    const user = userEvent.setup();
    await user.click(
      await screen.findByRole('button', {
        name: 'Certificate failed for app.example.com',
      }),
    );
    await screen.findByRole('dialog', { name: 'Certificate: app.example.com' });
    expect(screen.getByText('DNS validation failed')).toBeTruthy();
    await user.click(screen.getByRole('button', { name: 'Retry' }));
    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/certificate/retry',
        expect.objectContaining({
          method: 'POST',
          body: JSON.stringify({ domain: 'app.example.com' }),
        }),
      ),
    );
  });
  it('adds and edits services in dialogs', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: string) =>
        input === '/api/setup/status'
          ? response({
              tailnet: {
                peers: [{ name: 'server', ips: ['100.100.1.2'], online: true }],
              },
            })
          : response({ routes: [], health: {} }),
      ),
    );
    render(<Dashboard />);
    const user = userEvent.setup();
    await waitFor(() =>
      expect(
        (
          screen.getByRole('button', {
            name: 'Add service',
          }) as HTMLButtonElement
        ).disabled,
      ).toBe(false),
    );
    await user.click(screen.getByRole('button', { name: 'Add service' }));
    await screen.findByRole('dialog', { name: 'Add service' });
    await user.type(screen.getByLabelText('Domain'), 'new.example.com');
    await screen.findByRole('option', { name: 'server' });
    await user.selectOptions(screen.getByLabelText('Host'), 'server');
    expect((screen.getByLabelText('Port') as HTMLInputElement).value).toBe('3000');
    const buttons = screen.getAllByRole('button', { name: 'Add service' });
    await user.click(buttons[buttons.length - 1]);
    await user.click(await screen.findByRole('button', { name: 'Edit new.example.com' }));
    await screen.findByRole('dialog', { name: 'Edit service' });
    expect((screen.getByLabelText('Domain') as HTMLInputElement).value).toBe('new.example.com');
    expect((screen.getByLabelText('Host') as HTMLSelectElement).value).toBe('server');
    expect((screen.getByLabelText('Port') as HTMLInputElement).value).toBe('3000');
  });
  it('requires reauthentication only when reported by the backend', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => response({ routes: [], connection: { needsLogin: true } })),
    );
    render(<Dashboard />);
    await screen.findByRole('alertdialog', {
      name: 'Tailscale authentication required',
    });
    expect(screen.getByRole('link', { name: 'Back to setup →' }).getAttribute('href')).toBe(
      '/setup?step=2',
    );
  });
  it('skips publishing when a service already exists', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: string) =>
        input === '/api/setup/session'
          ? response({ claimed: true, authenticated: true, tailnet: true })
          : input === '/api/status'
            ? response({ routes: [{ domain: 'app.example.com' }], revision: 1 })
            : response({
                settings: {
                  hostname: 'ts-egress',
                  admins: ['you@example.com'],
                  email: 'you@example.com',
                  acceptTerms: true,
                  connect: true,
                },
                tailnet: {
                  state: 'connected',
                  magicDNS: true,
                  https: true,
                  adminURL: 'https://egress.tail.ts.net',
                  peers: [],
                },
              }),
      ),
    );
    render(<Setup />);
    await screen.findByRole('heading', { name: 'Service configured' });
    expect(screen.queryByRole('heading', { name: '3. Publish a service' })).toBeNull();
    expect(screen.queryByText(/Publish/)).toBeNull();
    expect(screen.getByRole('button', { name: 'Services' })).toBeTruthy();
    await userEvent.setup().click(screen.getByRole('button', { name: 'Gateway settings' }));
    await screen.findByRole('heading', { name: '1. Configure this gateway' });
    expect(screen.getByLabelText('Tailscale administrators')).toBeTruthy();
  });
  it('hands the sealed local console off to the private dashboard', async () => {
    const assign = vi.fn();
    vi.stubGlobal('location', { ...window.location, search: '', assign });
    const fetchMock = vi.fn(async (input: string) =>
      input === '/api/setup/session'
        ? response({ claimed: true, authenticated: true, tailnet: false })
        : response({
            settings: {
              hostname: 'ts-egress',
              admins: [],
              email: 'you@example.com',
              acceptTerms: true,
              connect: true,
              tailnetOnly: true,
            },
            tailnet: {
              state: 'connected',
              magicDNS: true,
              https: true,
              adminURL: 'https://egress.tail.ts.net',
              peers: [],
            },
          }),
    );
    vi.stubGlobal('fetch', fetchMock);
    render(<Setup />);
    await screen.findByRole('heading', { name: '2. Connect to Tailscale' });
    await screen.findByText(/Opening the private dashboard/);
    await waitFor(() => expect(assign).toHaveBeenCalledWith('https://egress.tail.ts.net/'));
    expect(screen.queryByRole('button', { name: 'Publish & finish →' })).toBeNull();
    expect(fetchMock.mock.calls.some((c) => c[0] === '/api/status')).toBe(false);
  });
  it('prepares a sign-in link without opening a blank tab', async () => {
    const tab = { location: { href: '' }, close: vi.fn() };
    vi.stubGlobal(
      'open',
      vi.fn(() => tab),
    );
    const fetchMock = vi.fn(async (input: string) =>
      input === '/api/setup/session'
        ? response({ claimed: true, authenticated: true, tailnet: false })
        : input === '/api/setup/reauthenticate'
          ? response({ authURL: 'https://login.tailscale.com/a/xyz' })
          : response({
              settings: {
                hostname: 'ts-egress',
                admins: [],
                email: 'you@example.com',
                acceptTerms: true,
                connect: true,
                tailnetOnly: true,
              },
              tailnet: {
                state: 'needs-login',
                magicDNS: true,
                https: true,
                peers: [],
              },
            }),
    );
    vi.stubGlobal('fetch', fetchMock);
    render(<Setup />);
    const user = userEvent.setup();
    await screen.findByRole('heading', { name: '2. Connect to Tailscale' });
    expect(screen.getByText(/Signed out of Tailscale/)).toBeTruthy();
    expect(
      (
        screen.getByRole('button', {
          name: 'Open private dashboard',
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(true);
    await user.click(screen.getByRole('button', { name: 'Sign in to Tailscale ↗' }));
    const link = await screen.findByRole('link', { name: 'Authorize on Tailscale ↗' });
    expect(link.getAttribute('href')).toBe('https://login.tailscale.com/a/xyz');
    expect(window.open).not.toHaveBeenCalled();
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/setup/reauthenticate',
      expect.objectContaining({ method: 'POST' }),
    );
  });
  it('offers Tailscale logout only in a connected tailnet session', async () => {
    const fetchMock = vi.fn(async (input: string) =>
      input === '/api/setup/session'
        ? response({ claimed: true, authenticated: true, tailnet: true })
        : input === '/api/status'
          ? response({ routes: [] })
          : input === '/api/geolocation'
            ? response({ enabled: false })
            : input === '/api/setup/tailscale/logout'
              ? response(null, 204)
              : response({
                  settings: {
                    hostname: 'ts-egress',
                    admins: ['you@example.com'],
                    email: 'you@example.com',
                    acceptTerms: true,
                    connect: true,
                  },
                  tailnet: {
                    state: 'connected',
                    magicDNS: true,
                    https: true,
                    adminURL: 'https://egress.tail.ts.net',
                    peers: [],
                  },
                }),
    );
    vi.stubGlobal('fetch', fetchMock);
    render(<Setup />);
    const user = userEvent.setup();
    await screen.findByRole('heading', { name: '2. Connect to Tailscale' });
    const trigger = await screen.findByRole('button', {
      name: 'Log out of Tailscale',
    });
    expect(trigger.className).toContain('danger');
    await user.click(trigger);
    const confirm = await screen.findByRole('button', { name: 'Confirm' });
    expect(confirm.className).toContain('danger');
    await user.click(confirm);
    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/setup/tailscale/logout',
        expect.objectContaining({ method: 'POST' }),
      ),
    );
    await screen.findByRole('alertdialog', { name: 'Tailscale logout requested' });
    expect(screen.getByText('http://127.0.0.1:8080')).toBeTruthy();
  });
  it('expands client activity and displays country flags without changing routes', async () => {
    const fetchMock = vi.fn(async (input: string) =>
      input.startsWith('/api/activity')
        ? response({
            activity: {
              requests: 3,
              received: 5,
              sent: 4096,
              errors: 0,
              limited: false,
              clients: [
                {
                  ip: '8.8.8.8',
                  requests: 3,
                  received: 5,
                  sent: 4096,
                  errors: 0,
                  lastSeen: 1700000000,
                },
              ],
            },
            countries: { '8.8.8.8': { country: 'United States', flag: '🇺🇸' } },
          })
        : input === '/api/geolocation'
          ? response({ enabled: true })
          : response({
              routes: [
                {
                  domain: 'app.example.com',
                  upstream: 'http://server:3000',
                  enabled: true,
                },
              ],
              requests: 3,
              failures: 0,
              sites: {
                'app.example.com': {
                  requests: 3,
                  received: 5,
                  sent: 4096,
                  errors: 0,
                },
              },
            }),
    );
    vi.stubGlobal('fetch', fetchMock);
    render(<Dashboard />);
    const user = userEvent.setup();
    const expand = await screen.findByRole('button', {
      name: '▸ app.example.com',
    });
    expect(fetchMock.mock.calls.filter((c) => c[0].startsWith('/api/activity'))).toHaveLength(0);
    await user.click(expand);
    await screen.findByText('🇺🇸 United States');
    expect(screen.getByText('8.8.8.8')).toBeTruthy();
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/geolocation',
      expect.objectContaining({ method: 'GET' }),
    );
    expect(fetchMock.mock.calls.filter((c) => c[0].includes('geolocation=enabled'))).toHaveLength(
      0,
    );
    await user.click(screen.getByLabelText('Look up public IP countries'));
    await waitFor(() =>
      expect(fetchMock.mock.calls.some((c) => String(c[0]).includes('geolocation=enabled'))).toBe(
        true,
      ),
    );
    await user.click(screen.getByRole('button', { name: '▾ app.example.com' }));
    expect(screen.queryByText('8.8.8.8')).toBeNull();
  });
});
