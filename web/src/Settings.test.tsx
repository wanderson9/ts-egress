// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Settings } from './Settings';

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});
beforeEach(() => vi.stubGlobal('CSS', { escape: (v: string) => v }));
const response = (value: unknown, status = 200) =>
  new Response(status === 204 ? null : JSON.stringify(value), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
function setup(admins: string[] = ['admin@example.com']) {
  const fetcher = vi.fn(async (url: string, init?: RequestInit) => {
    if (init?.method === 'PUT') return response(null, 204);
    if (url === '/api/geolocation') return response({ enabled: false });
    if (url === '/api/blocks') return response({ ips: [] });
    return response({
      settings: {
        hostname: 'gateway',
        admins,
        email: 'admin@example.com',
        acceptTerms: true,
        connect: true,
      },
      settingsRevision: 3,
      identity: 'admin@example.com',
      tailnet: { state: 'connected', peers: [] },
    });
  });
  vi.stubGlobal('fetch', fetcher);
  return fetcher;
}
describe('Settings drafts and administrator changes', () => {
  it('locks the settings draft during an immediate country-lookup preference save', async () => {
    const fetcher = setup();
    render(<Settings />);
    const admins = (await screen.findByLabelText('Tailscale administrators')) as HTMLInputElement;
    fireEvent.change(admins, { target: { value: 'admin@example.com,other@example.com' } });
    let finish!: (value: Response) => void;
    fetcher.mockImplementationOnce(
      () =>
        new Promise<Response>((resolve) => {
          finish = resolve;
        }),
    );
    await userEvent.setup().click(
      screen.getByRole('checkbox', {
        name: 'Send selected public client IPs to ipwho.is for country lookup',
      }),
    );
    expect(admins.disabled).toBe(true);
    fireEvent.submit(admins.closest('form')!);
    expect(fetcher.mock.calls.some(([url]) => url === '/api/setup/settings')).toBe(false);
    finish(response(null, 204));
    await waitFor(() => expect(admins.disabled).toBe(false));
  });
  it('reports that settings persisted when the follow-up status request fails', async () => {
    const fetcher = setup();
    render(<Settings />);
    const input = await screen.findByLabelText('Tailscale administrators');
    let persisted = false;
    fetcher.mockImplementation(async (url: string, init?: RequestInit) => {
      if (url === '/api/setup/settings' && init?.method === 'PUT') {
        persisted = true;
        return response(null, 204);
      }
      if (persisted) throw new Error('offline');
      return response({ ips: [] });
    });
    fireEvent.change(input, { target: { value: 'admin@example.com,other@example.com' } });
    await userEvent.setup().click(screen.getByRole('button', { name: 'Save settings' }));
    expect(
      await screen.findByText(/Settings were saved, but status could not be refreshed/),
    ).toBeTruthy();
  });
  it('disables save when unchanged and when normalized admins are unchanged', async () => {
    setup();
    render(<Settings />);
    const admins = await screen.findByLabelText('Tailscale administrators');
    const save = screen.getByRole('button', { name: 'Save settings' });
    expect((save as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(admins, { target: { value: ' admin@example.com, ' } });
    expect((save as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(admins, { target: { value: 'other@example.com' } });
    expect((save as HTMLButtonElement).disabled).toBe(false);
    fireEvent.change(admins, { target: { value: 'admin@example.com' } });
    expect((save as HTMLButtonElement).disabled).toBe(true);
  });
  it('cancel and confirm reload preserve/discard draft respectively', async () => {
    setup();
    render(<Settings />);
    const user = userEvent.setup();
    const input = await screen.findByLabelText('Tailscale administrators');
    fireEvent.change(input, { target: { value: 'draft@example.com' } });
    await user.click(screen.getByRole('button', { name: 'Reload settings' }));
    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    expect((input as HTMLInputElement).value).toBe('draft@example.com');
    await user.click(screen.getByRole('button', { name: 'Reload settings' }));
    await user.click(screen.getByRole('button', { name: 'Confirm' }));
    await waitFor(() =>
      expect((screen.getByLabelText('Tailscale administrators') as HTMLInputElement).value).toBe(
        'admin@example.com',
      ),
    );
  });
  it('guards restricted-to-empty and sends only broadening confirmation', async () => {
    const fetcher = setup();
    render(<Settings />);
    const user = userEvent.setup();
    fireEvent.change(await screen.findByLabelText('Tailscale administrators'), {
      target: { value: '' },
    });
    await user.click(screen.getByRole('button', { name: 'Save settings' }));
    expect(
      await screen.findByRole('dialog', { name: 'Allow all untagged tailnet users?' }),
    ).toBeTruthy();
    await user.click(screen.getByRole('button', { name: 'Allow access and save' }));
    await waitFor(() =>
      expect(fetcher).toHaveBeenCalledWith(
        '/api/setup/settings',
        expect.objectContaining({
          headers: expect.objectContaining({ 'X-Confirm-Admin-Change': '1' }),
        }),
      ),
    );
    const headers = fetcher.mock.calls.find(([url]) => url === '/api/setup/settings')?.[1]
      ?.headers as Record<string, string>;
    expect(headers['X-Confirm-Admin-Lockout']).toBeUndefined();
  });
  it('warns before unload while dirty', async () => {
    setup();
    render(<Settings />);
    fireEvent.change(await screen.findByLabelText('Tailscale administrators'), {
      target: { value: 'other@example.com' },
    });
    const event = new Event('beforeunload', { cancelable: true });
    window.dispatchEvent(event);
    expect(event.defaultPrevented).toBe(true);
  });
  it('locks fields while a confirmed reload is pending', async () => {
    const fetcher = setup();
    render(<Settings />);
    const input = (await screen.findByLabelText('Tailscale administrators')) as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'admin@example.com,draft@example.com' } });
    let finish!: (value: Response) => void;
    fetcher.mockImplementationOnce(
      () =>
        new Promise<Response>((resolve) => {
          finish = resolve;
        }),
    );
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Reload settings' }));
    await user.click(screen.getByRole('button', { name: 'Confirm' }));
    expect(input.disabled).toBe(true);
    finish(
      response({
        settings: {
          hostname: 'gateway',
          admins: ['admin@example.com'],
          email: 'admin@example.com',
          acceptTerms: true,
          connect: true,
        },
        settingsRevision: 4,
        identity: 'admin@example.com',
        tailnet: { state: 'connected', peers: [] },
      }),
    );
    await waitFor(() => expect(input.disabled).toBe(false));
    expect(input.value).toBe('admin@example.com');
    expect(
      (screen.getByRole('button', { name: 'Save settings' }) as HTMLButtonElement).disabled,
    ).toBe(true);
  });
  it('locks the draft during a save and preserves it when the save fails', async () => {
    let finish!: (value: Response) => void;
    const fetcher = setup();
    fetcher.mockImplementation(async (url: string, init?: RequestInit) => {
      if (url === '/api/setup/settings' && init?.method === 'PUT')
        return new Promise<Response>((resolve) => {
          finish = resolve;
        });
      if (url === '/api/geolocation') return response({ enabled: false });
      if (url === '/api/blocks') return response({ ips: [] });
      if (url === '/api/setup/status')
        return response({
          settings: {
            hostname: 'gateway',
            admins: ['admin@example.com'],
            email: 'admin@example.com',
            acceptTerms: true,
            connect: true,
          },
          settingsRevision: 3,
          identity: 'admin@example.com',
          tailnet: { state: 'connected', peers: [] },
        });
      return response(null, 204);
    });
    render(<Settings />);
    const input = await screen.findByLabelText('Tailscale administrators');
    fireEvent.change(input, {
      target: { value: 'admin@example.com,draft@example.com' },
    });
    fireEvent.click(screen.getByRole('button', { name: 'Save settings' }));
    await waitFor(() => expect((input as HTMLInputElement).disabled).toBe(true));
    expect(
      (screen.getByRole('button', { name: 'Reload settings' }) as HTMLButtonElement).disabled,
    ).toBe(true);
    finish(response({ error: 'save failed' }, 500));
    await screen.findByText(/save failed/);
    expect((screen.getByLabelText('Tailscale administrators') as HTMLInputElement).value).toBe(
      'admin@example.com,draft@example.com',
    );
  });
});
