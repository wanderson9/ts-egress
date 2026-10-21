import { useEffect, useState } from 'react';
import { Button } from 'react-aria-components';
import { api, type Activity } from './api';
import { Confirm } from './components';

export function bytes(n = 0) {
  if (n < 1024) return `${n} B`;
  const i = Math.min(Math.floor(Math.log(n) / Math.log(1024)), 4);
  return `${(n / 1024 ** i).toFixed(1)} ${['B', 'KiB', 'MiB', 'GiB', 'TiB'][i]}`;
}
export function ClientDetails({ domain }: { domain: string }) {
  const [data, setData] = useState<Activity>();
  const [error, setError] = useState('');
  const [page, setPage] = useState(0);
  const [sort, setSort] = useState('traffic');
  const [geolocation, setGeolocation] = useState(false);
  const [geoAllowed, setGeoAllowed] = useState(false);
  useEffect(() => {
    api<{ enabled: boolean }>('/api/geolocation')
      .then((value) => setGeoAllowed(value.enabled))
      .catch(() => setGeoAllowed(false));
  }, []);
  useEffect(() => {
    let active = true;
    let timer: ReturnType<typeof setTimeout>;
    async function poll() {
      try {
        const next = await api<Activity>(
          `/api/activity?domain=${encodeURIComponent(domain)}${geolocation && geoAllowed ? '&geolocation=enabled' : ''}`,
        );
        if (active) {
          setData((prev) => (JSON.stringify(prev) === JSON.stringify(next) ? prev : next));
          setError('');
        }
      } catch (e) {
        if (active) setError((e as Error).message);
      }
      if (active) timer = setTimeout(poll, 5000);
    }
    void poll();
    return () => {
      active = false;
      clearTimeout(timer);
    };
  }, [domain, geolocation, geoAllowed]);
  const clients = [...(data?.activity.clients ?? [])].sort((a, b) =>
    sort === 'ip'
      ? a.ip.localeCompare(b.ip)
      : sort === 'traffic'
        ? b.received + b.sent - (a.received + a.sent) || a.ip.localeCompare(b.ip)
        : b[sort as 'requests' | 'sent' | 'received' | 'errors' | 'lastSeen'] -
            a[sort as 'requests' | 'sent' | 'received' | 'errors' | 'lastSeen'] ||
          a.ip.localeCompare(b.ip),
  );
  const pages = Math.max(1, Math.ceil(clients.length / 20)),
    current = Math.min(page, pages - 1);
  return (
    <section className="client-details" aria-label={`Client activity for ${domain}`}>
      <div className="heading">
        <h3>Clients</h3>
        <label>
          <input
            type="checkbox"
            checked={geolocation}
            disabled={!geoAllowed}
            onChange={(event) => setGeolocation(event.target.checked)}
          />
          {geoAllowed ? 'Look up public IP countries' : 'Country lookups disabled'}
        </label>
        {!geoAllowed && <a href="/settings">Enable in Settings</a>}
        <label>
          Sort by{' '}
          <select
            value={sort}
            onChange={(e) => {
              setSort(e.target.value);
              setPage(0);
            }}
          >
            {[
              ['traffic', 'Total traffic'],
              ['requests', 'Requests'],
              ['sent', 'Sent'],
              ['received', 'Received'],
              ['errors', 'Errors'],
              ['lastSeen', 'Last seen'],
              ['ip', 'Client IP'],
            ].map(([value, label]) => (
              <option key={value} value={value}>
                {label}
              </option>
            ))}
          </select>
        </label>
      </div>
      {error && <p role="alert">{error}</p>}
      {!data ? (
        <p role="status">Loading activity…</p>
      ) : (
        <>
          <div className="table-wrap">
            <table aria-label={`Client IPs for ${domain}`}>
              <thead>
                <tr>
                  {[
                    'Client IP',
                    'Country',
                    'Total traffic',
                    'Requests',
                    'Received',
                    'Sent',
                    'Errors',
                    'Last seen',
                    'Actions',
                  ].map((label) => (
                    <th key={label} scope="col">
                      {label}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {clients.slice(current * 20, current * 20 + 20).map((client) => (
                  <tr key={client.ip}>
                    <th scope="row">
                      {client.ip.startsWith('Other /') ? (
                        <code>{client.ip}</code>
                      ) : (
                        <a
                          href={`https://www.abuseipdb.com/check/${encodeURIComponent(client.ip)}`}
                          target="_blank"
                          rel="noopener noreferrer"
                          aria-label={`Check ${client.ip} on AbuseIPDB (opens in a new tab)`}
                        >
                          <code>{client.ip}</code>
                        </a>
                      )}
                    </th>
                    <td>
                      {data.countries[client.ip] ? (
                        <span title={data.countries[client.ip].country}>
                          {data.countries[client.ip].flag} {data.countries[client.ip].country}
                        </span>
                      ) : (
                        '—'
                      )}
                    </td>
                    <td>{bytes(client.received + client.sent)}</td>
                    <td>{client.requests.toLocaleString()}</td>
                    <td>{bytes(client.received)}</td>
                    <td>{bytes(client.sent)}</td>
                    <td>{client.errors.toLocaleString()}</td>
                    <td>{new Date(client.lastSeen * 1000).toLocaleTimeString()}</td>
                    <td>
                      {data.blockingAvailable && !client.ip.startsWith('Other /') && (
                        <Confirm
                          destructive={!data.blockedIPs?.includes(client.ip)}
                          label={data.blockedIPs?.includes(client.ip) ? 'Unblock IP' : 'Block IP'}
                          title={`${data.blockedIPs?.includes(client.ip) ? 'Unblock' : 'Block'} ${client.ip}?`}
                          onConfirm={async () => {
                            const blocked = !data.blockedIPs?.includes(client.ip);
                            await api('/api/blocks', 'PUT', { ip: client.ip, blocked });
                            setData((prev) =>
                              prev
                                ? {
                                    ...prev,
                                    blockedIPs: blocked
                                      ? [...(prev.blockedIPs ?? []), client.ip]
                                      : (prev.blockedIPs ?? []).filter((ip) => ip !== client.ip),
                                  }
                                : prev,
                            );
                          }}
                        >
                          This changes access to all public services for this IP. Shared IPs may
                          represent multiple users. Existing streams may continue.
                        </Confirm>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {!clients.length && <p>No client activity recorded yet.</p>}
          <div className="actions">
            <Button
              className="secondary"
              isDisabled={current === 0}
              onPress={() => setPage(current - 1)}
            >
              ← Previous
            </Button>
            <span>
              Page {current + 1} of {pages}
            </span>
            <Button
              className="secondary"
              isDisabled={current >= pages - 1}
              onPress={() => setPage(current + 1)}
            >
              Next →
            </Button>
          </div>
          {data.activity.limited && (
            <p>
              IP detail capacity reached; excess traffic is grouped as Other. Site totals continue
              counting.
            </p>
          )}
        </>
      )}
    </section>
  );
}
