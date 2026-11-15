# multi-service apps

See [persistent local volumes](volumes.md) for single-writer replacement, durable 503 routing, retained data and explicit reuse after destroy.

Vamos supports **1–8 services per app**, each built from a Dockerfile or pulled from a registry image reference. Every service joins the app's private bridge network; `http` opts a service into public HTTP routing. No database/storage semantics are implied.

The runnable [private-backend example](../examples/private-backend/vamos.yaml) uses one shared build for public `web` and private `backend`. No local Docker daemon or registry is needed.

## Configuration and commands

```yaml
app: private-demo
services:
  web:
    build: {context: ., dockerfile: Dockerfile}
    command: [python, app.py, web]
    env:
      BACKEND_URL: http://backend:9000
    http: {port: 8080, health: /healthz}
  backend:
    build: {context: ., dockerfile: Dockerfile}
    command: [python, app.py, backend]
```

Public `web` gets `https://<app>.<base-domain>`; other public services get `https://<app>-<service>.<base-domain>`. Generated hostnames are reserved globally, including pending deployments. For example, `foo/bar` conflicts with `foo-bar/web`. Oversized DNS labels/hostnames fail before mutation. Private services have no generated public route or host port bindings, even if their image declares `EXPOSE`.

```sh
vamos deploy
vamos status                         # Overall operation and each effective service
vamos status --service backend
vamos logs --service backend
vamos logs --service backend --candidate
vamos resume OPERATION_ID
vamos deploy --supersede OPERATION_ID
```

Flags precede positional arguments. Deploy defaults to the local manifest selection, merged into the remote app; `deploy --service NAME` selects one service without applying other local changes. Existing `update: manual` services require scoped deployment with `--yes` to update; ordinary deploy creates missing manual services but leaves unchanged existing ones pinned. Changed protected configuration or staged secrets cause preflight refusal. Logs default to `web`, or the sole service; otherwise selection is required. `--candidate` selects a pending replacement, not the active container, and output identifies its source.

See the [PostgreSQL workflow](postgres.md) for image services, protected updates, scoped bootstrap, and noninteractive `run` commands before a web service exists. [Preview-first rollback](rollback.md) restores eligible captured service releases, preserving actual excluded/unselected services even after partial deployment. Manual services are never rolled back, including with explicit scope; excluded services with incomplete runtime recovery block rollback. [Independent service directories](services.md) support partial manifests, explicit bindings and preview-first service removal with stable service identities. Omitted services remain untouched, never implicitly deleted. Public/private kind changes remain unsupported; [exec and console](cli.md) attach to verified existing instances.

## Replica groups

Set `replicas: 2` on a service to run two instances of one captured release.
Omission and `replicas: 1` are equivalent. Counts are bounded to **1–8 per
service and 32 desired instances across the merged app**; these are accounting
bounds, not host-capacity reservations. Volume-backed services require one
instance. Zero does not stop or remove a service. Count changes respect scoped
selection and manual-service protection like other configuration changes.

All instances share one immutable image, configuration and secret-version set.
A public, volume-free replacement starts and readiness-checks the entire new
group, admits aliases and rechecks readiness, then switches the generated and
custom hostnames to the complete ordered upstream set. Only afterward does it
drain and retire every old member. Budget host capacity for both groups during
replacement. Private replacements stop and prove **all** old instances stopped
before starting any new instance; new private workers may run concurrently and
the application must support that concurrency. Volume-backed replacements retain
their existing singleton stop/start and durable public 503 behavior.

Partial creation, alias admission and retirement retain exact member identities
and port intents for resume. Rollback restores captured counts through normal
group replacement; cleanup, service removal and destruction cover every member.
Status includes desired/current/running/available counts and per-instance
observations. A partially running group is not reported as fully available.
These observations are not continuous application-health monitoring; Docker's
restart policy remains responsible for restarting containers. Same-host replicas
do not provide host availability. Same-app DNS admission for public services
remains non-atomic and can temporarily expose both generations; private worker
replacement still proves the old group stopped first.

Logs, exec and console default to the sole instance for singleton services.
For a multi-instance current or candidate group, select an explicit 1-based
ordinal; sessions pin the actual Docker ID and never follow a replacement:

```sh
vamos logs --service web --replica 2
vamos logs --service web --candidate --replica 1 --follow
vamos exec --service web --replica 2 -- printenv HOSTNAME
vamos console --service web --replica 1
```

Replica operations use journal v8 and preserve v7 service incarnations and
per-service history. Existing v1–v7 state remains readable and recoverable;
legacy scalar container names and ports are preserved, and unchanged singleton
route rendering is byte-compatible. Removing replicas does not downgrade a v8
journal. Group recovery paths have offline regression coverage, and the
[expanded runtime acceptance](testing.md#runtime-feature-acceptance) verifies
real Docker/Caddy replicas with Rails and Python apps. The historical live
results below describe the earlier singleton milestone.

## Deployment and partial success

1. Validate all configuration, transitions, secrets, hostname ownership, and supported daemon policy. Capture immutable secret references before superseding anything.
2. Capture selected distinct normalized build specifications once in a checksummed bundle. Build or pull sequentially and inspect every immutable image before runtime changes. An image-declared volume is allowed only when it exactly matches the service's owned named mount; extra or anonymous volumes are refused. Runtime command/env/secrets do not split shared builds.
3. Create missing private services first, alphabetically, with service-name DNS. Observe each process for two seconds without a restart. This is **startup observation, not functional readiness**.
4. Update public services alphabetically: start a candidate without its service alias, check HTTP readiness, admit its DNS alias, recheck readiness, switch its Caddy route, drain for two seconds, then disable old restart and stop it.
5. Replace changed existing private services alphabetically. Record retirement intent, disable the old restart policy, stop with a 30-second grace period, and prove it stopped **before** starting its replacement. Private clients experience downtime and must retry.
6. Check that selected containers are running, then record the completed app snapshot including the actual unchanged versions of skipped services. Skipped service health is reported separately. A no-op does not evict the previous meaningful completed snapshot.

Selected automatic or explicitly scoped services resolve their images from captured inputs, with BuildKit caching for builds. Unchanged existing manual services are not rebuilt or pulled. A matching effective configuration, immutable image, and secret pins avoids container replacement. There is no dependency graph or implicit ordering beyond these stages. Existing public services must tolerate old/new private versions; readiness checks that require a not-yet-updated private dependency can block promotion. Functional dependency health and retry behavior are application responsibilities.

**A deploy is not a transaction.** If two public services succeed and a private replacement fails, the public services stay promoted. The old private container stays retired; Vamos never implicitly restarts it. Status separates the overall operation, per-service transition, effective container, candidate, running state, and availability. `available` is a process/restart/retirement observation, not continuous HTTP or application-level health monitoring.

Resume uses captured contexts, images, configuration, and secret versions. It skips completed transitions, reconciles uncertain route/alias effects, and continues the remaining work. Staging newer secrets does not change those captured inputs. Secret status reports applied versions per service when effective versions differ.

Explicit supersede must preserve the captured service scope. It preserves effective mixed state and last successful snapshots, removes only verified unserved candidates, and starts a new captured operation. It cannot bypass an active worker, uncertain route promotion, or public drain. It can settle a previously recorded private retirement but cannot resurrect that worker. No completed snapshot is recorded for a partially successful operation.

A pending public promotion may be cancelled only when the recorded intent matches the journals and **both the durable Caddyfile and live configuration are proven old**, distinct from the candidate routes. Supersede then disables restart, stops, and removes the verified candidate (including its DNS endpoint) before clearing promotion intent. This is not rollback of any already-promoted service.

- New, unknown, or drifted routing remains blocked.
- For a disk-before-reload interruption, use resume to reconcile first. Resume may still fail candidate readiness; once the old file and live configuration are proven, corrected supersede can cancel the promotion.
- If cancellation is interrupted, repeat supersede against the original operation ID. After cancellation is durably recorded, resume can instead recreate the original captured candidate without its service alias.

## DNS and isolation

Services use their service names, such as `backend:9000`, within their own app network. Separate app networks do not provide cross-app DNS or direct private reachability. Containers still have outbound internet access, and other apps can use intentionally public HTTPS endpoints. This is separation for trusted applications, not a sandbox against host administrators or Docker access.

Public candidates receive **no stable service alias during initial startup**. Docker cannot atomically update an existing endpoint's aliases. After initial readiness, Vamos journals intent, disconnects/reconnects only the candidate with its alias, verifies the same pinned loopback port, and rechecks readiness before Caddy promotion. This can interrupt candidate dependency connections and change its private IP.

During promotion/drain, DNS can return both old and new versions. This overlap is normally brief, but a failed or interrupted promotion can prolong it until resume or a permitted supersede settles the operation. Alias attachment precedes the second readiness sample, so this is **not atomic or continuously readiness-gated DNS**. A failed post-attachment readiness check attempts to withdraw the candidate endpoint; failed withdrawal remains explicit, not a claimed rollback. Resume recognizes a detached candidate only in the recorded aliasing transition. Clients must tolerate DNS caching, reconnects, and interrupted connections.

The supported host uses Vamos's Docker-managed bridge/NAT configuration. Deployment rejects changed daemon configuration and unsupported app-network options, including routed/unprotected gateways, custom direct-routing interfaces, and IPv6 app networks. It does not rewrite firewall rules or restart Docker to repair drift. Arbitrary manual daemon/firewall changes are unsupported; runtime ownership checks are not a complete firewall audit.

## Compatibility, retention, and limits

- Existing version-1 `web` deployments remain readable and resumable. A new client upgrades a **ready** legacy journal on deployment, preserving actual resource identities and the old route until replacement succeeds. Finish an incomplete legacy deployment with resume, or supersede it using a corrected single-public-`web` configuration first. That recovery remains version 1; the next ordinary deployment upgrades it. Merely querying/uploading the executor never restarts hello or adds aliases to its existing endpoint.
- Version-2 journals keep each effective service configuration separately from the latest desired app configuration. Cleanup protects effective slots, pending work, and current/previous completed app snapshots. Shared operation IDs are not treated as container identities. Cleanup/destruction continue using exact ownership proofs; no prefix-based deletion or broad prune is introduced.
- Version-4 journals capture registry image identities, protected-service skips and explicit deployment scope. Version-1/2/3 recovery stays supported; older journal versions do not acquire these new fields implicitly.
- Destruction removes all app public routes before containers/network. Secret versions remain retained. See [app lifecycle](app-lifecycle.md) and [secrets](secrets.md).
- The **32 MiB aggregate bundle limit** includes all tar payloads and manifest overhead. Hash/length/entry validation happens before runtime mutation; a journal checksum detects changed saved bundles on resume. Docker ignore semantics are unchanged.
- At most **64 secret environment mappings across the merged app configuration**; existing per-value/store limits remain. Independent manifests share this bound, the 8-service bound and the 32-alias app bound. Metadata-only SSH responses remain capped at **64 KiB**: oversized aggregate responses fail with complete, actionable JSON, not truncated JSON. Query `status --service NAME` for large mixed states; `secrets list` returns retained metadata separately.
- Local Docker inspection remains bounded at 8 MiB per stream. Mutations retain the existing global host lock and worker deadlines. This milestone does not add a scheduler or background control-plane daemon.

## Verification

Historical evidence below predates storage lifecycle acceptance unless explicitly noted. See [persistent volumes](volumes.md) for the storage boundary; no new live/full-suite results are implied.

Offline tests cover bundle integrity/deduplication, aggregate bounds, hostname collisions, multi-route promotion and drift, captured secrets across partial success, public alias admission/lost responses, cancellation of proven-old pending promotions (including interrupted candidate removal), refusal of uncertain cancellation, private stop-before-start/restart-loop detection, supersede, meaningful completed snapshots, legacy migration, status/log selection, cross-app resource references, and multi-service cleanup/destruction.

### Observed live results

Tested from macOS against the dedicated Ubuntu 26.04 amd64 host with Docker 29.1.3 and Caddy 2.6.2:

- Deployed public `web`/`api` and private `backend` from one deduplicated build. Both generated HTTPS routes reached the backend through service DNS; the private service had no port bindings.
- A separate private-only app could resolve neither service names nor connect to the other app's private or public container IPs. Rechecked isolation after reboot.
- Forced a private startup failure after both public services promoted. Confirmed mixed effective state, the previous completed snapshot, and the old backend stopped with restart disabled. Staged a newer secret, repaired the captured candidate's startup gate, and resumed: every service used its captured older pin and the newer version stayed pending. A subsequent deploy applied the newer version.
- Performed an orderly host reboot. Current public ports and secret pins remained unchanged, all three services recovered, and previously retired workers stayed stopped. Both functional HTTPS/backend paths and the original hello endpoint passed afterward.
- Forced a public candidate startup failure. It had no service alias, old routes remained functional, and explicit supersede recovered without replacing completed history with the failed attempt.
- Deployed the checked-in private-backend example and verified its public-to-private request. Its first immediate HTTPS attempt preceded certificate issuance; HTTPS succeeded after Caddy obtained the certificate. Deploy success does not wait for public TLS readiness.
- Cleanup retained exactly six containers: the three current and three previous completed service versions. Captured journals/archives contained no synthetic secret values; temporary staging/env files were absent.
- Destroyed all three test apps. Their routes, containers, networks, and captured artifacts were removed; only destruction receipts and synthetic secret storage remained. The original version-1 hello deployment kept its container identity and pinned port and remained healthy. Its current/previous containers were the only containers left.

### Final-readiness cancellation follow-up

The recovery change in commit `1e79e18` was subsequently exercised against real Docker/Caddy on the same host, using disposable `promotion-probe` and `promotion-peer` apps:

1. Deployed a healthy old version. Its operation was `ec2f4378cb41699fb4799e606649e301`.
2. Deployed a replacement whose health endpoint returned 200 for the first three checks (initial readiness, pre-alias check, post-alias check), then permanently returned 503. Confirmed from logs and the journal that operation `876b01df841de61fb643dff6014c0cc2` failed at **final promotion readiness**, not initial startup or alias admission. The candidate remained running with its service alias, while both Caddy's durable file and live configuration matched the recorded old routes.
3. Verified the original HTTPS endpoint still served the old version. Resume retried the captured unhealthy candidate and failed without changing that route.
4. Superseded with corrected inputs. Operation `a5410d6c6fa4c62171b92c259e096958` completed; the failed candidate was absent, only the corrected endpoint remained on the app network, and the original successful deployment remained the previous completed snapshot. The old successful container was stopped with restart disabled.
5. Deployed the separate peer app successfully, proving the promotion fence no longer blocked another app. Both public HTTPS endpoints and hello were healthy.
6. Destroyed both probes and audited the host: their routes, networks, containers, and captured artifacts were removed; only destruction receipts remained. No probe secrets were staged. Hello retained its original version-1 journal, container identity, and port; its current/previous containers were the only containers left. No temporary staging/env files remained.

The health-check count is a fixture for this implementation, not a public API contract. Uncertain/live-new route refusal and interrupted cancellation remain covered by local regressions; this follow-up did not repeat those cases remotely or perform another reboot.

Local `mise run check` passed (formatting, vet, race tests, and native/Linux amd64/arm64 builds); Windows packages also cross-compiled. Native Windows/Linux client execution, arm64 host execution, forced power loss, and exhaustive remote worker-kill injection remain separate verification work.

## Custom-domain routing and recovery

[Custom aliases](domains.md) extend each public service's generated hostname;
they never replace it or imply redirects. Server-wide ownership includes desired,
current, candidate and retiring service configurations across apps, including
custom/generated collisions. Historical snapshots alone do not reserve aliases.
Release an alias through a completed deploy before transferring it to another
service; a single cross-service move cannot bypass the old effective claim.

Alias edits use normal readiness-checked deployment with atomic Caddy route
replacement per service. For public volume replacement, durable 503 routing
covers the union of old and new aliases until promotion. No app-wide atomicity
is promised. Rollback rechecks ownership before changes and may refuse a
historical alias now owned elsewhere.

Version 6 journals support domain-capable normal deploy and rollback. Versions
1–5 remain strict, recoverable no-alias formats; v5 remains rollback-only for
no-alias apps. Removing aliases does not downgrade the journal. Binding-aware partial deployment
uses v7, preserving domain history and adding service incarnations and independent
predecessors; see [independent services](services.md).
