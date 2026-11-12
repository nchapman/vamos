# Soar: a small app platform for one server

**Status: multi-service deployment, persistent local volumes and the PostgreSQL workflow are implemented.** Configuration validation, host inspection/setup/teardown, public/private Dockerfile-built or prebuilt-image services with same-app DNS, status/logs/resume/supersede, preview-first app cleanup/destruction, and staged runtime secrets work. Protected manual updates, scoped deployment and noninteractive one-off migration commands work. Rollback, exec, custom domains, backup/restore and other broader workflows below remain proposed. Examples below that use these deferred commands describe design targets, not available CLI APIs. See [README.md](README.md), [host lifecycle](docs/host-lifecycle.md), [deployment](docs/deployment.md), [multi-service semantics](docs/multi-service.md), [app lifecycle](docs/app-lifecycle.md), and [secrets](docs/secrets.md) for the exact boundary and verification limits.

The implemented [storage lifecycle](docs/volumes.md) supersedes the original volume proposal. It uses immutable owned local volumes, single-writer stop/start, durable public 503 routes, retained data, and explicit `--reuse-data` after destroy. Storage journals use v3; ready/incomplete v1/v2 remain recoverable. Backup/restore/preparation and data deletion are deferred. Database/manual-update/scoped-deploy and prebuilt images are implemented; custom domains and rollback remain proposed; acceptance scenarios are not claims of completed live or full-suite validation.

See [PostgreSQL](docs/postgres.md), the [runnable Ledger recipe](examples/ledger/README.md)
and [run recovery and limits](docs/deployment.md#one-off-commands) for the current contract.
Implementation completion is separate from live acceptance evidence.

## 1. The goal

Rent a good Ubuntu 24.04+ server, install Soar once, and deploy our apps from Dockerfiles. Keep Fly's app-oriented experience without building a cloud provider.

The user should think about:

- Apps, their services, and their environment variables.
- Which services need public URLs and custom domains.
- Which services need persistent data.
- Whether a deploy succeeded, and how to inspect or undo it.

They should not repeatedly configure servers, registries, reverse proxies, certificates, or container networking for each app.

**Choose boring internals: Go + SSH + Docker Engine/BuildKit + Caddy + files on disk.** Docker is an implementation choice, not the product. Do not build a runtime, scheduler, or database operator.

### Boundaries

- Client CLI targets macOS, Linux, and Windows. It must not depend on Ubuntu, APT, systemd, or local Docker; remote operations use the client's OpenSSH executable.
- Deployment hosts and the remote executor target Ubuntu 24.04 or newer with systemd, on amd64 or arm64. No support for older Ubuntu, other server distributions, or Ubuntu derivatives. Keep host-specific installation and service management out of shared client code.
- Many apps may share one server; each app is confined to one server, with one instance per declared service initially.
- Trusted applications operated by the server owner, not hostile tenants.
- Dockerfile builds and existing OCI images. No buildpacks or framework detection.
- No multi-host networking, autoscaling, scale-to-zero, high availability, dashboard, or plugin system.
- Databases and Redis are ordinary private services with explicit storage and lifecycle rules, not magically managed products.
- A failed server takes its apps offline. Backups and recovery are necessary; this design does not disguise that fact.

## 2. The experience

```sh
# Once: install prerequisites, configure the host, remember it locally.
soar server add --yes --domain apps.example.com home root@203.0.113.10

# In a project containing a Dockerfile:
soar init notes --port 8080       # Write a small soar.yaml; do not infer a framework.
soar deploy                     # Use the project's bound server; no registry configuration.
# https://notes.apps.example.com

soar apps
soar status
soar logs --service web --follow
soar rollback
```

The first added server becomes the default. `init` writes `soar.yaml` and binds the project to that server in local, gitignored `.soar/` metadata. For a hand-written configuration, the first server-contacting command creates that binding. `--server home` explicitly selects another target for that invocation; it never silently changes the binding. Resource names and configuration stay portable between machines.

Commands infer the app from `soar.yaml`. Outside a project, use `--app notes --server home`; operations requiring local source/configuration still require a project. Every mutation prints its resolved `server / app / service` target before acting. Secret staging may register an app's identity before its first deploy, without creating containers or routes.

For an app that needs a session secret, add `SESSION_SECRET: session-secret` under its service's `secrets` map, then:

```sh
soar secrets stage session-secret   # Prompt without echo; never put values in argv.
soar deploy                        # Apply staged values to changed services.
```

Before installing packages or changing host state, `server add` reads `/etc/os-release` to verify `ID=ubuntu` and a numeric `VERSION_ID` of at least 24.04, then checks that systemd is the running host service manager and the architecture is amd64 or arm64. Missing/unrecognized OS metadata or an unsupported host fails with the detected OS and the Ubuntu 24.04+ requirement. Installation uses APT and systemd directly; do not add distro-specific backends. These read-only compatibility checks exist in `server inspect` and are reused by root-only provisioning. Initial fresh-host setup and conservative teardown are implemented; see [the lifecycle boundary](docs/host-lifecycle.md) for ownership, authorization, and recovery constraints. Inspection reports root/non-root UID without invoking sudo, and does not claim that non-root privileged setup is authorized.

Setup tells the owner to point `*.apps.example.com` at the server and open TCP 80/443. Caddy obtains certificates for individual hostnames; Soar does not require wildcard certificates or a DNS-provider integration. Incorrect DNS produces an actionable diagnostic, not an indefinite wait.

SSH remains the management boundary. There is no public Soar administration API. Setup may require root; subsequent operations use a dedicated account. Docker control is effectively root access, so this is not a restricted multi-user hosting service.

### Domains are normal app configuration

```sh
# In the project: update soar.yaml and apply a routing-only change to the deployed app.
soar domains add notes.example.com --service web
soar domains add www.notes.example.com --service web
soar domains list
soar domains remove www.notes.example.com --service web
```

These commands are conveniences over `services.<name>.http.domains`. They do not rebuild or restart containers, and do not silently apply other pending local configuration changes. The next `soar deploy` also reconciles domains from the file. If applying a domain change fails, preserve working routes and report the unapplied local edit. Repeat the same add/remove command to retry only that edit; these commands are idempotent.

`domains add` prints the DNS target and distinguishes route configuration from DNS validation and certificate issuance:

```text
home / notes / web
Route configured
DNS pending: point notes.example.com at 203.0.113.10
Certificate pending
Existing URL: https://notes.apps.example.com
```

`domains list` reports DNS pending/incorrect, certificate pending/failed, or HTTPS ready. Pending custom-domain validation never blocks deployment on the generated hostname or disables working aliases. Invalid names and ownership collisions fail validation; unresolved DNS is an asynchronous status, not a reason to tear down a working app. A successful Caddy reload is not proof that HTTPS is ready.

Each public service always gets a generated hostname: `<app>.<base-domain>` for `web`, or `<app>-<service>.<base-domain>` for other services. Custom domains are additional aliases, not automatic redirects. Reserve all hostnames globally on the server, including generated names; reject collisions rather than taking another app's route. Wildcard custom domains and path-based routing are outside v1.

## 3. The small configuration model

One `soar.yaml`, one app, one private network, several optional services. An app groups collaborating services, potentially from different images, into one deployment configuration. Unlike Fly's default organization-wide private networking, unrelated Soar apps cannot connect privately.

| Field | Meaning |
| --- | --- |
| `app` | Stable app name; unique on the target server. |
| `services` | Named containers belonging to this app. Names also provide private DNS. |
| `build` | Build context directory and Dockerfile path relative to that context. |
| `image` | Existing image reference; mutually exclusive with `build`. |
| `command` | Optional argument array replacing the image's CMD, retaining its ENTRYPOINT. No implicit shell. |
| `env` | Literal, nonsecret environment variables. |
| `secrets` | Map of container environment variable names to app-scoped secret names. Missing secrets fail preflight. |
| `http` | Opt-in public HTTP endpoint: container port, readiness path, optional custom domains. |
| `volume` | Optional single app-scoped named volume with an absolute container mount path. Never a shared volume or arbitrary host path. |
| `update` | `auto` by default; `manual` protects an existing service from ordinary app deploys. Independent of storage. |

App, service, volume, and secret names are validated as simple DNS labels; environment-variable keys use normal environment-variable syntax, including uppercase letters and underscores. Reject unknown keys. Do not add templating, variable interpolation, inferred database URLs, or implicit service dependencies. Store complete connection URLs as secrets when needed; hostnames in those URLs are the service names.

App names and server defaults keep infrastructure out of the file. Secret values never appear in the file. Operational settings get conservative documented defaults before becoming configuration fields.

### Private networking

- Every service joins its app's private user-defined bridge network. `db:5432` and `redis:6379` work within that app, not between apps.
- No service publishes a host port by default. `http` exposes only HTTP through Caddy, not arbitrary TCP ports.
- For a public service, publish a randomly allocated **loopback-only** host port. Run Caddy on the host and route to that port. Caddy does not need to join every private network.
- Workers, databases, and Redis have no host port mappings. Containers still have outbound internet access; “private” means no public ingress, not an air gap.
- Soar configures and verifies isolation between its app networks. This is useful separation for trusted apps, not a defense against host administrators or a compromised Docker daemon.
- Do not support cross-app networks in v1. Put collaborating services in one app, or use an explicitly external service.
- For occasional administration, `soar exec --service db -- ...` runs a client inside a service. Do not expose a database publicly just to inspect it.

### Storage and update policy are separate

**`update: auto` (default):** normal `soar deploy` updates changed application services. Public services without volumes start as candidates, pass HTTP readiness, then receive traffic. Services with volumes use stop-old/start-new, including SQLite-backed websites: same deploy command, explicit downtime in the deployment summary. Private services, including workers, also use stop/start.

**`update: manual`:** normal deploy creates a missing service but never updates an existing one. Use this for databases and durable Redis queues. Changes to a protected service's image, command, environment, secrets, or mounts fail app-deploy preflight with the exact next command. Do not silently ignore requested changes or refresh a mutable image tag on an ordinary deploy.

```sh
soar deploy --service db --yes  # Approve an existing manual db update; omit --yes to preview.
```

`deploy --service` is also the simple bootstrap primitive: start just the database before initializing a schema. It validates the whole file's structure but requires secrets/builds only for the selected service, never starts dependencies implicitly, and never applies other services' pending changes. It records the resulting effective app state as a release; unchanged services retain their actual versions. Existing selected manual-service updates require `--yes`; without it the command previews. Newly selected database creation needs no `--yes`. Ordinary deploy may still archive a manually built protected service’s context; it does not refresh or restart that service. Protected policy changes also fail preflight with the exact scoped approval command.

**A volume always implies single-writer replacement, not manual updates.** Reject shared volume mounts across services initially. Build/pull and validate before stopping the old container. For public stop/start services, temporarily return unavailable, wait for replacement HTTP readiness, then update Caddy to its new loopback port. Record the container, route, and effective version for recovery. If startup fails, retain data and report the failed operation; never automatically start an old binary against potentially changed data.

For shutdown, honor the image's stop signal (SIGTERM by default), allow 30 seconds, then force termination. Disable the superseded container's restart policy before stopping it, and verify it has stopped before starting its replacement. Retired workers must not reappear after a reboot. This prevents deliberate old/new overlap, not duplicate queue processing.

Database version upgrades and on-disk migrations remain operator responsibilities. Removing any volume-backed or manually updated service requires `soar service remove --service db`, with confirmation and retained volumes; remove its declaration too before a later deploy. A missing declaration alone must not destroy it.

### Secrets and retained data

`secrets stage` stores a new version without restarting any service. `status` shows pending secret changes and affected services, never values. The next successful deployment of a service selects its staged versions; ordinary container restarts and host reboots keep that service's pinned versions. Staging a database initialization password does not change the database's actual password.

Secret values live in a permission-restricted server-side store, outside release history. Releases record immutable secret-version references so rollback can recreate the previous configuration without writing plaintext into release metadata. Updating a secret does not silently mutate running containers. Retain referenced versions; warn that rollback may restore revoked credentials. Host administrators and Docker access can inspect runtime secrets. Build-time secrets are outside v1, and build contexts must not contain them.

Named volumes survive deploys, rollbacks, and app deletion. `soar destroy` removes containers and routes but retains data and secrets; deleting those requires a separate explicit, confirmed action. Renaming an app or volume is not a data migration. Reject unexplained storage identity changes and point the operator to manual migration.

## 4. What needs to exist

### A thin CLI and host-side executor

Use the same small Go executable locally and remotely. Use the system SSH client for existing agent/key/config support. The first slice uses bounded Docker CLI subprocesses with argument arrays and structured JSON inspection, sharing the host-lifecycle process boundary; add an SDK only when a concrete need warrants it. Do not generate Compose files as a second deployment state model. The local CLI sends authenticated commands over SSH; the host-side executor owns mutations and reports results. Fresh-host inspection is a bootstrap exception: before Soar is installed, SSH runs a fixed read-only shell collector. The installed executor runs the identical collector locally and shares the same Go parser/policy. Its versioned JSON protocols support host lifecycle and the narrow app-deployment slice; bootstrap inspection alone does not upload or invoke a remote Go binary. No always-running custom control-plane daemon is needed initially. Docker restarts containers and Caddy restarts through the host's service manager after reboot.

Store root/private state under a directory such as `/var/lib/soar/`:

- App configuration and active release pointer.
- Release records: image IDs/digests, effective configuration, secret-version references.
- Operation journal, candidate container identities, and route targets.
- Secret store and stable volume identities.

Use atomic file replacement, per-app operation locks, and a separate short-lived lock for server-wide hostname allocation and route changes. Query Docker and Caddy to reconcile reality with recorded state after interrupted operations. Do not assume an SSH disconnect undid an operation.

### Build and transfer

Send the build context over SSH and build on the server with BuildKit. Respect Docker's `.dockerignore` semantics, including Dockerfile-specific ignore files. Exclude `.git` as an additional Soar safety rule; warn that other sensitive files must be explicitly ignored. Do not invent a slightly different ignore parser. No registry is required for locally built images. Private prebuilt images can use registry credentials configured once on the host.

Build once per distinct build specification in a deploy; web and worker services can reuse the result. Capture immutable image IDs/digests in release records rather than relying on mutable tags. Place limits on build concurrency and configure container log rotation so routine activity cannot trivially exhaust disk. Clean old build cache and unreferenced images, never retained release images or volumes.

### Deploy safely, without promising transactions

1. Validate configuration, required secrets, hostname ownership, storage rules, and update protection before mutating running services. Show the target, changes, protected services left untouched, and any downtime. For scoped deploys, operate only on the selected service.
2. Build/pull required images and finish validation of resolved image changes before touching active containers. A build failure leaves the running app alone.
3. Create missing networks/volumes and start missing private services, assigning stable private DNS immediately. This also applies when adding services to an existing app. Database initialization is asynchronous; clients retry. Data-service creation is not automatically undone on failure. For an initial schema, use the scoped-deploy and one-off-run recipe below before deploying public services.
4. Create/update public services. Without a volume, start a candidate while retaining the old container and route until HTTP readiness succeeds. With a volume, stop the old writer first, then start and check the replacement. Existing manual services remain untouched unless explicitly selected.
5. Switch ready public routes using a validated Caddy reload. For candidate deployments, keep the old container for a bounded drain interval before stopping it. Long-lived connections can be interrupted at the end of that interval. Stop/start services instead have an explicit unavailable interval.
6. Replace changed existing private services sequentially, respecting manual protection. They must stay running through a short startup observation window; this is not proof of functional readiness. Update stable private DNS during cutover.
7. Record the completed release and clean up obsolete containers, retaining the previous release's images and configuration.

While a public candidate initially starts, the stable private service name continues to resolve to the active version. After readiness, journal a candidate-only endpoint reconnect to add its alias, verify its pinned port, and check readiness again before Caddy promotion. DNS can briefly include both old/new versions during promotion and drain; alias attachment precedes the second sample, so this is not an atomic readiness-gated DNS switch. Reconnect can interrupt candidate connections, and DNS caches may outlive the change. Clients must reconnect/retry. A private service has an interruption during stop/start replacement.

An app deploy is **not an atomic transaction across containers or data**. If a later worker fails after web promotion, report a partial deployment and show each service's actual version. Retain the previous completed release and operation journal so a retry or rollback can reconcile safely. Web and worker versions must tolerate overlap.

`soar rollback` restores automatic, volume-free services from the previous completed release without rebuilding. Show all excluded services. Volume-backed application code can be restored only with explicit `soar rollback --service web --allow-downtime` confirmation and an operator decision that the old code is data-compatible. Manual services are excluded from rollback; select a vetted image/config and use scoped deploy for an intentional change. No rollback reverts volumes, external side effects, or database schemas.

### One-off commands, not migration magic

```sh
soar deploy --service db
soar run --service web -- ./bin/migrate
soar deploy
```

`run` builds/pulls the selected service image from the current project, attaches a temporary container to the existing app network, and injects that service's configured environment and latest staged secrets. It replaces CMD, retains ENTRYPOINT, publishes no port, mounts no volumes, and starts no dependencies. It requires only that service's referenced secrets; it does not require a running or healthy instance of that service. This handles PostgreSQL migrations and administrative clients that live in the app image. The image must support the supplied command.

Return the command's exit code and record the operation/result without changing the active release. `run` does not apply staged values to running services. Serialize it with other app mutations. Do not automatically retry it after interruption: schema/data side effects may already have occurred. The operator checks the result and chooses whether rerunning is safe. `run` is noninteractive in this milestone: no stdin, TTY or implicit shell. `exec` remains deferred. Runs have no service aliases and reject all image-declared volumes; use a volume-free client image. Output is bounded to 8 KiB from the last 100 log lines, with a 20-minute worker deadline and 25-minute default client deadline. Disconnect/timeout does not stop the container. Use `status --run OP` and `logs --run OP`; ordinary status discovers the latest run. Pending/running/unknown runs block mutations; building-phase failure permits a new explicit command. A definitively unknown start intent requires operator host reconciliation, not automatic rerun. There is no cancel/attach API. Cleanup removes terminal containers, keeps the latest 32 terminal results and prunes at most 32 older records per cleanup against a 256-record limit; IID replay receipts remain retained.

For SQLite, a volume-free temporary container cannot migrate the database. The SQLite example's entrypoint initializes its own data (no migrations) before starting HTTP, while single-writer stop/start guarantees exclusive access. This is explicit application behavior, not framework detection. Automatic release hooks are outside v1; use backward-compatible schemas when rollback matters.

### Failure output and recovery are part of the interface

```text
home / dispatch — operation op-42 incomplete
web: release 12, serving
worker: replacement failed; stopped
 db: unchanged
Previous completed release: 11
Resume captured deployment: soar resume op-42
Restore eligible services:  soar rollback
```

An SSH disconnect does not imply cancellation or successful completion. `status` reconciles the operation journal with live containers and routes and reports what is running, whether an executor is still active, and the next supported action. `resume <id>` waits for/acquires the operation lock, reconciles first, and continues the captured images, config, and secret versions—not the caller's current working tree. It never launches a second executor for an active operation.

A fresh `deploy` refuses to start while an incomplete deployment exists and points to resume or rollback. If the captured configuration itself needs correction, `soar deploy --supersede op-42` reconciles and closes that stopped operation, then plans from the current project and actual running state; it cannot supersede an active executor or automatically undo data changes. Failed volume-backed updates remain unavailable until explicitly resumed with the same image or superseded with a corrected build. Domain command retries stay route-only; one-off commands are never silently resumed.

### Operations that cannot be hand-waved away

- `status`: actual service versions, readiness, domains/TLS state, pending secrets, incomplete operations, and precise next actions.
- `logs`: service-specific container output, plus useful build/deploy errors.
- `exec` / `run`: argument-preserving execution in an existing / temporary container, without an implicit shell.
- Restart policies for reboot/process crashes; readiness failures are surfaced, not treated as a magical repair mechanism.
- Refuse deploys when disk space is clearly insufficient; keep the previous release intact.
- Before storing important data: document and test off-host backups and a restore onto a fresh server. Use database-native backups or stop the service for a consistent copy, not a casual tar of a live database directory. Redis used as a queue needs an explicit durability/backup decision too.
- Restoring also requires app configuration, secret material, and any necessary registry access. One local volume is not a backup.

## 5. Five configurations to test the model

These are design scenarios; journal and Ledger are runnable, while the broader custom-domain/rollback workflows remain proposed. Dockerfiles and application code must listen on `0.0.0.0` at the declared port and implement the declared readiness endpoint. Example image tags select a major release for readability; production should pin a vetted patch tag or digest. A first install resolves tags; existing `update: manual` services are not automatically refreshed. Run each walkthrough from its project directory after saving the shown `soar.yaml`. `./bin/migrate`, servers, and health endpoints are application-owned example interfaces, not generated by Soar.

### 1. A stateless website with two custom domains

```yaml
app: studio
services:
  web:
    build:
      context: .
      dockerfile: Dockerfile
    http:
      port: 3000
      health: /healthz
      domains:
        - studio.example.com
        - www.studio.example.com
```

```sh
soar deploy
soar domains add preview.studio.example.com --service web
soar domains list
# Edit code, then:
soar deploy
soar rollback
```

**Tests:** the smallest useful app; multiple hostnames; generated `studio.apps.example.com` still works even when custom DNS is pending. Adding a domain must not rebuild the image. Domain aliases do not imply redirects or solve application-level host validation. A bad build/readiness check leaves the previous route live; `status` explains whether to resume or supersede the failed deployment.

### 2. A small app with SQLite and uploaded files

This scenario is now runnable: see [journal](examples/journal/README.md). It is an
unauthenticated test demo, not a production app.

```yaml
app: journal
services:
  web:
    build:
      context: .
      dockerfile: Dockerfile
    volume:
      name: data
      mount: /data
    http:
      port: 8080
      health: /readyz
```

Ordinary `soar deploy` performs stop/start replacement. The root entrypoint only
initializes permissions on a new empty root-owned mount, then drops to numeric
UID/GID 10001 before restart-safe SQLite table creation and HTTP serving. Existing
incompatible data permissions fail; no recursive chown, migration or copying.
The image declares no `VOLUME`. SQLite and uploads share `/data`.

**Acceptance targets:** write/read markers across replacement, failed startup,
corrected supersede, reboot, cleanup, destroy and explicit `deploy --reuse-data`.
Failed readiness retains data and leaves the public service at durable 503, not
running an old binary against possibly changed data. Status exposes identities,
missing/retained state and possible mutation. Missing registered storage fails
closed before execution; this is not a continuous integrity monitor. Docker's
ordinary reference guards prevent volume removal while containers refer to it;
out-of-band filesystem corruption is not covered. Backup/restore remains deferred.
These targets are not a claim that live tests have passed.

### 3. An API with private PostgreSQL

The variant below illustrates a complete connection-URL secret. The current
[runnable Ledger](examples/ledger/README.md) instead uses literal HOST/DB/USER
and one shared `database-password`, and has no custom domains. Follow that README
for the implemented empty-host recipe.

```yaml
app: ledger
services:
  web:
    build:
      context: .
      dockerfile: Dockerfile
    secrets:
      DATABASE_URL: database-url
    http:
      port: 8080
      health: /readyz
      domains:
        - api.ledger.example.com
  db:
    image: postgres:17
    update: manual
    env:
      POSTGRES_DB: ledger
      POSTGRES_USER: ledger
    secrets:
      POSTGRES_PASSWORD: database-password
    volume:
      name: postgres
      mount: /var/lib/postgresql/data
```

```sh
soar secrets stage database-password
soar secrets stage database-url
soar deploy --service db
soar run --service web -- ./bin/migrate
soar deploy
# Later code/schema update (migration must remain compatible with the running API):
soar run --service web -- ./bin/migrate
soar deploy
```

**Tests:** web and database share a private network; `db` has no public port or URL. Enter a `database-url` of the form `postgresql://ledger:<URL-encoded-password>@db:5432/ledger`, matching `database-password`. Soar does not generate or interpolate it. The migration command retries until Postgres accepts connections and exits nonzero on failure; only proceed to deploy after success. The API readiness endpoint checks connectivity and required schema. This works on an empty server without first deploying an unhealthy API.

The standard PostgreSQL image uses these initialization variables only for an empty data directory. Changing `database-password` does not change an existing role password. For rotation with an accepted interruption, use an operator-controlled SQL client outside Soar (`exec` and interactive runs are not implemented), change the role password securely, stage matching credentials, explicitly `soar deploy --service db --yes` to align initialization configuration, then deploy the API. Do not put credentials in SQL arguments or shell history. Production role privileges and zero-interruption rotation remain operator concerns.

### 4. Web, background worker, PostgreSQL, and Redis

```yaml
app: dispatch
services:
  web:
    build:
      context: .
      dockerfile: Dockerfile
    command: ["./bin/server"]
    secrets:
      DATABASE_URL: database-url
      SESSION_SECRET: session-secret
    env:
      REDIS_URL: redis://redis:6379/0
    http:
      port: 3000
      health: /readyz
      domains:
        - dispatch.example.com
  worker:
    build:
      context: .
      dockerfile: Dockerfile
    command: ["./bin/worker"]
    secrets:
      DATABASE_URL: database-url
    env:
      REDIS_URL: redis://redis:6379/0
  db:
    image: postgres:17
    update: manual
    env:
      POSTGRES_DB: dispatch
      POSTGRES_USER: dispatch
    secrets:
      POSTGRES_PASSWORD: database-password
    volume:
      name: postgres
      mount: /var/lib/postgresql/data
  redis:
    image: redis:7
    update: manual
    command: ["redis-server", "--appendonly", "yes", "--appendfsync", "everysec"]
    volume:
      name: redis
      mount: /data
```

```sh
soar secrets stage database-password
soar secrets stage database-url
soar secrets stage session-secret
soar deploy --service db
soar deploy --service redis
soar run --service web -- ./bin/migrate
soar deploy
# Code changes rebuild once and update web + worker, not db + redis:
soar deploy
```

**Tests:** one image built for two processes; three private services; shared Postgres and Redis within the app; durable queue storage. The database URL uses `dispatch` as database/user and `db` as host with the matching password. The migration command follows example 3's retry/exit contract. Redis is deliberately unauthenticated inside this trusted app network and has no published port. This is not appropriate for untrusted services sharing the network.

Web and worker must retry startup connections. New web code may enqueue work consumed by the old worker before worker replacement completes: jobs must tolerate version overlap and retries. A worker replacement has a brief processing pause; graceful shutdown cannot guarantee exactly-once execution. If worker startup fails after web promotion, `status` reports the mixed state and offers resume, supersede, or rollback as described above. Redis AOF with `everysec` can still lose recent writes on failure—it is neither an off-host backup nor a zero-loss queue guarantee.

### 5. A multi-service app with two public endpoints and a private renderer

```yaml
app: workspace
services:
  web:
    build:
      context: ./frontend
      dockerfile: Dockerfile
    env:
      API_ORIGIN: https://api.workspace.example.com
    http:
      port: 3000
      health: /healthz
      domains:
        - workspace.example.com
        - www.workspace.example.com
  api:
    build:
      context: ./api
      dockerfile: Dockerfile
    env:
      RENDER_URL: http://renderer:8080
    http:
      port: 8080
      health: /readyz
      domains:
        - api.workspace.example.com
  renderer:
    build:
      context: ./renderer
      dockerfile: Dockerfile
```

```sh
soar deploy
soar domains add preview.workspace.example.com --service web
soar logs --service renderer --follow
# Update any of the Dockerfiles, then:
soar deploy
```

**Tests:** multiple Dockerfiles and public services, several domains, and a private stateless HTTP service. The application-owned renderer listens on port 8080 and receives no public route, even though it serves HTTP. `api` receives `workspace-api.apps.example.com`. On first deployment the renderer starts before the API readiness check, which retries and verifies renderer availability. No config surgery or third-party credential bootstrap is needed.

On later deployments, API and renderer versions must tolerate overlap. If a renderer change must precede an API change, explicitly `soar deploy --service renderer` first; there is no dependency scheduler. This frontend is a server that reads `API_ORIGIN` at runtime. Static browser bundles cannot magically read container environment variables. Browsers use the public API hostname, not private `api` or `renderer` DNS; CORS, cookies, and application authentication remain application responsibilities.

## 6. What the examples force us to decide

| Pressure | Smallest honest answer |
| --- | --- |
| “Can I add Postgres or Redis?” | Yes: private image-based services with volumes and explicit update protection. |
| “Can my worker reach them?” | Yes: app-local service DNS, without public ports or link declarations. |
| “Can I attach several domains?” | Planned: custom aliases and domain commands remain deferred. |
| “Can one app expose web and API?” | Yes: opt-in HTTP per service; everything else stays private. |
| “Do volume-backed apps use normal deploy?” | Yes, with stop/start downtime; manual update protection is a separate choice. |
| “Does rollback restore my database?” | No: code/config rollback and data recovery are separate operations. |
| “Will this manage database upgrades, migrations, and backups for me?” | No: explicit operations and tested recovery documentation first. |
| “Can I share one Redis across unrelated apps?” | Not through an implicit cross-app network in v1. |

## 7. Build order and acceptance checks

Current implementation checklist (not a live-test completion checklist):

- [x] Remote builds and pulled image references resolved to immutable identity.
- [x] Private multi-service networking, staged secrets and persistent owned volumes.
- [x] Manual-service preflight protection and preview/approval for scoped updates.
- [x] Scoped captures preserve actual unselected configuration and select only required builds/secrets.
- [x] Noninteractive one-off commands with bounded results and explicit unknown-outcome recovery.
- [x] Terminal run cleanup and bounded result retention; destroy retains volumes/secrets.
- [ ] Custom domains, rollback, exec/interactive commands and service removal.
- [ ] Backup/restore tooling and tested off-host recovery for important data.
- [ ] New PostgreSQL live acceptance evidence; do not infer it from earlier volume tests.

The sequence below retains design/acceptance targets, including deferred features:


1. **One stateless app end to end.** Setup, target binding, build transfer, generated URL, HTTPS, logs, status. Follow example 1 from a fresh Ubuntu 24.04 host; verify explicit target overrides do not silently change future targets. Check that older Ubuntu, other distributions, and missing OS metadata fail before host mutation. Ubuntu 24.04 is the host CI baseline; newer Ubuntu releases remain within the host support scope. Native macOS/Linux/Windows client CI is configured for command behavior, configuration paths, subprocesses, and SSH option acceptance; client support is not limited to the host CI image. These offline tests do not replace a real Ubuntu SSH smoke test.
2. **Safe replacement and routing.** Readiness, domain edits, rollback, locks, interrupted-operation recovery. A bad build/readiness check leaves the old volume-free route live. Duplicate hostnames fail safely; incorrect custom DNS leaves generated URLs usable and reports pending, not HTTPS success. Confirm web/API domains route to the correct services.
3. **Private multi-service apps.** Networks, staged secrets, workers, build reuse, one-off commands, partial deploys. Follow examples 3–5 without editing YAML to bootstrap. Migrate an empty database while the API cannot yet pass readiness. Same-app connections succeed; external and other-app connections fail. Staged secrets remain pending across reboot. Interrupted worker replacement never resurrects a retired worker.
4. **Persistent data and protected services.** Example 2 uses ordinary deploy and never overlaps writers. App deploy leaves protected db/Redis untouched; changed protected config gives an exact next command. Test changed loopback ports, failed readiness, interrupted stop/start, retained volumes, and explicit data-compatible code rollback. Exercise scoped updates and pending secret versions.
5. **Make it safe for actual personal data.** Log/cache retention, disk exhaustion, off-host backup, fresh-server restore including secrets and target binding. Interrupt around route promotion and verify status/resume uses captured inputs, while supersede uses the corrected project. Never silently rerun a one-off migration.

## 8. What we borrow from Fly—and what we deliberately do differently

Cross-checked against official Fly documentation during design review:

- **Keep one deployment entry point.** Fly's [normal deploy supports volume-backed Machines](https://fly.io/docs/launch/deploy/#volume-mounts-and-fly-deploy); local volumes do not imply a separate app-update workflow. Soar likewise uses normal deploy with stop/start for volume-backed apps, while explicitly protecting database updates.
- **Name deferred secrets honestly.** Fly's [`secrets set` applies changes and `--stage` defers them](https://fly.io/docs/apps/secrets/). Soar intentionally exposes staging only, applies values through deployment, and pins versions across restart/reboot. It is not a drop-in copy of Fly's secret semantics.
- **Group collaborating services.** Fly's [standard process groups](https://fly.io/docs/launch/processes/) use one app image; Soar allows multiple images in one app so Postgres/Redis need no separate deployment projects. Shared web/worker builds retain the convenient same-image case.
- **Choose a tighter private boundary.** Fly's [default private network](https://fly.io/docs/networking/private-networking/) spans an organization. Soar isolates each app and offers simple service DNS within it.
- **Make domain state visible.** Fly's [domain setup](https://fly.io/docs/networking/custom-domain/) distinguishes DNS/certificate provisioning. Soar does too, but maps each hostname to an explicit service rather than implying hostnames select Fly process groups.
- **Support explicit schema setup without a framework.** Fly offers a [`release_command`](https://fly.io/docs/reference/configuration/) in a temporary Machine without attached volumes. Soar starts with a manual, volume-free `run` command, not automatic migration hooks.

The SME feedback led to separating storage from update protection, naming secret staging explicitly, defining target/recovery contracts, and replacing the credential-bootstrap-heavy search example with a private renderer. Keep single-writer safety and recovery internals; reject dependency graphs, database operators, automatic credential provisioning, and multi-host features for this scope.

The result should feel like a small app appliance: install once, describe only the app, deploy, inspect. If routine use requires knowing which containers, host ports, or Caddy entries to edit manually, the abstraction is not finished.
