# Runtime limits, release commands, and replica groups

Design and compatibility contract for the implemented runtime features. Operator
instructions are in [resources](resources.md), [release commands](release-commands.md)
and [replica groups](multi-service.md#replica-groups); verification evidence is
recorded separately in [testing](testing.md). No cloud scheduler, multi-host
networking, or Rails behavior is added to Vamos itself.

## Resource limits

Optional per-service configuration:

```yaml
resources:
  cpus: 0.5
  memory: 512m
```

`cpus` is a CPU-time ceiling, in increments of 0.001, from 0.01 through
1024 (also bounded by the deployment host's CPU count). `memory` accepts Docker sizes (e.g. `512m`, `1.5g`, or bare bytes), from 6 MiB through 1 TiB. At least
one must be supplied; explicit zero is not an unlimited alias. Omission retains
the existing unlimited behavior. These are per-container limits, not reservations
or service-wide budgets. They apply to ordinary runtimes, one-off commands, and
release commands, not BuildKit.

Memory limits also set the combined memory/swap ceiling to the same value, so
there is no additional swap allowance. Required kernel/runtime capabilities must
be available; unsupported limits must not silently become unlimited. Captured
limits participate in runtime verification, service equality, manual-update
protection, resume, and rollback. Changing a limit uses normal replacement.

Public replacement temporarily runs the old and new groups. Operators must
budget for that overlap and for the database, release process, and host services.
Limits are not a promise that the host has sufficient capacity.

## Release commands

One optional directive belongs to the requested deployment, not to sticky app-wide
remote policy:

```yaml
release:
  service: web
  command: [bin/release]
```

The runner must be a selected, non-skipped service. A scoped deployment which
excludes the configured runner is rejected rather than silently omitting the
hook. Independent source directories do not inherit each other's hook. Ordinary
`run`, status, maintenance, and rollback never execute it. A configured command
is attempted even when the selected runtime would otherwise be unchanged.

After all selected images are resolved, but before any selected application
runtime is created or retired, ensure the app network and execute the command
using the selected service's new immutable image, captured environment, secret
pins, and resource limits. Replace CMD, preserve image ENTRYPOINT, and provide no
stdin, TTY, ports, service alias, or volume. Reject image-declared volumes too.
There is no second build or implicit dependency startup. Bootstrap databases in
a separate deployment before using a release command.

A durable successful result opens the rollout gate. A nonzero exit blocks
rollout. Resume observes the original execution and does not rerun a successful
or failed command. In particular, after a recorded start intent, never send
another start merely because the response was lost. Running or uncertain
executions fence unrelated mutations, including supersede and maintenance. Only
the exact owning deployment can resume observation of its execution. A known
terminal failure may be explicitly superseded by a new deployment attempt.

This provides at most one orchestrated start attempt per deployment, not exactly
once external side effects. Losing execution evidence may require operator host
reconciliation. SSH disconnection or worker timeout does not prove cancellation.
Code rollback does not reverse database migrations. Schemas and job payloads must
remain compatible with overlapping old and new application versions.

## Replica groups

Optional `replicas` is a positive, bounded count, defaulting to one. Omission and
explicit one have the same effective behavior. Initial bounds are 8 per service
and 32 total desired instances per app; these are accounting bounds, not capacity
reservations. Volume-backed services reject counts above one. Zero is not stop or
delete. Manual protection and selected-only updates continue to apply.

A service release contains one image/configuration/secret set and an ordered list
of instance identities and loopback ports. It is not expanded into synthetic
services. New instance names and exact labels carry their ordinal. Legacy scalar
release identities remain readable and retain their names and ports. A central
accessor handles the two representations; ambiguous or contradictory records
are rejected.

Service-level phases continue to control the group:

- Public, volume-free services start and qualify the entire new group, admit
  service aliases and recheck readiness, switch the full Caddy upstream set,
  then drain and retire the complete old group.
- Private services retire and prove every old instance stopped before starting
  the replacement group. New workers may run concurrently with one another,
  but this does not introduce old/new-worker overlap.
- Volume-backed services retain single-writer stop/start and durable public 503
  routing where applicable.

Count-only changes use the same whole-group replacement. Each port intent is
persisted before creation. Resume addresses the exact recorded members after
partial creation, alias admission, or retirement. An unhealthy candidate member
blocks promotion of the group.

Caddy receives deterministic upstream sets for generated and custom hostnames.
Singleton route rendering remains byte-compatible with existing routes. The
first implementation does not promise continuous application-health monitoring
or a replica-replacement controller; Docker restart policy remains responsible
for restarting containers. Same-host replicas do not provide host availability.
Same-app DNS admission for public services remains non-atomic and can temporarily
include both generations before public promotion, as in the singleton workflow.
Private worker replacement still proves the old group stopped first.

Status exposes per-instance observations and group counts, without labeling a
partially running group fully healthy. Logs/exec/console retain singleton defaults
and require an explicit ordinal for multi-instance groups. Attach still pins the
actual Docker ID. Cleanup, destruction, service removal, rollback, and history
must enumerate all group members, including partially processed candidates.

## Durable schema and compatibility

Release captures and replica groups use deployment journal v8. Read and recover v1–v7 with
their original semantics; do not rename/restart old runtimes merely to upgrade
metadata. Preserve v7 independent-directory service incarnations, selected sets,
and per-service history. Once upgraded, do not downgrade a journal during
maintenance or rollback. Capability predicates must recognize only supported
versions, not all future version numbers.

New optional fields must preserve canonical historical run records. Release
execution records link to their parent deployment, use the existing bounded
one-off execution boundary, and retain evidence required by the owning operation.
The complete verified run result is persisted before opening the parent gate.

## Representative acceptance fixtures

Keep the existing fast, standard-library Python Ledger as a cross-framework
fixture. Add `examples/rails-ledger` as a real, small Rails/PostgreSQL API with a
separate Solid Queue worker, shared immutable app image, and no web-local storage.
It is an unauthenticated disposable demo, not a production application template.
Core Vamos never recognizes Rails, Puma, or Solid Queue.

The Rails image must not migrate at boot. A release script checks connectivity
with a bounded retry and then attempts migration once. Readiness checks the
application and queue schema; it does not require a live worker. Jobs perform an
idempotent database effect. Instance markers let acceptance tests observe two web
upstreams and a distinct worker. Database credentials and SECRET_KEY_BASE are
staged privately; no external storage accounts or production app access is needed.

Required verification includes resource inspection and cgroup enforcement,
failed-hook gating, lost-start/result recovery, successful-hook/no-rerun after
rollout failure, 1→N→1 scaling, partial group creation/retirement, route
reconciliation, explicit instance sessions, scoped rollback/removal, retained
state, and machine restart. Real Rails database/queue tests and Linux acceptance
remain distinct from Go fakes, manifest validation, and Windows compilation.
