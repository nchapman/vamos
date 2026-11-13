# PostgreSQL: protected service, explicit schema command

The runnable [ledger example](../examples/ledger/README.md) contains the full
empty-host recipe, Dockerfile, pinned Python driver, schema initializer and tests.
PostgreSQL is an ordinary private service, not a managed database product.

## Bootstrap and update contract

From the example directory, after configuring a server and staging
`database-password`:

```sh
soar deploy --service db
soar run --service web -- ./bin/migrate
soar deploy
```

The first command creates only db, requiring only its secrets and no web build.
It uses `postgres:17`, `update: manual`, and named volume `data` mounted at
`/var/lib/postgresql/data`. Database/user are literal `ledger`; web connects to
`HOST=db` with DB/USER constants and the same staged password name. There is no
implicit dependency scheduler, credential generation, or schema hook.

A private container being running does not establish SQL readiness. The explicit
initializer retries connections within bounded attempts, then executes its
idempotent schema transaction once. Web readiness queries the initialized schema;
web startup never migrates. Only deploy web after the migration succeeds.

Ordinary deploy never refreshes an unchanged manual service's mutable image tag.
It refuses requested protected image, environment, command, secrets or policy
changes rather than silently ignoring them. Explicit existing db updates use:

```sh
soar deploy --service db --yes
```

This acknowledges the scoped plan and possible downtime. It does not deploy
other services' pending changes. Image tags should be vetted/pinned for important
data; PostgreSQL major upgrades and on-disk compatibility remain the operator's
responsibility, not something this command makes safe.

## One-off execution and durable results

`soar run --service web -- ./bin/migrate` uses the selected service image,
configured environment and latest selected staged secrets on the existing app
network. It needs no healthy/running web container. It starts no dependencies,
mounts no volumes, publishes no ports, and overrides CMD while retaining
ENTRYPOINT. It is noninteractive: no stdin or TTY. Output and exit status are
captured without changing the active release or applying secrets to running
services.

After failure or disconnect, inspect the recorded operation:

```sh
soar status --run OP
soar logs --run OP
```

Never assume a lost connection cancelled SQL. Commands are never automatically
retried; the operator must establish whether another invocation is safe. An
idempotent initializer is not a substitute for versioned, backward-compatible
migrations. New schemas must tolerate the old app while it is still serving.

Run output is a bounded 8 KiB tail (at most 100 log lines), not an attached
terminal. The worker deadline is 20 minutes; a client timeout does not stop its
container. Active or uncertain runs block other app mutations. A proven
pre-start build failure can be cleaned up or followed by a new explicit run;
uncertain execution intent requires operator reconciliation, not automatic
restart. Cleanup removes terminal command containers in batches of at most 32,
retains the newest 32 completed results, and prunes at most 32 older/pre-start
records per invocation. Repeat cleanup for large histories. Immutable IID
receipts remain until destroy to prevent replay of commands whose result was
pruned. The app admits at most 256 retained run records.

## Credentials, isolation and retained data

PostgreSQL's `POSTGRES_DB`, `POSTGRES_USER` and `POSTGRES_PASSWORD` initialize an
**empty** data directory. Staging a new initialization password does not change
an existing SQL role password. It can instead break application connections when
selected by a later run/deploy. Rotation needs a deliberate SQL credential change
and matching staged application credentials; do not put passwords in argv,
source, logs or shell history. The example's shared initialization role is demo
simplicity, not a production least-privilege design.

`db:5432` is reachable within the app network, with no public port or HTTP route.
Unrelated app networks are isolated, not mutually trusted by default. Containers
can still reach outbound networks; host administrators and Docker access remain
trusted. Only the example web API is publicly routed, and it is unauthenticated.

The volume survives replacement. Stop/start updates may interrupt connections.
Deploys are not transactions across containers and database state: reverting code
cannot revert SQL, schema changes, volume contents or other side effects. Failed
updates must not automatically start an old database binary against changed data.
Destruction retains storage; reuse must be explicit. Backup/restore automation
and database upgrade orchestration are deferred. Before important use, arrange
and test database-consistent off-host backups and recovery including configuration
and secrets. A single local volume is neither a backup nor high availability.

## Code rollback is not database recovery

```sh
soar rollback --service web        # preview captured web code/config
soar rollback --service web --yes # apply after checking schema compatibility
```

[Rollback](rollback.md) restores captured local images, configuration and secret
versions, not SQL or schema state. The manual `db` service is never rolled back,
even with `--service db`; intentional database changes require a vetted scoped
deploy. Use backward-compatible migrations so old web code can read current data.
Captured credentials may have been revoked, and staged secrets are not selected.
`soar run` remains noninteractive and is never automatically retried or undone.

## Verification for this milestone

The workflow was exercised from macOS against the existing dedicated Ubuntu
26.04 amd64 host with Docker 29.1.3 and Caddy 2.6.2, using disposable app
`pgprobe` and the ledger source:

- Scoped bootstrap created only PostgreSQL, with one owned named volume and no
  published ports. The official image's declared volume matched that mount.
- `./bin/migrate` succeeded before any web container existed. Its temporary
  container had restart disabled, no mounts, ports, or service alias.
- A subsequent whole-app deploy served the public API. An inserted row survived
  a source-code web update; PostgreSQL kept its original container/image.
- An ordinary deploy refused a changed protected database environment. Scoped
  preview changed nothing; explicit `--yes` replacement preserved the volume and
  row, left web untouched, and kept the old database stopped with restart disabled.
- A command exiting 23 returned exactly 23 to the CLI. Status reported its outcome
  separately from the active application state.
- A command deliberately outlived an eight-second client deadline. The worker
  continued, committed exactly one SQL insert, and recorded exit 0 and output for
  later `status --run` / `logs --run`. No rerun was submitted.
- Cleanup removed completed command containers/images while their captured logs
  remained readable. The API still returned its original row.
- Destroy removed probe containers, network, routes and captured operation files.
  Only the original hello containers remained; hello's HTTPS route stayed healthy.
  The synthetic secret and volume `soar-vol-2563ad8359829fd94e57f53d607f3749`
  were intentionally retained, with `/var/lib/soar/volumes/pgprobe.json` provenance.

Local verification passed formatting, vet, the full race-enabled Go suite,
native and Linux amd64/arm64 builds, Windows package cross-compilation, the ledger
and SQLite example tests, and local Markdown-link/complete-YAML checks. Two code
reviews were triaged; regressions cover the discovered CLI-output and recovery
issues. Native Windows execution, arm64 host execution, private-registry auth,
hard power loss, exhaustive worker-kill injection, and large live run-history
stress tests were not performed. No reboot or backup/restore test was part of
this milestone.
