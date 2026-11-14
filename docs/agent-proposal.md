# Proposal: server coordinator agent, non-root deploys, and shared engine boundary

**Status: proposed architecture (not yet implemented).**
Current Soar commands still upload a Linux executor over `root` SSH per operation
and serialize all host mutations with immediate busy refusal (`/run/lock/soar-host.lock`
and a single `soar-host-operation.service` unit). Local development (`soar dev` /
`soar.dev.yaml`) is intentionally deferred; it is included here only so component
boundaries do not couple the core app engine to Ubuntu, systemd, Caddy, or root.

## 1. Why evolve from the stateless-server model

Uploading an executor per command kept the initial server footprint minimal, but
exposes four structural limits once CI pipelines and multiple apps share a host:

1. **Privilege boundary (especially GitHub Actions):** Uploading and running a
   client-supplied binary as `root` makes every deployment credential equivalent
   to unrestricted root SSH access on the production server. Separating *permission
   to deploy an app* from *permission to administer the host* requires running
   installed, root-owned server code rather than client-uploaded executables.
2. **Concurrent deployments and server self-governance:** Today, nothing queues
   incoming work on the server. If three GitHub workflows deploy three apps at
   the same time, one acquires the host lock and two fail immediately as busy.
   The server—not independent laptops and CI runners—should own admission,
   durable queueing, and concurrency limits matched to its CPU, memory, and disk.
3. **Client/server versioning:** Without a server-side version contract, raw local
   builds default to `dev` and client/executor skew is caught only indirectly when
   wire JSON or journal schemas diverge. With installed server binaries, version
   safety becomes a fast capability handshake plus a deliberate `soar server upgrade`
   workflow.
4. **Future local dev parity (deferred):** Users will eventually want to run a
   local dev stack from the same configuration style (for example `soar.dev.yaml`)
   on macOS, Linux, or Windows. Splitting the container engine cleanly from the
   Ubuntu/systemd/Caddy control plane now avoids duplicating app networking,
   secret injection, and service semantics later.

## 2. Three-layer component split

```text
                  ┌──────────────────────────────────────┐
                  │         1. Config & App Spec         │
                  │  soar.yaml (+ future soar.dev.yaml)  │
                  └──────────────────┬───────────────────┘
                                     │
            ┌────────────────────────┴────────────────────────┐
            ▼                                                 ▼
┌───────────────────────────┐                     ┌───────────────────────────┐
│   Remote Control Plane    │                     │  Future Local Dev Runner  │
│  soar-agent (root daemon) │                     │   in-process in soar CLI  │
│  • Auth & app ACLs        │                     │  • Deferred milestone     │
│  • Durable job queue      │                     │  • No daemon, no root     │
│  • Host concurrency caps  │                     │  • macOS / Linux / Win    │
│  • Spawns systemd workers │                     │  • Local dev secrets      │
└─────────────┬─────────────┘                     └─────────────┬─────────────┘
              │                                                 │
              └──────────────────────┬──────────────────────────┘
                                     ▼
                  ┌──────────────────────────────────────┐
                  │      2. Shared Container Engine      │
                  │  App bridge network + private DNS,   │
                  │  builds/pulls, volumes, env-files,   │
                  │  readiness probes, release/exec/logs │
                  └───────────┬──────────────┬───────────┘
                              ▼              ▼
                     Server Backends     Dev Backends (deferred)
                     Caddy + /var/lib    127.0.0.1 ports + .soar/dev
```

### Layer 1: Config and app spec

`internal/config` and validation remain shared across CLI validation, server
deployments, and future local development: `app`, `services`, `build`/`image`,
`command`, `env`, `secrets`, `http`, `volume`, `resources`, and `release`. No
Ubuntu, systemd, or Caddy details enter the manifest schema.

### Layer 2: Shared container engine

Extract the Docker-facing primitives currently interleaved with server rollout
state into an engine boundary:

- Per-app isolated Docker bridge network and service-name DNS (`db:5432`, `redis:6379`).
- Image resolution (BuildKit builds and digest-verified pulls).
- Owned volume attachment and container creation with private `--env-file`
  injection (never secret-bearing `docker` argv).
- HTTP/process readiness probes, one-off `run`/`release` execution, `exec`, and
  log streaming.

Ingress and state persistence sit behind explicit interfaces so production uses
Caddy loopback routes and `/var/lib/soar` journals while a future `soar dev` can
publish `127.0.0.1` ports and write under `.soar/dev`. Remote CLI commands (`deploy`,
`status`, `logs`, `secrets`, `rollback`) continue to require **zero local Docker**.

### Layer 3: `soar-agent` coordinator and supervised workers

On the server, `soar server add` installs both `soar-agent` and `soar-executor`
under `/usr/local/lib/soar/` (root-owned, recorded in `/var/lib/soar/host.json`)
and enables `soar-agent.service`:

- `soar-agent` is a small root-owned systemd daemon listening on a local Unix
  socket (`/run/soar/agent.sock`). It owns authentication, capability handshakes,
  admission control, durable queueing, concurrency semaphores, and observation.
- Actual deployments and maintenance jobs still run in bounded transient systemd
  worker units (`soar-op-<id>.service`) invoking the installed `soar-executor`.
  A hung build, OOM, or worker crash cannot corrupt or wedge the coordinator.

## 3. Privilege separation and authentication

| Role | Transport & Identity | Allowed Operations |
| --- | --- | --- |
| **Host administrator** | `root` SSH | `server add`, `server upgrade`, `server configure`, `server teardown`, principal/ACL management |
| **Operator / CI deploy** | Non-root access to `soar-agent` | Authorized app operations (`deploy`, `status`, `logs`, `secrets`, `run`, `rollback`, `resume`, etc.) |

Key invariants:

- **Clients never upload executables during app operations.** Payloads contain
  only structured request metadata, bounded build contexts, or private secret/backup
  inputs.
- **SSH-first non-root transport (no new public listener required):** Bootstrap
  provisions a dedicated unprivileged account (for example `soar`) that is **not**
  in the `docker` group, has no interactive login shell, disables port/X11/agent
  forwarding, and uses an SSH `ForceCommand` / socket bridge to speak only to
  `/run/soar/agent.sock`.
- **Scoped CI credentials:** SSH `authorized_keys` entries (or agent tokens) bind
  each credential to a named principal ID; `soar-agent` enforces server-side ACLs
  (for example allowing a GitHub repository credential to deploy and inspect only
  `app: billing`, not stage host changes or touch `app: studio`). A later opt-in
  HTTPS endpoint behind Caddy can verify short-lived GitHub Actions OIDC JWTs
  against the same principal/ACL table without static keys.
- **Explicit trust boundary:** Removing `root` SSH from GitHub eliminates arbitrary
  host command execution, binary replacement, and cross-app control. It does **not**
  make Soar a hostile multi-tenant sandbox: permission to deploy code to an app
  inherently grants runtime access to that app's volumes and bound secrets, and
  Dockerfile builds still assume trusted application source.

## 4. Server-owned queueing, locking, and concurrency

### Durable admission and idempotency

1. Client connects to `soar-agent`, completes the version/capability check, and
   submits an operation with a client-generated idempotency key and required inputs.
2. `soar-agent` validates authorization, app/service bindings, and staging quotas,
   persists the job under `/var/lib/soar/queue/<op-id>/`, and returns `accepted`
   with queue position and `operation_id`.
3. If the network drops during submission or waiting, retrying with the same
   idempotency key (or `soar status` / `soar logs` / `soar wait <op-id>`) reattaches
   to the recorded operation rather than enqueueing a duplicate deploy.
4. Clients may stay connected to stream live phase/build/readiness progress or
   disconnect safely at any point after admission; `soar-agent` owns execution to
   completion.

### Host-configured concurrency and granular locks

Instead of one host-wide non-blocking lock covering builds through route switches,
the server enforces a layered concurrency policy stored in host configuration:

- **Admission limits:** `max_queue_depth` and `max_staged_bytes` bound disk and
  memory pressure, failing fast with an explicit queue-full error rather than
  exhausting `/var` or `/run`.
- **Host capacity semaphores:**
  - `max_concurrent_builds` (default `1` on small hosts; configurable higher on
    larger servers) caps simultaneous BuildKit builds.
  - `max_concurrent_deploys` (default `1`; configurable higher) caps simultaneous
    container rollouts across distinct apps.
- **Locking hierarchy:**
  1. **Host lifecycle barrier:** `server upgrade`, `server configure`, and
     `server teardown` quiesce admissions and wait for active app workers before
     mutating host-level state.
  2. **Per-app mutation lock:** Strictly **one** active mutation per app. Three
     simultaneous deploys to *App A*, *App B*, and *App C* all succeed: on a small
     host (`max_concurrent_deploys: 1`) two wait in the queue and run in order;
     on a larger host (`max_concurrent_deploys: 3`) all three run concurrently.
     Two simultaneous deploys to *App A* serialize cleanly in FIFO order (with
     optional explicit cancellation of a still-`queued` job).
  3. **Short shared-resource locks:** Caddy configuration compare-and-swap and
     reload acquire a dedicated ingress lock only for the brief route transaction
     (milliseconds), never during image builds, pulls, release commands, or
     container readiness probes.

### Supervision and restart recovery

On `soar-agent` restart or host reboot, the coordinator reconciles `/var/lib/soar/queue/`
against live `soar-op-<id>.service` units and per-app state journals before
accepting new work:

- Live worker units are re-adopted and observed until exit.
- Unstarted `queued` operations remain queued in order and schedule once capacity
  is free.
- Workers that vanished mid-flight transition according to existing journal rules:
  `soar-agent` **never blindly restarts non-idempotent phases** (such as a release
  command whose start intent was recorded, or an uncertain volume/route step).
  Uncertain operations remain fenced for explicit `resume` / `supersede` diagnosis
  on that app without wedging unrelated apps' queues.

## 5. Versioning and upgrade model

With installed server binaries, synchronization uses three explicit layers rather
than per-command binary uploads:

1. **Build identity (provenance & diagnostics):** Release and local build tasks
   stamp `soar`, `soar-agent`, and `soar-executor` with a deterministic version
   label and source/recipe build digest reported by `soar version` and `server status`.
2. **Agent API handshake (wire compatibility):** Every client session begins with
   a lightweight `hello` RPC returning `agent_build`, `api_min`, `api_max`, and
   supported capability flags.
   - If the client is older than `api_min`, it fails before sending payloads:
     `upgrade local soar CLI to >= X`.
   - If the server is older than what the requested feature/API requires, it fails
     before mutation: `server agent supports API vN; run 'soar server upgrade'`.
3. **State schema versions (disk compatibility):** Journals, secret stores, volume
   registries, and queue records keep strict versioned schemas. Upgrades are
   forward-compatible (new `soar-agent` reads prior schema versions); downgrading
   server binaries below persisted schema versions continues to fail closed.

### `soar server upgrade`

Host upgrades become an explicit root administrative command:

1. Inspect the target architecture and verify the local matching Linux bundle
   (`soar-agent` + `soar-executor` checksums and ELF architecture).
2. Acquire the host lifecycle barrier: pause new queue admissions and wait for
   active worker units to finish (bounded by `--timeout`; never kill running
   deployments by default).
3. Stage and atomically replace `/usr/local/lib/soar/soar-agent` and
   `/usr/local/lib/soar/soar-executor`, update recorded digests in
   `/var/lib/soar/host.json`, restart `soar-agent.service`, and verify the post-upgrade
   handshake and health checks.

Distro APT / Docker / Caddy package upgrades remain a separate concern from Soar
binary upgrades and can later build on the same drain-and-barrier primitive.

## 6. Seam preserved for future local dev (deferred)

`soar dev` is **out of scope** for the initial agent milestones, but this split
ensures nothing blocks it later:

- `soar.dev.yaml` (or `-f`) will parse through Layer 1 and run Layer 2 **in-process**
  inside the `soar` CLI against a local Docker socket—no `soar-agent`, no `root`,
  no Ubuntu/systemd requirement, and no Caddy on ports 80/443.
- Dev-only conveniences (such as live source bind mounts or local port overrides,
  if added later) will be gated to `soar dev` validation and rejected by `soar deploy`
  so production immutability stays intact.
- Open local-dev UX choices—source bind mounts vs fast rebuilds, local secret
  file/store ergonomics, localhost port mapping, and multiplexed service logs—are
  deferred until we start the `soar dev` milestone.

## 7. Phased implementation sequence

1. **Phase 1 — Shared engine boundary & protocol spec:** Separate the container/app
   execution engine from SSH transport, define the `soar-agent` socket RPC and
   version handshake, and add deterministic build stamping across client and Linux
   artifacts.
2. **Phase 2 — Installed `soar-agent`, durable serial queue, non-root SSH deploy,
   and `soar server upgrade`:** Provision `soar-agent` + restricted `soar` SSH
   bridge during `server add`, persist and queue concurrent submissions (`max_concurrent_deploys=1`
   default), stream/reattach operation status, and ship `soar server upgrade`.
   This resolves GitHub root SSH exposure, multi-app busy failures, and client/server
   versioning together.
3. **Phase 3 — Configurable multi-app concurrency & scoped CI auth:** Introduce
   per-app locks, short Caddy route locks, tunable `max_concurrent_builds` /
   `max_concurrent_deploys`, and per-principal/per-app ACLs for CI credentials
   (plus optional GitHub OIDC verification).
4. **Phase 4 — Local dev stack (deferred):** Build `soar dev` atop the shared
   Layer 1 + Layer 2 engine for macOS, Linux, and Windows.
