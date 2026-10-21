import { useEffect, useState } from 'react';
import { Button, Form, Link } from 'react-aria-components';
import { api, type SetupStatus } from './api';
import { Check, Confirm, Field, Notice, Status } from './components';
import { LoggedOut } from './Dialogs';

export function Settings() {
  useEffect(() => {
    document.title = 'Settings · ts-egress';
  }, []);
  const [data, setData] = useState<SetupStatus>();
  const [admins, setAdmins] = useState('');
  const [geo, setGeo] = useState<boolean>();
  const [blocks, setBlocks] = useState<string[]>([]);
  const [error, setError] = useState('');
  const [message, setMessage] = useState('');
  const [busy, setBusy] = useState(false);
  const [loggedOut, setLoggedOut] = useState(false);
  useEffect(() => {
    api<SetupStatus>('/api/setup/status')
      .then((value) => {
        setData(value);
        setAdmins((value.settings.admins ?? []).join(','));
      })
      .catch((e) => setError(e.message));
    api<{ enabled: boolean }>('/api/geolocation')
      .then((v) => setGeo(v.enabled))
      .catch((e) => setError(e.message));
    api<{ ips: string[] }>('/api/blocks')
      .then((v) => setBlocks(v.ips))
      .catch((e) => setError(e.message));
  }, []);
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
      {!data ? (
        <p role="status">Loading settings…</p>
      ) : (
        <>
          <section className="panel padded">
            <h2>Gateway</h2>
            <Form
              onSubmit={async (event) => {
                event.preventDefault();
                setBusy(true);
                setError('');
                try {
                  await api(
                    '/api/setup/settings',
                    'PUT',
                    {
                      ...data.settings,
                      admins: admins
                        .split(',')
                        .map((v) => v.trim())
                        .filter(Boolean),
                    },
                    {
                      'If-Match': String(data.settingsRevision),
                    },
                  );
                  const next = await api<SetupStatus>('/api/setup/status');
                  setData(next);
                  setAdmins((next.settings.admins ?? []).join(','));
                  setMessage('Settings saved.');
                } catch (e) {
                  setError((e as Error).message);
                } finally {
                  setBusy(false);
                }
              }}
            >
              <Field label="Tailnet device name" value={data.settings.hostname} isDisabled />
              <Field
                label="Tailscale administrators"
                description="Comma-separated login names. Empty allows any untagged user on your tailnet."
                value={admins}
                onChange={setAdmins}
              />
              <Field
                label="Public certificate contact email"
                type="email"
                isRequired
                value={data.settings.email}
                onChange={(email) => setData({ ...data, settings: { ...data.settings, email } })}
              />
              <Check
                isSelected={data.settings.acceptTerms}
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
                <Button
                  className="secondary"
                  isDisabled={busy}
                  onPress={async () => {
                    setBusy(true);
                    setError('');
                    try {
                      const next = await api<SetupStatus>('/api/setup/status');
                      setData(next);
                      setAdmins((next.settings.admins ?? []).join(','));
                    } catch (e) {
                      setError((e as Error).message);
                    } finally {
                      setBusy(false);
                    }
                  }}
                >
                  Reload settings
                </Button>
                <Button type="submit" isDisabled={busy}>
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
                setBusy(true);
                setError('');
                try {
                  await api('/api/geolocation', 'PUT', { enabled });
                  setGeo(enabled);
                  setMessage('Country lookup preference saved.');
                } catch (e) {
                  setError((e as Error).message);
                } finally {
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
            {blocks.length ? (
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
                            setBusy(true);
                            setError('');
                            try {
                              await api('/api/blocks', 'PUT', { ip, blocked: false });
                              setBlocks((prev) => prev.filter((v) => v !== ip));
                            } catch (e) {
                              setError((e as Error).message);
                            } finally {
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
              <p>No blocked IPs.</p>
            )}
          </section>
          <section className="panel padded">
            <h2>Tailscale</h2>
            <Status tone={data.tailnet.state === 'connected' ? 'ok' : 'warn'}>
              {data.tailnet.state === 'connected' ? 'Connected' : 'Connection needs attention'}
            </Status>
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
    </>
  );
}
