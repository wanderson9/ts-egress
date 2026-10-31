// @vitest-environment jsdom
import { afterEach, expect, it, vi } from 'vitest';
import { act, cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Confirm } from './components';

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

it('keeps a pending confirmation open when Escape is pressed', async () => {
  vi.stubGlobal('CSS', { escape: (value: string) => value });
  let finish!: () => void;
  const action = new Promise<void>((resolve) => {
    finish = resolve;
  });
  const user = userEvent.setup();
  render(
    <Confirm label="Apply" title="Apply changes?" onConfirm={() => action}>
      Changes affect public services.
    </Confirm>,
  );
  await user.click(screen.getByRole('button', { name: 'Apply' }));
  await user.click(screen.getByRole('button', { name: 'Confirm' }));
  await user.keyboard('{Escape}');
  expect(screen.getByRole('alertdialog', { name: 'Apply changes?' })).toBeTruthy();
  expect((screen.getByRole('button', { name: 'Cancel' }) as HTMLButtonElement).disabled).toBe(true);
  await act(async () => {
    finish();
  });
  expect(screen.queryByRole('alertdialog')).toBeNull();
});
