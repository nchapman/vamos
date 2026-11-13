# Proposal: application backup and recovery

**Status: design proposal, not implemented.** All YAML and `soar backup` /
`soar restore` commands below are proposed interfaces; current binaries reject
them. This document designs **PostgreSQL, SQLite, and files together**. It does
not claim backup or restore verification. Existing [volume](volumes.md),
[secret](secrets.md), and [runtime](runtime-evolution.md) contracts remain in force.

## Goal and first boundary

Recover an application on a fresh server after losing the original server,
without silently substituting newer code, staged secrets, or empty storage.
A retained Docker volume and a code rollback are not backups.

Design all three built-in methods now; implement them incrementally against the
same contract. Do not call the milestone complete after PostgreSQL alone. No
user-installed plugins, arbitrary shell hooks, cross-host coordination, database
upgrades, continuous WAL archiving/PITR, in-place restore, or automatic DNS cutover
in the initial boundary. Logical PostgreSQL dumps target small apps with measured
backup windows, not every production database or a near-zero data-loss budget.

A **source** names a particular database or directory. A **set** groups sources
into one capture operation with an explicit consistency policy. A **recovery
point** is the remotely committed result: source artifacts plus the runtime,
secret, and ownership metadata needed to interpret them. A **repository** is an
independently recoverable, encrypted off-host destination.

## Opt-in and configuration

Do not detect database products from image names, ports, environment variables,
or frameworks. The user selects a method; preflight verifies its prerequisites.
A declaration is not proof that the target is the declared database product.
Never fall back from a failed database backup to copying live files.

Use a separate app-level backup plan, provisionally `backup.yaml`, rather than
one `backup` property per service. This lets SQLite and uploads share one volume,
and avoids giving a scoped deploy authority to replace another directory's
backup policy. Applying a plan is a separate, preview-first operation against an
explicit existing app. It neither deploys services nor starts dependencies.

### PostgreSQL example

For the existing [Rails example](../examples/rails-ledger/README.md):

```yaml
app: rails-ledger
sources:
  primary:
    type: postgres
    service: database
    database: rails_ledger
    user: rails_ledger
    credentials:
      service: database
      env: POSTGRES_PASSWORD
    restore:
      profile: single-owner
sets:
  daily:
    sources: [primary]
    consistency: online-per-source
    repository: offsite
```

The credential selector means the **applied secret version** supplying that
service's environment key, not a plaintext value, an arbitrary environment lookup,
or the newest staged version. Require a declared secret mapping. Verify that the
captured value actually authenticates as the specified database user; Postgres
initialization environment does not prove its current SQL password. Separate
backup-only credential management is a later extension, not an implicit
latest-secret selection rule.

The connection targets the captured same-app database service, not an arbitrary
URL. Its container must already be running, verified, and uniquely identified;
initial PostgreSQL sources require a singleton service. No published database
port is needed. Ports default to 5432; an explicit validated port can override it.
Initially allow one PostgreSQL database source per database service in a set;
multiple databases on that server must be reported as omitted coverage, not
silently included. Whole-cluster and multi-database privilege coordination are
outside this first profile.

### SQLite and files example

For the existing [Journal example](../examples/journal/README.md):

```yaml
app: journal
sources:
  entries:
    type: sqlite
    service: web
    volume: data
    path: journal.sqlite
  uploads:
    type: files
    service: web
    volume: data
    path: uploads
sets:
  daily:
    sources: [entries, uploads]
    consistency: quiesced
    stop: [web]
    repository: offsite
  database-only:
    sources: [entries]
    consistency: online-per-source
    repository: offsite
```

Paths are relative to the registered volume root, never host paths or arbitrary
container mounts. `.` selects an entire volume for `files`; SQLite requires one
existing regular database file. No globbing, interpolation, path traversal,
symlink traversal, or automatic missing-source creation. Two sources can read
different paths on the same volume, but overlapping restore destinations are
rejected within a set, including SQLite's reserved journal/WAL/SHM sidecar names.
Different sets may reference the same source. The database-only set explicitly
omits uploads and must not be labeled a complete Journal recovery point.

A PostgreSQL-plus-uploads app would use the same `quiesced` set shape, listing
its web and worker services under `stop` while leaving PostgreSQL running to
produce a native dump. These are explicit backup coordination instructions,
not inferred application dependencies or deploy-order changes.

### Policy identity and authorization

Plan installation resolves source and stop-service names to current immutable
service incarnations, volume IDs and bindings, under the app/host identity.
Removing/re-adding a service or changing storage identity invalidates that
selection; scheduled work refuses until explicitly reconfigured. A normal
compatible deployment is not invalidation: each backup captures the then-current
runtime identities, configuration and secret pins anew.

Plan application replaces the full backup plan, with a revision-bound preview
and explicit approval. It does not use deployment's selected-service merge rules.
Omitting a source from a new plan never deletes its historical backups. Preview
lists changed coverage, schedules, repository and downtime permissions. Backup
policy is stored separately from deployment journals; an ordinary deploy or
rollback cannot enable, disable, or silently revert scheduled backups.

Proposed user flow (not implemented):

```sh
soar backup configure --server home --app journal -f backup.yaml
soar backup configure --server home --app journal -f backup.yaml --yes
soar backup create --server home --app journal --set daily
soar backup create --server home --app journal --set daily --allow-downtime --yes
soar backup list --server home --app journal
soar backup status --server home --app journal --operation OPERATION
soar backup resume --server home --app journal --operation OPERATION
```

`create` previews unless approved. Downtime requires explicit authorization even
for a manual/protected service. This is backup-specific stop/restart authority,
not permission to redeploy that service or consume its pending secret versions.
There are no schedules by default. Repository configuration alone is not opt-in.

## Shared contract and method-specific behavior

| Responsibility | Shared coordinator | Built-in method |
| --- | --- | --- |
| Selection | Capture service incarnations, effective config, exact runtime/volume IDs and secret pins | Validate database/directory selection and capabilities |
| Consistency | Enforce set policy and authorized stop/restart lifecycle | Produce a consistent artifact or refuse |
| Execution | Own helper identities, resource/deadline limits and private staging | Use pinned, compatible tooling without arbitrary hooks |
| Persistence | Seal artifacts, encrypt/upload, commit repository manifest | Record format and source/tool versions |
| Restore | Allocate fresh identities/storage, enforce isolation and explicit activation | Import/extract into that fresh target |
| Verification | Distinguish completed upload, artifact checks and restore drill | Check database structure or restored file content/metadata |

Do not force every method into a command returning a byte stream: SQLite needs
file access and files need directory metadata, while PostgreSQL needs a network
connection. Methods declare required capabilities; only the coordinator grants
mounts, network access, credentials, writer suspension and target-write authority.
This is a closed built-in interface, not a general privileged command runner.
Existing `soar run` stays volume-free. Backup helpers require a separate, narrowly
owned execution boundary; do not weaken ordinary run/session ownership rules.

### PostgreSQL (`postgres`)

- Use a custom-format logical dump of one explicitly named database. Capture
  server/client versions, encoding/locale requirements, extensions and the
  ownership profile. Readiness is a real authenticated query, not Docker's
  process-running flag. Verify dump tooling against the server; begin with the
  same supported major version for capture and restore, not implicit upgrades.
- A dump is internally consistent under concurrent normal writes. It is not a
  snapshot of every database, all filesystem contents, or the entire cluster.
  DDL can conflict with dump locks; schema changes are not safe to ignore.
- The initial `single-owner` restore profile is deliberately narrower than
  cluster recovery: one application/database-owning login role, plus explicitly
  supported built-in roles and PUBLIC grants. Inventory actual object owners,
  ACLs, default privileges, grantors/grantees, memberships, role attributes and
  settings, extensions and tablespaces. Refuse dependencies outside the supported
  model rather than silently flattening an arbitrary multi-role database.
- Store a structured role/bootstrap manifest alongside the dump. Recreate the
  supported role and its privileges, database/schema/object ownership, ACLs and
  default privileges; do not use blanket `--no-owner --no-acl` normalization.
  Bootstrap a fresh database with the captured encoding/locale requirements,
  then import using a separate maintenance identity. Recreate the login password
  from its captured, authenticated secret binding, not an assumed SQL password
  hash or latest staged value. Unsupported role settings or memberships fail.
- Privileged role attributes require a separate prominent restore approval,
  provisionally `--allow-privileged-roles`, before creating them. The Rails/Ledger
  demos use the image's initialization role, which is a superuser: test the
  default refusal and explicit approval rather than silently demoting it or
  granting privileges. A nonprivileged fixture must also restore without that
  approval. Production least-privilege role design remains the app's decision.
- `pg_dump` does not supply cluster-global roles/password hashes, replication,
  server tuning, or physical recovery. The bounded role manifest above supplies
  only the supported dependencies of the selected database; never replay an
  unfiltered `pg_dumpall --globals-only`. A generic tar of PGDATA is not this
  method. Supporting richer role graphs needs a separate explicit design.
- Verify archive readability, then test real import and expected schema/data in
  a restore drill. A readable archive alone is not proof of recoverability.

### SQLite (`sqlite`)

- Use SQLite's Online Backup API on one database. Never copy just the main file
  of a live WAL-mode database or mistake its sidecars for disposable data.
  Record SQLite/tool versions and reject unsupported encrypted/custom-VFS
  databases. Multiple attached databases are separate sources, not one atomic
  snapshot by assumption.
- A controlled helper opens the source read-only through its exact owned local
  volume. SQLite locking, WAL visibility, directory access and sidecar lifetime
  must work across the helper/application boundary. Read-only WAL access has
  prerequisites; inability to meet them fails rather than granting write access
  to live storage or using `immutable=1` on a changing database.
- Online capture has a bounded busy/restart budget and total deadline. Sustained
  writes can prevent completion. The result must be a closed, standalone
  database artifact before sealing, checked with `integrity_check` and
  `foreign_key_check`. No automatic repair or journal-mode changes on the source.
- A quiesced set first copies the frozen database and recovery-relevant WAL or
  rollback-journal files into bounded private scratch. Open that copy with a
  supported SQLite engine, recover it if necessary, and use the backup API to
  produce the standalone artifact. Rebuild transient shared-memory state in
  scratch rather than depending on copied locks. Missing recovery dependencies
  or a multi-database journal outside the declared single-database scope fail.
  Never repair the original volume. Unsupported online layouts can be configured
  for explicit downtime; there is no silent online-to-stopped fallback.
- Restore the standalone database at its captured relative path in a fresh
  volume, with recorded numeric ownership/permissions and no stale source
  sidecars. Check it again before allowing application execution.

### Files (`files`)

- Initially require a quiesced set that stops the volume-owning service; no
  optimistic copying of mutable live files. An immutable/live-files mode would
  need a later explicit contract. Stopping a service does not flush application
  transactions that never committed or make arbitrary database files valid.
- Copy the selected tree with its relative names, file bytes, numeric UID/GID,
  ordinary modes, timestamps, and required parent/root directory metadata.
  Compatible directory metadata must agree when sources share a target volume.
- Initially support directories and regular files only. Reject symlinks,
  hardlinks, sockets, devices, FIFOs, nested mounts, privileged mode bits and
  unsupported ACLs/xattrs rather than silently dropping semantics. Bound entry
  counts, lengths, depth and expanded bytes; account for sparse-file expansion.
- Open beneath verified directory handles, do not follow changing path components,
  and detect source identity/content changes during a purported frozen capture.
  Detectable drift fails the set; root/daemon modification remains outside the
  guarantee. Never run an application entrypoint to copy files.
- Restore only beneath a fresh owned target root. Validate the entire manifest
  and extraction limits first; reject archive traversal, links, duplicate paths
  and conflicting entries. Compare restored content digests and metadata.

## Consistency, downtime and interruption

`online-per-source` promises a consistent database artifact **per source**.
Capture timestamps are intervals; a set of sequential captures is not an atomic
application snapshot. Files are not eligible for this policy initially.

`quiesced` means all explicitly declared application writers remain stopped for
all source captures. The volume owner is mandatory for SQLite/files sets using
this policy. For network databases the user must enumerate every app writer,
including worker replicas; Soar cannot infer SQL clients from environment values.
An external writer, database-side job, or administrator can invalidate a claimed
cross-source consistency guarantee. Preflight reports these assumptions, not
fictional proof that no such writer exists. Stopping writers cannot repair a
pre-existing database/file mismatch or create an application transaction spanning
sources; Journal's APIs explicitly do not provide one. No cross-server freeze is
offered.

Capture starts only from a supported stable app state: no incomplete deploy,
release command, maintenance, backup suspension, or pending/running/unknown
one-off operation. Active exec/console sessions must drain or cause refusal;
blocking only new sessions cannot stop an already-attached writer. A backup
cannot silently kill user sessions. Hold the existing host mutation lock while
capturing runtime metadata, image bytes, secret material and source artifacts;
this conservatively blocks other apps' mutations too. Reads remain available.
Do not add a speculative per-app locking scheme in this milestone.

For quiescence, preflight tooling, credentials, source access, repository access,
local staging budget and the stop set before changing availability. Capture exact
running state and restart policies for every selected replica. Journal and verify
public 503 routes, disable restart, stop with a bounded grace period and prove the
writers stopped. A forced termination aborts consistency-sensitive capture rather
than silently downgrading the guarantee. Unknown volume references block work.

Restart only the exact previously running runtimes with their captured policies
and pins, never latest configuration or an old retired generation. Services that
were already stopped stay stopped. Restore public routes only after readiness;
private process observation is not a database readiness check. No release command,
migration, image build, or staged-secret application occurs during reactivation.

Proposed durable phases:

```text
planned -> preflight -> suspending (if needed) -> capturing
        -> local-sealed -> reactivating (if needed) -> uploading -> committed
```

- A durable maintenance fence survives worker death. During suspension, Docker
  restart must not resurrect writers after a host reboot; captured 503 routes
  survive unrelated route generation. Resume inspects exact observed identities.
- Failed capture first reconciles helpers and writer state, then attempts safe
  reactivation. Preserve the capture error even if reactivation succeeds. A
  reactivation error remains separately visible; do not report a backup as
  successful while its suspension is unresolved. `cancel` also means reconcile
  and reactivate, not erase evidence or blindly restart services.
- Lost helper start/result responses require observation of the owned helper.
  Never duplicate an execution while it may still hold a source connection or
  target mount. A known failed attempt can be explicitly abandoned after it is
  stopped; a fresh capture gets a new attempt ID and recaptures the entire set.
  Never combine a source from yesterday's failed attempt with today's capture.
- A local seal proves all artifacts and their manifest are durable and immutable.
  After successful reactivation, upload may release the host mutation lock; the
  sealed copy no longer depends on live volumes/images/secrets. Upload retry
  reuses those exact bytes. Staging loss requires a new capture, not silent
  replacement under an old identity.
- Commit the recovery-point manifest only after all referenced objects are
  remotely stored and checked. An uncertain commit response is reconciled by
  immutable operation identity. Partial uploads are not advertised as recovery
  points and never authorize retention pruning.

The durable operation state and Docker helper ownership rules must be specified
and fault-tested before implementation, including every stop/start/route and
remote-commit boundary. A shell trap or client process lifetime is not recovery.

## What a recovery point contains

Capture the actual effective per-service configurations and immutable images,
not just the last local YAML or last whole-app completed release. Scoped updates
can intentionally produce a mixed configuration. Include:

- Source/set/plan revisions, app and service provenance, volume bindings,
  consistency policy, stop set and timestamp intervals.
- Format/tool versions, sizes, content digests and source validation outcomes.
- Service commands, environment, resources, replica counts, route declarations,
  image identity and platform, and exact applied secret mappings.
- Encrypted values of every required applied secret version, including different
  versions of the same secret used by different services. Do not copy latest
  pointers or pending values as active state. Missing pins fail closed. Restoring
  an old credential value does not restore its validity at an external provider.
- Exported exact image content for the initial self-contained recovery profile,
  including locally built images and required backup tooling. A digest in a
  manifest is not an off-host image copy. Restore initially requires a compatible
  CPU architecture; no rebuild or tag resolution fallback.
- The coverage inventory and acknowledged exclusions: selected databases/paths,
  omitted sources and persistent stores, external services, unsupported cluster
  state, and operator-provided prerequisites. A SQL dump does not cover all
  contents of its physical volume. Unknown/unclassified coverage blocks calling
  a point app-recoverable; partial points remain explicitly labeled partial.

Do not restore source host IDs, Docker names, live deployment journals, stale
bindings, operation workers, or in-flight release execution. They are provenance,
not instructions to replay. Backup policy may be included as configuration, but
schedules stay disabled after restore.

## Repository, security, scheduling and retention

Use a mature encrypted backup/storage engine rather than invent encryption,
deduplication or a remote repository format. Restic is a candidate, not a selected
or integrated dependency. Backend selection must demonstrate the commit,
verification, locking, streaming, credentials and retention contract for all
three methods. Begin with one off-host backend, not an arbitrary plugin API.

The existing bounded metadata protocol stays metadata-only: do not embed dump,
image or secret bytes in status JSON or a build-context bundle. Backup data needs
an authenticated streaming/file-transfer path with separate byte/deadline limits,
private staging and content verification; cap manifests and paginate catalogs.

Repository definitions and credentials live in a **new server-level backup
configuration boundary**, separate from app runtime secrets. Do not inject them
into application containers or source YAML. Import/configure credentials through
private input; do not expose them in argv, URLs in diagnostics, or ordinary status.
Repository identity is pinned and rechecked; an endpoint change is not adoption
permission. Recovering the repository must not require the lost server registry.
Keep repository location/access instructions and decryption material independently
available; losing the only decryption key means losing the backups.

Encrypt source artifacts, images, manifests and secret material before off-host
storage. Bound root-private local staging and cleanup it after confirmed commit;
this is not a promise of encrypted local staging or secure erasure. Preflight
estimates and runtime byte limits must prevent filling the host disk. Helper CPU,
memory, time and staging budgets are explicit, separate from application limits.
Helpers receive minimum mount/network/credential authority, no Docker socket,
and pinned tooling rather than executing arbitrary application scripts.

Treat dumps and archives as executable/untrusted input at restore boundaries.
Postgres restores can execute SQL chosen by the source; verify provenance and
require operator trust, not merely a valid checksum. Run import only in an
isolated recovery environment without unrelated secrets or host privileges.
Numeric file ownership is restored only in newly allocated owned storage; no
recursive ownership repair of an existing volume is authorized.

Scheduling is a separate explicit enable action tied to a plan revision, set,
repository and downtime approval. Use UTC initially, one in-flight capture per
app, bounded lock waits and no overlapping/catch-up storm after a missed run.
Record missed and failed runs. Refuse after identity/policy drift rather than
following a recycled service name. Installation, deploy and restored policy do
not implicitly enable a timer.

Retention is also separate authority: preview affected recovery-point IDs before
manual pruning; automated pruning requires its own approved policy. Preserve
explicitly pinned points and never remove the last completed point by normal
retention. Keep artifact references until every referencing point is removed.
App cleanup, source removal, destroy and host teardown never delete off-host
backups. Teardown must reconcile active backup suspension and unresolved private
staging rather than erase recovery evidence. Garbage-collect incomplete uploads
only after proving no active/uncertain operation can still commit them.

Encryption does not protect against an attacker who can delete the repository.
Prefer separate upload versus prune credentials and backend-enforced retention
or immutable storage where available; document limitations when the chosen engine
requires broader permissions. Do not describe an ordinary writable repository
as ransomware-proof.

## Restore is a separate preview-first operation

Proposed interaction, usable without the source server:

```sh
soar backup inspect --repository offsite --point POINT
soar restore --server recovery --app journal-recovered --repository offsite --point POINT
soar restore --server recovery --app journal-recovered --repository offsite --point POINT --yes
soar restore activate --server recovery --app journal-recovered --operation OPERATION --yes
```

1. Require an explicit initialized target and absent app identity, including no
   retained volume/secret registry under that name. Same app name is possible on
   a fresh server; on the original server use a new name. There is no overwrite,
   merge, volume adoption, or `--reuse-data` shortcut.
2. Verify repository identity, trusted provenance, every required object, formats,
   image/platform availability, disk budgets, secret requirements and coverage.
   Preview generated hostname changes, original domains, role/permission
   requirements and unresolved external dependencies. No latest-tag fallback.
3. Journal fresh app/service/volume/secret identities with old-to-new provenance
   mappings. Import each distinct required secret version and preserve per-service
   pins, even when they differ. Restore may not use ordinary deploy's implicit
   latest-secret selection. Persist captured desired configuration for explicit
   activation; do not replay the deployment journal or release directive.
4. Allocate empty, stopped targets. Files and SQLite populate fresh volumes with
   no application process running. PostgreSQL needs a running **isolated database
   engine** to import into: use a pinned, supported maintenance bootstrap recipe,
   restart disabled, no application network or published port, with SQL readiness
   verified. Initialize only that approved target volume/role/DB; bypass arbitrary
   service entrypoints and initialization-script directories. Stop the maintenance
   engine cleanly after import/verification and leave the target inactive. The
   recipe must be compatible with the captured database runtime image and numeric
   ownership; unsupported custom bootstrap requirements fail before target writes.
5. Verify restored artifacts and method-specific checks. Keep public routes off
   and background workers stopped. Starting an arbitrary application entrypoint
   can migrate data or send traffic, so even a validation boot is an explicit,
   journaled authorization step with declared service selection and networking.
6. Explicit activation approves selected restored runtimes and target routes,
   without release hooks. Avoid accidental source/target double processing: the
   operator must fence old writers/workers if the original server still exists.
   Original custom domains and external side effects need explicit approval;
   DNS cutover is manual. Recover first; deploy newer code/migrations separately.

Restore is journaled independently of capture. A lost import response must not
start a second importer. Resume inspects owned helpers and target state; a failed
non-idempotent import does not restart into partially populated storage. Mark it
incomplete and require an explicitly approved fresh target attempt, retaining
failed storage/provenance for diagnosis until separate cleanup. Losing local
restore evidence is not permission to adopt matching names or rerun migrations.

A restored queue can contain jobs whose external effects already occurred. The
Rails example's application and queue tables share one database snapshot; that
does not undo email, payment, webhook or object-store effects. Application
idempotency and an operator queue-recovery decision are still required.

## Status, success and acceptance criteria

Keep these separate: last attempted capture, last remotely committed point,
source capture age/interval, last failure or missed schedule, consistency and
coverage, unresolved downtime, artifact verification, and **last restore drill**.
Never convert upload success into a restore-tested badge. Report actual recovery
point age; a daily schedule is not a promise of at most 24 hours of data loss when
runs fail. Measure restore duration and agree acceptable loss/recovery time before
choosing schedules or introducing more complex backup methods.

Required implementation acceptance matrix (none run for this proposal):

| Exercise | PostgreSQL | SQLite | Files |
| --- | --- | --- | --- |
| Native correctness | Concurrent writes; real custom dump/import; roles/extensions/profile refusals | WAL and rollback-journal modes; concurrent writers; busy deadline; readonly/sidecar refusal; integrity checks | Frozen nested tree, binary/empty files, numeric permissions and timestamps |
| Shared-volume composition | Database plus uploads with all declared writers stopped | Journal DB plus uploads restored into one fresh volume | Reject overlapping paths, conflicting directory metadata and forbidden entries |
| Restore safety | No app workers/hooks; matching engine/role prerequisites; failed import isolation | No old sidecars or live overwrite; recorded UID/GID | Traversal/link/duplicate-path/expansion attacks fail without escaping new storage |
| Failure recovery | Lost start/result, dump failure, DDL conflicts | Interrupted helper, source replacement, incomplete output | Stop/restart/503 loss, force-stop refusal, reboot while suspended |

For **every** method test secret staging drift, missing pinned secrets, changed
service incarnations, unsupported tooling, interrupted upload/commit, corrupt or
missing objects, disk exhaustion, schedule overlap, retention failure and restore
without original host metadata. Test source failure after another source finished:
there must be no committed mixed-attempt set. Prove inactive services remain
inactive and unrelated retired containers never restart.

Live fixtures: use Rails/PostgreSQL for logical database recovery, Journal for
SQLite-plus-files, and a files-only fixture to prevent hiding file semantics
behind SQLite. Record checksums, rows and permissions, complete an off-host backup,
discard the disposable source host, and recover onto a separate fresh owned host.
Test local-build image recovery and mixed applied secret pins with newer pending
versions. Isolated test repositories are not evidence of a chosen cloud backend's
retention or outage guarantees; that needs a separate acceptance run.

Implementation order: (1) resolve the backend/helper/bootstrap feasibility gates;
(2) implement bounded configuration, preview and durable coordinator contracts
with all three capability shapes; (3) implement each method and fresh-target
restore, testing the common contract as each lands; (4) complete the destructive
fresh-host drills for all three; (5) enable scheduling/retention only after manual
recovery and interrupted-operation recovery are proven. Do not ship an advertised
universal backup command while only one method has actually been verified.

## Decisions still requiring implementation evidence

- Storage engine/backend and exact key, repository lock, commit and prune model.
- PostgreSQL single-owner inventory/role-attribute rules, privilege-approval UX,
  supported extensions, tool versions, and trusted isolated bootstrap profile.
  Reject unsupported layouts explicitly.
- Read-only SQLite helper behavior across real Docker mounts/locking and WAL
  modes. Do not relax storage safety to make a convenience path work.
- Archive format, file metadata support and exact resource/metadata size bounds.
- Durable state schemas, worker deadlines, source-selection parser and final CLI
  spelling. Existing v1–v8 deployment recovery must remain unchanged; new schema
  support must be explicit rather than treating future versions as compatible.

These are bounded feasibility/design gates, not permission to defer the SQLite
or files recovery contract until after a PostgreSQL-only abstraction ships.

## References

- [PostgreSQL pg_dump](https://www.postgresql.org/docs/current/app-pgdump.html):
  consistent single-database exports, scope and restore trust boundary.
- [PostgreSQL SQL dumps](https://www.postgresql.org/docs/current/backup-dump.html):
  cluster-global role/tablespace limitations and restoration prerequisites.
- [SQLite Online Backup API](https://www.sqlite.org/backup.html) and
  [backup locking](https://www.sqlite.org/c3ref/backup_finish.html).
- [SQLite read-only WAL](https://www.sqlite.org/wal.html) and
  [immutable URI warning](https://www.sqlite.org/uri.html).
