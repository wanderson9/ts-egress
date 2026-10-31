import { useEffect, useRef, useState } from 'react';
import { Button, Form, Link } from 'react-aria-components';
import { api, type SetupStatus } from './api';
import { Check, Confirm, Field, Notice, Status } from './components';
import { LoggedOut, Popup } from './Dialogs';

export function Settings() {
  useEffect(() => {
    document.title = 'Settings · ts-egress';
  }, []);
  const [data, setData] = useState<SetupStatus>();
  const [savedSettings, setSavedSettings] = useState<SetupStatus['settings']>();
  const [admins, setAdmins] = useState('');
  const [geo, setGeo] = useState<boolean>();
  const [blocks, setBlocks] = useState<string[]>();
  const [blocksError, setBlocksError] = useState('');
  const [connectionError, setConnectionError] = useState('');
  const [connectionChecked, setConnectionChecked] = useState<Date>();
  const [error, setError] = useState('');
  const [message, setMessage] = useState('');
  const [busy, setBusy] = useState(false);
  const busyRef = useRef(false);
  const [loggedOut, setLoggedOut] = useState(false);
  const [confirmLockout, setConfirmLockout] = useState(false);
  const [confirmOpenAccess, setConfirmOpenAccess] = useState(false);
  const [accessLost, setAccessLost] = useState(false);
  const nextAdmins = admins
    .split(',')
    .map((v) => v.trim())
    .filter(Boolean);
  const removesAccess =
    !!data?.identity && nextAdmins.length > 0 && !nextAdmins.includes(data.identity);
  const savedAdmins = data?.settings.admins ?? [];
  const changesToOpenAccess = savedAdmins.length > 0 && nextAdmins.length === 0;
  const normalizedAdmins = (values: string[]) =>
    values
      .map((value) => value.trim())
      .filter(Boolean)
      .join(',');
  const dirty =
    !!data &&
    !!savedSettings &&
    (normalizedAdmins(nextAdmins) !== normalizedAdmins(savedSettings.admins ?? []) ||
      data.settings.email !== savedSettings.email ||
      data.settings.acceptTerms !== savedSettings.acceptTerms);
  async function saveSettings(confirmed = false) {
    if (!data || busyRef.current) return;
    if ((removesAccess || changesToOpenAccess) && !confirmed) {
      if (removesAccess) setConfirmLockout(true);
      else setConfirmOpenAccess(true);
      return;
    }
    busyRef.current = true;
    setBusy(true);
    setError('');
    let persisted = false;
    try {
      await api(
        '/api/setup/settings',
        'PUT',
        { ...data.settings, admins: nextAdmins },
        {
          'If-Match': String(data.settingsRevision),
          ...(confirmed && changesToOpenAccess ? { 'X-Confirm-Admin-Change': '1' } : {}),
          ...(confirmed && removesAccess ? { 'X-Confirm-Admin-Lockout': '1' } : {}),
        },
      );
      persisted = true;
      setConfirmLockout(false);
      setConfirmOpenAccess(false);
      if (removesAccess) {
        setAccessLost(true);
        setSavedSettings({ ...data.settings, admins: nextAdmins });
        setData({ ...data, settings: { ...data.settings, admins: nextAdmins } });
        setAdmins(nextAdmins.join(','));
        setMessage('Settings saved. Your administrator access was removed.');
        return;
      }
      const next = await api<SetupStatus>('/api/setup/status');
      setData(next);
      setSavedSettings(next.settings);
      setAdmins((next.settings.admins ?? []).join(','));
      setMessage('Settings saved.');
    } catch (e) {
      setError(
        persisted
          ? `Settings were saved, but status could not be refreshed. Reload before saving again. ${(e as Error).message}`
          : (e as Error).message,
      );
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
  }
  useEffect(() => {
    api<{ enabled: boolean }>('/api/geolocation')
      .then((v) => setGeo(v.enabled))
      .catch((e) => setError(e.message));
  }, []);
  useEffect(() => {
    if (accessLost || loggedOut) return;
    let active = true;
    let timer: ReturnType<typeof setTimeout>;
    let initialized = false;
    async function poll() {
      await Promise.all([
        api<SetupStatus>('/api/setup/status')
          .then((value) => {
            if (!active) return;
            if (!initialized) {
              setData(value);
              setSavedSettings(value.settings);
              setAdmins((value.settings.admins ?? []).join(','));
              initialized = true;
            } else {
              // Refresh observations, never overwrite an administrator's draft or revision.
              setData((previous) => (previous ? { ...previous, tailnet: value.tailnet } : value));
            }
            setConnectionError('');
            setConnectionChecked(new Date());
          })
          .catch((e) => {
            if (active) setConnectionError(e.message);
          }),
        api<{ ips: string[] }>('/api/blocks')
          .then((value) => {
            if (active) {
              setBlocks(value.ips);
              setBlocksError('');
            }
          })
          .catch((e) => {
            if (active) setBlocksError(e.message);
          }),
      ]);
      if (active) timer = setTimeout(poll, 5000);
    }
    void poll();
    return () => {
      active = false;
      clearTimeout(timer);
    };
  }, [accessLost, loggedOut]);
  useEffect(() => {
    const listener = (event: BeforeUnloadEvent) => {
      if (dirty) {
        event.preventDefault();
        event.returnValue = '';
      }
    };
    window.addEventListener('beforeunload', listener);
    return () => window.removeEventListener('beforeunload', listener);
  }, [dirty]);
  return (
    <>
      <header>
        <Link className="brand" href="/dashboard">
          ts-egress
        </Link>
        <Link href="/dashboard">Services</Link>
      </header>
      <div className="page-heading">
        <h1>Settings</h1>
      </div>
      <Notice message={error || message} />
      {accessLost ? (
        <p>
          Use an allowed Tailscale identity to manage this gateway. If no allowed administrator can
          connect, use the host's administrator recovery command.
        </p>
      ) : !data ? (
        connectionError ? (
          <p role="alert">Settings unavailable: {connectionError}</p>
        ) : (
          <p role="status">Loading settings…</p>
        )
      ) : (
        <>
          <section className="panel padded">
            <h2>Gateway</h2>
            <Form
              onSubmit={(event) => {
                event.preventDefault();
                void saveSettings();
              }}
            >
              <Field label="Tailnet device name" value={data.settings.hostname} isDisabled />
              <Field
                label="Tailscale administrators"
                description="Comma-separated login names. Empty allows any untagged user on your tailnet."
                value={admins}
                isDisabled={busy}
                onChange={setAdmins}
              />
              <Field
                label="Initial ACME registration email"
                description="Used only for initial registration. Existing account contacts must be updated with the certificate authority."
                type="email"
                isRequired
                isDisabled={busy || data.settings.connect}
                value={data.settings.email}
                onChange={(email) => setData({ ...data, settings: { ...data.settings, email } })}
              />
              <Check
                isSelected={data.settings.acceptTerms}
                isDisabled={busy}
                onChange={(acceptTerms) =>
                  setData({ ...data, settings: { ...data.settings, acceptTerms } })
                }
              >
                I accept the Let's Encrypt subscriber agreement.
              </Check>
              <Link href="https://letsencrypt.org/repository/" target="_blank" rel="noreferrer">
                Read the subscriber agreement
              </Link>
              <div className="actions">
                {dirty ? (
                  <Confirm
                    label="Reload settings"
                    title="Discard unsaved changes?"
                    isDisabled={busy}
                    onConfirm={async () => {
                      if (busyRef.current) return;
                      busyRef.current = true;
                      setBusy(true);
                      try {
                        const next = await api<SetupStatus>('/api/setup/status');
                        setData(next);
                        setSavedSettings(next.settings);
                        setAdmins((next.settings.admins ?? []).join(','));
                      } catch (e) {
                        setError((e as Error).message);
                      } finally {
                        busyRef.current = false;
                        setBusy(false);
                      }
                    }}
                  >
                    Your unsaved edits will be discarded.
                  </Confirm>
                ) : (
                  <Button
                    className="secondary"
                    isDisabled={busy || dirty}
                    onPress={async () => {
                      if (busyRef.current) return;
                      busyRef.current = true;
                      setBusy(true);
                      setError('');
                      try {
                        const next = await api<SetupStatus>('/api/setup/status');
                        setData(next);
                        setSavedSettings(next.settings);
                        setAdmins((next.settings.admins ?? []).join(','));
                      } catch (e) {
                        setError((e as Error).message);
                      } finally {
                        busyRef.current = false;
                        setBusy(false);
                      }
                    }}
                  >
                    Reload settings
                  </Button>
                )}
                <Button type="submit" isDisabled={busy || !dirty}>
                  Save settings
                </Button>
              </div>
            </Form>
          </section>
          <section className="panel padded">
            <h2>Country lookups</h2>
            <Check
              isSelected={geo ?? false}
              isDisabled={geo === undefined || busy}
              onChange={async (enabled) => {
                if (busyRef.current) return;
                busyRef.current = true;
                setBusy(true);
                setError('');
                try {
                  await api('/api/geolocation', 'PUT', { enabled });
                  setGeo(enabled);
                  setMessage('Country lookup preference saved.');
                } catch (e) {
                  setError((e as Error).message);
                } finally {
                  busyRef.current = false;
                  setBusy(false);
                }
              }}
            >
              Send selected public client IPs to ipwho.is for country lookup
            </Check>
          </section>
          <section className="panel padded">
            <h2>Blocked IPs</h2>
            <p>Blocks apply to all public services. Existing streams are not disconnected.</p>
            {blocksError && (
              <p role="alert">
                Blocked IP status unavailable: {blocksError}.{' '}
                {blocks
                  ? 'Displayed list may be outdated.'
                  : 'The block list has not been verified.'}
              </p>
            )}
            {blocks === undefined ? (
              !blocksError && <p role="status">Loading blocked IPs…</p>
            ) : blocks.length ? (
              <table aria-label="Blocked IPs">
                <thead>
                  <tr>
                    <th>Client IP</th>
                    <th>Action</th>
                  </tr>
                </thead>
                <tbody>
                  {blocks.map((ip) => (
                    <tr key={ip}>
                      <td>
                        <code>{ip}</code>
                      </td>
                      <td>
                        <Button
                          className="secondary"
                          isDisabled={busy}
                          onPress={async () => {
                            if (busyRef.current) return;
                            busyRef.current = true;
                            setBusy(true);
                            setError('');
                            try {
                              await api('/api/blocks', 'PUT', { ip, blocked: false });
                              setBlocks((prev) => prev?.filter((v) => v !== ip));
                            } catch (e) {
                              setError((e as Error).message);
                            } finally {
                              busyRef.current = false;
                              setBusy(false);
                            }
                          }}
                        >
                          Unblock {ip}
                        </Button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            ) : (
              !blocksError && <p>No blocked IPs.</p>
            )}
          </section>
          <section className="panel padded">
            <h2>Tailscale</h2>
            <Status
              tone={connectionError ? 'warn' : data.tailnet.state === 'connected' ? 'ok' : 'warn'}
            >
              {connectionError
                ? 'Connection status unavailable'
                : data.tailnet.state === 'connected'
                  ? 'Connected'
                  : 'Connection needs attention'}
            </Status>
            {connectionError && (
              <p role="alert">{connectionError}. Previous status may be outdated.</p>
            )}
            {connectionChecked && <p>Last checked {connectionChecked.toLocaleTimeString()}</p>}
            {data.tailnet.error && <Notice message={data.tailnet.error} />}
            <div className="actions">
              <Confirm
                destructive
                label="Log out of Tailscale"
                title="Log out of Tailscale?"
                isDisabled={busy}
                onConfirm={async () => {
                  await api('/api/setup/tailscale/logout', 'POST', {});
                  setLoggedOut(true);
                }}
              >
                The private dashboard and public services will become unavailable until you sign
                back in from the gateway's local console.
              </Confirm>
            </div>
          </section>
        </>
      )}
      {loggedOut && <LoggedOut />}
      {confirmLockout && (
        <Popup
          title="Remove your administrator access?"
          onClose={() => {
            if (!busy) setConfirmLockout(false);
          }}
          blocking={busy}
        >
          <p>
            You will lose dashboard access immediately. Another allowed administrator must restore
            it, or you must run administrator recovery on the host.
          </p>
          {error && <p role="alert">{error}</p>}
          <div className="actions">
            <Button
              className="secondary"
              isDisabled={busy}
              onPress={() => setConfirmLockout(false)}
            >
              Cancel
            </Button>
            <Button
              className="danger"
              isDisabled={busy}
              onPress={() => {
                void saveSettings(true);
              }}
            >
              Save and remove my access
            </Button>
          </div>
        </Popup>
      )}
      {confirmOpenAccess && (
        <Popup
          title="Allow all untagged tailnet users?"
          onClose={() => {
            if (!busy) setConfirmOpenAccess(false);
          }}
          blocking={busy}
        >
          <p>
            An empty administrator list allows any untagged user on your tailnet to access the
            dashboard.
          </p>
          <div className="actions">
            <Button
              className="secondary"
              isDisabled={busy}
              onPress={() => setConfirmOpenAccess(false)}
            >
              Cancel
            </Button>
            <Button
              className="danger"
              isDisabled={busy}
              onPress={() => {
                void saveSettings(true);
              }}
            >
              Allow access and save
            </Button>
          </div>
        </Popup>
      )}
    </>
  );
}
