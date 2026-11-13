# Persistent local volumes

The storage lifecycle supports Dockerfile-built public/private services with a
singular `volume: {name: data, mount: /data}`: one per service, no sharing across
services or apps. Volumes are owned, named Docker `local` volumes, not host bind
mounts, anonymous storage, or external adoption. An image-declared `VOLUME` is
allowed only at the exact configured singular owned named-volume mount; additional
or anonymous image mounts remain rejected. One-off `run` rejects all image-declared
volumes, so use a volume-free client image.
The runnable [journal demo](../examples/journal/README.md) stores SQLite and
uploads under one mount. It is unauthenticated and not a production application.

## Identity and execution

Volume registry provenance and exact Docker ownership/driver checks establish
identity. Bindings are immutable from creation intent, including failed initial
deployments: service, logical name and mount path cannot silently change. Adding
a new service is supported; converting an existing service between stateless and
volume-backed requires a future explicit migration workflow.

All images and storage are checked before retiring active services. Candidates
are created stopped with verified explicit mounts and `volume-nocopy`. Changed
volume-backed services always stop/start, never overlap writers. For public
services, a journaled, verified **durable HTTP 503 route** precedes retirement;
readiness and route activation end that interval. Private services retain their
process-only startup observation, not database readiness. Unchanged services do
not restart. Whole-app deployment remains non-atomic across services.

Execution intent records that data **may have mutated** before code can run.
Failed startup retains storage and keeps the old writer retired; there is no
automatic code/schema rollback. Resume uses captured inputs. Explicit supersede
uses corrected code against the same storage, retaining binding, authorization,
and possible-mutation evidence even when the first deploy never became ready.
Other apps' route updates must preserve settled 503 routes.

A missing registered volume fails closed before application execution rather
than becoming an empty database. This is **not a continuous storage-integrity
monitor**. Normal Docker reference guards prevent ordinary volume removal while
containers (including stopped containers) refer to it. Out-of-band filesystem
corruption or privileged host/daemon modification is not covered; Docker restart
is not a data-integrity check. On ordinary reboot current containers use their
restart policies; retired writers remain restart-disabled.

## Permissions, retention and visibility

Applications initialize new empty storage and choose numeric UID/GID permissions.
Soar never silently copies, migrates, or recursively chowns existing data.
Entrypoints must tolerate repeated initialization after interrupted startup.

Status exposes logical names, bindings and immutable identities, missing/retained
state, and whether execution may have mutated data—not data contents. Completed
release snapshots hold references, never snapshots of database/upload contents.

Cleanup and destroy retain every volume and its ownership evidence, including
failed-startup data. Destroy removes routes and verified containers/network, not
data or secrets; incomplete work must first reach a supported maintenance state.
Redeploying a destroyed app requires explicit `deploy --reuse-data` for exact
retained bindings. This acknowledges reuse, not schema compatibility or adoption
of arbitrary data. Ordinary updates/resume do not need that flag. Host teardown
refuses retained registry state even when recorded storage is missing.

Ready and incomplete legacy v1/v2 journals remain recoverable under their original
rules. Storage-aware operations use journal v3; ready legacy state transitions on
an explicit new deployment, not by weakening old no-mount invariants.

## Scope and verification

There is no data deletion command, backup, restore, preparation command,
replication, or data rollback in this milestone. Local persistence is not a
backup. The [backup/recovery proposal](backups.md) designs explicit PostgreSQL,
SQLite and files methods without changing this implemented boundary; its YAML
and commands are not supported yet. Prebuilt images, manual updates, scoped deploy and noninteractive one-off
migration commands are now implemented; see [PostgreSQL](postgres.md). Custom
domains are supported through [normal deployment](domains.md). Preview-first
[service removal](services.md#remove-a-service-not-the-app) retains data and secrets;
re-add needs `--re-add --service NAME --yes` and, for retained volume data,
`--reuse-data`. New service incarnations invalidate old directory bindings. [Code/config rollback](rollback.md)
for a volume-backed automatic service requires `--service NAME --allow-downtime --yes`
and an operator decision that captured code is compatible with current data. Manual
services remain excluded. It uses single-writer stop/start (durable HTTP 503 for
public services), never restores volume contents or undoes schema changes. Existing stateless candidate routing, secrets pinning, bounded
retention and recovery guarantees remain in place.

The [original proposal](volume-proposal.md) is superseded by this implemented
lifecycle boundary; its acceptance scenarios remain useful validation targets.
Historical test evidence elsewhere documents the versions tested then, not proof
of storage coverage.

### Verification — 2026-11-12 (earlier volume milestone)

These results predate PostgreSQL workflow support; image-declared `VOLUME` was
unsupported in that milestone. The current exception above does not imply new
live verification.

Full formatting/vet/race-test checks and native/Linux amd64/arm64 builds passed,
as did Windows test cross-compilation and the journal example's Python tests.
Regression coverage includes public/private volume replacement, stopped-candidate
and 503 ordering, unchanged deploys, lost Docker mutation responses, missing or
replaced volumes, immutable bindings, failed-first-deploy recovery, crash-loop
supersede, activation cancellation from 503, and other apps preserving 503 routes.

On the disposable Ubuntu 26.04 amd64 host (Docker 29.1.3, Caddy 2.6.2), a public
SQLite/uploads app retained its database rows and upload bytes across:

- A changed deployment on the same immutable volume identity.
- Deliberately failed startup **after a committed database write**, public 503,
  and corrected supersede. The retired writer did not restart.
- Cleanup and a host reboot; the current writer restarted and the old writer
  remained stopped with restart disabled.
- Destroy and redeploy with `--reuse-data`. Redeploy without the flag was refused.

Real Docker inspection confirmed stopped creation with `volume-nocopy`; both
created and stopped container references prevented ordinary volume removal.
The probe app was finally destroyed: its route, containers and app network were
removed, while its volume and private registry were intentionally retained.
The pre-existing hello app remained intact and healthy after the reboot.

This is not verification of hard power loss, filesystem corruption, native
Windows clients, arm64 hosts, or backup/restore. Missing-volume refusal is also
covered by injected boundary tests, not destructive loss of the retained probe's
data.
