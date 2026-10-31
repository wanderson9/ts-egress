import { Fragment, useEffect, useRef, useState } from 'react';
import { Button, Link } from 'react-aria-components';
import { api, type Route, type Totals, type ServiceHealth } from './api';
import { bytes, ClientDetails } from './Activity';
import { Check, Confirm, Notice } from './components';
import { CertificateDialog, Reauthenticate, ServiceEditor } from './Dialogs';

type Status = {
  routes: Route[];
  revision: number;
  sites?: Record<string, Totals>;
  health?: Record<string, ServiceHealth>;
  connection?: { needsLogin: boolean };
};
export function Dashboard() {
  useEffect(() => {
    document.title = 'Services · ts-egress';
  }, []);
  const [routes, setRoutes] = useState<Route[]>([]),
    [sites, setSites] = useState<Record<string, Totals>>({}),
    [health, setHealth] = useState<Record<string, ServiceHealth>>({});
  const [dirty, setDirty] = useState(false),
    [loaded, setLoaded] = useState(false),
    [message, setMessage] = useState(''),
    [statusError, setStatusError] = useState('');
  const [revision, setRevision] = useState(0);
  const originalRoutes = useRef<Route[]>([]);
  const [saving, setSaving] = useState(false);
  const [loading, setLoading] = useState(false);
  const savingRef = useRef(false);
  const loadingRef = useRef(false);
  const requestEpoch = useRef(0);
  const [domainFilter, setDomainFilter] = useState('');
  const visibleRoutes = routes.filter((route) =>
    route.domain.includes(domainFilter.trim().toLowerCase()),
  );
  const [expanded, setExpanded] = useState<string[]>([]),
    [editor, setEditor] = useState<{ route?: Route }>(),
    [certificateDomain, setCertificateDomain] = useState<string>(),
    [needsLogin, setNeedsLogin] = useState(false);
  function accept(data: Status) {
    setSites(data.sites ?? {});
    setHealth(data.health ?? {});
    setNeedsLogin(data.connection?.needsLogin ?? false);
    setStatusError('');
  }
  async function refreshStatus() {
    const epoch = requestEpoch.current;
    const data = await api<Status>('/api/status');
    if (epoch === requestEpoch.current) accept(data);
  }
  async function load() {
    if (savingRef.current || loadingRef.current) return;
    loadingRef.current = true;
    setLoading(true);
    const epoch = ++requestEpoch.current;
    try {
      const data = await api<Status>('/api/status');
      if (epoch !== requestEpoch.current || savingRef.current) return;
      accept(data);
      setRevision(data.revision);
      setRoutes(data.routes);
      originalRoutes.current = data.routes;
      setDirty(false);
      setLoaded(true);
    } finally {
      loadingRef.current = false;
      setLoading(false);
    }
  }
  async function confirmLoad() {
    await load();
  }
  useEffect(() => {
    let active = true,
      timer: ReturnType<typeof setTimeout>;
    async function poll() {
      const epoch = requestEpoch.current;
      try {
        const data = await api<Status>('/api/status');
        if (active && epoch === requestEpoch.current) {
          accept(data);
          setLoaded(true);
          if (!dirty && !savingRef.current && !loadingRef.current) {
            setRoutes(data.routes);
            originalRoutes.current = data.routes;
            setRevision(data.revision);
          }
        }
      } catch (e) {
        if (active && epoch === requestEpoch.current) setStatusError((e as Error).message);
      }
      if (active) timer = setTimeout(poll, 5000);
    }
    void poll();
    return () => {
      active = false;
      clearTimeout(timer);
    };
  }, [dirty]);
  useEffect(() => {
    const listener = (e: BeforeUnloadEvent) => {
      if (dirty) {
        e.preventDefault();
        e.returnValue = '';
      }
    };
    window.addEventListener('beforeunload', listener);
    return () => window.removeEventListener('beforeunload', listener);
  }, [dirty]);
  async function save() {
    if (savingRef.current || loadingRef.current) return;
    savingRef.current = true;
    requestEpoch.current++;
    setSaving(true);
    let persisted = false;
    try {
      await api('/api/routes', 'PUT', routes, {
        'If-Match': String(revision),
        ...(originalRoutes.current.some(
          (old) =>
            old.basicAuth && !routes.some((next) => next.domain === old.domain && next.basicAuth),
        )
          ? { 'X-Confirm-Auth-Removal': '1' }
          : {}),
      });
      persisted = true;
      const data = await api<Status>('/api/status');
      accept(data);
      setRevision(data.revision);
      setRoutes(data.routes);
      originalRoutes.current = data.routes;
      setDirty(false);
      setMessage('Changes saved.');
    } catch (e) {
      if (persisted) {
        throw new Error(
          `Changes were saved, but status could not be refreshed. Reload before saving again. ${(e as Error).message}`,
        );
      }
      throw e;
    } finally {
      requestEpoch.current++;
      savingRef.current = false;
      setSaving(false);
    }
  }
  return (
    <>
      <header>
        <Link className="brand" href="/">
          ts-egress
        </Link>
        <Button className="secondary" onPress={() => window.location.assign('/settings')}>
          Settings
        </Button>
      </header>
      <div className="heading page-heading">
        <h1>Services</h1>
        <div className="actions">
          {dirty && <span>Unsaved changes</span>}
          {dirty ? (
            <Confirm
              label="Reload"
              title="Discard unsaved changes?"
              isDisabled={saving || loading}
              onConfirm={confirmLoad}
            >
              Your unsaved edits will be discarded.
            </Confirm>
          ) : (
            <Button
              className="secondary"
              isDisabled={saving || loading}
              onPress={() => {
                void load().catch((e) => setStatusError(e.message));
              }}
            >
              Refresh
            </Button>
          )}
          <Button isDisabled={!loaded || saving || loading} onPress={() => setEditor({})}>
            Add service
          </Button>
          <Confirm
            label="Save changes"
            title="Apply service changes?"
            isDisabled={!dirty || saving || loading}
            onConfirm={save}
          >
            Enabled services will be public. Removed or disabled services stop accepting new proxied
            requests; existing connections may continue.
            {originalRoutes.current.some(
              (old) =>
                old.basicAuth &&
                !routes.some((route) => route.domain === old.domain && route.basicAuth),
            ) && ' Removing HTTP Basic authentication exposes that service to the public.'}
          </Confirm>
        </div>
      </div>
      {statusError && (
        <p className="error" role="alert">
          Status unavailable: {statusError}. Displayed status may be outdated.
        </p>
      )}
      <section className="panel">
        <label className="native-field">
          Filter domains{' '}
          <input
            type="search"
            value={domainFilter}
            onChange={(e) => setDomainFilter(e.target.value)}
          />
        </label>
        <p className="upstream">Traffic: last 24 hours; resets on restart.</p>
        <div className="table-wrap">
          <table className="site-table" aria-label="Service routes">
            <thead>
              <tr>
                {[
                  'Domain',
                  'Enabled',
                  'Host',
                  'Certificate',
                  'Requests',
                  'Received',
                  'Sent',
                  'Errors',
                  'Actions',
                ].map((label) => (
                  <th key={label} scope="col">
                    {label}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {visibleRoutes.map((route) => {
                const open = expanded.includes(route.domain),
                  total = sites[route.domain],
                  state = dirty ? undefined : health[route.domain],
                  cert = state?.certificate;
                return (
                  <Fragment key={route.domain}>
                    <tr>
                      <th scope="row">
                        <Button
                          className="site-toggle"
                          aria-expanded={open}
                          aria-controls={`activity-${route.domain}`}
                          onPress={() =>
                            setExpanded((prev) =>
                              open
                                ? prev.filter((d) => d !== route.domain)
                                : [...prev, route.domain],
                            )
                          }
                        >
                          {open ? '▾' : '▸'} {route.domain}
                        </Button>
                        <small className="upstream">
                          <span
                            className={`backend-light ${!route.enabled ? 'disabled' : statusError || !state?.backend || ['Unknown', 'Stale'].includes(state.backend.state) ? 'unknown' : state.backend.state === 'Online' ? 'online' : 'offline'}`}
                            role="img"
                            aria-label={`Backend ${!route.enabled ? 'disabled' : statusError ? 'status unavailable' : (state?.backend?.state ?? 'unknown')}`}
                            title={
                              !route.enabled
                                ? 'Backend check disabled'
                                : statusError
                                  ? 'Backend status unavailable'
                                  : `${state?.backend?.state ?? 'Not checked'}${state?.backend?.detail ? ': ' + state.backend.detail : ''}${state?.backend?.checked ? ' — checked ' + new Date(state.backend.checked).toLocaleString() : ''}. HTTP origin check; separate from Tailscale host status.`
                            }
                          />{' '}
                          {route.upstream}
                        </small>
                      </th>
                      <td>
                        <Check
                          aria-label={`Enable ${route.domain}`}
                          isSelected={route.enabled}
                          isDisabled={saving || loading}
                          onChange={(enabled) => {
                            setRoutes((prev) =>
                              prev.map((r) => (r.domain === route.domain ? { ...r, enabled } : r)),
                            );
                            setDirty(true);
                          }}
                        />
                      </td>
                      <td>
                        <span
                          title={state?.host.detail}
                          className={state?.host.state === 'Offline' ? 'error' : ''}
                        >
                          {statusError ? 'Unknown' : (state?.host.state ?? 'Unknown')}
                        </span>
                      </td>
                      <td>
                        {cert ? (
                          <Button
                            className={`status-button ${['Expired', 'Invalid', 'Failed', 'Error', 'Not yet valid'].includes(cert.state) ? 'error' : ''}`}
                            aria-label={`Certificate ${cert.attempt === 'Failed' || cert.state === 'Failed' || cert.state === 'Error' ? 'failed' : cert.state.toLowerCase()} for ${route.domain}`}
                            onPress={() => setCertificateDomain(route.domain)}
                          >
                            {cert.attempt === 'Issuing'
                              ? 'Issuing'
                              : cert.attempt === 'Failed' ||
                                  cert.state === 'Error' ||
                                  cert.state === 'Failed'
                                ? 'Failed'
                                : cert.state}
                          </Button>
                        ) : (
                          <Button
                            className="status-button"
                            aria-label={`Certificate unknown for ${route.domain}`}
                            onPress={() => setCertificateDomain(route.domain)}
                          >
                            Unknown
                          </Button>
                        )}
                        {cert?.state === 'Not issued' && cert.detail === 'Waiting on traffic' && (
                          <small className="upstream">Waiting on traffic</small>
                        )}
                        {cert?.expires && !cert.expires.startsWith('0001') && (
                          <small className="upstream">
                            Expires {new Date(cert.expires).toLocaleDateString()}
                          </small>
                        )}
                      </td>
                      <td>{(total?.requests ?? 0).toLocaleString()}</td>
                      <td>{bytes(total?.received)}</td>
                      <td>{bytes(total?.sent)}</td>
                      <td>{(total?.errors ?? 0).toLocaleString()}</td>
                      <td>
                        <div className="row-actions">
                          <Button
                            className="secondary"
                            aria-label={`Edit ${route.domain}`}
                            isDisabled={saving || loading}
                            onPress={() => setEditor({ route })}
                          >
                            Edit
                          </Button>
                          <Button
                            className="secondary"
                            isDisabled={saving || loading}
                            aria-label={`Remove ${route.domain}`}
                            onPress={() => {
                              setRoutes((prev) => prev.filter((r) => r.domain !== route.domain));
                              setDirty(true);
                            }}
                          >
                            Remove
                          </Button>
                        </div>
                      </td>
                    </tr>
                    <tr hidden={!open}>
                      <td colSpan={9} id={`activity-${route.domain}`}>
                        {open && (
                          <>
                            {state?.lastError && (
                              <p className="error">
                                Last upstream error: {state.lastError}
                                {state.lastResponse &&
                                  ` (${new Date(state.lastResponse).toLocaleString()})`}
                              </p>
                            )}
                            <ClientDetails domain={route.domain} />
                          </>
                        )}
                      </td>
                    </tr>
                  </Fragment>
                );
              })}
            </tbody>
          </table>
          {loaded && routes.length > 0 && !visibleRoutes.length && (
            <p className="padded">No matching services.</p>
          )}
          {!routes.length && (
            <p className="padded">{loaded ? 'No services configured.' : 'Loading services…'}</p>
          )}
        </div>
      </section>
      <Notice message={message} />
      {editor && (
        <ServiceEditor
          route={editor.route}
          onClose={() => {
            if (!saving && !loading) setEditor(undefined);
          }}
          onSave={(route) => {
            if (savingRef.current || loading) return;
            if (
              routes.some((r) => r.domain === route.domain && r.domain !== editor.route?.domain)
            ) {
              setMessage('Domain already configured.');
              return;
            }
            setRoutes((prev) =>
              editor.route
                ? prev.map((r) => (r.domain === editor.route?.domain ? route : r))
                : [...prev, route],
            );
            setDirty(true);
            setEditor(undefined);
            setMessage('Save changes to apply.');
          }}
        />
      )}
      {certificateDomain && (
        <CertificateDialog
          domain={certificateDomain}
          certificate={health[certificateDomain]?.certificate}
          enabled={routes.some((r) => r.domain === certificateDomain && r.enabled)}
          onClose={() => setCertificateDomain(undefined)}
          onRefresh={refreshStatus}
        />
      )}
      {needsLogin && <Reauthenticate />}
    </>
  );
}
