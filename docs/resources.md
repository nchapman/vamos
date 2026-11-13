# Runtime resource limits

Services may declare CPU and memory hard limits:

```yaml
services:
  web:
    build: {context: ., dockerfile: Dockerfile}
    resources:
      cpus: 0.5
      memory_mb: 512
    http: {port: 3000, health: /readyz}
```

Both fields are optional, but an explicitly supplied `resources` mapping must
contain at least one. Omission preserves the existing unlimited runtime default.
Explicit zero is rejected rather than interpreted as unlimited.

- `cpus`: 0.01–1024, in increments of 0.001. A value of 0.5 permits half a CPU's
  worth of execution time, not a dedicated core. Deployment also rejects a value
  above Docker's reported CPU count. Docker uses a 100ms CFS period; Linux's 1ms
  minimum quota makes 0.01 the smallest supported value through `--cpus`.
- `memory_mb`: an integer count of **MiB** (1024² bytes), from 6 to 1048576.
  The combined memory/swap limit is set to the same byte count, allowing no
  additional swap. Exceeding the memory limit may cause the kernel to kill a
  process; Soar does not disable the OOM killer.

Limits apply per container, including `soar run`, using that service's captured
configuration. They do not limit BuildKit, reserve capacity, or prevent aggregate
host pressure. Public replacement temporarily runs both old and new containers;
budget for that overlap, databases, one-off commands, and host services.

Changes use ordinary deployment replacement, not an in-place `docker update`.
They participate in protected manual-service approval, scoped deployment,
captured-input resume, and code/config rollback. Removing `resources` explicitly
returns a selected service to unlimited defaults through replacement. Omitted or
protected services are not silently changed.

Soar checks requested Docker/kernel capabilities before advancing a deployment's
runtime changes, including before retiring a private service. Creation checks
again rather than silently accepting discarded limits. Runtime inspection
compares CPU, memory, and swap settings with the captured configuration and
rejects drift or competing settings. Legacy non-bundle deployment requests cannot
request limits; use a current client with the matching executor.

## Verification boundary

Configuration and fake-Docker tests cover validation, old nil-field JSON
compatibility, Docker arguments and capability admission, changed/removed limits,
manual and scoped behavior, runtime tampering, and one-off commands. Live cgroup
and Rails acceptance are part of the runtime-evolution work and are not yet
claimed here.

References: [Docker resource constraints](https://docs.docker.com/engine/containers/resource_constraints/)
and [Linux CFS bandwidth control](https://www.kernel.org/doc/Documentation/scheduler/sched-bwc.txt).
