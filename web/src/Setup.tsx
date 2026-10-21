import { useEffect, useRef, useState } from 'react';
import { Button, Form, Link } from 'react-aria-components';
import { api, safeHTTPS, type Route, type Settings, type SetupStatus } from './api';
import { Check, Confirm, Field, Notice, Status } from './components';
import { LoggedOut } from './Dialogs';

type Session = { claimed: boolean; authenticated: boolean; tailnet?: boolean };
const initial: Settings = {
  hostname: 'ts-egress',
  admins: [],
  email: '',
  acceptTerms: false,
  connect: false,
};
export function Setup() {
  const [session, setSession] = useState<Session>();
  useEffect(() => {
    document.title = `${!session ? 'Gateway setup' : !session.authenticated ? (session.claimed ? 'Sign in' : 'Create administrator') : location.pathname === '/password' ? 'Change password' : 'Gateway setup'} · ts-egress`;
  }, [session?.authenticated, session?.claimed]);
  const [status, setStatus] = useState<SetupStatus>();
  const [draft, setDraft] = useState<Settings>(initial);
  const [password, setPassword] = useState('');
  const [confirmPassword, setConfirmPassword] = useState('');
  const [message, setMessage] = useState('');
  const [busy, setBusy] = useState(false);
  const [waitingForLogin, setWaitingForLogin] = useState(false);
  const [requestedLoginURL, setRequestedLoginURL] = useState<string>();
  const [device, setDevice] = useState('');
  const [domain, setDomain] = useState('');
  const [port, setPort] = useState('3000');
  const [protocol, setProtocol] = useState('http');
  const [step, setStep] = useState(1);
  const [refreshing, setRefreshing] = useState(false);
  const [loggedOut, setLoggedOut] = useState(false);
  const [preconfigured, setPreconfigured] = useState(false);
  const [settingsRevision, setSettingsRevision] = useState(0);
  const [geolocationEnabled, setGeolocationEnabled] = useState(false);
  const heading = useRef<HTMLHeadingElement>(null);
  const initialized = useRef(false);
  useEffect(() => {
    heading.current?.focus();
    setMessage('');
  }, [step]);
  function acceptStatus(data: SetupStatus) {
    data.tailnet.peers = [...data.tailnet.peers]
      .map((p) => ({ ...p, ips: [...p.ips].sort() }))
      .sort((a, b) => (a.name || a.ips[0]).localeCompare(b.name || b.ips[0]));
    setStatus((previous) => (JSON.stringify(previous) === JSON.stringify(data) ? previous : data));
  }
  async function refreshDevices() {
    setRefreshing(true);
    try {
      acceptStatus(await api<SetupStatus>('/api/setup/status'));
    } catch (e) {
      setMessage((e as Error).message);
    } finally {
      setRefreshing(false);
    }
  }
  useEffect(() => {
    api<Session>('/api/setup/session')
      .then(setSession)
      .catch((e) => setMessage(e.message));
  }, []);
  useEffect(() => {
    if (!session?.tailnet || !session.authenticated) return;
    api<{ enabled: boolean }>('/api/geolocation')
      .then((value) => setGeolocationEnabled(value.enabled))
      .catch(() => setGeolocationEnabled(false));
  }, [session?.tailnet, session?.authenticated]);
  useEffect(() => {
    if (!session?.authenticated || loggedOut) return;
    let active = true,
      timer: ReturnType<typeof setTimeout>;
    let first = true;
    async function poll() {
      try {
        const data = await api<SetupStatus>('/api/setup/status');
        if (!active) return;
        acceptStatus(data);
        if (first) {
          if (!initialized.current) {
            setDraft({ ...data.settings, admins: data.settings.admins ?? [] });
            setSettingsRevision(data.settingsRevision);
            initialized.current = true;
            const forceConnect = new URLSearchParams(location.search).get('step') === '2';
            if (
              session?.tailnet &&
              data.settings.connect &&
              !forceConnect &&
              data.tailnet.state === 'connected' &&
              data.tailnet.magicDNS &&
              data.tailnet.https &&
              !data.tailnet.authURL &&
              data.tailnet.adminURL
            ) {
              const sites = await api<{ routes: Route[]; revision: number }>('/api/status');
              if (active) {
                setPreconfigured(sites.routes.length > 0);
                setStep(data.settings.connect ? (sites.routes.length ? 4 : 2) : 1);
              }
            } else setStep(data.settings.connect ? 2 : 1);
          }
          first = false;
        }
      } catch (e) {
        if (active) setMessage((e as Error).message);
      }
      if (active && (step === 2 || (status?.settings.tailnetOnly && !session?.tailnet)))
        timer = setTimeout(poll, 3000);
    }
    void poll();
    return () => {
      active = false;
      clearTimeout(timer);
    };
  }, [session?.authenticated, session?.tailnet, step, status?.settings.tailnetOnly, loggedOut]);
  async function save(connect: boolean, advance = false) {
    setBusy(true);
    try {
      const settings = {
        ...draft,
        admins: draft.admins.map((a) => a.trim()).filter(Boolean),
        connect,
      };
      if (!settings.email || !settings.acceptTerms)
        throw new Error(
          'Add a certificate contact email and accept the certificate terms to continue.',
        );
      await api('/api/setup/settings', 'PUT', settings, { 'If-Match': String(settingsRevision) });
      const data = await api<SetupStatus>('/api/setup/status');
      setDraft({ ...data.settings, admins: data.settings.admins ?? [] });
      setSettingsRevision(data.settingsRevision);
      acceptStatus(data);
      if (advance && !preconfigured) setStep(2);
      else setMessage('Follow the authorization link when it appears.');
    } catch (e) {
      setMessage((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  async function saveGeolocation(enabled: boolean) {
    try {
      await api('/api/geolocation', 'PUT', { enabled });
      setGeolocationEnabled(enabled);
      setMessage(enabled ? 'Country lookups enabled.' : 'Country lookups disabled.');
    } catch (error) {
      setMessage((error as Error).message);
    }
  }
  const tailnet = status?.tailnet;
  const loginURL = safeHTTPS(tailnet?.authURL) ?? requestedLoginURL,
    adminURL = safeHTTPS(tailnet?.adminURL);
  const peers = tailnet?.peers ?? [];
  const chosenDevice = device;
  const selectedPeer = peers.find((p) => (p.name || p.ips[0]) === device);
  const ready =
    tailnet?.state === 'connected' &&
    !tailnet.authURL &&
    tailnet.magicDNS &&
    tailnet.https &&
    !!adminURL;
  const handoff = !!status?.settings.tailnetOnly && !session?.tailnet;
  useEffect(() => {
    if (loginURL || tailnet?.state === 'connected' || tailnet?.state === 'error')
      setWaitingForLogin(false);
    if (tailnet?.state === 'connected') setRequestedLoginURL(undefined);
  }, [loginURL, tailnet?.state]);
  useEffect(() => {
    if (!waitingForLogin) return;
    const timer = setTimeout(() => {
      setWaitingForLogin(false);
      setMessage('The sign-in link is taking longer than expected. You can try again.');
    }, 30000);
    return () => clearTimeout(timer);
  }, [waitingForLogin]);
  const connectionLabel =
    (
      {
        'not-connected': 'Not connected',
        connecting: 'Connecting…',
        'needs-login': 'Signed out of Tailscale',
        connected: 'Connected',
        error: 'Connection error',
        unknown: 'Connection status unavailable',
      } as Record<string, string>
    )[tailnet?.state ?? ''] ?? 'Checking Tailscale…';
  const connectionTone = (
    ready
      ? 'ok'
      : ((
          {
            connected: 'ok',
            connecting: 'warn',
            'needs-login': 'warn',
            error: 'error',
          } as Record<string, string>
        )[tailnet?.state ?? ''] ?? 'idle')
  ) as 'ok' | 'warn' | 'error' | 'idle';
  // Publishing is hidden once any domain is already configured.
  const stepDefs: [number, string][] = preconfigured
    ? [
        [1, 'Configure'],
        [2, 'Connect'],
        [4, 'Ready'],
      ]
    : [
        [1, 'Configure'],
        [2, 'Connect'],
        [3, 'Publish'],
        [4, 'Ready'],
      ];
  // Request the link first. Never open a blank tab while Tailscale prepares it.
  async function signIn() {
    if (waitingForLogin || busy) return;
    setWaitingForLogin(true);
    setBusy(true);
    try {
      const result = await api<{ authURL?: string }>('/api/setup/reauthenticate', 'POST', {});
      setRequestedLoginURL(safeHTTPS(result.authURL));
      await refreshDevices();
    } catch (e) {
      setWaitingForLogin(false);
      setMessage((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  async function reconnect() {
    setBusy(true);
    try {
      await api('/api/setup/connect', 'POST', {});
      await refreshDevices();
    } catch (error) {
      setMessage((error as Error).message);
    } finally {
      setBusy(false);
    }
  }
  // Once the private dashboard is ready, the sealed local console has nothing
  // left to manage, so hand the browser over to the tailnet address.
  useEffect(() => {
    if (handoff && ready && adminURL && location.pathname !== '/password')
      window.location.assign(adminURL);
  }, [handoff, ready, adminURL]);
  async function publish() {
    try {
      if (!status?.settings.acceptTerms)
        throw new Error('Save the certificate contact and accept its terms first.');
      const data = await api<{ routes: Route[]; revision: number }>('/api/status');
      const normalized = domain.trim().toLowerCase();
      if (data.routes.some((r) => r.domain === normalized))
        throw new Error('This domain already has a route. Edit it in the route manager.');
      if (
        !selectedPeer ||
        !normalized ||
        !/^\d+$/.test(port) ||
        Number(port) < 1 ||
        Number(port) > 65535
      )
        throw new Error('Choose an available device, public domain and valid service port.');
      const host = chosenDevice.includes(':') ? `[${chosenDevice}]` : chosenDevice;
      await api(
        '/api/routes',
        'PUT',
        [
          ...data.routes,
          {
            domain: normalized,
            upstream: `${protocol}://${host}:${port}`,
            enabled: true,
          },
        ],
        { 'If-Match': String(data.revision) },
      );
      setStep(4);
      setMessage('Service saved.');
    } catch (e) {
      setMessage((e as Error).message);
    }
  }
  return (
    <>
      <header>
        <Link className="brand" href={session?.tailnet ? '/dashboard' : '/'}>
          ts-egress
        </Link>
        {session?.tailnet && (
          <Button className="secondary" onPress={() => window.location.assign('/dashboard')}>
            Services
          </Button>
        )}
      </header>
      <div className="page-heading">
        <h1>
          {session?.authenticated
            ? 'Gateway setup'
            : session?.claimed
              ? 'Sign in'
              : 'Create administrator'}
        </h1>
      </div>
      {!session ? (
        <p role="status">Loading local console…</p>
      ) : !session.authenticated ? (
        <section className="panel padded">
          <Form
            onSubmit={async (e) => {
              e.preventDefault();
              if (!session.claimed && password !== confirmPassword) {
                setMessage('Passwords do not match.');
                return;
              }
              setBusy(true);
              try {
                await api(session.claimed ? '/api/setup/login' : '/api/setup/claim', 'POST', {
                  password,
                });
                setPassword('');
                setConfirmPassword('');
                setSession(await api<Session>('/api/setup/session'));
                setMessage('Signed in to the local console.');
              } catch (e) {
                setMessage((e as Error).message);
              } finally {
                setBusy(false);
              }
            }}
          >
            <Field
              label="Administrator password"
              type="password"
              minLength={12}
              maxLength={72}
              autoComplete={session.claimed ? 'current-password' : 'new-password'}
              isRequired
              value={password}
              onChange={setPassword}
            />
            {!session.claimed && (
              <Field
                label="Confirm administrator password"
                type="password"
                minLength={12}
                maxLength={72}
                autoComplete="new-password"
                isRequired
                value={confirmPassword}
                onChange={setConfirmPassword}
              />
            )}
            <div className="step-actions">
              <span />
              <Button type="submit" isDisabled={busy}>
                {busy ? 'Saving…' : session.claimed ? 'Sign in →' : 'Create administrator →'}
              </Button>
            </div>
          </Form>
        </section>
      ) : !status ? (
        <p role="status">Loading gateway status…</p>
      ) : !session.tailnet && location.pathname === '/password' ? (
        <LocalPassword
          onChanged={() => {
            initialized.current = false;
            setStatus(undefined);
            setSession({ ...session, authenticated: false });
            setMessage('Password changed. Sign in again.');
          }}
        />
      ) : handoff ? (
        <section className="panel padded">
          <h2 ref={heading} tabIndex={-1}>
            2. Connect to Tailscale
          </h2>
          <Status tone={connectionTone}>{connectionLabel}</Status>
          {ready ? (
            <p role="status">
              Opening the private dashboard at <a href={adminURL}>{adminURL}</a>…
            </p>
          ) : (
            <>
              <p>
                Service management runs on the gateway's private tailnet dashboard, not this
                console.
              </p>
              <div className="actions">
                {['error', 'not-connected', 'stopped'].includes(tailnet?.state ?? '') && (
                  <Button isDisabled={busy} onPress={reconnect}>
                    Retry connection
                  </Button>
                )}
                {loginURL ? (
                  <Link className="primary-link" href={loginURL} target="_blank" rel="noreferrer">
                    Authorize on Tailscale ↗
                  </Link>
                ) : waitingForLogin || tailnet?.state === 'connecting' ? (
                  <span role="status">Preparing Tailscale sign-in link. Please wait…</span>
                ) : tailnet?.state === 'needs-login' ? (
                  <Button isDisabled={busy} onPress={signIn}>
                    Sign in to Tailscale ↗
                  </Button>
                ) : null}
                <Button
                  className="secondary"
                  isDisabled={tailnet?.state !== 'connected' || !adminURL}
                  onPress={() => {
                    if (adminURL) window.location.assign(adminURL);
                  }}
                >
                  Open private dashboard
                </Button>
              </div>
            </>
          )}
          <Notice message={tailnet?.error ?? ''} />
          <Link href="/password">Change administrator password</Link>
        </section>
      ) : (
        <>
          <div className="wizard-toolbar">
            <ol className="stepper" aria-label="Setup progress">
              {stepDefs.map(([n, label], index) => (
                <li
                  key={label}
                  aria-current={step === n ? 'step' : undefined}
                  className={step > n ? 'complete' : ''}
                >
                  <span>{step > n ? '✓' : index + 1}</span>
                  {label}
                </li>
              ))}
            </ol>
            {!session.tailnet && (
              <Button
                className="secondary"
                onPress={async () => {
                  try {
                    await api('/api/setup/logout', 'POST', {});
                    initialized.current = false;
                    setSession({ claimed: true, authenticated: false });
                    setStatus(undefined);
                    setStep(1);
                    setMessage('Signed out.');
                  } catch (e) {
                    setMessage((e as Error).message);
                  }
                }}
              >
                Sign out
              </Button>
            )}
          </div>
          {step === 1 && (
            <section className="panel padded">
              <h2 ref={heading} tabIndex={-1}>
                1. Configure this gateway
              </h2>
              <Form
                onSubmit={(e) => {
                  e.preventDefault();
                  void save(status?.settings.connect ?? false, true);
                }}
              >
                <Field
                  label="Tailnet device name"
                  value={draft.hostname}
                  onChange={(hostname) => setDraft({ ...draft, hostname })}
                  isDisabled={status?.settings.connect}
                  isRequired
                  pattern="[a-z0-9-]{1,63}"
                />
                <Field
                  label="Tailscale administrators"
                  description="Optional. Comma-separated Tailscale login names. Leave empty to allow anyone on your tailnet."
                  value={draft.admins.join(',')}
                  onChange={(value) => setDraft({ ...draft, admins: value.split(',') })}
                />
                <Field
                  label="Public certificate contact email"
                  isRequired
                  type="email"
                  value={draft.email}
                  onChange={(email) => setDraft({ ...draft, email })}
                />
                <Check
                  isSelected={draft.acceptTerms}
                  onChange={(acceptTerms) => setDraft({ ...draft, acceptTerms })}
                >
                  I accept the Let's Encrypt subscriber agreement.
                </Check>
                <Link href="https://letsencrypt.org/repository/" target="_blank" rel="noreferrer">
                  Read the subscriber agreement ↗
                </Link>
                <div className="step-actions">
                  <span />
                  <Button type="submit" isDisabled={busy || !status || !draft.acceptTerms}>
                    {busy ? 'Saving…' : preconfigured ? 'Save settings' : 'Save & continue →'}
                  </Button>
                </div>
              </Form>
              {session.tailnet && status.settings.connect && (
                <section className="settings-extra">
                  <Check isSelected={geolocationEnabled} onChange={saveGeolocation}>
                    Send selected public visitor IPs to ipwho.is for country lookup
                  </Check>
                </section>
              )}
              {!session.tailnet && <Link href="/password">Change administrator password</Link>}
            </section>
          )}
          {step === 2 && (
            <section className="panel padded">
              <h2 ref={heading} tabIndex={-1}>
                2. Connect to Tailscale
              </h2>
              <Status tone={connectionTone}>{connectionLabel}</Status>
              {!ready && (
                <ul>
                  {[
                    ['MagicDNS enabled', tailnet?.magicDNS],
                    ['Tailnet HTTPS enabled', tailnet?.https],
                    ['Public certificate terms accepted', status?.settings.acceptTerms],
                  ]
                    .filter(([, ok]) => !ok)
                    .map(([label]) => (
                      <li key={String(label)}>{label}</li>
                    ))}
                </ul>
              )}
              {tailnet?.error && (
                <p className="error" role="alert">
                  {tailnet.error}
                </p>
              )}
              {(!tailnet?.magicDNS || !tailnet?.https) && (
                <p>
                  Enable MagicDNS and HTTPS in{' '}
                  <Link
                    href="https://login.tailscale.com/admin/dns"
                    target="_blank"
                    rel="noreferrer"
                  >
                    Tailscale DNS settings
                  </Link>
                  .
                </p>
              )}
              <div className="step-actions">
                {preconfigured ? (
                  <Button className="secondary" onPress={() => setStep(1)}>
                    Gateway settings
                  </Button>
                ) : (
                  <Button className="secondary" onPress={() => setStep(1)}>
                    ← Back
                  </Button>
                )}
                <div className="actions">
                  {!status?.settings.connect ? (
                    <Button isDisabled={busy || !status} onPress={() => save(true)}>
                      Connect to Tailscale →
                    </Button>
                  ) : !session.tailnet &&
                    ['error', 'not-connected', 'stopped'].includes(tailnet?.state ?? '') ? (
                    <Button isDisabled={busy} onPress={reconnect}>
                      Retry connection
                    </Button>
                  ) : loginURL ? (
                    <Link className="primary-link" href={loginURL} target="_blank" rel="noreferrer">
                      Authorize on Tailscale ↗
                    </Link>
                  ) : waitingForLogin || tailnet?.state === 'connecting' ? (
                    <span role="status">Preparing Tailscale sign-in link. Please wait…</span>
                  ) : tailnet?.state === 'needs-login' ? (
                    <Button isDisabled={busy} onPress={signIn}>
                      Sign in to Tailscale ↗
                    </Button>
                  ) : !ready ? (
                    <span>Waiting for tailnet readiness…</span>
                  ) : null}
                  {session.tailnet &&
                    status?.settings.connect &&
                    tailnet?.state === 'connected' && (
                      <Confirm
                        destructive
                        label="Log out of Tailscale"
                        title="Log out of Tailscale?"
                        isDisabled={busy}
                        onConfirm={async () => {
                          setBusy(true);
                          try {
                            await api('/api/setup/tailscale/logout', 'POST', {});
                            setLoggedOut(true);
                          } catch (e) {
                            setMessage((e as Error).message);
                          } finally {
                            setBusy(false);
                          }
                        }}
                      >
                        This is a hard logout. This dashboard becomes unreachable over Tailscale —
                        including for you — and published services stop until you sign back in from
                        the gateway's local console.
                      </Confirm>
                    )}
                  <Button isDisabled={!ready} onPress={() => setStep(preconfigured ? 4 : 3)}>
                    Continue →
                  </Button>
                </div>
              </div>
            </section>
          )}
          {step === 3 && !preconfigured && (
            <section className="panel padded">
              <h2 ref={heading} tabIndex={-1}>
                3. Publish a service
              </h2>
              <p>Point the domain’s DNS records to this host and allow inbound TCP 80/443.</p>
              <Field
                label="Public domain"
                placeholder="app.example.com"
                value={domain}
                onChange={setDomain}
                isRequired
              />
              <label className="native-field">
                Tailnet device
                <select value={chosenDevice} onChange={(e) => setDevice(e.target.value)}>
                  <option value="" disabled>
                    Choose a device
                  </option>
                  {device && !selectedPeer && (
                    <option value={device} disabled>
                      {device} (no longer visible)
                    </option>
                  )}
                  {peers.map((peer) => (
                    <option key={peer.name || peer.ips[0]} value={peer.name || peer.ips[0]}>
                      {peer.name || peer.ips[0]} · {peer.online ? 'online' : 'offline'}
                    </option>
                  ))}
                </select>
              </label>
              <div className="device-details">
                <span>
                  {selectedPeer
                    ? selectedPeer.ips.join(' · ')
                    : peers.length
                      ? ''
                      : 'No devices found.'}
                </span>
                <Button className="secondary" isDisabled={refreshing} onPress={refreshDevices}>
                  {refreshing ? 'Refreshing…' : 'Refresh devices'}
                </Button>
              </div>
              <Field
                label="Service port"
                type="number"
                min={1}
                max={65535}
                value={port}
                onChange={setPort}
                isRequired
              />
              <label className="native-field">
                Backend protocol
                <select value={protocol} onChange={(e) => setProtocol(e.target.value)}>
                  <option value="http">HTTP over WireGuard</option>
                  <option value="https">HTTPS (verified certificate)</option>
                </select>
              </label>
              <div className="step-actions">
                <Button className="secondary" onPress={() => setStep(2)}>
                  ← Back
                </Button>
                <div className="actions">
                  <Link href="/dashboard">Skip to route manager</Link>
                  <Confirm
                    label="Publish & finish →"
                    title="Make this service public?"
                    isDisabled={
                      !selectedPeer ||
                      !domain.trim() ||
                      !/^\d+$/.test(port) ||
                      Number(port) < 1 ||
                      Number(port) > 65535 ||
                      !status?.settings.acceptTerms
                    }
                    onConfirm={publish}
                  >
                    Anyone on the internet will be able to access {domain}. Protect sensitive
                    applications with their own authentication.
                  </Confirm>
                </div>
              </div>
            </section>
          )}
          {step === 4 && (
            <section className="panel padded">
              <h2 ref={heading} tabIndex={-1}>
                Service configured
              </h2>
              {domain ? (
                <p>
                  <strong>{domain}</strong> → {protocol}://{chosenDevice}:{port}
                </p>
              ) : (
                <p>A service is already configured; publishing is skipped.</p>
              )}
              {domain && (
                <p>
                  Certificate pending the first HTTPS connection. Public availability has not been
                  checked.
                </p>
              )}
              <div className="step-actions">
                {preconfigured && (
                  <Button className="secondary" onPress={() => setStep(1)}>
                    Gateway settings
                  </Button>
                )}
                {!preconfigured && (
                  <Button
                    className="secondary"
                    onPress={() => {
                      setDomain('');
                      setStep(3);
                    }}
                  >
                    ← Add another service
                  </Button>
                )}
                <Link className="primary-link" href="/dashboard">
                  Services →
                </Link>
              </div>
            </section>
          )}
        </>
      )}
      {loggedOut && <LoggedOut />}
      <Notice message={message} />
    </>
  );
}

function LocalPassword({ onChanged }: { onChanged: () => void }) {
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  return (
    <section className="panel padded">
      <h2>Change administrator password</h2>
      <Form
        onSubmit={async (event) => {
          event.preventDefault();
          const values = new FormData(event.currentTarget);
          setBusy(true);
          try {
            await api('/api/setup/password', 'POST', {
              current: values.get('current'),
              next: values.get('next'),
            });
            onChanged();
          } catch (e) {
            setError((e as Error).message);
          } finally {
            setBusy(false);
          }
        }}
      >
        <Field
          label="Current password"
          name="current"
          type="password"
          autoComplete="current-password"
          isRequired
        />
        <Field
          label="New password"
          name="next"
          type="password"
          minLength={12}
          maxLength={72}
          autoComplete="new-password"
          isRequired
        />
        <Button type="submit" isDisabled={busy}>
          Change password
        </Button>
      </Form>
      <Notice message={error} />
      <Link href="/">Back to console</Link>
    </section>
  );
}
