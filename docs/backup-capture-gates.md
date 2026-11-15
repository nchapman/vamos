# Capture implementation gates

These are engineering constraints from the source/documentation review following
shared-plan implementation, **not completed runtime experiments**. No capture or
restore support is established by configuring a plan. All three methods must pass
these gates before the backup/recovery milestone can be called complete.

## SQLite

Use a separate, pinned, network-disabled helper with a direct SQLite Backup API
wrapper; keep CGO out of the Vamos executor. The inspected `go-sqlite3` v1.14.32
wrapper collapses `SQLITE_OK`, `SQLITE_BUSY` and `SQLITE_LOCKED` in `Backup.Step`, so
it cannot directly implement distinct busy-result accounting. SQLite 3.50.4 was
the source-inspection baseline, not a production security-release recommendation.
Select a maintained release and pin source, build inputs and both architecture
artifacts before shipping.

- Mount the actual source volume read-only, preserving sidecar visibility and
  local-filesystem locking. Never use `immutable=1`, a source-write fallback, or
  a main-file-only live copy. No application entrypoint or repository credentials.
- Read-only WAL access is conditional on usable engine/sidecar/locking state.
  Existence checks are insufficient. Test writer closure/reopening, missing,
  stale and unreadable sidecars, concurrent checkpoints and WAL truncation on
  actual Linux Docker mounts; do not invent a writer-liveness requirement.
- Require observed `SQLITE_DONE`, successful finish and closure, then independently
  open a standalone destination and pass integrity and foreign-key checks.
  Successful `finish()` alone does not prove completion. Bound total work, busy
  results and elapsed time; there is no public exact restart counter.
- For quiesced recovery, freeze declared writers and copy recovery-relevant main,
  WAL and rollback-journal files into private scratch. SHM is not authoritative
  recovery state. Refuse out-of-scope super-journal dependencies **before** opening
  scratch: relocation can otherwise change hot-journal interpretation. Recovery
  writes belong only in scratch, followed by a new standalone Backup API output.
- Native Linux amd64/arm64 acceptance must cover committed WAL-only rows,
  uncommitted writes, writer churn, lock/budget exhaustion, scratch recovery,
  source non-mutation, path substitution, corrupt data, disk-full and helper death.

References: [read-only WAL](https://www.sqlite.org/wal.html#read_only_databases),
[Backup API](https://www.sqlite.org/c3ref/backup_finish.html),
[hot journals](https://www.sqlite.org/lockingv3.html#hot_journals), and inspected
[3.50.4 locking source](https://github.com/sqlite/sqlite/blob/version-3.50.4/src/os_unix.c).

## PostgreSQL 17 single-owner profile

Treat `single-owner` as a closed, versioned profile with a reproducible bootstrap
recipe, not permission to import arbitrary role graphs or server configuration.
The specialist assessment is a proposed implementation gate; its PostgreSQL API
claims still need authoritative documentation checks and executable fixtures.

- Initial scope: one selected database on a singleton PostgreSQL 17 service, one
  application login owner, default tablespaces, no application-role memberships
  or role/database overrides, and only explicitly supported baseline extensions
  (initially `plpgsql`). Preserve supported ACLs, grantors, grant options, default
  ACLs and role attributes; reject unsupported dependencies rather than omit them.
- Account for `public`/`pg_database_owner` and initialization-owned baseline
  objects. Test both an application initialization superuser (Ledger) and a
  separate bootstrap administrator with a nonprivileged app owner. A temporary
  import identity must not silently become owner of baseline objects.
- Prove the captured password was used, not merely that a connection succeeded.
  Verify libpq 17's SCRAM `require_auth` and password-use reporting; reject trust,
  peer and ambient credential fallback as password proof. Verify expected session
  user/database and reauthenticate after restore. Do not infer equality between
  independently pinned app and database secrets, including Ledger's two secrets.
- Share an exported read-only repeatable-read snapshot between supported catalog
  inventory and `pg_dump`. Document the administrative-stability assumption:
  external role/password, ACL, DDL and configuration changes are not fenced by
  Vamos's host lock. Sequence state is not ordinary MVCC table data. Detected drift
  and dump/lock failures must fail capture, not trigger automatic migration/retry.
- Fresh storage only. Invoke approved `initdb` and `postgres` directly, bypassing
  image entrypoints/init scripts. No published ports, application network, release
  hooks or workers. Restore with ownership/ACLs, fail on the first import error,
  verify normalized catalogs and effective permissions, then stop cleanly.
- Privileged-role approval binds exact attributes, recovery point and restore
  attempt, before target initialization. Approval does not widen the profile.
  Dumps can execute source-controlled SQL with importer privileges: source trust
  and isolation remain necessary even for a nonprivileged app fixture.

Acceptance must include positive/negative authorization, new-object default ACLs,
wrong/staged passwords, trust-auth refusal, both baseline recipes, unsupported
role/extension/tablespace dependencies, concurrent transactional invariants,
interrupted imports and entrypoint/worker sentinels.

## Restic 0.19.1 publication

The current backend runner is for bounded repository metadata only. It drops
stdout on failure and has a two-minute deadline/64 KiB buffers; do not reuse it
unchanged for publication or artifact streaming.

- Exit `3` can follow publication of an **incomplete snapshot**, including a JSON
  summary with an ID. A lost result can occur after publication. Preserve process
  outcome independently of the bounded JSON message stream. Known partial results
  are rejected; an uncertain result needs independent sealed-artifact verification.
- Restic acknowledges pack/index uploads before saving the snapshot object. That
  is not full remote artifact readback. A restic snapshot may be discoverable
  before Vamos may advertise it as a verified recovery point.
- Backup locks are non-exclusive. Vamos must durably own execution identity and
  prove a previous upload worker dead before retrying. Tags, PID, lock absence or
  age are not idempotency/death proof. Never automatically unlock.
- Read-only `--no-lock` discovery/dump is possible, but offers no retention lease
  against concurrent pruning. Destructive maintenance needs separate coordination.
- Discover using one AND tag group, full IDs and bounded candidate/time/output
  limits. Filtering scans snapshots; there is no bounded server-side pagination.
  Exceeded limits mean indeterminate, not absent; multiple matches mean conflict.
- Verify exact sealed manifest bytes before interpreting them. Then stream every
  referenced artifact, requiring exact byte count/hash and successful read exit.
  Bind the receipt to repository ID, full snapshot ID, manifest digest, level and
  time. Verify file metadata through the sealed format as well as payload hashes.
  Full readback is still not a PostgreSQL import or a restore drill.

Pinned upstream evidence: [backup outcomes/publication](https://github.com/restic/restic/blob/v0.19.1/cmd/restic/cmd_backup.go),
[archiver ordering](https://github.com/restic/restic/blob/v0.19.1/internal/archiver/archiver.go),
[lock modes](https://github.com/restic/restic/blob/v0.19.1/cmd/restic/lock.go),
[JSON stream](https://github.com/restic/restic/blob/v0.19.1/internal/ui/backup/json.go),
[tag semantics](https://github.com/restic/restic/blob/v0.19.1/internal/data/snapshot.go),
and [exact-file dump](https://github.com/restic/restic/blob/v0.19.1/cmd/restic/cmd_dump.go).

## Shared coordinator before methods

Implement durable reversible suspension, session drain/fences, immutable operation
and capture-attempt identities, bounded private staging, sealing, worker-death
reconciliation and cleanup before permitting publication. Existing deployment
stop/retirement routines are not a reversible suspension protocol. Fresh-target
restore and explicit activation remain separate from capture. No automatic DNS,
workers, migration hooks, source repair or shared writable service volumes.
