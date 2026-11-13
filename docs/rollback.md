# Rollback: captured code and configuration

`soar rollback` is preview-first. It restores eligible services from captured
release history, without a build, pull, or source archive. It is not automatic
failure recovery, database recovery, or an atomic transaction across services.

```sh
soar rollback                         # preview target, exclusions and downtime
soar rollback --yes                   # apply eligible automatic services
soar rollback --service web           # preview only web
soar rollback --service web --yes     # apply only web

# Outside a project: no local source/config required
soar rollback --app ledger --server home
soar rollback --app ledger --server home --service web --yes
```

## Selection and immutable inputs

- A ready app-wide rollback targets its `PreviousCompleted` snapshot. In v7,
  scoped `--service NAME` instead targets that service's `Previous` distinct
  successfully completed release; other services' deploys and no-ops do not
  evict it. An incomplete deployment targets the last `Completed` stable baseline,
  not the release before it. No useful predecessor means rollback fails.
  Legacy journals retain their original snapshot semantics. See
  [independent services](services.md) for the v7 contract and verification boundary.
- Removal filters captured snapshots and records removed membership. Rollback
  never resurrects a removed service; intentional re-add creates a new incarnation.
- Whole-app rollback selects automatic, volume-free services. `--service NAME`
  narrows selection; it does not override protection. Manual services are never
  rolled back, even when explicitly selected or when the target was manual.
  Use an intentional scoped deploy with a vetted image/config instead.
- Excluded and unselected services retain their **actual current** configuration
  and runtime, including already-promoted services in a partial deployment.
  A service absent from the target is excluded, not implicitly removed.
  An excluded service with incomplete runtime recovery blocks rollback; resume
  or supersede it first rather than recording a false healthy release.
- Targets use per-service captured immutable local image IDs, configuration and
  secret-version references. Selected images and secrets are checked before
  retiring runtime. Missing immutable inputs fail closed: no rebuild, pull,
  mutable-tag resolution, or fallback to staged secrets. Keep host image storage
  and referenced secret versions intact.
- Captured credentials may have been revoked. Rollback warns about this; restoring
  a value cannot restore its external validity.

## Volume-backed automatic services

Explicitly select a volume-backed automatic service, acknowledge data
compatibility, and authorize downtime when applying:

```sh
soar rollback --service web --allow-downtime       # preview
soar rollback --service web --allow-downtime --yes # apply
```

`--allow-downtime` is for scoped rollback; it does not authorize manual-service
rollback. Applying a volume-backed rollback requires both this flag and `--yes`.
Storage identity and public/private service kind cannot change. Replacement uses
single-writer stop/start; public volume-backed services have durable HTTP 503
routing until readiness and activation. Private replacement also has downtime.

**Rollback cannot undo volume writes, database schemas, committed SQL, external
side effects, or one-off commands.** Old code must tolerate current data. Use
backward-compatible migrations; do not automatically start an old database binary
against data modified by a newer one. `soar run` remains noninteractive, with no
stdin/TTY and no automatic retry or undo. See [PostgreSQL](postgres.md) and
[persistent volumes](volumes.md).

## Recovery and journal compatibility

Pending routing promotion/retirement transitions require `soar resume OP` first.
Rollback does not bypass active-operation or uncertain-runtime recovery guards.
Interrupted rollback uses the normal `soar resume OP` command and captured inputs;
do not add new rollback selection or authorization flags to resume.

For no-alias apps, rollback writes a **v5 rollback-only journal**. Older v1–v4 journals remain
readable under their recovery rules. Legacy v1 history lacks captured service
configurations and cannot be rolled back until modern release history exists.
A normal deploy after no-alias v5 rollback returns to the v4 deployment journal format.
Domain-capable apps use v6 for both deployment and rollback and stay v6 after
alias removal to preserve history. Versions 1–5 strictly reject aliases.

Public stateless replacement retains the normal readiness/routing behavior;
private services use stop/start. App-wide completion is not atomic, and rollback
can itself fail or be interrupted. Inspect status and resume rather than assuming
all services changed together. See [multi-service recovery](multi-service.md).

## Verification for this milestone

Live acceptance used disposable ledger app `rbprobe` on the existing Ubuntu
26.04 amd64 host, with Docker 29.1.3 and Caddy 2.6.2:

- Bootstrapped PostgreSQL and ledger v1, then stored a row through the public API.
- Ran an explicit backward-compatible migration adding a nullable `note` column,
  deployed v2, and verified the public readiness response reported v2.
- Ran rollback preview from an empty local directory, with `--app`/`--server`
  and no project configuration. Preview and a rejected explicit manual-db
  rollback left the app journal byte-for-byte unchanged and v2 serving.
- Staged a different synthetic password without applying it. Whole-app rollback
  excluded db, restored web's exact v1 image and captured secret version, and
  left the newly staged version pending. Readiness reported v1 and the API
  returned the original row.
- Verified PostgreSQL's Docker container ID, start timestamp and volume creation
  timestamp were unchanged. SQL inspection confirmed both the added column and
  original row remained. Docker's image event history was empty over the
  preview/apply window: no image build, load or pull occurred.
- Cleanup preserved rollback inputs. A subsequent scoped web rollback restored
  v2 from retained history while the row remained readable.
- Destroy removed the probe's containers, network, routes and captured operation
  files. Hello's original containers and HTTPS route stayed healthy. Synthetic
  secret versions and volume `soar-vol-f98f97740bf1293515fe6b5b34f129dd` were
  intentionally retained with `/var/lib/soar/volumes/rbprobe.json` provenance;
  previously retained PostgreSQL data was also left intact.

The first public probe encountered certificate provisioning delay; HTTPS worked
once issuance completed. Deployment still does not claim DNS/certificate readiness.

Local verification passed formatting, vet, the full race-enabled Go suite,
native/Linux amd64/arm64 builds, Windows cross-compilation, both example test
suites, and Markdown-link/complete-YAML checks. Tests cover distinct images for a
shared source key, captured secrets, missing images, excluded services, malformed
captures, partial-deployment targets, failed readiness, interrupted rollback and
volume-backed single-writer recovery. Review found no actionable issues.

Live volume-backed code rollback, forced worker termination, hard power loss,
private-registry outages, native Windows execution and arm64 host execution were
not exercised. Backup/restore remains deferred. This evidence does not verify custom domains.

## Custom-domain ownership

Rollback restores captured aliases for selected services, retaining their generated
URLs. Preview shows current and target custom domains. Historical snapshots do
not reserve removed aliases: rollback rechecks server-wide ownership and refuses
an alias reclaimed by another service/app before making changes. Release that
alias from its current owner with a successful deployment before retrying, or
use a forward deploy with corrected aliases instead. Do not edit journals or
Caddy manually. See [domain workflow and collision recovery](domains.md).
