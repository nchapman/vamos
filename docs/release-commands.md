# Release commands

A release command is an **operation-scoped deployment gate**, commonly used for database migrations. It is not a dependency scheduler, an application boot command, or a command run once per replica.

```yaml
app: ledger
release:
  service: web
  command: [bin/release]
services:
  web:
    build: {context: ., dockerfile: Dockerfile}
    command: [bin/web]
    http: {port: 3000, health: /readyz}
    resources: {cpus: 0.5, memory_mb: 512}
    env: {DB_HOST: database, DB_NAME: ledger, DB_USER: ledger}
    secrets: {DB_PASSWORD: database-password}
```

The runner service must be declared locally, selected by the deployment, and not skipped by `update: manual`. An excluded or skipped runner is rejected before building or changing services. The command is an argument array: Soar does not implicitly invoke a shell.

## Execution order and inputs

1. Capture the selected configuration, source bundle, and secret versions.
2. Build or pull the selected images and verify resource-limit support.
3. Execute one release container from the runner's **new captured image**, with its captured environment, pinned secrets, and resource limits.
4. Persist a successful exit result before changing any selected service runtime.
5. Start or replace the services using normal deployment ordering and readiness checks.

The release container retains the image's ENTRYPOINT and replaces CMD. It has the app network but **no published ports, service DNS alias, or application volume mounts**. Images declaring `VOLUME` are rejected for this execution. Restart policy is `no`. The command must terminate with exit code zero to open the gate.

All selected images are prepared first, but no selected application runtime is started, stopped, or promoted before the command succeeds. Existing services remain available while it runs, subject to their own health. A migration can still change the database beneath those services: use backward-compatible schema changes and an expand/contract migration strategy.

## Bootstrap dependencies explicitly

A release command does not start its database or worker. Bootstrap required private services separately before the first gated application deployment:

1. Use a dependency-only manifest **without `release`** to deploy the database.
2. Stage the application's secrets.
3. Deploy the application manifest with the release command.

Simply selecting `--service database` from a manifest whose release runner is `web` is rejected rather than silently skipping the gate. Likewise, use a manifest without the release directive for an intentional worker-only update. Omission of a service from a partial manifest does not delete it.

The directive belongs only to that captured deployment. A later deployment whose manifest omits `release` does not inherit an earlier command. Resuming an existing deployment uses its original directive and inputs, not changes to the local file.

## Observe failures and resume safely

```sh
soar status --server local --app ledger
soar status --server local --app ledger --json
soar logs --server local --app ledger --run RELEASE_RUN_OPERATION
soar resume --server local --app ledger DEPLOYMENT_OPERATION
```

Status distinguishes the deployment operation from the release run operation. JSON includes `release` metadata with the service, execution identity, phase and, when known, exit code. Command output belongs to explicit run logs, not status metadata.

- **Exit zero:** the durable result opens the rollout gate. Repeated resume does not execute the command again.
- **Nonzero exit:** rollout remains blocked. Resume reports the same failure; it does not retry the command.
- **Running or uncertain outcome:** rollout and unrelated mutations remain fenced. The owning deployment may resume observation of that exact execution. A lost start response, interrupted wait, or stopped container without a reliable exit result is not permission to send another start.
- **Creation interrupted before start intent:** resume may reconcile the exact verified, never-started container before its first start. This does not authorize restarting an execution whose start intent was already saved.

After a known failure, inspect logs and database state first. To attempt a corrected command, explicitly submit a **new** deployment with `--supersede FAILED_DEPLOYMENT_OPERATION`. That is a new execution, not an automatic retry; migrations must be safe for the database state left by the failed attempt. Active or unknown executions cannot be superseded merely to bypass the safety fence.

A successful migration is not undone if a later service fails readiness. Resume continues the runtime rollout without repeating the release command. Rollback restores eligible captured runtime versions; it **neither executes release commands nor reverses database migrations**. See [rollback](rollback.md).

## Boundaries

- Single-host execution only; no cross-host leader election or distributed exactly-once guarantee.
- No automatic retries, migration rollback, dependency inference, shared replica volume, or build-time resource reservation.
- Secret staging after capture does not change the command's pinned inputs.
- The executor's normal operation deadline applies. An interrupted wait does not kill the container or prove execution stopped.
- Completed execution records remain inspectable through the ordinary run/log interface. Cleanup preserves the current deployment's result evidence; stopped execution containers may be removed after their result is durable.
- New release captures use journal version 8. Older journal identities and routes remain readable without metadata-only runtime replacement. An old executor cannot operate on a version-8 journal.

See the [Rails reference app](../examples/rails-ledger/README.md) for Rails, PostgreSQL and a separately deployed Solid Queue worker, and [one-off commands](deployment.md#one-off-commands) for explicitly submitted commands outside deployment.
