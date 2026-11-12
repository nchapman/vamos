# First stateless deployment slice

Implemented: one Dockerfile-built public `web` service per app, remote Buildx builds, candidate readiness, Caddy routing with automatic HTTPS, repeat deployment, captured-input resume, explicit pre-promotion supersede, status, and bounded logs.

Not implemented: multiple services, prebuilt images, custom domain aliases, volumes, secrets, manual update policies, app destruction, rollback, streaming logs, `init`, or automatic image/context/container garbage collection. These configurations fail explicitly; they are not partially applied. Multiple single-service apps can share a host, with separate networks and merged Caddy routes.

## Deploy an app

The host must already be bootstrapped and registered with the real base domain. Point wildcard DNS at its address and allow inbound TCP 80/443 through host/cloud firewalls. Soar does not change DNS or firewall policy. The current commands require root SSH access.

```sh
mise run build
./bin/soar deploy -f examples/hello/soar.yaml
./bin/soar status -f examples/hello/soar.yaml
./bin/soar logs -f examples/hello/soar.yaml --tail 100
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

Literal `env` values and a `command` argument array are supported. An image's default CMD/ENTRYPOINT are retained unless CMD is explicitly replaced. Image-declared volumes are refused, even if the YAML has no volumes, because this slice does not support stateful replacement. The app must listen on `0.0.0.0` inside its container.

### Target selection

Without `--server`, a project's first contacting command binds the registered default in private `.soar/server.json`. A nested `.soar/.gitignore` keeps metadata out of Git. Later commands use that binding, not a newly changed default. Missing registrations, implicit app renames, and changed bound target addresses fail rather than silently selecting another host.

`--server NAME` overrides only that invocation and does not create/change the project binding. Outside a project, use:

```sh
./bin/soar status --server droplet --app hello --json
./bin/soar logs --server droplet --app hello --tail 50
```

Flags precede positional arguments. `--app` is only for status/logs/resume and requires `--server`. Status reports the journal phase and live container-running state separately; `ready` describes the last operation, not a guarantee that the app has remained healthy forever. `host-operation-active` describes the shared host mutation lock, not necessarily an operation for this app.

## Build and runtime boundaries

- No local Docker daemon or registry is required. The client sends a tar context and matching Linux executor over strict OpenSSH; Buildx runs on the host.
- Contexts are limited to **32 MiB including tar overhead**. Docker ignore patterns use Moby's parser/matcher, including Dockerfile-specific ignore-file precedence and negations. Dockerfile/selected ignore file are included for the builder. `.git` and `.soar` are always excluded, even if an ignore rule attempts to reinclude them.
- Symlinks cannot escape the context; absolute, dangling, or reserved-directory links are refused. Special files are refused. Directory symlinks are archived, not traversed. Do not edit the tree during capture; archiving is not an atomic filesystem snapshot.
- Sensitive files such as `.env` are **not automatically detected**. Exclude them explicitly. Source archives are retained privately on the host for recovery.
- Docker commands use argument arrays, bounded subprocess output, and structured JSON inspection. No app value becomes shell source. This slice uses the existing CLI boundary rather than adding a second Docker SDK dependency surface.
- **Build commands use host networking** because bootstrap disables Docker's shared default bridge. This supports Dockerfile `RUN` instructions and dependency downloads, but builds can reach host-local services. Only build trusted Dockerfiles; this is not an untrusted-build sandbox.
- Each app gets a labeled user-defined bridge network. The only published service port is allocated on `127.0.0.1`; Caddy proxies to it. Containers retain outbound internet access. Private service aliases and multi-service networking are deferred.
- Containers use `unless-stopped` and the host's bounded `local` Docker log driver. Superseded containers have restart disabled before being stopped with a 30-second grace period. A short drain interval precedes that stop; long-lived requests may be interrupted.
- The implementation retains images, stopped prior containers, and captured contexts. It does not run a broad prune. Monitor disk usage; automatic retention/garbage collection is a later feature.

## Deployment and recovery

A transient `soar-host-operation.service` worker survives client disconnection, as with host lifecycle operations. App mutations share the host kernel lock; the first slice serializes them globally. Read-only status/logs use the installed executor and remain available while a worker runs. Client mutation deadlines default to 25 minutes, executor work to 20 minutes, and the systemd hard limit to 21 minutes.

Before building, a private versioned journal under `/var/lib/soar/apps/<app>/state.json` records the operation and its operation-specific source archive. Builds produce immutable image IDs. The old container and route remain unchanged while building and checking candidate HTTP readiness.

Promotion records its old/new Caddy intent durably, verifies the existing file and live admin API configuration against known app routes, validates the new configuration, and checks the candidate again. It then replaces/reloads Caddy and verifies the actual configuration before retiring the old container. Manual file/runtime drift blocks promotion; Soar does not take over unrelated Caddy configuration. On a proven reload failure with the runtime still old, the old durable file is restored. An uncertain reload remains explicitly pending for reconciliation.

```sh
# First inspect what actually happened after an error or disconnect.
./bin/soar status --server droplet --app hello

# Resume the exact captured context/config/image, independent of current local files.
./bin/soar resume --server droplet --app hello OPERATION_ID

# If a build or pre-promotion candidate needs corrected inputs:
./bin/soar deploy -f examples/hello/soar.yaml --supersede OPERATION_ID
```

A new deploy refuses an incomplete operation unless `--supersede` names it exactly. Supersede is limited to pre-promotion building/starting states, preserves the current route/release, and removes only a verified owned failed candidate. It never supersedes an active worker or a pending promotion/drain; resume those first. Old captured archives remain intact, so interruption while selecting new inputs cannot silently change what an old operation resumes.

Resume discovers a candidate whose creation succeeded before a lost response and can start that same verified stopped container. It refuses foreign resources and reused operation identities. If a candidate disappears after its port was recorded, resume refuses before creating a replacement with a different port; this requires deliberate operator recovery, not a silent route substitution. An interrupted initial capture before the first journal exists can leave an unjournaled app directory; automatic adoption is refused. Diagnose it rather than deleting arbitrary state.

The journal is not an OS transaction or universal rollback mechanism. If a promoted candidate later fails, status reports the live condition; restoring previous code/routes automatically is not implemented. Captured images still depend on Docker/host storage remaining intact. A build resumed before an image ID was captured may resolve mutable base-image tags again; pin base-image digests when reproducible rebuilds matter.

Host teardown deliberately refuses any app state or Docker workloads. Re-running `server add` after deploying apps is not an app operation and may reject the app-managed Caddyfile as differing from the bootstrap-only configuration. Use app status/deploy commands; no broad relaxation of host ownership guards was added.

## Logs

`soar logs` reads a bounded snapshot from the current release, not an unbounded stream. Line count defaults to 100 (maximum 1000); byte limits may truncate further. Output includes timestamps. Failed build diagnostics are bounded tails in the journal and error response. Logs can contain application-controlled data or secrets; Soar does not redact arbitrary application output.

## Verification

Offline tests exercise Docker ignore behavior, safe/limited capture, protocol/transport boundaries, project bindings, candidate replacement ordering, preservation of the live app on build/readiness failures, run-side-effect recovery, supersede matching/captured archives, journal validation, Caddy reload recovery/drift refusal, and bounded diagnostics. Docker/Caddy/systemd boundaries are mocked in these tests.

### Observed real-host results

The macOS client tested the disposable Ubuntu 26.04 amd64 droplet as root, using Docker 29.1.3/Buildx 0.30.1 and Caddy 2.6.2:

- The empty host was safely torn down/rebootstrapped from placeholder `apps.soar.test` to `pastime.gg`, retaining the local server name `droplet`.
- `hello.pastime.gg` resolved to the droplet. First deployment built the example remotely and reached loopback readiness; Caddy obtained a Let's Encrypt certificate. A public TLS-verifying curl returned HTTP/2 200 and `/healthz` returned `ok`.
- Docker inspection showed an owned `soar-hello` bridge network and a single `127.0.0.1:<allocated-port>` mapping. Caddy alone exposed HTTP/HTTPS; its admin API remained on loopback.
- A changed-page deployment served the new content. The previous container was stopped with restart policy `no`, while the new container had `unless-stopped`.
- Docker 29's object-valued `Platform` inventory field exposed an overly strict decoder. A recorded-output regression failed before correction; the next replacement passed.
- A Dockerfile with `RUN` exposed the bootstrap-disabled default bridge. Host-network builds fixed that integration; a resumed captured build then executed its intentionally failing command and returned the expected exit-code-23 diagnostic. The old public page remained byte-for-byte unchanged.
- Explicit supersede accepted new inputs containing a successful `RUN` but a deliberately failing health path. A **10-second client timeout** reported an unknown outcome; the worker ran for 31 seconds and persisted the readiness timeout before exiting when writing to the disconnected client. The old page still served unchanged, and status showed the current container running alongside the incomplete operation.
- Superseding that failed candidate with the normal example succeeded. Public HTML matched the checked-in `examples/hello/index.html` exactly, and `/healthz` returned `ok`. The failed candidate was removed; previous successful containers/images/archives were intentionally retained.
- CLI status and bounded container logs worked over the installed executor throughout the tested states. Resuming the final ready operation was idempotent. No staging directories remained under `/run`.

`mise run check` passed locally, including race-enabled tests and Linux amd64/arm64 builds. Windows test binaries cross-compiled. A 1,000-file context archive also passed with garbage collection disabled and a **64-file-descriptor limit**.

Native Windows/Linux client runs, arm64 execution, reboot/power-loss recovery, and all possible external Docker/Caddy edits still need separate verification; cross-compilation is not native execution. Client timeout/recovery was tested, not a forced kill of the remote worker during Caddy promotion.
