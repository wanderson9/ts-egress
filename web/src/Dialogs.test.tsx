// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Dashboard } from './Dashboard';
import { validPublicDomain } from './Dialogs';

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('validPublicDomain', () => {
  it('matches lowercase dotted DNS rules and actual IPv4 rejection', () => {
    expect(validPublicDomain('app.example.com')).toBe(true);
    expect(validPublicDomain('App.example.com')).toBe(false);
    expect(validPublicDomain('192.0.2.1')).toBe(false);
    expect(validPublicDomain('999.1.2.3')).toBe(true);
    expect(validPublicDomain('-bad.example.com')).toBe(false);
    expect(validPublicDomain(`${'a'.repeat(64)}.example.com`)).toBe(false);
  });
});

function status(certificates: Record<string, unknown> = {}) {
  return new Response(
    JSON.stringify({
      routes: ['app.example.com', 'other.example.com'].map((domain) => ({
        domain,
        upstream: 'http://host:3000',
        enabled: true,
      })),
      revision: 1,
      health: Object.fromEntries(
        Object.entries(certificates).map(([domain, certificate]) => [
          domain,
          { host: { state: 'Online' }, certificate },
        ]),
      ),
    }),
    { status: 200 },
  );
}

describe('Dashboard interactions', () => {
  it('opens renewal failure details and permits retry when validity remains Valid', async () => {
    const fetchMock = vi.fn(async (input: string) =>
      input === '/api/certificate/retry'
        ? new Response(null, { status: 202 })
        : status({
            'app.example.com': { state: 'Valid', attempt: 'Failed', detail: 'Renewal failed' },
          }),
    );
    vi.stubGlobal('fetch', fetchMock);
    render(<Dashboard />);
    await userEvent
      .setup()
      .click(await screen.findByRole('button', { name: 'Certificate failed for app.example.com' }));
    expect(screen.getByText('Valid')).toBeTruthy();
    expect(screen.getByText('Renewal failed')).toBeTruthy();
    await userEvent.setup().click(screen.getByRole('button', { name: 'Retry' }));
    expect(fetchMock).toHaveBeenCalledWith('/api/certificate/retry', expect.anything());
  });

  it('filters routes and leaves validation errors in the editor', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: string) =>
        input === '/api/setup/status'
          ? new Response(
              JSON.stringify({ tailnet: { peers: [{ name: 'host', ips: [], online: true }] } }),
              { status: 200 },
            )
          : status(),
      ),
    );
    render(<Dashboard />);
    await screen.findByRole('button', { name: 'Certificate unknown for app.example.com' });
    fireEvent.change(screen.getByLabelText('Filter domains'), { target: { value: 'other' } });
    expect(
      screen.queryByRole('button', { name: 'Certificate unknown for app.example.com' }),
    ).toBeNull();
    await userEvent.setup().click(screen.getByRole('button', { name: 'Add service' }));
    fireEvent.change(await screen.findByLabelText('Domain'), {
      target: { value: '-bad.example.com' },
    });
    const dialog = screen.getByRole('dialog', { name: 'Add service' });
    fireEvent.change(await within(dialog).findByLabelText('Host'), {
      target: { value: 'host' },
    });
    await userEvent.setup().click(within(dialog).getByRole('button', { name: 'Add service' }));
    expect(await screen.findByRole('alert')).toBeTruthy();
  });
});
