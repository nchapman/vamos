# Proposal: first volume-backed deployment milestone

**Status: proposed, not implemented or approved.** Deployment still rejects volumes. This narrows the broader storage design in [PLAN.md](../PLAN.md); it is not a commitment to implement databases, manual updates, scoped deployment, or rollback next.

## Recommendation

Add persistent local storage for small Dockerfile-built applications, beginning with SQLite and uploads. Keep the normal whole-app deploy workflow, but require **single-writer stop/start replacement**, including public services. A volume is live mutable data, not part of an immutable release snapshot.

Recommended first boundary:

- At most **one named volume per service**, eight desired volumes per app. SQLite and uploads can share directories under one `/data` mount. Keep the existing list-shaped configuration; reject a second mount explicitly at deploy preflight.
- Ordinary Docker `local` volumes only, with no driver options, host bind mounts, anonymous volumes, subpath mounts, or sharing between services/apps. Continue rejecting image-declared `VOLUME`, even when its path resembles a declared mount.
- Keep `update: auto`. Changed volume-backed services have downtime; unchanged services do not restart. Private service observation remains process-only, not database readiness.
- A service receives its volume binding when volume creation intent is recorded, not when its first deployment becomes healthy. Reject adding/removing/renaming a mount, changing its path/owner, or converting an existing service between stateless and volume-backed. Such changes need an explicit migration workflow later; adding a new service remains possible.
- No data deletion command, snapshots, automatic backups, restore automation, quotas, replication, database management, or code/data rollback in this milestone. A tested manual backup/restore runbook is nevertheless a release requirement.

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
    volumes:
      - name: data
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

Proposed additional commands are limited to storage preparation and inspection:

```sh
soar volumes prepare              # Validate/capture mount bindings; create volumes, start no app code
soar volumes list                 # Ownership, binding, identity, retained/missing state; never contents
soar deploy --reuse-data          # Explicit reuse after destroy or manual restore into prepared volumes
```

Ordinary initial deploy prepares its own new volumes automatically. Preparation exists so an operator can restore data **before any application process runs**, without a new scoped-deploy feature or hand-editing Soar journals. It requires supported configuration and host ownership, but no application secrets or builds. `--reuse-data` authorizes attaching exact retained/prepared identities, never arbitrary adoption, deletion, renaming, or schema compatibility. Record that authorization in the operation so resume does not ask again. It is not needed for ordinary updates of an existing service or continued recovery of its captured operation. An explicitly named supersede inherits attachment authorization for the same captured volume identities, including after a failed **first** deployment. Preserve the binding and possible-mutation record independently of whether `Current` ever existed. Supersede cannot silently turn that failed first service into a fresh volume or a stateless service; correcting a binding mistake requires a separate identity/migration decision.

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

## Backup and restore gate

Before recommending storage for important data, publish and execute a runbook that:

1. Produces a consistent off-host backup of **the database and uploads together**. The baseline runbook stops all writers, disables restart, and backs up the complete data directory. An online alternative requires an application-owned consistency mechanism: SQLite's backup API alone does not coordinate uploaded files with their database references. Copying only a live SQLite `.db` while ignoring WAL files is not a backup strategy.
2. Retains compatible source/configuration, resolved image metadata, secret material, and required dependencies. Protect secrets separately; a volume archive alone cannot restore an app.
3. Bootstraps a fresh host, uses `volumes prepare` to allocate new host-owned identities without starting app code, and restores bytes using a trusted, explicit operator-controlled helper with exclusive access. Do not transplant old host IDs or fabricate registry records.
4. Removes the restore helper, checks ownership/permissions, and deploys compatible code with `--reuse-data`; verifies old records, an existing upload and its database reference, and new writes/uploads. Backup verification must inspect application data, not merely HTTP readiness.

This is a manual runbook, not an implemented backup/restore service. Out-of-band Docker/host modification, missing storage, and filesystem corruption require operator recovery; normal Docker restart is not an integrity check. A technical spike must check missing-volume behavior on create/start/reboot before the implementation claims fail-closed recovery. If the supported daemon cannot uphold that boundary, revise restart policy or the design explicitly—do not paper over it with empty-volume recreation.

## Implementation and approval gates

Use an explicit storage-aware journal version rather than weakening v2's no-mount invariants. Keep incomplete legacy/v2 operations recoverable with their original rules; migrate ready state only on an explicit new deployment. Preserve the global mutation lock and bounded, value-free runtime diagnostics. Volume-only registration/status and retained-data teardown refusal must work without a deployment journal, as secret-only registration does today.

Implement in three reviewable steps:

1. Registry/ownership, immutable binding validation, preparation/listing, and retention/teardown guards.
2. Verified mounts, stopped candidate preparation, writer retirement, durable 503 routes, and recovery/supersede.
3. A runnable SQLite/uploads example, failure-injection tests, live reboot/retention/destruction tests, and off-host backup/fresh-host restore verification.

Required regressions include lost volume-create and container-start responses; missing/foreign/replaced volumes; unexpected mount references; delimiter-safe mount arguments; restart resurrection; interrupted initialization and readiness failure after a schema write; failed-first-deploy supersede retaining its binding, authorization, and possible data changes; mixed app progress; uncertain 503/activation reloads; interrupted supersede; changed mount bindings; data surviving cleanup/destroy/redeploy; and teardown refusing retained/missing recorded storage.

**Decisions to approve before implementation:** the one-volume-per-service bound; immutable bindings/new-service-only attachment; `volumes prepare/list` plus explicit retained-data reuse; no implicit copy/chown or data deletion; and the backup/restore gate. No storage implementation is authorized merely by this proposal.

Docker references: [volume lifecycle, implicit creation, and `volume-nocopy`](https://docs.docker.com/engine/storage/volumes/), [volume removal restrictions](https://docs.docker.com/reference/cli/docker/volume/rm/). These describe Docker primitives, not a substitute for Soar's ownership and writer checks.
