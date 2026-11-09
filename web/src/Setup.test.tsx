// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Setup } from './Setup';

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const response = (value: unknown, status = 200) =>
  new Response(status === 204 ? null : JSON.stringify(value), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });

function setup() {
  const fetcher = vi.fn(async (url: string, init?: RequestInit) => {
    if (url === '/api/setup/session')
      return response({ claimed: true, authenticated: true, tailnet: false });
    if (url === '/api/setup/status')
      return response({
        settings: {
          hostname: 'ts-egress',
          admins: [],
          email: '',
          acceptTerms: false,
          connect: false,
        },
        settingsRevision: 1,
        tailnet: { state: 'not-connected', peers: [] },
      });
    if (url === '/api/setup/settings' && init?.method === 'PUT') return response(null, 204);
    return response({});
  });
  vi.stubGlobal('fetch', fetcher);
  vi.stubGlobal('CSS', { escape: (value: string) => value });
  return fetcher;
}

describe('first setup administrator access', () => {
  it('requires administrator login names by default and exposes explicit warned open access opt-in', async () => {
    const fetcher = setup();
    render(<Setup />);
    const user = userEvent.setup();
    const admins = await screen.findByLabelText('Tailscale administrators');

    expect(screen.getByText(/At least one administrator is required/)).toBeTruthy();
    expect(screen.getByRole('checkbox', { name: /Allow all untagged tailnet users/ })).toBeTruthy();
    expect(screen.getByRole('alert').textContent).toMatch(/full gateway administration/i);

    fireEvent.change(screen.getByLabelText('Initial ACME registration email'), {
      target: { value: 'admin@example.com' },
    });
    await user.click(screen.getByRole('checkbox', { name: /I accept the Let's Encrypt/ }));
    await user.click(screen.getByRole('button', { name: /Save & continue/ }));
    expect(await screen.findByText(/explicitly opt in to open access/i)).toBeTruthy();
    expect(fetcher.mock.calls.some(([url]) => url === '/api/setup/settings')).toBe(false);

    await user.click(screen.getByRole('checkbox', { name: /Allow all untagged tailnet users/ }));
    await user.click(screen.getByRole('button', { name: /Save & continue/ }));
    await waitFor(() =>
      expect(fetcher).toHaveBeenCalledWith('/api/setup/settings', expect.anything()),
    );
    const saveCall = fetcher.mock.calls.find(([url]) => url === '/api/setup/settings');
    const body = JSON.parse(String(saveCall?.[1]?.body));
    expect(body.admins).toEqual([]);
    expect(body.openAccess).toBe(true);
  });
});
