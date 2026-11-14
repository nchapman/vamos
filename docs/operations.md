# Whole-system operations

A practical checklist for operating Soar's current single-host implementation.
Commands below are observation or preview examples unless explicitly described
otherwise; substitute your recorded server, app, service, and operation IDs.
This guide does not turn proposed backup/restore or host-upgrade workflows into
implemented features.

## Operating envelope

- Use a **dedicated, trusted Ubuntu 24.04+ systemd host**, amd64 or arm64, with
  root SSH administration. Keep host keys independently verified and authentication
  noninteractive; do not weaken strict known-host checking to restore access.
- Trust all administrators, application code, Dockerfiles, and build inputs.
  [Build networking](builds.md) isolates the builder from host-local services and
  Soar app bridges, but the privileged builder is not a hostile-code sandbox.
  App networks and local identity bindings are not a hostile multi-tenant
  security boundary. Coordinate firewall changes with builds; do not remove
  their network guards while a builder is running.
- Keep the client and matching Linux executor artifacts together. App operations,
  including status/logs, stage the matching executor; they do not upgrade the
  bootstrap-owned installed binary. Host inspect/status/connect need no artifact.
- Treat the host, disk, Docker, Caddy, and network as shared failure domains.
  **Same-host replicas are not HA.** They can spread traffic and assist public
  replacement, but cannot survive loss of the host. Private groups stop all old
  instances before replacement; volume-backed services are single-instance.
- Coordinate operators and automation. Mutations share a global host lock, not
  a distributed scheduler or an access-control system. It cannot exclude arbitrary
  root commands, distro package managers, or application writes.

See [host lifecycle](host-lifecycle.md), [deployment](deployment.md), and
[multi-service behavior](multi-service.md) for the implemented boundaries.

## Daily health and pre-change checks

- [ ] Probe public HTTPS **from outside the host**: DNS, certificate validity and
  expiry, expected response content, latency, and errors for generated and custom
  domains. A loopback readiness check and verified Caddy route switch do not prove
  Internet reachability or certificate issuance.
- [ ] Check application behavior independently: a safe synthetic transaction,
  database connectivity and query latency, connection saturation, storage growth,
  queue age/depth, worker progress, retries, and failed jobs. Choose checks that
  test your app's dependencies without unwanted production side effects.
- [ ] Compare recorded state with live containers and per-instance observations.
  `ready` is recorded rollout state, not a continuous health guarantee. Host
  `ready`, app inventory, and a successful `server connect` are not health checks.
- [ ] Check disk **and inodes**, memory/OOM events, CPU, I/O latency, filesystem
  errors, and resource growth. Alert early enough to build, recover, and retain
  evidence—not only when a filesystem is full.
- [ ] Confirm independently tested recovery is current before destructive or
  stateful changes. Record target identities, current operation IDs, and who is
  coordinating the change. Do not start new work over an uncertain operation.

```sh
soar server status --json home
soar apps --server home
soar status --server home --app workspace --json
soar logs --server home --app workspace --service web --tail 100
```

`apps` is recorded inventory, including retained state, not a runtime probe.
Status adds live observations but does not replace external monitoring. Soar's
logs and journals are bounded diagnostics, not an external monitoring/audit store.

## Capacity and retention

Budget **aggregate peak usage**, not just steady-state container limits:

- All apps, databases, queues, Docker/containerd, Caddy, and the OS need headroom.
  Public replacement overlaps old and new replica groups. Include builds, image
  pulls/unpacking, release containers, one-off commands, and retained images and
  source archives. Serialization does not remove overlap with running workloads.
- [Runtime CPU/memory limits](resources.md) are per container, not reservations
  or aggregate admission control. Omitted limits are unlimited. They do not cap
  BuildKit. Memory limits permit no additional swap; exceeding them can cause OOM
  termination. Do not size the host by adding only the new group's limits.
- Track filesystem bytes/inodes for Docker data, volumes, captured source, Soar
  metadata, secret stores, system journals, and certificates. The bounded Docker
  `local` log driver does not bound database growth or all host logs.
- Schedule reviewed [cleanup](app-lifecycle.md) before inventories become large:

  ```sh
  soar cleanup --server home --app workspace        # Preview only
  # After review: soar cleanup --server home --app workspace --yes
  ```

  Apply recomputes its plan. Cleanup retains effective versions, per-service
  predecessors, current/previous completed app snapshots, and required inputs.
  It does not delete volumes, secrets, shared base images, **BuildKit cache**, or
  unknown resources. Tagged/digested or otherwise referenced images may remain.
  There is no automatic Soar garbage collection or Soar-managed BuildKit cache
  budget; measure cache separately and plan operator-controlled maintenance,
  not broad prune.
- Public maintenance details are bounded to 1,024 resource/retention entries
  and 32 KiB of preview JSON. Large plans explicitly mark details incomplete and
  report exact totals; approval covers the entire freshly recomputed plan, not
  only displayed entries. Run records are limited to 256; cleanup keeps the latest
  32 terminal results and prunes at most 32 older records per pass. Failed or
  truncated runtime inventories still require diagnosis; they are never treated
  as empty and are not permission to delete journals or provenance.
- [Secret retention](secrets.md#retention-and-limits) is finite: **64 names,
  256 retained versions, and 1 MiB serialized storage per app** (each value at
  most 8192 bytes). Cleanup/destroy retain every version; there is no supported
  secret deletion/GC command. Plan rotation and independent custody before these
  bounds are exhausted. Host teardown also refuses retained secret/volume state.

## Rollouts, long requests, and workers

- Public readiness is an HTTP rollout gate, not business-level correctness.
  Private readiness is a brief running-state observation, not a database/queue
  semantic check. Watch the external and application checks above throughout a
  deployment and afterward.
- Public retirement has a short drain (currently two seconds), followed by Docker
  stop with a 30-second grace period. Long requests, streams, and WebSockets can
  be interrupted; no configurable application-aware connection drain is provided.
- Private replacements stop old instances first. Workers must handle stop signals,
  stop accepting work, and finish or safely release jobs within the stop budget.
  Use application-level idempotency, durable acknowledgements/leases, and tested
  retry behavior. Soar does not drain queues or guarantee exactly-once jobs.
- Volume-backed replacement has explicit downtime and retained data; code rollback
  is not data rollback. Multi-service changes are not atomic: earlier promotions
  remain effective if a later service fails.
- [Release commands](release-commands.md) run from the new image before selected
  runtime changes while existing services may still use the database. Use
  **backward-compatible SQL**, expand/contract sequencing, and coordinated changes
  across independently deployed services. Bootstrap dependencies separately.
  Success persists before rollout; later readiness failure does not undo SQL.
  Failed or uncertain commands are **not automatically retried**, and rollback
  neither runs nor reverses migrations.

## Diagnose first; resume only the exact operation

After timeout, disconnect, or an unexpected result:

1. Record the target and operation ID; retain diagnostics securely. Do not assume
   the remote worker or application command stopped when the client exited.
2. Query host status and app status/inventory. A host-status query error, including
   system-bus failure, means worker state could not be established—not inactivity.
   A host may remain ready while apps are pending. Wait for an active worker;
   do not launch competing recovery or maintenance.
3. Inspect current/candidate instances and logs, plus explicit run results:

   ```sh
   soar status --server home --app workspace
   soar logs --server home --app workspace --service web --candidate --tail 100
   # Add --replica N when a group has multiple instances.
   soar status --server home --app workspace --run RUN_OPERATION
   soar logs --server home --app workspace --run RUN_OPERATION
   ```

   Release-run and owning deployment IDs are distinct. `logs --follow` pins one
   verified container; it does not follow future replacements. Logs can expose
   secrets and application-controlled content.
4. If SSH works but runtime/systemd state is suspect, use read-only host diagnosis
   under the trusted root SSH workflow. For example, on the host:

   ```sh
   systemctl status soar-host-operation.service docker.service caddy.service --no-pager
   journalctl -u soar-host-operation.service -u docker.service -u caddy.service --since '1 hour ago' --no-pager
   df -h
   df -i
   free -h
   ```

   A missing transient worker unit can be normal after completion; daemon service
   state alone does not prove routes or data healthy. Correlate with application
   checks and recorded identities, not just process existence.
5. For a recoverable app operation, resume **its exact captured ID**, not a new
   deploy or migration:

   ```sh
   soar resume --server home --app workspace DEPLOYMENT_OPERATION
   ```

   Resume preserves captured inputs and reconciles supported transitions. It is
   not a universal repair command or permission to repeat uncertain side effects.
   Release resume observes the owning execution; ambiguous one-off starts may need
   operator reconciliation. Exec/console have no durable `status --run` record and
   may continue after disconnection. Inspect side effects before deciding to retry.
   Only use exact-ID supersede where [deployment recovery](deployment.md#deployment-and-recovery)
   explicitly permits it; never bypass uncertain routing or execution fences.

For incomplete **host bootstrap**, use the separate
[partial-bootstrap recovery procedure](host-lifecycle.md#interruption-and-recovery).
Do not run add on a populated host to restore client access.

## Fresh-client access and recovery custody

If only the local registry was lost, independently verify the SSH host key and
restore root credentials, then use the **existing installed** name/domain:

```sh
soar server status --json home root@host
soar server connect --domain apps.example.com home root@host
soar apps --server home
soar status --server home --app workspace
soar service bind api -f api/soar.yaml --server home
```

Connect verifies identity/ready host ownership and writes only local registration;
its default verification timeout is 15 seconds. It has no `--yes`, `--executor`,
or `--json`. It prints the installation ID and makes the first registration the
local default. It does not bind projects: inspect and explicitly bind each
existing service in each intended directory. Subsequent explicit `--server`
selections do not rewrite the normal project target. See the full
[fresh-client procedure](host-lifecycle.md#reconnect-from-a-fresh-client).

Keep recovery material **independent of both the host and any single laptop**:

- SSH keys/recovery access, verified host fingerprints, DNS/provider access, and
  the installed name, domain, target, and installation ID.
- A private copy of the client registry and project targeting records, plus
  reviewed manifests, source, Dockerfiles, and the matching client/executor bundle.
  Bindings must be reverified after host/service identity changes, not blindly
  copied or edited to force adoption.
- Secret values and the version references needed by retained releases, under
  secure custody outside the host; registry credentials and immutable image
  artifacts with recorded digests, or tested reproducible build inputs. Verify
  those artifacts remain retrievable; a digest alone is not a recovery copy.
  Host-local captured images and source are not off-host recovery copies.
  Mutable tags may change.
- Application-consistent database, queue, SQLite, and file recovery material,
  recovery instructions, and measured restore tests on an isolated target.
  Define RPO/RTO and test cross-service consistency, credentials, application
  read/write behavior, and cutover—not merely that files can be downloaded.

**Application capture, restore, scheduling, and pruning are not implemented.**
[Repository setup](backup-repositories.md) and [backup plans](backup-plans.md)
are not evidence of captured recoverable data. Use independently tested external
application-consistent backup/recovery procedures. Connect only recovers local
access to a surviving installation; it cannot recover a lost host or its data.

## OS and runtime maintenance boundary

**There is no supported host upgrade or ownership-reconciliation API.** The
following is a documented **operator procedure**, not an automated or validated
Soar in-place upgrade workflow. This gap remains until implemented and tested.

1. Test the specific distro/runtime/security updates on a disposable equivalent
   host with representative apps, data, package versions, and the same Soar bundle.
   Exercise daemon restart/reboot and application recovery there first.
2. Ensure independently tested external recovery and artifacts are available;
   schedule an outage and define recovery/cutover criteria. Do not promise rolling
   availability on one host. Coordinate writes/jobs as required for consistency.
3. Prevent concurrent root/Soar operations, automation, and package-manager work;
   establish that workers and uncertain executions are settled before maintenance.
   Soar's lock is not a general maintenance lock.
4. Perform reviewed, **operator-controlled distro maintenance** appropriate to the
   host and security advisory. Do not freeze vulnerable packages to satisfy Soar's
   recorded versions. There is no generic safe APT repair/prune recipe here.
5. Verify daemon restart and runtime invariants: Docker/Caddy functioning, intended
   containers and restart behavior, resource limits, networks, loopback ports,
   persistent volume identity/mounts, and no unexpected exposed listeners. Verify
   durable/live route agreement, public HTTPS, database integrity and read/write
   behavior, and queue progress. Use Soar status plus independent checks; do not
   use add as the maintenance validation command.
6. Record package/configuration changes and remaining limitations. **Package drift
   can block setup/teardown even if app operations still work.** Connect does not
   repair drift or upgrade the executor. Never edit journals, recorded versions,
   or hashes to bless drift or bypass ownership checks. Successful smoke checks
   do not create an ownership-reconciliation API.

If a supported **fresh-identity host lifecycle** is required, commission a new
fresh host through bootstrap and migrate applications using a tested external
procedure, with planned data consistency and cutover. This is not Soar restore;
there is no supported automated host-loss migration or in-place re-adoption path.
Do not assume the old populated host can be torn down with `--purge`: retained
workloads/data and drift still trigger the documented refusal guards.
