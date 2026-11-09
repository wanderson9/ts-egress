# ts-egress

A self-hosted reverse proxy that publishes HTTP and HTTPS services from your Tailscale network on public domains. Includes automatic Let's Encrypt certificates, a private administration dashboard, backend health checks, and client traffic statistics.

Runs as a single container or standalone binary. No host Tailscale installation, privileged container, or configuration files required.

## Supported architectures

| Architecture | Container | Linux binary |
| :--- | :---: | :---: |
| x86-64 / amd64 | Yes | Yes |
| arm64 | Yes | Yes |

## Usage

### Docker Compose

Save as `compose.yaml`:

```yaml
services:
  ts-egress:
    image: ghcr.io/wanderson9/ts-egress:v0.2.2
    restart: unless-stopped
    ports:
      - "127.0.0.1:8080:8080"
      - "80:80"
      - "443:443"
    volumes:
      - ts-egress-data:/data
    cap_drop:
      - ALL
    cap_add:
      - NET_BIND_SERVICE
    security_opt:
      - no-new-privileges:true
    read_only: true
    tmpfs:
      - /tmp
    sysctls:
      net.ipv4.ip_unprivileged_port_start: "0"

volumes:
  ts-egress-data:
```

Start the container:

```sh
docker compose up -d
```

### Ports and storage

| Mapping | Purpose |
| :--- | :--- |
| `127.0.0.1:8080:8080` | Local setup and recovery console |
| `80:80/tcp` | HTTP redirects and certificate validation |
| `443:443/tcp` | Public HTTPS services |
| `/data` | Persistent settings, routes, Tailscale identity, certificates, and IP blocks |

Keep `/data` persistent and backed up. The container runs as UID/GID `10001:10001`; bind-mounted storage must be writable by that user. Run exactly one instance against a state directory/volume: state is not coordinated between writers, so do not share it between replicas. Stop the service before taking a consistent backup; restore the complete state directory (including Tailscale identity and certificate data) while stopped, then restart. Treat backups as secrets.

## Application setup

1. Find the one-time setup token in the logs (`docker compose logs ts-egress`). A new token is generated at each start until the gateway is claimed.
2. Open **http://localhost:8080**, enter the token, and create an administrator password.
3. Set the gateway name and certificate contact email.
4. Connect to Tailscale using the authorization link. The account that authorizes the gateway becomes its administrator; add others in Settings. Enable MagicDNS and HTTPS certificates in your tailnet's DNS settings.
5. Open the private dashboard and add a service: public domain, tailnet device, port, and HTTP or HTTPS backend.
6. Point the domain's public DNS records at the gateway host and allow inbound TCP ports **80** and **443**.

For a remote host, forward the local console over SSH:

```sh
ssh -L 8080:127.0.0.1:8080 user@host
```

After enrollment, administration is available at the gateway's private Tailscale HTTPS address. The local console remains available for password changes and reconnection. Tailnet access policies must permit the gateway to reach your backend services.

The ACME email is used for initial account registration and is locked once connection setup begins. It does not update an existing Let's Encrypt account contact; update existing account contacts with the certificate authority rather than deleting certificate or account state.

> **Keep port 8080 bound to host loopback** exactly as shown. The container listens on all interfaces internally, so publishing `8080:8080`, using host networking, or allowing untrusted containers on the same network can expose first-run setup and administrator recovery. Do not proxy this listener through a public reverse proxy. The setup token stops someone who reaches an unclaimed listener from claiming it without log access, but a published listener still exposes administrator sign-in to password guessing. Use the SSH tunnel above for remote administration.

### Tailnet access policy

Use a dedicated tag for the gateway and grant it only the backend ports it must reach. For example, adapt this fragment to your tailnet's ACL/grant policy and actual tagged backend identities and ports:

```json
{
  "tagOwners": { "tag:ts-egress": ["autogroup:admin"] },
  "grants": [
    { "src": ["tag:ts-egress"], "dst": ["tag:app-backend"], "ip": ["tcp:8080"] }
  ]
}
```

Enroll the gateway as `tag:ts-egress`; make backend devices `tag:app-backend` (or use explicit device selectors supported by your policy). Add only the required protocol/ports, and separately allow intended human administrators to reach the gateway's dashboard. The gateway uses its own Tailscale identity for backend connections—not the public visitor's identity or permissions. A visitor can reach any enabled public route, so use app-level authentication for private content.

Define ownership for your backend tags as well, and remove any existing broad allow-all rule that would override this restriction. A tagged gateway has no owning account, so set administrator login names during setup. An untagged gateway with no administrators adopts the account that authorized it. A tagged gateway with an empty allowlist admits nobody unless open access is explicitly enabled. Open access admits every untagged user that can reach the dashboard, including users of other tailnets if you share the gateway node with them.

### Upgrading to v0.2.2 or later

- **Administrators.** An empty allowlist no longer admits everyone. An untagged gateway adopts the account that authorized it as administrator, so a single-user install needs no action. On a shared tailnet, that account should add other administrators in Settings. A tagged gateway denies everyone until administrators are set, so add them before upgrading, or use `-recover-admin -recover-admins` as described under [Backup and recovery](#backup-and-recovery).
- **Basic authentication passwords.** New passwords must be at least 12 bytes. Stored passwords keep working.
- **Client address headers.** The gateway now removes client-supplied `X-Real-IP`, `True-Client-IP`, `CF-Connecting-IP`, and similar headers, including spellings with underscores. Backends should read the client address from `X-Forwarded-For`.
- **Short upstream names.** An upstream that uses a short device name now fails if several peers share it. Use the full MagicDNS name.

## Features

- Automatic public TLS certificates and renewal.
- HTTP streaming and WebSocket support.
  Public transfers have a 60-second read/write idle timeout, including stalled upstream response bodies, and a concurrent limit of 32 requests per source (512 gateway-wide, 256 per service). A source is one IPv4 address or one IPv6 /64. WebSocket clients must send traffic or heartbeat pings at least every 60 seconds; active streams have no total duration limit, but they end when their service is changed, disabled, or removed. Shared NAT clients share the source limit.
- Separate Tailscale host, backend HTTP, and certificate status.
  Backend checks use the public Host header. Certificate metadata refreshes within five seconds; status does not verify public DNS or external reachability.
- Rolling 24-hour client traffic statistics, sorted by total bandwidth. Statistics reset on restart.
- Optional country lookups through ipwho.is; enabling this discloses selected public client IPs to that provider.
- Persistent, gateway-wide blocking of IP addresses and CIDR ranges. Blocks affect new proxy requests, not existing streams, and use the direct connection source IP. Docker's userland proxy can present IPv6 clients as the Docker gateway address; if clients show that address, limits and blocks apply to all of them together, so enable IPv6 on the container network.

The gateway makes outbound connections to Tailscale, Let's Encrypt/ACME, configured peer backends, GitHub for cached release-version checks when the UI requests version information, and (only when country lookup is enabled) ipwho.is. Its public endpoints are intended to be reachable from the internet: configured routes are public unless protected by Basic authentication or the backend itself. This is a single-instance reverse proxy, not a WAF, DDoS mitigation service, or high-availability deployment. Keep the image and standalone binary updated, verify release checksums, and review release notes before upgrading; back up state before upgrades.

Upstreams must be Tailscale peers; wildcard domains, subnet-route destinations, and raw TCP/UDP proxying are not supported. Optional per-service HTTP Basic authentication is supported over HTTPS. Usernames and passwords are stored as route credentials (passwords are bcrypt-hashed and at least 12 bytes). After 10 failed attempts, a source is blocked from further attempts for up to a minute; clients behind the same NAT share that limit. Basic credentials are reusable secrets, so require HTTPS and avoid reusing passwords. This is a single shared credential per service, not per-user accounts, SSO, MFA, or role-based authorization; it does not replace the backend application's own security controls.

## Backup and recovery

For Compose, stop the only service before copying the volume, and restore it with the service stopped. For example, with Docker's named volume:

```sh
mkdir -m 700 -p ./backup
chmod 700 ./backup
sudo chown 10001:10001 ./backup
docker compose stop ts-egress
docker compose run --rm --no-deps -T --entrypoint tar -v "$PWD/backup:/backup" ts-egress \
  -C /data -czf /backup/ts-egress-data.tgz .
# Restore to the same volume only while ts-egress is stopped:
docker compose run --rm --no-deps -T --entrypoint sh -v "$PWD/backup:/backup" ts-egress \
  -c 'cd /data && tar -xzf /backup/ts-egress-data.tgz'
docker compose start ts-egress
```

Protect the archive: it contains gateway identity and configuration. To recover the local administrator password from a Docker volume, stop the service and run the same image with the recovery flag (the password is read interactively without being displayed; do not put it on the command line):

```sh
docker compose stop ts-egress
docker compose run --rm --no-deps -it --entrypoint /usr/local/bin/ts-egress ts-egress \
  -state-dir /data -recover-admin
docker compose start ts-egress
```

For a native install, stop the process and run `./ts-egress -state-dir ./data -recover-admin`; then restart normally. Recovery can optionally replace the Tailscale admin allowlist with `-recover-admins user@example.com` (comma-separated for multiple users), which also turns off open access. Do not use recovery as a way to change account email; it only resets the local password and optionally administrators.

## Standalone binary

Download the archive for your architecture from [Releases](https://github.com/wanderson9/ts-egress/releases), verify its SHA-256 checksum and build attestation, and extract it. Releases from v0.2.2 onward can be verified with `gh attestation verify <archive> --repo wanderson9/ts-egress`. The web interface is included.

```sh
./ts-egress -state-dir ./data
```

Native defaults: setup on `127.0.0.1:8080`, public HTTP on `:80`, and HTTPS on `:443`. Binding low ports requires appropriate host permissions. Run `./ts-egress -help` for listener and recovery options.

## Support

View container logs:

```sh
docker compose logs -f ts-egress
```

Report security vulnerabilities privately as described in [SECURITY.md](SECURITY.md). Report other problems through [GitHub Issues](https://github.com/wanderson9/ts-egress/issues). Include the application version, deployment method, and relevant logs with credentials and authorization URLs removed.

## Building locally

```sh
docker compose up -d --build
```

For native development, install Go 1.27.3+ and Node.js 24, then run `make build`. Run `make check` for the test suite.
