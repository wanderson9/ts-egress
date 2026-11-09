# Container release review

> Historical review below describes the original v0.2.1 findings. Remediation is in progress in this checkout; see the remediation addendum at the end rather than treating the original validation limitations as current results.

Reviewed 2026-11-08 at commit `3abd862` (v0.2.1). Application code was not changed.

## Verdict

Hold broad promotion until the vulnerable dependency is updated and the supplied container has passed a real startup/restart/persistence smoke test. No direct public-management bypass or tailnet control-plane privilege escalation was identified in this review. This is not a guarantee of absence of vulnerabilities.

## Findings

### High / release blocker: Go vulnerability scan fails

`go.mod:27` pins `github.com/klauspost/compress v1.20.0`. `govulncheck v1.9.0` reports GO-2026-6865 / CVE-2026-116591, a panic vulnerability fixed in v1.20.2. Reported symbol paths involve sync callbacks and do not independently demonstrate remote exploitability through this app. Update the dependency, regenerate go.sum, and rerun the scanner and tests. The current release verification job invokes this scanner and will fail on this finding.

Advisory: https://pkg.go.dev/vuln/GO-2026-6865

### Medium: supplied smoke test cannot start

`Makefile:22` publishes both container 8080 and container 80 on host TCP 18080. Use distinct host ports. Additionally, `make smoke` is not invoked by `.github/workflows/ci.yml`; the container job only builds the image and checks Compose syntax. Add an actual runtime test and verify restart with persisted settings. Verify both supported architectures before claiming runtime support for both.

### Medium / security deployment caveat: empty allowlist grants proxy administration

`cmd/ts-egress/main.go:215-231`, `internal/setup/store.go:44`, and `web/src/Setup.tsx:8-14`: an empty allowlist accepts any untagged identity that can reach the dashboard. Such a user can publish gateway-reachable peer services, change administrator access, and log the gateway out. This is intentional and documented, not an authentication bypass, but it is broader than a least-privilege default. Prefer requiring explicit administrator selection or explicit open-access opt-in. Document a least-privilege gateway ACL/grant example. Backend connections use the gateway's identity, not the public visitor's permissions.

### Medium / deployment caveat: setup isolation relies on host port publication

`Dockerfile:25` binds setup to all container interfaces; `internal/setup/server.go:114-118` checks Host rather than the source address. A non-browser client can forge a loopback Host/Origin. The supplied Compose loopback binding is therefore an essential security boundary, not optional guidance. Before first claim, anyone who reaches the setup listener can claim it. Document the danger of `-p 8080:8080`, host networking, untrusted containers on the same network, and reverse-proxying setup. This is not a demonstrated bypass of the supplied Compose deployment.

### Low: README contradicts implemented authentication

`README.md:95` says built-in public application authentication is unsupported, while the app implements optional per-service HTTP Basic authentication. Document supported Basic authentication, lack of per-user/RBAC/SSO features, credential handling, and relevant limitations rather than claiming there is no authentication feature.

## Positive observations

- Public listeners route only configured services and do not dispatch management handlers.
- Private management verifies Host and WhoIs identity; tagged nodes are rejected.
- Mutations require same-origin JSON/custom headers; settings and routes have revision checks.
- Persistent sealing prevents local management reopening on restart/outage.
- Upstreams are HTTP(S) origins, resolved against known peers and dialed by peer IP. No arbitrary DNS resolution/subnet destination fallback was found.
- TLS verification is enabled for HTTPS upstreams; forwarding/Tailscale identity headers are sanitized.
- Password verifiers are hashed and redacted; local sessions are random and revoked on password changes.
- Docker uses a non-root UID, dropped capabilities, no-new-privileges, read-only root filesystem, and private state directory.
- Streaming, connection admission, activity retention, health probes, and external lookup work have explicit bounds.

## Validation performed

- Go vet: passed.
- Go tests: all four packages passed. Statement coverage: command 29.8%, gateway 81.4%, setup 77.5%, UI 52.8%.
- Go race tests: blocked; environment has no C compiler. Not counted as passed.
- Go vulnerability scan: failed on GO-2026-6865.
- Native build: passed.
- Native HTTP smoke: passed startup, unauthenticated denial, Host/Origin rejection, first claim, authenticated status, logout/session revocation, and public management isolation. Temporary state was outside the repository; no real tailnet enrollment was performed.
- Frontend: 46 tests across five files passed; formatting, typecheck, production build, and full npm audit passed with zero reported vulnerabilities. Rebuilt embedded assets matched tracked files.
- Both Compose files: syntax validation passed.
- Actual Docker build/run, image vulnerability scan, and architecture runtime checks: not performed; Docker daemon access denied.
- Interactive browser check was blocked by the browser panel rewriting Host, which the setup handler correctly rejected. No successful browser end-to-end check is claimed.

## Remaining release gate

Update the vulnerable dependency, repair and automate runtime smoke testing, rerun race tests with a compiler, scan the built image, and perform real-tailnet enrollment/ACL-denial, HTTPS issuance, WebSocket, logout/reauthentication, recovery, and container restart/persistence checks. Preserve a single writer per state volume; do not market this release as HA, a WAF, or DDoS protection. Document backup/restore and host-based administrator recovery commands.

## Remediation addendum

- First setup now requires administrator login names or explicit warned UI opt-in to open access. Existing empty allowlists/API behavior are preserved; production administrators were not silently changed.
- Added a direct production-dialer regression test: known peer names/IPs dial only the resolved peer IP; loopback, metadata, subnet, unknown peer IPs, and unrelated names never reach the tsnet dialer.
- Documentation now covers Basic authentication, least-privilege policy, recovery, backups, and deployment/privacy limitations. Backup commands use Compose's real mounted volume, not an unprefixed assumed volume name.
- Production read-only checks confirmed claimed/sealed setup, unauthenticated status rejection, Host rejection, and public HTTP management isolation. Production has an empty admin allowlist.
- The deployment file at `/home/williamanderson/ts-egress-deploy/compose.yaml` was corrected to bind setup to `127.0.0.1`; the running production container has not been recreated, so the change is not live yet.
- Image scan of the original production image found an additional fixed HIGH advisory in `golang.org/x/net v0.60.0` (CVE-2026-78661, fixed in v0.62.0). Dependency remediation includes this finding.
- Frontend consolidation: all 47 tests, formatting, typecheck, and production build passed.
- Consolidated Go vet, all Go tests, and compiler-enabled race tests passed. `govulncheck` reports zero called-symbol vulnerabilities (one non-called required-module advisory remains informational).
- Linux arm64 cross-build passed; no arm64 runtime was available for execution.
- Docker candidate image build passed; Trivy found zero fixed HIGH/CRITICAL vulnerabilities in the candidate OS and Go binary. This is a severity-filtered scan, not a claim that every severity is clear.
- Isolated Docker smoke passed claim/login, unauthenticated denial, authenticated status, public management isolation, restart/persisted claim, session invalidation, and login after restart. The test now rediscovers ephemeral host ports after restart. Docker recovery command and state directory permissions also passed on a disposable volume.
- Non-root image backup/archive and restore commands passed using disposable data/backup/restore volumes with the documented ownership requirements.
- Live tailnet identity-changing or outage-producing tests require a maintenance window and were not performed.
