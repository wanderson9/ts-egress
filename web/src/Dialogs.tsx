import { useEffect, useState, type ReactNode } from 'react';
import { Button, Dialog, Heading, Modal, ModalOverlay, Form, Link } from 'react-aria-components';
import { api, type Route, type ServiceHealth, type SetupStatus } from './api';
import { Field } from './components';

export function Popup({
  title,
  children,
  onClose,
  blocking = false,
}: {
  title: string;
  children: ReactNode;
  onClose?: () => void;
  blocking?: boolean;
}) {
  return (
    <ModalOverlay
      isOpen
      isDismissable={!blocking}
      isKeyboardDismissDisabled={blocking}
      onOpenChange={(open) => {
        if (!open) onClose?.();
      }}
    >
      <Modal>
        <Dialog role={blocking ? 'alertdialog' : 'dialog'}>
          <Heading slot="title">{title}</Heading>
          {children}
        </Dialog>
      </Modal>
    </ModalOverlay>
  );
}
export function ServiceEditor({
  route,
  onSave,
  onClose,
}: {
  route?: Route;
  onSave: (route: Route) => void;
  onClose: () => void;
}) {
  const original = route ? new URL(route.upstream) : undefined;
  const originalHost = original?.hostname.replace(/^\[|\]$/g, '') ?? '';
  const [domain, setDomain] = useState(route?.domain ?? ''),
    [host, setHost] = useState(originalHost),
    [port, setPort] = useState(
      original?.port || (original?.protocol === 'https:' ? '443' : route ? '80' : '3000'),
    ),
    [protocol, setProtocol] = useState(original?.protocol === 'https:' ? 'https' : 'http');
  const [domainError, setDomainError] = useState('');
  const [authUser, setAuthUser] = useState(route?.basicAuthUser ?? ''),
    [authPassword, setAuthPassword] = useState('');
  const [peers, setPeers] = useState<SetupStatus['tailnet']['peers']>([]),
    [loading, setLoading] = useState(true),
    [error, setError] = useState('');
  async function load() {
    setLoading(true);
    try {
      const data = await api<SetupStatus>('/api/setup/status');
      setPeers([...data.tailnet.peers].sort((a, b) => a.name.localeCompare(b.name)));
      setError('');
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setLoading(false);
    }
  }
  useEffect(() => {
    void load();
  }, []);
  const options = peers.map((peer) => {
    const matches =
      peer.name === host || peer.name.split('.')[0] === host || peer.ips.includes(host);
    return {
      value: matches ? host : peer.name || peer.ips[0],
      label: `${peer.name || peer.ips[0]}${peer.online ? '' : ' (offline)'}`,
    };
  });
  const missing = !!host && !options.some((option) => option.value === host);
  return (
    <Popup title={route ? 'Edit service' : 'Add service'} onClose={onClose}>
      <Form
        onSubmit={(e) => {
          e.preventDefault();
          const normalizedDomain = domain.trim().toLowerCase();
          if (!validPublicDomain(normalizedDomain)) {
            setDomainError(
              'Enter a lowercase public DNS name with at least one dot (not an IP address).',
            );
            return;
          }
          setDomainError('');
          if (!host || !/^\d+$/.test(port) || Number(port) < 1 || Number(port) > 65535) return;
          const address = host.includes(':') ? `[${host}]` : host;
          onSave({
            domain: normalizedDomain,
            upstream: `${protocol}://${address}:${port}`,
            enabled: route?.enabled ?? true,
            basicAuthUser: authUser.trim() || undefined,
            basicAuthPassword: authPassword || undefined,
            basicAuth: !!authUser.trim(),
          });
        }}
      >
        <Field
          label="Domain"
          isRequired
          value={domain}
          placeholder="app.example.com"
          onChange={(value) => {
            setDomain(value);
            setDomainError('');
          }}
        />
        {domainError && (
          <p className="error" role="alert">
            {domainError}
          </p>
        )}
        <label className="native-field">
          Host
          <select required value={host} onChange={(e) => setHost(e.target.value)}>
            <option value="" disabled>
              {loading ? 'Loading hosts…' : 'Select a host'}
            </option>
            {missing && <option value={host}>{host} (not currently visible)</option>}
            {options.map((option) => (
              <option key={option.value} value={option.value}>
                {option.label}
              </option>
            ))}
          </select>
        </label>
        <div className="device-details">
          <span>{error || (!loading && !peers.length ? 'No hosts found.' : '')}</span>
          <Button className="secondary" isDisabled={loading} onPress={load}>
            {loading ? 'Loading…' : 'Refresh hosts'}
          </Button>
        </div>
        <fieldset>
          <legend>HTTP Basic authentication</legend>
          <Field label="Username" value={authUser} onChange={setAuthUser} />
          <Field
            label={route?.basicAuth ? 'New password (leave blank to keep)' : 'Password'}
            type="password"
            value={authPassword}
            onChange={setAuthPassword}
          />
        </fieldset>
        <div className="endpoint-fields">
          <Field
            label="Port"
            type="number"
            min={1}
            max={65535}
            isRequired
            value={port}
            onChange={setPort}
          />
          <label className="native-field">
            Protocol
            <select value={protocol} onChange={(e) => setProtocol(e.target.value)}>
              <option value="http">HTTP</option>
              <option value="https">HTTPS</option>
            </select>
          </label>
        </div>
        <div className="actions">
          <Button className="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit" isDisabled={!host || loading}>
            {route ? 'Update service' : 'Add service'}
          </Button>
        </div>
      </Form>
    </Popup>
  );
}
export function validPublicDomain(value: string): boolean {
  if (value.length > 253 || !value.includes('.') || value !== value.toLowerCase()) return false;
  const ipv4 = value.split('.');
  if (
    ipv4.length === 4 &&
    ipv4.every((part) => /^(0|[1-9]\d{0,2})$/.test(part) && Number(part) <= 255)
  )
    return false;
  return value
    .split('.')
    .every((label) => label.length <= 63 && /^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/.test(label));
}
export function CertificateDialog({
  domain,
  certificate,
  enabled,
  onClose,
  onRefresh,
}: {
  domain: string;
  certificate?: ServiceHealth['certificate'];
  enabled: boolean;
  onClose: () => void;
  onRefresh: () => Promise<void>;
}) {
  const [busy, setBusy] = useState(false),
    [error, setError] = useState('');
  return (
    <Popup title={`Certificate: ${domain}`} onClose={onClose}>
      <p>{certificate?.state ?? 'Unknown'}</p>
      {certificate?.attempt === 'Failed' && (
        <p>Review the failure details, then retry when the cause is resolved.</p>
      )}
      {certificate?.state === 'Expired' && (
        <p>Check DNS and gateway reachability, then retry certificate issuance when available.</p>
      )}
      {certificate?.state === 'Invalid' && (
        <p>Check the domain’s DNS records and gateway certificate configuration.</p>
      )}
      {certificate?.state === 'Not yet valid' && (
        <p>Check the gateway clock and certificate validity dates.</p>
      )}
      {certificate?.attempt && <p>Last certificate attempt: {certificate.attempt}</p>}
      {certificate?.detail && <pre className="error-detail">{certificate.detail}</pre>}
      {error && (
        <p className="error" role="alert">
          {error}
        </p>
      )}
      <div className="actions">
        <Button className="secondary" onPress={onClose}>
          Close
        </Button>
        {(certificate?.attempt === 'Failed' ||
          certificate?.state === 'Failed' ||
          certificate?.state === 'Error') && (
          <Button
            isDisabled={busy || !enabled || certificate?.attempt === 'Issuing'}
            onPress={async () => {
              setBusy(true);
              try {
                await api('/api/certificate/retry', 'POST', { domain });
                setError('');
                await onRefresh();
              } catch (e) {
                setError((e as Error).message);
              } finally {
                setBusy(false);
              }
            }}
          >
            {busy || certificate?.attempt === 'Issuing' ? 'Retrying…' : 'Retry'}
          </Button>
        )}
      </div>
    </Popup>
  );
}
export function Reauthenticate() {
  return (
    <Popup title="Tailscale authentication required" blocking>
      <p>Reconnect this gateway from its local setup console or an SSH tunnel.</p>
      <div className="actions">
        <Link className="primary-link" href="/setup?step=2">
          Back to setup →
        </Link>
      </div>
    </Popup>
  );
}
export function LoggedOut() {
  return (
    <Popup title="Tailscale logout requested" blocking>
      <p>
        Logout is running. This dashboard may become unreachable. Check the local console for the
        result and reconnect there.
      </p>
      <p>
        To reconnect, open the gateway's local setup console on the host and choose{' '}
        <strong>Sign in to Tailscale</strong> or <strong>Retry connection</strong>:
      </p>
      <p>
        <code>http://127.0.0.1:8080</code>
      </p>
      <p>
        From another machine, forward that port first, for example{' '}
        <code>ssh -L 8080:127.0.0.1:8080 user@host</code>, then open the address above. Tailscale
        sign-in does not work from this page while the gateway is logged out.
      </p>
    </Popup>
  );
}
