# Custom domains

Custom domains are additional public-service aliases, not redirects. Every public
service retains its generated URL: `https://<app>.<base-domain>` for `web`, or
`https://<app>-<service>.<base-domain>` for other services. Application host
validation, redirects, cookies and CORS remain application responsibilities.

## Configure and deploy

Add aliases to an existing service in `soar.yaml` (this fragment assumes its
build/image is already configured):

```yaml
http:
  port: 8080
  health: /readyz
  domains:
    - notes.example.com
    - www.notes.example.com
```

- At most 8 aliases per service and 32 per app.
- Names are case-insensitive ASCII DNS hostnames; use punycode for international
  names. Labels are at most 63 characters and hostnames at most 253, with at
  least one dot. Duplicates are rejected case-insensitively.
- No wildcard, URL scheme, IP address, port, path or trailing dot is accepted.
- Private services have no public aliases; aliases belong under `http`.

Configure DNS yourself at your DNS provider, pointing the aliases to the host's
public addresses. Ensure TCP 80/443 are reachable through host/cloud firewalls.
Soar does not automate DNS. Caddy requests public certificates automatically;
issuance and DNS propagation can take time.

```sh
# After editing services.web.http.domains:
soar deploy --service web
soar status
# Or apply all eligible changes:
soar deploy
soar status --service web

# Preview captured code/config (including alias) rollback, then apply:
soar rollback --service web
soar rollback --service web --yes
soar status
```

There is no `soar domains add/list/remove` command. Edit the list and deploy to
add or remove aliases; use status to inspect them. Removing an alias removes its
route and releases its claim after completion; it does not edit DNS or revoke
certificates. Caddy continues to manage its own certificate storage. This is normal deployment,
not a routing-only operation: normal build/image, secret selection, update
protection and readiness rules apply. A scoped deploy leaves unrelated pending
service edits unapplied. See [deployment](deployment.md) and [rollback](rollback.md)
for protected services and volume downtime authorization.

## Routing, ownership and collision recovery

Caddy routing changes atomically per service after readiness, not atomically
across an app. Stateless replacement retains old routes until promotion. Public
volume-backed replacement instead installs durable HTTP 503 routes for the union
of old and new aliases (and the generated hostname), then promotes the ready
replacement. Failed replacement does not automatically restart an old writer.

Server-wide reservation checks include desired, current, candidate and retiring
configurations across apps. Custom aliases can collide with other custom aliases
or generated names; generated names also collide with one another. An alias
identical to its own service's generated hostname is deduplicated for routing.
Historical completed snapshots alone do not reserve removed aliases.

To transfer `notes.example.com` from one service to another:

1. Remove it from the old owner's YAML and deploy that service successfully.
2. Resolve incomplete operations with status/resume before proceeding; an old
   effective or retiring claim cannot be released by editing local YAML alone.
3. Add it to the new owner's YAML and deploy that service.
4. Run status and check the public endpoint externally.

Do not move the alias between services in a single deploy: release before
transfer. On a collision, inspect the named owners, correct the requested alias
or complete its release from the existing owner, then retry. Do not remove
journals or hand-edit Caddy to bypass ownership checks.

Rollback rechecks ownership before changes. If a historical alias has been
reclaimed, rollback may refuse it. Release it from the new owner first, or use a
forward deploy with corrected aliases instead of restoring that snapshot.

## Reading status

Status lists current and captured desired aliases, not unrelated local edits:

| Field | Meaning |
| --- | --- |
| `route=configured` | Alias is in effective routing. |
| `route=pending` | Desired alias is not yet effective, or promotion requires reconciliation. |
| `route=unavailable` | Service routing is unavailable during replacement/retirement. |
| `DNS=matches-host` | Every resolved address matches a public host interface IP. |
| `DNS=pending` | No successful DNS resolution yet. |
| `DNS=unverified` | Resolved addresses do not exclusively match public host interface IPs. |
| `certificate=ready` | A local TLS handshake validated the hostname and certificate chain. |
| `certificate=pending` | That local TLS verification has not succeeded. |
| `HTTPS=ready` | Both DNS matches-host and local certificate ready. |

Only an explicit `soar status` with no active operation probes configured aliases.
Deploy/rollback responses, active-operation status, and pending/unavailable routes
leave DNS/certificate/HTTPS `unknown`: use status after the operation completes.
A successful route reload alone is not certificate readiness.

TLS probes connect to **127.0.0.1:443**, pin the alias as SNI, and validate the
certificate against trusted CAs. They do not connect through public DNS or bypass
certificate validation. DNS checks require **all** resolved IPv4/IPv6 addresses
to match public host interface addresses; stale AAAA records matter too. Checks
use the deployment host's resolver, whose cached or split-horizon answers may
differ from external clients.
NAT/CDN configurations can legitimately remain `unverified` and require manual
external checks. Even `HTTPS=ready` does not prove public reachability or
application health: test the public URL and application behavior from outside
the host separately.

## Compatibility and evidence

Version 6 journals support domain-capable normal deploy and rollback. Versions
1–5 remain strict, recoverable no-alias formats; v5 is rollback-only for no-alias
apps. A domain-capable app stays v6 after alias removal to preserve its history.
See [multi-service recovery](multi-service.md).

### Live acceptance

The disposable `dprobe` app was exercised on the existing Ubuntu 26.04 amd64 host
with Docker 29.1.3 and Caddy 2.6.2, using the base domain's existing wildcard DNS:

- Deployed `dprobe-a.pastime.gg` alongside the deliberately unresolvable
  `no-such-soar-dprobe.invalid`. The generated URL and valid alias both served
  `/healthz` over externally verified HTTPS. Status reported the valid alias
  ready and the invalid alias DNS/certificate pending without failing the app.
- Attempted to claim the existing `hello.pastime.gg`. Deployment rejected the
  collision before changing the probe journal; its checksum remained identical.
  Hello's original container ID and start timestamp were unchanged.
- Replaced both aliases with `dprobe-b.pastime.gg`. The old aliases disappeared
  from routing while the generated URL remained. Status initially reported a
  pending certificate, then ready after issuance; an external HTTPS check passed.
- Previewed and applied rollback. Preview showed the alias change, rollback
  restored the exact original image and aliases, and the generated URL and valid
  alias again passed external HTTPS checks. The invalid alias remained pending.
  JSON status reported the same ready/pending distinction.
- Cleanup retained release history. Destroy removed probe routes, containers,
  network, images and captured operation files. Only hello's original containers
  remained; its HTTPS health check passed. Previously retained PostgreSQL volumes
  were not altered.

Formatting, vet, the full race-enabled Go suite, native/Linux amd64/arm64 builds,
Windows cross-compilation, both example test suites, and Markdown-link/YAML checks
passed. Review found no actionable issues. Tests additionally cover pending and
reclaimed hostname conflicts, failed readiness, interrupted Caddy reload/resume,
volume-backed union-of-aliases 503 behavior, strict journal versions, alias limits,
and status selection without adopting unrelated local edits.

Live forced-worker interruption, volume-backed alias replacement, IPv6/NAT/CDN
setups, native Windows execution and arm64 host execution were not exercised.
Backup/restore, exec and service removal remain deferred.
