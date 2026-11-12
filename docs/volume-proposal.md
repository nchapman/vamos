# Proposal: first volume-backed deployment milestone

**Status: superseded by the implemented [storage lifecycle](volumes.md).**
The text below preserves the design rationale and acceptance targets, not a pending
authorization gate or verification report. End-to-end lifecycle implementation is
authorized; backup/restore/preparation remain deferred. Storage uses journal v3,
with ready/incomplete v1/v2 recovery preserved.

## Recommendation

Add persistent local storage for small Dockerfile-built applications, beginning with SQLite and uploads. Keep the normal whole-app deploy workflow, but require **single-writer stop/start replacement**, including public services. A volume is live mutable data, not part of an immutable release snapshot.

Recommended first boundary:

- At most **one named volume per service**, eight desired volumes per app. SQLite and uploads can share directories under one `/data` mount. Use a singular `volume` object; lists and the plural `volumes` key are rejected rather than advertising unsupported multiple mounts.
- Ordinary Docker `local` volumes only, with no driver options, host bind mounts, anonymous volumes, subpath mounts, or sharing between services/apps. Continue rejecting image-declared `VOLUME`, even when its path resembles a declared mount.
- Keep `update: auto`. Changed volume-backed services have downtime; unchanged services do not restart. Private service observation remains process-only, not database readiness.
- A service receives its volume binding when volume creation intent is recorded, not when its first deployment becomes healthy. Reject adding/removing/renaming a mount, changing its path/owner, or converting an existing service between stateless and volume-backed. Such changes need an explicit migration workflow later; adding a new service remains possible.
- No data deletion command, snapshots, automatic backups, restore automation, quotas, replication, database management, or code/data rollback in this milestone. Backup/restore design and runbooks are deferred, not an implementation or release gate for this milestone.

## Configuration and user experience

Use the configuration shape already accepted by local validation:

```yaml
app: notebook
services:
  web:
    build: {context: ., dockerfile: Dockerfile}
    env:
      DATABASE_PATH: /data/notebook.sqlite
    http: {port: 8080, health: /healthz}
    volume:
      name: data
      mount: /data
```

Before mutation, show the resolved target, created/reused volume identity, affected services, and downtime. After image resolution, identify which services actually require replacement. Do not describe the whole app as unchanged merely because its previous completed snapshot is retained.

```text
home / notebook / web
Volume data -> /data: reuse <volume-id>
Update: stop/start; public requests will receive 503 during replacement
Data is retained. Starting the new image may change it; no automatic rollback.
```

Normal deploy remains noninteractive and does not require confirmation on every data-backed code update. Resume/supersede remain explicit operations and must show the same data warning. Status should distinguish `serving`, `unavailable/retired`, `candidate starting`, and `route switch pending`, with volume names/IDs and whether replacement execution may already have changed data. Do not label the old retired container available or imply that a completed app snapshot includes a database snapshot.

Expose volume ownership, binding, identity, and retained/missing state through `status`, never data contents. Initial deploy creates its own new volume automatically. No separate volume preparation, listing, backup, or restore commands are proposed for this milestone.

The `soar deploy --reuse-data` flag acknowledges reuse after destroy. It authorizes attaching exact retained identities, never arbitrary adoption, deletion, renaming, or schema compatibility. Record that authorization in the operation so resume does not ask again. It is not needed for ordinary updates of an existing service or continued recovery of its captured operation. An explicitly named supersede inherits attachment authorization for the same captured volume identities, including after a failed **first** deployment. Preserve the binding and possible-mutation record independently of whether `Current` ever existed. Supersede cannot silently turn that failed first service into a fresh volume or a stateless service; correcting a binding mistake requires a separate identity/migration decision.

## Stable ownership and creation

Maintain a private, versioned per-app volume registry separate from deployment history, for example `/var/lib/soar/volumes/<app>.json`. It survives destroy. Each record binds:

- Host identity, app, service, logical volume name, and absolute container mount path.
- A random immutable volume ID and opaque Docker name, such as `soar-vol-<id>`; do not derive ambiguous names by concatenating app/service labels.
- Creation operation and state (`planned` or `created`). Runtime/release records reference that immutable ID.

Require both registry provenance and exact Docker labels, local driver/scope, and empty driver options. Matching names or labels alone are not adoption authority. Never derive permissions or deletion authority from Docker's host mountpoint path. Reject unknown containers referencing the volume, including stopped containers; retained, verified Soar containers from the same service are expected references.

Creation is an idempotent journaled operation: persist the allocated identity and creation intent, create with exact labels, inspect it, then persist `created` **before creating any application container**. A lost create response can adopt only that recorded intent. A `created`/previously attached volume that is now missing must fail closed—not become a fresh empty database. A name collision, unexpected attachment, driver drift, or inconsistent registry must block work before retiring an active service.

Bound retained metadata independently of desired mounts: propose 64 records per app and a 128 KiB registry limit. Exhaustion fails without pruning data. Keep the existing 64 KiB response boundary; inspection output must remain bounded and offer narrow queries if needed. Capacity limits are not disk quotas or guarantees about application growth.

## Replacement and public routing

Retain the existing whole-app ordering: build/inspect all required images first; missing private services, public services, then changed existing private services, alphabetically within each group. Do storage/attachment validation before retiring **any** active service. This does not introduce dependency ordering or transactional deployment.

For each changed volume-backed service:

1. **Prepare without executing.** Verify volumes; create the candidate stopped with restart disabled. Inspect its image/configuration and exact mounts before executing application code. Use explicit named `--mount` arguments with `volume-nocopy`; serialize mount arguments correctly rather than interpolating CSV options. Recheck known volume identities after container creation because Docker can implicitly create a missing named volume.
2. **Make a public service explicitly unavailable.** Journal, apply, and prove a Caddy route returning HTTP 503 for its reserved hostname before stopping the old process. Keep the hostname reserved. A failed/uncertain route withdrawal does not authorize retirement. Private services skip this step.
3. **Retire the old writer.** Persist retirement intent, disable restart, stop with the existing 30-second grace period, and prove it stopped. Verify that no other container can write this service's volume: every prior container is stopped with restart disabled, and unexpected references block startup. A timeout may force termination; applications must tolerate ordinary crash recovery.
4. **Record possible data mutation before execution.** Persist the candidate's captured image/config/secrets/volume IDs and execution intent before enabling restart or starting it. Even an interrupted start may already have run an entrypoint or migration. Never infer unchanged data from a failed health check.
5. **Start and qualify.** Start/adopt the verified candidate. Public candidates keep the existing no-alias initial readiness, alias admission, and readiness recheck; private candidates get their service alias and startup observation. The retired writer must stay stopped on every retry.
6. **Activate.** For public services, journal and prove the 503-to-candidate Caddy switch. Record the effective version only after the appropriate success condition. Retain old stopped containers/images/configuration according to normal cleanup policy, not as permission to restart them.

The 503 state must be a **durable per-service route state**, separate from `Current.Port` and desired configuration. Other app deployments must preserve it, not accidentally reconstruct the retired endpoint. A settled 503 route is not an unresolved promotion fence: other apps can continue once the worker releases the host lock. Uncertain route writes still require reconciliation.

## Recovery contract

| Boundary/failure | Required behavior |
| --- | --- |
| Build/image/storage preflight failure | Leave active processes/routes alone. Newly prepared volumes may remain; never compensate by deleting data. |
| Public unavailability switch uncertain | Keep old writer intact; reconcile file/live routing before retirement. |
| Old stop response lost | Reinspect ownership, restart policy, and running state; do not start a second writer. |
| Candidate create/start response lost | Adopt only the captured container and exact volume IDs; inspect before any retry. Mark data possibly changed once execution intent was recorded. |
| Candidate startup/readiness fails | Keep data and the old writer retired. Public service stays 503. Resume retries captured inputs; corrected supersede stops/removes the failed candidate before starting another writer on the **same** volume. |
| Final activation response lost | Reconcile recorded 503/new routes. Do not automatically return to the old binary, even if a previous release is retained. |
| Supersede after data mutation may have begun | Preserve retirement and storage identity; explicitly warn that corrected code must tolerate current data. Supersede is not a schema undo. |
| Normal host reboot | Current containers recover through their configured restart policy; retired containers remain restart-disabled. Verify this with real Docker. Soar is not a continuous storage-integrity monitor. |

Extend the existing proven-old promotion cancellation rule to the recorded **503** route, not to the retired binary's old endpoint. If routing is already new or uncertain, require reconciliation rather than assuming the candidate is unserved. Clearing a route intent must never clear writer retirement or the record that data may have changed.

Completed app snapshots retain code/configuration/secret/volume references only. A partial operation does not advance them. Readiness and filesystem existence cannot prove data integrity, compatibility, or successful business-level migrations.

## Permissions, retention, and destruction

Use `volume-nocopy` to avoid implicit image-file population. Applications own initialization and numeric UID/GID permissions; Soar must not recursively chown, chmod, copy, or migrate an existing volume. The first runnable example needs an explicit initialization pattern that works with a new root-owned volume and a least-privilege application process. Initialization and migrations must be restart-safe: single-writer replacement does not make startup execute exactly once, and resume or Docker restart may repeat it after partial mutation.

Cleanup never removes volumes, never uses `docker rm -v`/volume prune, and does not erase registry provenance. Known stopped containers can be removed without deleting their named volume. Never stop or remove an unknown container merely because it references Soar data.

Destroy withdraws all public routes, disables restart, proves owned writers stopped, and removes verified containers/network. It retains **every volume and secret**, even if the app never became ready. As today, unresolved deployment work must first be recovered to a supported maintenance state; do not quietly broaden incomplete-app destruction in this milestone. Failed initial deployments may therefore leave retained storage and require recovery before ordinary destroy.

Same-name redeployment may reuse data only with the exact stored service/name/path binding and explicit `--reuse-data` authorization. Omitted retained volumes remain retained, not deleted. App/service/volume renaming is not migration. Host teardown/purge must refuse retained volume registry state as well as actual Docker volumes, including when a recorded volume is missing. No first-slice command deletes either data or its ownership evidence.

## Deferred backup/restore work and storage limits

Persistence on one host is not a backup. This milestone neither chooses a backup approach nor implements or verifies backup/restore workflows. Those need a separate design discussion; the restore-driven preparation command is deferred with them. Do not claim retained volumes protect against host loss or data corruption.

Missing registered storage fails closed before execution. This is not continuous
storage-integrity monitoring: ordinary Docker volume removal is guarded while
containers reference it, including stopped containers. Out-of-band filesystem
corruption and privileged host/daemon modification are not covered. Normal Docker
restart is not an integrity check. Live fault/reboot checks remain acceptance
targets, not claimed results here.

## Original implementation sequence and acceptance targets

Use an explicit storage-aware journal version rather than weakening v2's no-mount invariants. Keep incomplete legacy/v2 operations recoverable with their original rules; migrate ready state only on an explicit new deployment. Preserve the global mutation lock and bounded, value-free runtime diagnostics. Retained-volume status and teardown refusal must work even without a deployment journal; missing deployment state is not authority to erase storage ownership.

Implement in three reviewable steps:

1. Registry/ownership, immutable binding validation, volume status, and retention/teardown guards.
2. Verified mounts, stopped candidate preparation, writer retirement, durable 503 routes, and recovery/supersede.
3. A runnable SQLite/uploads example, failure-injection tests, and live data-retention checks across replacement, failed startup, supersede, reboot, cleanup, destroy, and explicit data reuse.

Required regressions include lost volume-create and container-start responses; missing/foreign/replaced volumes; unexpected mount references; delimiter-safe mount arguments; restart resurrection; interrupted initialization and readiness failure after a schema write; failed-first-deploy supersede retaining its binding, authorization, and possible data changes; mixed app progress; uncertain 503/activation reloads; interrupted supersede; changed mount bindings; data surviving cleanup/destroy/redeploy; and teardown refusing retained/missing recorded storage.

**Implemented scope:** singular `volume`, one per service, no sharing, immutable
bindings, explicit retained-data reuse, and no implicit copy/chown or data deletion.
Backup/restore/preparation work remains deferred. The checklist above is not a
claim that new full-suite or live checks have already passed.

Docker references: [volume lifecycle, implicit creation, and `volume-nocopy`](https://docs.docker.com/engine/storage/volumes/), [volume removal restrictions](https://docs.docker.com/reference/cli/docker/volume/rm/). These describe Docker primitives, not a substitute for Soar's ownership and writer checks.
