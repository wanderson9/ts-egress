# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through [GitHub private vulnerability reporting](https://github.com/wanderson9/ts-egress/security/advisories/new). Do not open a public issue.

Include the affected version, your deployment method, reproduction steps, and the impact you observed. Remove real credentials and Tailscale authorization URLs from anything you send.

## Supported versions

Security fixes are made in the latest release only. Keep your image or binary up to date and review the release notes before upgrading.

## Scope

The security boundaries this project intends to enforce:

- Public listeners serve only configured services and never management functions.
- The tailnet dashboard admits only allowlisted Tailscale identities, or all untagged users if open access was explicitly enabled.
- The local console is reachable only through host loopback, and first-run setup requires the token printed in the gateway logs.
- Public traffic reaches only Tailscale peers, never arbitrary addresses.

ts-egress is not a WAF, DDoS mitigation service, or high-availability deployment, so volumetric denial of service is out of scope. Publishing the setup port beyond host loopback is a deployment error, not a vulnerability.
