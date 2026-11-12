# Stateless multi-service apps

Soar supports **1–8 Dockerfile-built services per app**. Every service joins the app's private bridge network; `http` opts a service into public HTTP routing. No database/storage semantics are implied.

The runnable [private-backend example](../examples/private-backend/soar.yaml) uses one shared build for public `web` and private `backend`. No local Docker daemon or registry is needed.

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
soar deploy
soar status                         # Overall operation and each effective service
soar status --service backend
soar logs --service backend
soar logs --service backend --candidate
soar resume OPERATION_ID
soar deploy --supersede OPERATION_ID
```

Flags precede positional arguments. Deploy is **whole-app**; `--service` is currently supported only by status/logs. Logs default to `web`, or the sole service; otherwise selection is required. `--candidate` selects a pending replacement, not the active container, and output identifies its source.

Prebuilt `image`, volumes, `update: manual`, custom domains, scoped deploy, service removal, public/private kind changes, rollback, and migration/one-off commands remain unsupported. Omitting an effective service is an error, never deletion authorization.

## Deployment and partial success

1. Validate all configuration, transitions, secrets, hostname ownership, and supported daemon policy. Capture immutable secret references before superseding anything.
2. Capture each distinct normalized build specification once in a checksummed bundle. Build sequentially and inspect every immutable image before runtime changes; image-declared volumes are refused. Runtime command/env/secrets do not split shared builds.
3. Create missing private services first, alphabetically, with service-name DNS. Observe each process for two seconds without a restart. This is **startup observation, not functional readiness**.
4. Update public services alphabetically: start a candidate without its service alias, check HTTP readiness, admit its DNS alias, recheck readiness, switch its Caddy route, drain for two seconds, then disable old restart and stop it.
5. Replace changed existing private services alphabetically. Record retirement intent, disable the old restart policy, stop with a 30-second grace period, and prove it stopped **before** starting its replacement. Private clients experience downtime and must retry.
6. Check that effective containers are running, then record the completed app snapshot. A no-op does not evict the previous meaningful completed snapshot.

All services are rebuilt from captured inputs, with BuildKit caching. A matching effective configuration, immutable image, and secret pins avoids container replacement. There is no dependency graph or implicit ordering beyond these stages. Existing public services must tolerate old/new private versions; readiness checks that require a not-yet-updated private dependency can block promotion. Functional dependency health and retry behavior are application responsibilities.

**A deploy is not a transaction.** If two public services succeed and a private replacement fails, the public services stay promoted. The old private container stays retired; Soar never implicitly restarts it. Status separates the overall operation, per-service transition, effective container, candidate, running state, and availability. `available` is a process/restart/retirement observation, not continuous HTTP or application-level health monitoring.

Resume uses captured contexts, images, configuration, and secret versions. It skips completed transitions, reconciles uncertain route/alias effects, and continues the remaining work. Staging newer secrets does not change those captured inputs. Secret status reports applied versions per service when effective versions differ.

Explicit supersede preserves effective mixed state and last successful snapshots, removes only verified unserved candidates, and starts a new captured operation. It cannot bypass an active worker, uncertain route promotion, or public drain. It can settle a previously recorded private retirement but cannot resurrect that worker. No completed snapshot is recorded for a partially successful operation.

A pending public promotion may be cancelled only when the recorded intent matches the journals and **both the durable Caddyfile and live configuration are proven old**, distinct from the candidate routes. Supersede then disables restart, stops, and removes the verified candidate (including its DNS endpoint) before clearing promotion intent. This is not rollback of any already-promoted service.

- New, unknown, or drifted routing remains blocked.
- For a disk-before-reload interruption, use resume to reconcile first. Resume may still fail candidate readiness; once the old file and live configuration are proven, corrected supersede can cancel the promotion.
- If cancellation is interrupted, repeat supersede against the original operation ID. After cancellation is durably recorded, resume can instead recreate the original captured candidate without its service alias.

## DNS and isolation

Services use their service names, such as `backend:9000`, within their own app network. Separate app networks do not provide cross-app DNS or direct private reachability. Containers still have outbound internet access, and other apps can use intentionally public HTTPS endpoints. This is separation for trusted applications, not a sandbox against host administrators or Docker access.

Public candidates receive **no stable service alias during initial startup**. Docker cannot atomically update an existing endpoint's aliases. After initial readiness, Soar journals intent, disconnects/reconnects only the candidate with its alias, verifies the same pinned loopback port, and rechecks readiness before Caddy promotion. This can interrupt candidate dependency connections and change its private IP.

During promotion/drain, DNS can return both old and new versions. This overlap is normally brief, but a failed or interrupted promotion can prolong it until resume or a permitted supersede settles the operation. Alias attachment precedes the second readiness sample, so this is **not atomic or continuously readiness-gated DNS**. A failed post-attachment readiness check attempts to withdraw the candidate endpoint; failed withdrawal remains explicit, not a claimed rollback. Resume recognizes a detached candidate only in the recorded aliasing transition. Clients must tolerate DNS caching, reconnects, and interrupted connections.

The supported host uses Soar's Docker-managed bridge/NAT configuration. Deployment rejects changed daemon configuration and unsupported app-network options, including routed/unprotected gateways, custom direct-routing interfaces, and IPv6 app networks. It does not rewrite firewall rules or restart Docker to repair drift. Arbitrary manual daemon/firewall changes are unsupported; runtime ownership checks are not a complete firewall audit.

## Compatibility, retention, and limits

- Existing version-1 `web` deployments remain readable and resumable. A new client upgrades a **ready** legacy journal on deployment, preserving actual resource identities and the old route until replacement succeeds. Finish an incomplete legacy deployment with resume, or supersede it using a corrected single-public-`web` configuration first. That recovery remains version 1; the next ordinary deployment upgrades it. Merely querying/uploading the executor never restarts hello or adds aliases to its existing endpoint.
- Version-2 journals keep each effective service configuration separately from the latest desired app configuration. Cleanup protects effective slots, pending work, and current/previous completed app snapshots. Shared operation IDs are not treated as container identities. Cleanup/destruction continue using exact ownership proofs; no prefix-based deletion or broad prune is introduced.
- Destruction removes all app public routes before containers/network. Secret versions remain retained. See [app lifecycle](app-lifecycle.md) and [secrets](secrets.md).
- The **32 MiB aggregate bundle limit** includes all tar payloads and manifest overhead. Hash/length/entry validation happens before runtime mutation; a journal checksum detects changed saved bundles on resume. Docker ignore semantics are unchanged.
- At most **64 secret environment mappings across the desired app configuration**; existing per-value/store limits remain. Partial operations can temporarily retain more effective mappings. Metadata-only SSH responses remain capped at **64 KiB**: oversized aggregate responses fail with complete, actionable JSON, not truncated JSON. Query `status --service NAME` for large mixed states; `secrets list` returns retained metadata separately.
- Local Docker inspection remains bounded at 8 MiB per stream. Mutations retain the existing global host lock and worker deadlines. This milestone does not add a scheduler or background control-plane daemon.

## Verification

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

Local `mise run check` passed (formatting, vet, race tests, and native/Linux amd64/arm64 builds); Windows packages also cross-compiled. Native Windows/Linux client execution, arm64 host execution, forced power loss, and exhaustive remote worker-kill injection remain separate verification work.
