// @vitest-environment jsdom
import { afterEach, expect, it, vi } from 'vitest';
import { act, cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Dashboard } from './Dashboard';

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

it('retains the draft after a rejected route save', async () => {
  vi.stubGlobal('CSS', { escape: (value: string) => value });
  vi.stubGlobal(
    'fetch',
    vi.fn(async (_input: string, init?: RequestInit) =>
      init?.method === 'PUT'
        ? new Response('route configuration changed', { status: 409 })
        : response({
            routes: [{ domain: 'app.example.com', upstream: 'http://server:3000', enabled: true }],
            revision: 1,
          }),
    ),
  );
  const user = userEvent.setup();
  render(<Dashboard />);
  await user.click(await screen.findByRole('checkbox', { name: 'Enable app.example.com' }));
  await user.click(screen.getByRole('button', { name: 'Save changes' }));
  await user.click(screen.getByRole('button', { name: 'Confirm' }));
  expect(await screen.findByRole('alert')).toHaveProperty(
    'textContent',
    'route configuration changed',
  );
  await user.click(screen.getByRole('button', { name: 'Cancel' }));
  const checkbox = screen.getByRole('checkbox', {
    name: 'Enable app.example.com',
  }) as HTMLInputElement;
  expect(checkbox.checked).toBe(false);
  expect(checkbox.disabled).toBe(false);
  expect(screen.getByText('Unsaved changes')).toBeTruthy();
});

it('locks route controls while a manual refresh is pending', async () => {
  vi.stubGlobal('CSS', { escape: (value: string) => value });
  const current = {
    routes: [{ domain: 'app.example.com', upstream: 'http://server:3000', enabled: true }],
    revision: 1,
  };
  const fetcher = vi.fn(async () => response(current));
  vi.stubGlobal('fetch', fetcher);
  render(<Dashboard />);
  const checkbox = (await screen.findByRole('checkbox', {
    name: 'Enable app.example.com',
  })) as HTMLInputElement;
  let finish!: (value: Response) => void;
  fetcher.mockImplementationOnce(
    () =>
      new Promise<Response>((resolve) => {
        finish = resolve;
      }),
  );
  await userEvent.setup().click(screen.getByRole('button', { name: 'Refresh' }));
  expect(checkbox.disabled).toBe(true);
  expect((screen.getByRole('button', { name: 'Add service' }) as HTMLButtonElement).disabled).toBe(
    true,
  );
  await act(async () => {
    finish(response(current));
  });
  expect(checkbox.disabled).toBe(false);
});

it('distinguishes a saved configuration from a failed follow-up refresh', async () => {
  vi.stubGlobal('CSS', { escape: (value: string) => value });
  let persisted = false;
  vi.stubGlobal(
    'fetch',
    vi.fn(async (_input: string, init?: RequestInit) => {
      if (init?.method === 'PUT') {
        persisted = true;
        return response(null, 204);
      }
      if (persisted) throw new Error('offline');
      return response({
        routes: [{ domain: 'app.example.com', upstream: 'http://server:3000', enabled: true }],
        revision: 1,
      });
    }),
  );
  const user = userEvent.setup();
  render(<Dashboard />);
  await user.click(await screen.findByRole('checkbox', { name: 'Enable app.example.com' }));
  await user.click(screen.getByRole('button', { name: 'Save changes' }));
  await user.click(screen.getByRole('button', { name: 'Confirm' }));
  expect((await screen.findByRole('alert')).textContent).toContain(
    'Changes were saved, but status could not be refreshed',
  );
});
const response = (value: unknown, status = 200) =>
  new Response(status === 204 ? null : JSON.stringify(value), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });

it('locks route editing and ignores stale polling responses during a delayed save', async () => {
  vi.stubGlobal('CSS', { escape: (value: string) => value });
  let finishSave!: () => void;
  let statusCount = 0;
  let finishStalePoll!: () => void;
  let revision = 1;
  const fetchMock = vi.fn((input: string, init?: RequestInit) => {
    if (init?.method === 'PUT')
      return new Promise<Response>((resolve) => {
        finishSave = () => resolve(response(null, 204));
      });
    statusCount++;
    if (statusCount === 2) {
      return new Promise<Response>((resolve) => {
        finishStalePoll = () =>
          resolve(
            response({
              routes: [{ domain: 'stale.example.com', upstream: 'http://old:3000', enabled: true }],
              revision: 0,
            }),
          );
      });
    }
    const routes = [{ domain: 'app.example.com', upstream: 'http://server:3000', enabled: true }];
    return Promise.resolve(response({ routes, revision }));
  });
  vi.stubGlobal('fetch', fetchMock);
  const user = userEvent.setup();
  render(<Dashboard />);
  await screen.findByRole('checkbox', { name: 'Enable app.example.com' });
  await user.click(screen.getByRole('checkbox', { name: 'Enable app.example.com' }));
  await waitFor(() => expect(statusCount).toBe(2));
  await user.click(screen.getByRole('button', { name: 'Save changes' }));
  await user.click(screen.getByRole('button', { name: 'Confirm' }));
  await waitFor(() =>
    expect(fetchMock.mock.calls.some(([, init]) => init?.method === 'PUT')).toBe(true),
  );
  const checkbox = document.querySelector<HTMLInputElement>(
    'input[aria-label="Enable app.example.com"]',
  );
  const editButton = document.querySelector<HTMLButtonElement>(
    'button[aria-label="Edit app.example.com"]',
  );
  expect(checkbox?.disabled).toBe(true);
  expect(editButton?.disabled).toBe(true);
  revision = 2;
  await act(async () => {
    finishSave();
  });
  await waitFor(() => expect(screen.getByText('Changes saved.')).toBeTruthy());
  await act(async () => {
    finishStalePoll();
  });
  await waitFor(() =>
    expect(
      fetchMock.mock.calls.filter(([input]) => input === '/api/status').length,
    ).toBeGreaterThan(2),
  );
  await user.click(screen.getByRole('checkbox', { name: 'Enable app.example.com' }));
  await user.click(screen.getByRole('button', { name: 'Save changes' }));
  await user.click(screen.getByRole('button', { name: 'Confirm' }));
  await waitFor(() =>
    expect(fetchMock.mock.calls.filter(([, init]) => init?.method === 'PUT')).toHaveLength(2),
  );
  expect(
    fetchMock.mock.calls.filter(([, init]) => init?.method === 'PUT')[1][1]?.headers,
  ).toMatchObject({ 'If-Match': '2' });
  await act(async () => {
    finishSave();
  });
  expect(document.body.textContent).toContain('app.example.com');
  expect(screen.queryByText('stale.example.com')).toBeNull();
});
