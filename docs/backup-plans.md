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

Keep `backup.yaml` separate from `soar.yaml`. For Journal, SQLite and uploaded
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
  daily:
    sources: [database, uploads]
    consistency: quiesced
    stop: [web]
    repository: offsite
```

Paths must match your application's layout; plan setup does not inspect their
contents or existence. The [three-method design](backups.md) also shows PostgreSQL
sources, credential selectors and a PostgreSQL-plus-uploads set. PostgreSQL uses
`restore.profile: single-owner` (the default), and port 5432 by default. Those are
policy selections, not proof of SQL authentication or restore-profile support.

```sh
# Inspect both the current policy and the fully resolved proposed policy.
soar backup configure --server home --app journal -f backup.yaml --json

# Capture the file once, obtain a fresh preview, then apply that exact revision
# and resolved-identity digest using compare-and-swap.
soar backup configure --server home --app journal -f backup.yaml --yes

# Read installed metadata without inspecting containers or contacting storage.
soar backup plan --server home --app journal --json
```

`--app` is mandatory and must match the file. `-f` defaults to `backup.yaml`.
`--server` defaults to the registered default server; project-directory bindings
are deliberately ignored. Flags precede arguments; these commands take no
positional arguments. Both support `--executor PATH` and `--timeout DURATION`.
There is no interactive approval prompt. Without `--yes`, configure only previews;
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
`/var/lib/soar/backups/plans/<app>.json` (`0700` directory, `0600` file). They are
retained after ordinary app destruction. `backup plan` reports this retained
policy, **not** current source readiness. A missing record reports revision `none`.
If the apply result is lost, inspect the recorded plan; do not assume no change.

## Selection limits and remaining proof

Plans are bounded to 16 KiB, 16 named sources and 8 sets. Installed records are
bounded to 24 KiB, including resolved bindings. YAML aliases, anchors, explicit
tags, nulls, duplicate/unknown keys and extra documents are rejected.

- SQLite and files select clean relative paths inside an owned volume. SQLite
  selects a file; files may select `.`. Traversal, globs and environment expansion
  are unsupported. Selections within a set cannot overlap, including SQLite's
  reserved WAL, shared-memory and rollback-journal siblings.
- `online-per-source` accepts PostgreSQL and SQLite without a stop list. It makes
  no cross-source atomicity guarantee. Files require `quiesced`.
- A quiesced set declares its stop services explicitly and must include each
  selected SQLite/file owner. The selected PostgreSQL service remains running;
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
