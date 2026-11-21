# deployment

See [persistent local volumes](volumes.md) for single-writer replacement, durable 503 routing, retained data and explicit reuse after destroy.

Implemented: Dockerfile-built or prebuilt-image public/private services per app, remote Buildx builds, same-app DNS, candidate readiness for public services, stop/start private replacement, Caddy routing with automatic HTTPS, captured-input recovery, status, bounded logs, [explicit cleanup/destruction](app-lifecycle.md), and [staged runtime secrets](secrets.md). See [multi-service deployment](multi-service.md) for ordering, partial success, DNS handoff, and compatibility.

The binding-aware partial-manifest and service-removal implementation is described
in [independent services](services.md), including the live acceptance evidence.

[App inventory, initialization, streaming logs, exec and console](cli.md) are implemented. Exec/console target existing containers, separately from recoverable one-off `run` commands.

Not implemented: backup/restore automation, public/private kind changes, or automatic garbage collection. Unsupported configurations fail explicitly, not through partial application. Apps have separate networks and merged Caddy routes.

## Roll back captured code/config

[Rollback](rollback.md) previews eligible services with `vamos rollback`; apply with
`vamos rollback --yes`, optionally scoped with `--service NAME`. It restores captured
local images, configuration and secret versions without build, pull or source.
Manual services are excluded even when scoped. Volume-backed automatic services
require explicit selection and `--allow-downtime --yes`; data is never restored.
Pending routing transitions require resume first. See the rollback contract for
partial-deploy targets, exclusions, missing inputs and recovery.

## Deploy an app

The host must already be bootstrapped and registered with the real base domain. Point wildcard DNS at its address and allow inbound TCP 80/443 through host/cloud firewalls. Vamos does not change DNS or cloud firewall policy. The optional [managed host firewall](firewall.md) is configured through `server add --firewall=true` or `server firewall enable --yes`, not app deployment. Server administration requires root or passwordless-sudo SSH access; normal app commands use the restricted bridge.

```sh
mise run build
./bin/vamos deploy -f examples/hello/vamos.yaml
./bin/vamos status -f examples/hello/vamos.yaml
./bin/vamos logs -f examples/hello/vamos.yaml --tail 100
```

The runnable `examples/hello` app uses nginx to serve a static page and `/healthz`. Its generated hostname is `hello.<base-domain>`, for example `hello.pastime.gg`. A successful deployment proves loopback readiness and a verified Caddy route switch, **not public DNS resolution or certificate issuance**. Check the public HTTPS URL separately; Caddy acquires certificates asynchronously.

A minimal configuration:

```yaml
app: hello
services:
  web:
    build:
      context: .
      dockerfile: Dockerfile
    http:
      port: 80
      health: /healthz
```

Literal `env` values and a `command` argument array are supported. An image's default CMD/ENTRYPOINT are retained unless CMD is explicitly replaced. An image-declared `VOLUME` is allowed only when it exactly matches the service’s single configured owned named volume. Extra or anonymous mounts are rejected. One-off runs reject all image-declared volumes and require a volume-free client image. The app must listen on `0.0.0.0` inside its container.

### Target selection

Without `--server`, a project's first contacting command binds the registered default in private `.vamos/server.json`. A nested `.vamos/.gitignore` keeps metadata out of Git. Later commands use that binding, not a newly changed default. Missing registrations, implicit app renames, and changed bound target addresses fail rather than silently selecting another host.

`--server NAME` overrides only that invocation and does not create/change the normal project target binding. Separate service identity acknowledgments are saved for that explicit target/app; existing services require [explicit binding](services.md#explicit-identity-bindings). Outside a project, use:

```sh
./bin/vamos status --server droplet --app hello --json
./bin/vamos logs --server droplet --app hello --tail 50
```

Flags precede positional arguments, except the service wrapper accepts `service bind|remove NAME` before flags. `--app` is for commands that do not require local deployment inputs (status/logs/resume/cleanup/destroy/rollback, service bind/remove and secrets), and requires `--server`. Status reports the journal phase and live container-running state separately; `ready` describes the last operation, not a guarantee that the app has remained healthy forever. `host-operation-active` describes the shared host mutation lock, not necessarily an operation for this app.

## Build and runtime boundaries

- No local Docker daemon or registry is required. The client sends a versioned build bundle and matching Linux executor over strict OpenSSH; Buildx runs on the host.
- The complete bundle is limited to **32 MiB including all tar payloads and manifest overhead**. Identical normalized build specifications are captured and built once per deployment. Docker ignore patterns use Moby's parser/matcher, including Dockerfile-specific ignore-file precedence and negations. Dockerfile/selected ignore file are included for the builder. `.git` and `.vamos` are always excluded, even if an ignore rule attempts to reinclude them.
- Symlinks cannot escape the context; absolute, dangling, or reserved-directory links are refused. Special files are refused. Directory symlinks are archived, not traversed. Do not edit the tree during capture; archiving is not an atomic filesystem snapshot.
- Sensitive files such as `.env` are **not automatically detected**. Exclude them explicitly. Source archives are retained privately on the host for recovery.
- Docker commands use argument arrays, bounded subprocess output, and structured JSON inspection. No app value becomes shell source. This slice uses the existing CLI boundary rather than adding a second Docker SDK dependency surface.
- **Builds use an isolated BuildKit container and guarded dedicated network**, including Dockerfile `RUN` and daemon-side source fetches. Dependency downloads remain available; direct access to host-local services and Vamos application bridges is blocked. No Docker restart or change to the disabled default bridge is required. See [build networking](builds.md) for ownership, cache, authentication and firewall limits. Build inputs remain trusted; this is not an untrusted-build sandbox.
- Each app gets a labeled user-defined bridge network and service-name DNS. Each public service's only published port is selected on `127.0.0.1` and recorded before container creation; Caddy proxies to that stable endpoint. Private services publish no ports. Containers retain outbound internet access. See the [DNS and isolation boundary](multi-service.md#dns-and-isolation).
- Containers use `unless-stopped` and the host's bounded `local` Docker log driver. Superseded containers have restart disabled before being stopped with a 30-second grace period. A short drain interval precedes public-service retirement; long-lived requests may be interrupted. Private services stop before their replacements start.
- Deployment retains images, stopped prior containers, and captured contexts. Explicit `cleanup` preserves effective versions and current/previous completed app snapshots while removing safely identified older resources; it does not run a broad prune. Monitor disk usage; automatic retention/garbage collection remains a later feature.

## Image sources and protected scoped deployment

Each service specifies exactly one of `build` or `image`. Required image references
are pulled and resolved to immutable image identity for captured recovery, rather
than relying on a mutable tag on resume.

`update: manual` creates a missing service on ordinary deploy, but an existing
manual service is never refreshed or restarted by that deploy. Protected config,
policy or staged-secret changes fail preflight. Follow the suggested scoped preview (it keeps your server and manifest); only run the preview's approval command if that target and its downtime are intended. `--preview --service NAME` requires an existing protected update: a missing or now-automatic service will not be deployed. Like the ordinary protected-service preflight, the preview submits the selected build context and may create a coordinator queue record. It uses the deploy wait deadline (25 minutes by default), not the short status-query deadline. It does not create a resumable deployment. Older agents without this capability refuse the request; schedule a host upgrade rather than removing the guard. Without `--preview`, selecting an existing manual service without `--yes` previews, while selecting an undeployed service may create it. `--yes` requires `--service`. Initial selected database
creation needs no approval. A normal whole-app deploy can still archive the build
context of a manually built protected service; protection is not a promise to
skip local source capture.

Scoped deploy validates the file but captures only selected builds and selects
only required selected secrets. Unselected services retain their actual effective
configuration and versions, not pending local edits. No dependencies start
implicitly. See [PostgreSQL](postgres.md) and the [Ledger recipe](../examples/ledger/README.md).

## Release commands

Use a top-level [`release` directive](release-commands.md) to run a captured
migration command once before any selected service runtime changes. The runner
uses its new image, environment, pinned secrets and resource limits without
ports, aliases or application volumes. Failed or uncertain executions block the
rollout and are never automatically restarted. Dependencies must already exist;
rollback neither runs nor undoes migrations.

## One-off commands

```sh
vamos run --service web -- ./bin/migrate
vamos status --run OP
vamos logs --run OP
```

`run` builds/pulls the selected current-project image and captures its configured
environment and latest selected staged secrets. It uses the existing app network,
not a running web container, replaces CMD and retains ENTRYPOINT. It publishes no
ports, mounts no volumes, attaches no service aliases and starts no dependencies.
It does not change the active release or running services' secret pins.

This milestone is noninteractive: **no stdin, TTY or implicit shell**. Output is
bounded to 8 KiB from the last 100 log lines. The worker deadline is 20 minutes;
the client defaults to 25 minutes. Disconnect or timeout does not stop the
container. There is no automated retry, cancel or attach API. Known command exit
codes, including nonzero codes, are returned. Ordinary status discovers the latest
run; use its operation ID for `status --run OP` and `logs --run OP`.

Pending, running and unknown runs block mutations. A building-phase failure
allows a new explicit command. A definitively unknown start-intent outcome needs
operator host reconciliation, not an automated rerun: SQL may already have run.
Inspect results and side effects before deciding whether another command is safe.

Cleanup removes verified terminal run containers and retains the latest 32 terminal
results, pruning at most 32 older records per cleanup. The run-record limit is 256;
repeat cleanup as needed. IID replay receipts remain retained. Destroy retains
volumes and secrets. These are bounded records, not a complete audit-log service.

## Deployment and recovery

App mutations submitted through `vamos-agent` are durably queued under `/var/lib/vamos/queue/operations/` and executed inside supervised transient `vamos-op-<id>.service` systemd units (`/usr/local/lib/vamos/executor internal-agent worker`), surviving client/SSH disconnection. Hierarchical `flock` locks (`/run/lock/vamos-host.lock`, `/run/lock/vamos/host-op.lock`, `/run/lock/vamos/app-<app>.lock`, `/run/lock/vamos/ingress.lock`) isolate per-app concurrency while serializing host-wide and Caddy route changes. Read-only `status`/`logs` requests run directly via `vamos-agent` `read` RPCs (probing locks for active-operation state) without creating queue records. Ordinary app commands use the installed agent; an explicit `--executor FILE` override is not supported on this transport. Client mutation deadlines default to 25 minutes, executor work to 20 minutes, and the systemd hard limit to 21 minutes.

Before building, a private versioned journal under `/var/lib/vamos/apps/<app>/state.json` records the operation, its operation-specific source archive, and immutable secret-version references. Required secrets are resolved before build or supersede; values stay outside release history. Builds produce immutable image IDs. Every required build and image validation completes before runtime changes. Public services retain their old container and route while checking candidate HTTP readiness; private replacement has explicit downtime.

Promotion records its old/new Caddy intent durably, verifies the existing file and live admin API configuration against known app routes, validates the new configuration, and checks the candidate again. It then replaces/reloads Caddy and verifies the actual configuration before retiring the old container. Manual file/runtime drift blocks promotion; Vamos does not take over unrelated Caddy configuration. On a proven reload failure with the runtime still old, the old durable file is restored. An uncertain reload remains explicitly pending for reconciliation.

```sh
# First inspect what actually happened after an error or disconnect.
./bin/vamos status --server droplet --app hello

# Resume the exact captured context/config/image/secret versions, independent of current local files.
./bin/vamos resume --server droplet --app hello OPERATION_ID

# If a build or pre-promotion candidate needs corrected inputs:
./bin/vamos deploy -f examples/hello/vamos.yaml --supersede OPERATION_ID
```

A submission rejected by the agent has not started a deployment; check the app and access grant rather than trying to resume the request. After a lost connection, inspect the selected server/app before deciding whether an operation was recorded.

A new deploy refuses an incomplete operation unless `--supersede` names it exactly. Supersede preserves effective services, including any already-promoted services from a partially successful operation, and removes only verified unserved candidates. Retired private containers are never restarted implicitly. It never supersedes an active worker, uncertain or already-promoted routing, or public drain. A pending promotion whose recorded intent matches the journals can be cancelled only after both the durable Caddyfile and live configuration are proven old, distinct from the new routes; verified candidate removal precedes clearing intent. This also supports corrected single-web supersede for legacy version-1 journals. A disk-before-reload interruption requires resume to reconcile the durable file first. Repeat supersede if cancellation is interrupted; this is not rollback of an effective service. Old captured archives remain intact, so interruption while selecting new inputs cannot silently change what an old operation resumes.

Resume discovers a candidate whose creation succeeded before a lost response and can start that same verified stopped container. It refuses foreign resources and reused operation identities. A missing candidate with explicit port intent can be recreated only with the same port. Legacy dynamically allocated endpoints still refuse recreation after their port was recorded; no silent route substitution is allowed. An interrupted initial capture before the first journal exists can leave an unjournaled app directory; automatic adoption is refused. Diagnose it rather than deleting arbitrary state.

The journal is not an OS transaction or universal rollback mechanism. If a promoted candidate later fails, status reports the live condition; restoring previous code/routes automatically is not implemented. Captured images still depend on Docker/host storage remaining intact. A build resumed before an image ID was captured may resolve mutable base-image tags again; pin base-image digests when reproducible rebuilds matter.

Host teardown deliberately refuses active/incomplete app state or Docker workloads. Only strict, matching-host completed-destruction receipts with no extra app files can be purged; see [app lifecycle](app-lifecycle.md). Re-running `server add` after deploying apps is not an app operation and may reject the app-managed Caddyfile as differing from the bootstrap-only configuration. Use app status/deploy commands; no broad relaxation of host ownership guards was added.

## Logs

`vamos logs --follow` streams the selected container's initial tail and subsequent
output without snapshot byte limits. It never silently follows replacements;
`-f` still means the configuration path. See [log following](cli.md#follow-service-logs)
for timeout, candidate and disconnect behavior. `--run` diagnostics remain snapshots.

`vamos logs` reads a bounded snapshot from the current release, not an unbounded stream. Use `--service NAME` to select a service and `--candidate` to inspect a pending replacement. Without selection, logs choose `web`, or the only service; ambiguous apps require an explicit name. The output identifies the selected container. Line count defaults to 100 (maximum 1000); byte limits may truncate further. Output includes timestamps. Failed build diagnostics are bounded tails in the journal and error response. Logs can contain application-controlled data or secrets; Vamos does not redact arbitrary application output.

### Persisted build failures

Deploy build failures retain a diagnostic tail of up to **4096 bytes** in the
current operation's `last_error`. A failed one-off build retains up to **1024
bytes** in its existing run record; these remain readable even when no container
was ever created:

```sh
vamos status --server local --app shop --json
vamos status --server local --app shop --run OP --json
vamos logs --server local --app shop --run OP
```

A failed one-off build reports `not-started`, not a fabricated command exit code.
Reading its diagnostics never reruns it. These are bounded failure tails, not a
successful-build log archive or an unlimited stream, and existing run cleanup
retention applies.

Exact known app-secret values (including retained versions) are removed before
persistence and final truncation. If the required secret store is unavailable,
output is withheld. A partial first line at the subprocess capture boundary is
also discarded to avoid retaining a cut-through secret. Arbitrary, transformed,
unknown or other-app secrets cannot be reliably identified; inspect logs before
sharing them. Runtime container logs are unchanged. See the
[secret exposure boundary](secrets.md#storage-and-exposure-boundary).

## Verification

Historical evidence below predates storage lifecycle acceptance unless explicitly noted. See [persistent volumes](volumes.md) for the storage boundary; no new live/full-suite results are implied.

Offline tests exercise Docker ignore behavior, safe/limited capture, protocol/transport boundaries, project bindings, candidate replacement ordering, preservation of the live app on build/readiness failures, run-side-effect recovery, supersede matching/captured archives, journal validation, Caddy reload recovery/drift refusal, and bounded diagnostics. Docker/Caddy/systemd boundaries are mocked in these tests.

### Observed real-host results (original single-service milestone)

The macOS client tested the disposable Ubuntu 26.04 amd64 droplet as root, using Docker 29.1.3/Buildx 0.30.1 and Caddy 2.6.2:

- The empty host was safely torn down/rebootstrapped from placeholder `apps.vamos.test` to `pastime.gg`, retaining the local server name `droplet`.
- `hello.pastime.gg` resolved to the droplet. First deployment built the example remotely and reached loopback readiness; Caddy obtained a Let's Encrypt certificate. A public TLS-verifying curl returned HTTP/2 200 and `/healthz` returned `ok`.
- Docker inspection showed an owned `vamos-hello` bridge network and a single `127.0.0.1:<allocated-port>` mapping. Caddy alone exposed HTTP/HTTPS; its admin API remained on loopback.
- A changed-page deployment served the new content. The previous container was stopped with restart policy `no`, while the new container had `unless-stopped`.
- Docker 29's object-valued `Platform` inventory field exposed an overly strict decoder. A recorded-output regression failed before correction; the next replacement passed.
- A Dockerfile with `RUN` exposed the bootstrap-disabled default bridge. Host-network builds fixed that integration; a resumed captured build then executed its intentionally failing command and returned the expected exit-code-23 diagnostic. The old public page remained byte-for-byte unchanged.
- Explicit supersede accepted new inputs containing a successful `RUN` but a deliberately failing health path. A **10-second client timeout** reported an unknown outcome; the worker ran for 31 seconds and persisted the readiness timeout before exiting when writing to the disconnected client. The old page still served unchanged, and status showed the current container running alongside the incomplete operation.
- Superseding that failed candidate with the normal example succeeded. Public HTML matched the checked-in `examples/hello/index.html` exactly, and `/healthz` returned `ok`. The failed candidate was removed; previous successful containers/images/archives were intentionally retained.
- CLI status and bounded container logs worked over the installed executor throughout the tested states. Resuming the final ready operation was idempotent. No staging directories remained under `/run`.

`mise run check` passed locally, including race-enabled tests and Linux amd64/arm64 builds. Windows test binaries cross-compiled. A 1,000-file context archive also passed with garbage collection disabled and a **64-file-descriptor limit**.

Native Windows/Linux client runs, arm64 execution, forced power-loss recovery, and all possible external Docker/Caddy edits still need separate verification; cross-compilation is not native execution. Subsequent two-app routing, container/Docker restart, and full host reboot tests are recorded in [app lifecycle](app-lifecycle.md#verification). Client timeout/recovery was tested, not a forced kill of the remote worker during Caddy promotion.

## Custom domains

Public services accept `http.domains` (up to 32 across the app).
Edit YAML and use normal `vamos deploy`, optionally scoped with `--service`;
there is no route-only `vamos domains` command. Generated URLs remain available.
Alias replacement follows service readiness and atomic Caddy routing, not an
app-wide transaction. Use explicit `vamos status` after completion for DNS and
local certificate observations; deploy output is not HTTPS readiness evidence.
See [custom domains](domains.md) for workflow, ownership and probe limits.
