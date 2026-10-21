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
    image: ghcr.io/wanderson9/ts-egress:v0.1.2
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

Keep `/data` persistent and backed up. The container runs as UID/GID `10001:10001`; bind-mounted storage must be writable by that user.

## Application setup

1. Open **http://localhost:8080** and create an administrator password.
2. Set the gateway name, administrator access, and certificate contact email.
3. Connect to Tailscale using the authorization link. Enable MagicDNS and HTTPS certificates in your tailnet's DNS settings.
4. Open the private dashboard and add a service: public domain, tailnet device, port, and HTTP or HTTPS backend.
5. Point the domain's public DNS records at the gateway host and allow inbound TCP ports **80** and **443**.

For a remote host, forward the local console over SSH:

```sh
ssh -L 8080:127.0.0.1:8080 user@host
```

After enrollment, administration is available at the gateway's private Tailscale HTTPS address. The local console remains available for password changes and reconnection. Tailnet access policies must permit the gateway to reach your backend services.

The ACME email is used for initial account registration and is locked once connection setup begins. It does not update an existing Let's Encrypt account contact; update existing account contacts with the certificate authority rather than deleting certificate or account state.

> **Keep port 8080 bound to loopback.** Enabled services are public; protect sensitive applications with their own authentication. The administrator allowlist is optional—an empty list admits any untagged user on your tailnet.

## Features

- Automatic public TLS certificates and renewal.
- HTTP streaming and WebSocket support.
  Public transfers have a 60-second read/write idle timeout and a 32-request concurrent limit per source IP (512 gateway-wide). WebSocket clients must send traffic or heartbeat pings at least every 60 seconds; active streams have no total duration limit. Shared NAT clients share the source-IP limit.
- Separate Tailscale host, backend HTTP, and certificate status.
- Rolling 24-hour client traffic statistics, sorted by total bandwidth. Statistics reset on restart.
- Optional country lookups through ipwho.is; enabling this discloses selected public client IPs to that provider.
- Persistent, gateway-wide IP blocking. Blocks affect new proxy requests, not existing streams, and use the direct connection source IP.

Upstreams must be Tailscale peers. Wildcard domains, subnet-route destinations, raw TCP/UDP proxying, and built-in public application authentication are not supported.

## Standalone binary

Download the archive for your architecture from [Releases](https://github.com/wanderson9/ts-egress/releases), verify its SHA-256 checksum, and extract it. The web interface is included.

```sh
./ts-egress -state-dir ./data
```

Native defaults: setup on `127.0.0.1:8080`, public HTTP on `:80`, and HTTPS on `:443`. Binding low ports requires appropriate host permissions. Run `./ts-egress -help` for listener and recovery options.

## Support

View container logs:

```sh
docker compose logs -f ts-egress
```

Report problems through [GitHub Issues](https://github.com/wanderson9/ts-egress/issues). Include the application version, deployment method, and relevant logs with credentials and authorization URLs removed.

## Building locally

```sh
docker compose up -d --build
```

For native development, install Go 1.27.3+ and Node.js 24, then run `make build`. Run `make check` for the test suite.
