# Independent local service directories

This describes the implemented binding-aware partial-manifest workflow.
See the live acceptance below; older verification records cover the versions
tested then. Use a matching current client/executor and inspect the
resolved target before applying changes.

## One app, several source folders

Independent folders can each contain a `vamos.yaml` with the **same app name** and
different, app-wide unique service names. No Git repository, CI pipeline, local
Docker daemon, or image registry is required for Dockerfile builds. Source is
captured locally and built on the server; image-based services can still pull
from a registry explicitly.

For example, save this as `frontend/vamos.yaml` alongside its Dockerfile:

```yaml
app: workspace
services:
  web:
    build:
      context: .
      dockerfile: Dockerfile
    env:
      API_ORIGIN: https://workspace-api.apps.example.com
    http:
      port: 3000
      health: /healthz
```

Save this independent manifest as `api/vamos.yaml` alongside its own Dockerfile:

```yaml
app: workspace
services:
  api:
    build:
      context: .
      dockerfile: Dockerfile
    http:
      port: 8080
      health: /readyz
```

These are complete configuration examples, not runnable application code. Each
Dockerfile must supply a server listening on `0.0.0.0` with the declared health
endpoint. Substitute your configured base domain for `apps.example.com`.

```sh
cd frontend
vamos validate
vamos deploy --server home
cd ../api
vamos validate
vamos deploy --server home
```

Deploy **upserts the declared selection**, not the whole remote membership.
Omitted services keep their configuration, images, runtime, secret-version pins,
and pending staged secrets untouched. `--service NAME` further narrows selection
to one local declaration. Removing a declaration is never deletion authorization.
Existing manual services retain their protected-update rules; see
[multi-service deployment](multi-service.md).

`http` makes a service public; `web` is not a magic public-service type. The name
only affects generated URL spelling: public `web` gets `<app>.<base-domain>`;
other public services get `<app>-<service>.<base-domain>`. Without `http`, even a
service named `web` is private. All services in the app share service-name DNS
and the app network, regardless of their source folders.

## Explicit identity bindings

A binding identifies **server/host identity + app + service + incarnation**, not
a repository owner. A first genuinely new service may be created without a prior
service binding. The CLI saves its acknowledged identity after the host admits
the operation, including an admitted operation that later fails. A preflight
failure against an existing service does not silently adopt it.

Before deploying an existing service from a new directory, inspect it and bind
explicitly from that directory:

```sh
vamos status --app workspace --server home
vamos service bind api --app workspace --server home
vamos deploy --service api --server home
```

`service bind` only reads remote status and writes local metadata; it does not
deploy, restart, or change remote ownership. With `--app`, it writes in the
current directory; using `-f path/to/vamos.yaml` instead uses the manifest's
directory. Overlapping manifests/directories are permitted after explicit
binding. Several directories may bind the same service. These checks prevent
accidental target adoption, **not access control**, repository ownership, or
concurrent-edit conflict resolution. Root SSH access remains administrative.

Private `.vamos/` metadata includes the normal `server.json` target binding and
separate `services-<hash>.json` records keyed by resolved SSH target and app.
Service records contain host identity and incarnation, use private file
permissions, and are gitignored. Build contexts always exclude `.vamos` (and
`.git`); do not copy these records into source control or source bundles.

`--server` overrides only this invocation, never the normal project target.
Service identity records read/written by that invocation belong to that target
and app, not the default target. A replaced host or removed/re-created service
invalidates old bindings: inspect and explicitly bind again rather than blindly
retrying or editing identity files.

## Remove a service, not the app

Removal normally requires a settled ready app; reconcile ambiguous pending
routing first. It is preview-first, and applying recomputes the plan under the
host lock:

```sh
vamos service remove api --app workspace --server home
vamos service remove api --app workspace --server home --yes
# A protected update: manual service needs an additional explicit approval:
vamos service remove db --app workspace --server home --yes --allow-protected
```

The service wrapper accepts **NAME before flags**, as shown. Other positional
commands require flags first, for example:

```sh
vamos resume --app workspace --server home OPERATION_ID
```

Removal withdraws routes before stopping/removing the selected service's verified
containers. Other services remain. Volumes and secrets are retained; shared
network, images and archives are not deleted by service removal. Interrupted
removal resumes its captured maintenance operation. It is not a data purge.
Removing the last service is refused: use preview-first `vamos destroy` for the
app instead. Removal can also abandon an isolated failed initial creation when
that service was the operation's only selected service, other services are
unchanged, and no uncertain routing or retirement needs reconciliation. Broader
incomplete operations still require resume or supported supersede; removal is
not a way to discard another service's pending work.

A committed removal records a tombstone and removes the service from rollback
snapshots. **Rollback never resurrects removed membership.** Remove its local
declaration if it should stay absent. To intentionally re-create it, keep/restore
the declaration and explicitly select and approve it:

```sh
vamos deploy --server home --re-add --service api --yes
# If that service has retained volume data, separately authorize exact reuse:
vamos deploy --server home --re-add --service db --yes --reuse-data
```

Re-add creates a new incarnation; old directory bindings no longer authorize
updates to it. `--reuse-data` acknowledges retained storage reuse, not schema
compatibility, arbitrary volume adoption, or restoration of a backup. See
[volumes](volumes.md) and [app lifecycle](app-lifecycle.md).

## History, recovery and coordination

Journal v7 keeps each service's `Previous` distinct successfully completed release.
Deploying another directory's service, or a no-op deployment, does not replace
that predecessor. Ready-app `rollback --service NAME` uses that service history;
it fails if no useful predecessor exists. App-wide rollback still uses the
previous completed **app snapshot**, not an independently assembled set of every
service's predecessor. Incomplete deployment rollback uses the last completed
stable baseline. Protection, missing-input checks and volume downtime approval
still apply; see [rollback](rollback.md).

Versions 1–6 remain readable and recoverable using their legacy schemas. New v7
deployments require the binding-aware partial protocol; legacy whole-manifest
writers cannot bypass it. A ready legacy journal can gain identity/history
metadata without restarting unchanged containers. Metadata upgrade is not a
replacement deploy and does not retroactively manufacture distinct service
history. Finish legacy interrupted work through its supported recovery path.

Independent folders do **not** mean parallel runtime deployments: deployments,
maintenance and recovery remain serialized by the global host lock. Operations
are not app-wide transactions. Ambiguous route state must be reconciled before
new mutation; `resume` uses captured inputs rather than current local files.

Database/schema migrations require explicit coordination among every affected
service and operator. Use backward-compatible changes and intentional sequencing;
there is no dependency scheduler. An explicit, operation-scoped
[release command](release-commands.md) can gate selected runtime changes on a
migration's success; it is not inherited by other directories and never retries
a started execution automatically. Separate secret names/credentials do not automatically create separate database roles or enforce
SQL privileges. Configure roles and grants yourself; shared app networking is not
a hostile-tenant security boundary.

## Limits and verification boundary

The merged remote app, not each folder independently, is limited to **8 services,
32 desired instances (8 per service), 32 custom aliases (8 per public service),
and 64 secret environment mappings**.
`vamos validate` checks configuration syntax/schema and alias bounds, but currently
does not enforce the deployment service-count or aggregate secret-mapping bounds.
A successful local validation is not deployment admission: deploy validation and
remote planning must also validate the merged app and server-wide hostname
ownership. Splitting folders cannot evade these limits.

The **32 MiB aggregate source bundle** includes manifest overhead and only the
locally selected build contexts, not omitted remote services' sources. With no
`--service`, current CLI capture selects all local declarations, including manual
ones; do not assume it filters out every protected local build before transfer.
Ignore sensitive files explicitly. See [deployment](deployment.md) for transport
and recovery limits.

### Live acceptance

Verified using disposable app `svcprobe` on Ubuntu 26.04 amd64, Docker 29.1.3
and Caddy 2.6.2:

- Built orders, billing, inventory and notifications remotely from four separate
  local directories, each declaring only its own service. A fifth directory
  declared the protected PostgreSQL service. No CI or pre-published application
  images were needed.
- Ran the explicit migration once, wrote a ledger row through orders, and read
  that same row through all four services over externally verified HTTPS.
- A copied orders manifest without bindings was rejected without changing any
  service identity. Explicit binding wrote local metadata without deployment.
- Staged a different database initialization secret, then updated orders and
  billing independently. The omitted database retained its container, image,
  incarnation and applied secret version; the staged change remained pending.
- Cleanup preserved orders' predecessor. Scoped rollback restored orders' exact
  original image while billing remained at its newer release and the database
  stayed unchanged.
- Removed orders immediately after rollback. JSON preview was parseable, omitted
  services stayed unchanged, rollback could not recreate orders, and an ordinary
  stale deployment was refused. Explicit re-add created a new incarnation; the
  separately bound copied directory could no longer update it.
- Protected database removal required `--allow-protected`. Re-add without
  `--reuse-data` was refused. Explicit reuse preserved the exact volume identity
  and ledger row, with all four application containers unchanged. The original
  database password was re-staged before re-add; this did not perform SQL password
  rotation.
- A deliberately failing new private service was recorded as incomplete, then
  safely abandoned with service removal. The other five services were unchanged.
- Cleanup and app destruction removed probe routes, containers, network and
  captured operation files. Probe database data and secrets were intentionally
  retained. Hello's original container identities/start timestamps and HTTPS
  health were unchanged; previously retained data volumes were untouched.

Full formatting/vet/race-enabled tests, native/Linux amd64/arm64 builds, Windows
cross-compilation, both example test suites, and Markdown/YAML checks passed.
The review pipeline aborted; focused fallback reviews found a shared-build-key
removal defect, which was reproduced and fixed. The follow-up found no actionable
issues. Regression tests also cover interrupted removal/reload/resume, pending
route status, malformed transport acknowledgments, retained history, binding
replacement, and failed-creation abandonment.

Native Windows execution, arm64 host execution, and forced live worker/host
interruption were not exercised. Recovery interruption coverage is automated,
not a claim of live power-loss testing.
