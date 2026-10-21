import { Fragment, useEffect, useState } from 'react';
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
    accept(await api<Status>('/api/status'));
  }
  async function load() {
    try {
      const data = await api<Status>('/api/status');
      accept(data);
      setRevision(data.revision);
      setRoutes(data.routes);
      setDirty(false);
      setLoaded(true);
    } catch (e) {
      setStatusError((e as Error).message);
    }
  }
  async function confirmLoad() {
    await load();
    if (statusError) throw new Error(statusError);
  }
  useEffect(() => {
    let active = true,
      timer: ReturnType<typeof setTimeout>;
    async function poll() {
      try {
        const data = await api<Status>('/api/status');
        if (active) {
          accept(data);
          setLoaded(true);
          if (!dirty) {
            setRoutes(data.routes);
            setRevision(data.revision);
          }
        }
      } catch (e) {
        if (active) setStatusError((e as Error).message);
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
    try {
      await api('/api/routes', 'PUT', routes, { 'If-Match': String(revision) });
      setDirty(false);
      setMessage('Changes saved.');
    } catch (e) {
      throw e;
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
            <Confirm label="Reload" title="Discard unsaved changes?" onConfirm={confirmLoad}>
              Your unsaved edits will be discarded.
            </Confirm>
          ) : (
            <Button className="secondary" onPress={load}>
              Refresh
            </Button>
          )}
          <Button isDisabled={!loaded} onPress={() => setEditor({})}>
            Add service
          </Button>
          <Confirm
            label="Save changes"
            title="Apply service changes?"
            isDisabled={!dirty}
            onConfirm={save}
          >
            Enabled services will be public. Removed or disabled services stop accepting new proxied
            requests; existing connections may continue.
          </Confirm>
        </div>
      </div>
      {statusError && (
        <p className="error" role="alert">
          Status unavailable: {statusError}. Displayed status may be outdated.
        </p>
      )}
      <section className="panel">
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
              {routes.map((route) => {
                const open = expanded.includes(route.domain),
                  total = sites[route.domain],
                  state = health[route.domain],
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
                        {cert?.attempt === 'Failed' ||
                        cert?.state === 'Failed' ||
                        cert?.state === 'Error' ? (
                          <Button
                            className="status-button error"
                            aria-label={`Certificate failed for ${route.domain}`}
                            onPress={() => setCertificateDomain(route.domain)}
                          >
                            Failed
                          </Button>
                        ) : (
                          <span
                            className={
                              ['Expired', 'Invalid'].includes(cert?.state ?? '') ? 'error' : ''
                            }
                          >
                            {cert?.state ?? 'Unknown'}
                          </span>
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
                            onPress={() => setEditor({ route })}
                          >
                            Edit
                          </Button>
                          <Button
                            className="secondary"
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
          {!routes.length && (
            <p className="padded">{loaded ? 'No services configured.' : 'Loading services…'}</p>
          )}
        </div>
      </section>
      <Notice message={message} />
      {editor && (
        <ServiceEditor
          route={editor.route}
          onClose={() => setEditor(undefined)}
          onSave={(route) => {
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
