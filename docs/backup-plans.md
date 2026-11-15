# Shared backup plans

**Implemented: policy configuration, not application backups.** PostgreSQL,
SQLite and files share one plan schema and identity-bound preview/apply flow.
No source data is captured, no service is stopped, and no schedule, restore,
pruning or recovery-point listing is implemented. A configured plan is **not**
evidence that an application can be recovered.

First configure and verify an [encrypted repository](backup-repositories.md).
Plan configuration requires an owned ready host and a stable v7/v8 application
journal, created by a current identity-aware client. Older deployments remain
recoverable through their existing commands; plan setup does not silently upgrade
them. Use an explicit current-client deployment to establish stable service
identities before configuring a plan for an older application.

## Configure and inspect

Keep `backup.yaml` separate from `vamos.yaml`. For Journal, SQLite and uploaded
files can select different paths in the **same** service-owned volume:

```yaml
app: journal
sources:
  database:
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
  app-state:
    sources: [database, uploads]
    consistency: quiesced
    stop: [web]
    repository: offsite
```

Journal's deployed `web` service mounts `volume.name: data` at `/data`:
`journal.sqlite` therefore selects `/data/journal.sqlite`, and `uploads` selects
`/data/uploads`. Setup does not inspect path contents or existence.

For [Rails Ledger](../examples/rails-ledger/README.md), save this complete plan
as a separate file (for example `rails-backup.yaml`):

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
  app-state:
    sources: [primary]
    consistency: quiesced
    stop: [web, worker]
    repository: offsite
```

Rails Ledger has no uploads or application filesystem volume. Its application
and queue tables share `rails_ledger`. The stop list names application writers
(`web`, including its replicas, and `worker`), not the database or credential
provider: PostgreSQL must remain running for a future logical dump. Configuring
this policy neither stops writers nor grants permission for future downtime.
`restore: {profile: single-owner}` is **required**, not defaulted. An omitted or
zero PostgreSQL `port` defaults to 5432; nonzero ports must be 1–65535. These
selections do not prove SQL authentication or restore-profile support.

### Field reference

Selections resolve against the **effective deployed app on the selected server**,
not an adjacent local `vamos.yaml` or pending deployment changes.

| Field | Meaning |
| --- | --- |
| `app` | Existing deployed app; must equal `--app`. |
| `sources` keys | User-chosen source labels, referenced by `sets.*.sources`. |
| `service` | Deployed same-app service owning the volume or serving PostgreSQL. |
| `volume` | Deployed service's `volume.name`, not its mount path or Docker name. |
| `path` | Path relative to that volume's root. |
| `database`, `user` | Explicit PostgreSQL database and login role. |
| `credentials.service`, `credentials.env` | Deployed service and its case-sensitive secret-backed environment key. `POSTGRES_PASSWORD` is the key, **not** the secret name `database-password` or its value. |
| `sets` keys | Arbitrary group labels such as `app-state`; names are **not schedules**. |
| `sources`, `consistency`, `stop` in a set | Source labels, required consistency policy, and explicit writer services; see limits below. |
| `repository` | Verified server-scoped registered repository name (here `offsite`), not a URL or bucket. Register it on the selected server first. |

The credential service may differ from the PostgreSQL service. Setup verifies
that the selected environment key has an applied secret-backed mapping, not that
its password authenticates to PostgreSQL. Do not put credential values in YAML.

### Preview, replace and inspect

**Apply replaces the entire app-wide plan, unlike scoped deploy.** Omitted
sources and sets are not merged or retained in the policy. Use explicit
`--server`: project-directory bindings are ignored. Repository registration and
policy application do not create backups or authorize downtime.

```sh
# Inspect both the current policy and the fully resolved proposed policy.
vamos backup configure --server home --app journal -f backup.yaml --json

# Capture the file once, obtain a fresh preview, then apply that exact revision
# and resolved-identity digest using compare-and-swap.
vamos backup configure --server home --app journal -f backup.yaml --yes

# Read installed metadata from the host, without container or repository checks.
vamos backup plan --server home --app journal --json
```

`--app` is mandatory and must match the file. `-f` defaults to `backup.yaml`.
`--server` defaults to the registered default server; project-directory bindings
are deliberately ignored. Flags precede arguments; these commands take no
positional arguments. Both support `--executor PATH` and `--timeout DURATION`.
Configure preview prints current/proposed JSON metadata with a heading;
`--json` removes the heading, not a separate human-readable diff. Summarized
coverage changes and schedule/downtime previews are future work. There is no
interactive approval prompt. Without `--yes`, configure only previews;
`--yes` approves a **fresh** preview in that invocation, not a digest copied from
an earlier invocation. It authorizes policy replacement only, never downtime.

## What is pinned

The installed record binds:

- Each source and declared stop service to its immutable service incarnation.
- SQLite/file sources to their exact service-owned volume identity, name and mount.
- PostgreSQL credential selectors to the credential service's incarnation and a
  secret-backed environment mapping present in its effective deployed version.
- Each repository name to its verified full repository ID and public configuration
  digest. Repository credentials and passwords are never part of a plan record.

Preview inspects owned current runtime objects and their **applied** secret pins;
it does not substitute desired configuration or the newest staged secret version.
The record intentionally does not freeze a release/image or a secret version:
a future capture must resolve those again from the then-effective deployment.
Normal compatible deployments preserve these bindings. Removing/re-adding a
service, replacing a volume, or changing a repository identity invalidates a prior
approval. Apply also refuses if the installed policy revision changed. Replaying
an already-applied request is harmless only when the policy and freshly resolved
identities still match exactly.

Preview and apply share the existing host mutation lock with deployment operations.
Preview writes no plan record. Apply atomically replaces the entire app plan; it
does not merge omitted sources/sets, alter runtimes, or delete repository data.
Records are private, host-bound JSON under
`/var/lib/vamos/backups/plans/<app>.json` (`0700` directory, `0600` file). They are
retained after ordinary app destruction. `backup plan` reports this retained
policy, **not** current source readiness. A missing record reports revision `none`.
If the apply result is lost, inspect the recorded plan; do not assume no change.

## Selection limits and remaining proof

App, source, set, service, volume and repository names match
`[a-z][a-z0-9-]{0,62}`. PostgreSQL database/user names contain 1–63 ASCII letters,
digits, underscores or hyphens. Credential environment keys match
`[A-Za-z_][A-Za-z0-9_]{0,127}` (case-sensitive, at most 128 characters).

Plans are bounded to 16 KiB, 16 named sources and 8 sets. Installed records are
bounded to 24 KiB, including resolved bindings. YAML aliases, anchors, explicit
tags, nulls, duplicate/unknown keys and extra documents are rejected.

- SQLite and files select clean relative paths inside an owned volume. SQLite
  selects a file; files may select `.`. Paths are at most 512 bytes and 32
  components, clean and relative, with no trailing slash or backslash.
  `.` means the **whole volume**, not “everything else”; selecting it alongside
  SQLite on the same service/volume in a set is rejected as overlap. Traversal,
  globs and environment expansion are unsupported. Selections within a set cannot overlap, including SQLite's
  reserved WAL, shared-memory and rollback-journal siblings.
- `online-per-source` accepts PostgreSQL and SQLite without a stop list. It makes
  no cross-source atomicity guarantee. Files require `quiesced`.
- Every set requires nonempty, unique source references and a repository name.
  Stop lists have at most 8 unique services.
- A quiesced set declares a nonempty list of its stop services explicitly and
  must include each selected SQLite/file owner. The selected PostgreSQL service remains running;
  at most one database is selected per PostgreSQL service in a set. A stop list
  cannot prove that all database writers are stopped, especially external writers.
- PostgreSQL sources require a singleton service and an applied secret-backed
  credential selector. SQL authentication, engine/tool versions, ownership,
  privileges, ACLs, extensions and bootstrap reproducibility are **not yet tested**
  by this command.
- Coverage is unclassified. Unselected sources, undeclared stores and external
  dependencies are not inferred or protected. Future capture must explicitly
  report omissions before claiming completeness.

The shared coordinator is not a capture engine. Durable reversible suspension,
source capture helpers, encrypted manifests, exact publication verification,
fresh-target restore and activation approval remain separate implementation gates.
The [capture feasibility notes](backup-capture-gates.md) record reviewed constraints
and the runtime experiments still required; they do not claim those tests passed.
