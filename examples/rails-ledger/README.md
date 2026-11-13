# Rails Ledger — disposable API / worker acceptance example

**Unauthenticated demo. Do not expose this API to untrusted users or store real
financial/personal data.** Anyone who can reach it can create work and read entries.
Input is bounded, but there is no authentication, authorization, rate limiting,
request deduplication, or overall storage quota. Use a disposable private test
installation. The existing Python `examples/ledger` is unchanged.

This is a real API-only Rails app using PostgreSQL and a separate Solid Queue
worker, with one shared image for web, worker, and release. It has no Redis,
object store, uploads, assets, encrypted credentials, or local persistent state.
`soar.yaml` targets the resources, replicas, and operation-scoped release APIs.
Use a current Soar binary supporting all three features; older binaries do not
support this manifest. It declares two web replicas, one private
worker, and a manual PostgreSQL service, each limited to 0.5 CPU and 512 MiB with
no additional swap. Only PostgreSQL has a volume.

## API and processing semantics

```sh
curl -i https://YOUR_HOST/readyz
curl -i https://YOUR_HOST/entries \
  -H 'Content-Type: application/json' \
  -d '{"description":"  coffee   beans  ","amount":-25}'
# 202 Accepted; Location: /entries/ID
curl -i https://YOUR_HOST/entries/ID
```

Use flat JSON, with a nonblank string `description` (maximum 200 characters) and
an actual JSON integer `amount` between -1,000,000,000 and 1,000,000,000 inclusive.
Strings such as `"25"`, fractions, and missing fields are rejected with 422.
Only those two fields are accepted. Missing entries return JSON 404.

The response includes `id`, the original `description` and `amount`, `created_at`,
`status` (`pending`/`processed`), `normalized_description`, `processed_at`, and
`worker_marker`. Processing trims/collapses whitespace and uppercases the
text. It never changes the amount. Poll GET with a bounded deadline for completion.
Each response from an application controller includes an opaque, demo-only
`X-Demo-Instance` header. The marker is per process, refreshed after fork, and
contains no hostname, PID, address, environment data, or credentials. Compare
markers across fresh requests to observe web load balancing; compare the stored
worker marker to demonstrate separate worker execution. Responses are `no-store`.

**A successful POST commits the entry and queue rows together.** Both models
inherit the exact same Active Record connection pool (`solid_queue.connects_to =
nil`); `ApplicationJob.enqueue_after_transaction_commit = false` is deliberate.
`Entry.submit!` wraps insertion and enqueue in one transaction and treats a failed
enqueue as an error. Tests verify in-transaction queue visibility, rollback, SQL
enqueue failure, and real Solid Queue claim/deserialization/execution. Moving the
queue to another connection/database would break this guarantee and requires an
outbox/reconciliation design, not just a connection setting change.

The job locks the entry row and leaves already-processed entries unchanged. This
is an **idempotent durable effect, not exactly-once delivery**. Repeating POST
creates a new entry/job; there is no HTTP idempotency-key contract. Failed jobs
remain inspectable in Solid Queue and require explicit operator retry/discard;
successful queue rows are removed (`preserve_finished_jobs = false`). Recurring
tasks are disabled in `bin/jobs`. Entries and failures can still accumulate:
monitor them and destroy/reset this disposable database between acceptance runs.

`GET /readyz` checks a live DB connection, the two required migration versions,
and actual columns in the entry and all pinned queue tables (not cached schema).
It returns JSON 200 or generic 503 with no connection details. **It does not check
worker liveness**: accepting durable work during worker replacement is intentional.
This is readiness, not a promise that jobs will finish immediately.

## Runtime and configuration

Pinned: **Ruby 3.4.10, Rails 8.1.4.1, Solid Queue 1.7.2**. `Gemfile.lock` also locks
Puma 7.2.2 and pg 1.7.0 and includes Linux aarch64/x86_64 platforms. The Rails
meta-gem pulls in framework gems, but unused mail/storage/cable/text engines are
not loaded. No development/test-only gems are required by this example.

| Variable | Purpose / default |
| --- | --- |
| `RAILS_ENV` | `production` in the image and release script |
| `DB_HOST` | PostgreSQL network hostname; local default `127.0.0.1` |
| `DB_PORT` | `5432` |
| `DB_NAME` | Production `rails_ledger`; development `rails_ledger_development` |
| `DB_USER` | `rails_ledger` |
| `DB_PASSWORD` | Secret injected for web, worker, and release |
| `SECRET_KEY_BASE` | Secret injected for all production processes; same value across web replicas |
| `ALLOWED_HOSTS` | Nonsecret comma-separated **exact DNS hostnames**, no schemes/ports/wildcards; default `rails-ledger.localhost` |
| `DB_POOL` | Maximum connections per process, default `5` |
| `RAILS_MAX_THREADS` | Web threads, default `3`; keep `DB_POOL` at least this large |
| `PORT` | Web listen port, default `3000` |
| `RELEASE_DB_ATTEMPTS` | Connectivity attempts, default `15`, allowed `1..60` |
| `DB_TEST_NAME` | Disposable test database, default `rails_ledger_test`; independent of `DB_NAME` |
| `RAILS_LOG_LEVEL` | Default `info`; production logs go to stdout |

Use separate DB fields, not a credential-bearing URL. Rails itself supports
`DATABASE_URL` overrides; leave it **unset**, especially for tests. Provision the
PostgreSQL database/role before release. Initialization credentials only apply to
an empty PostgreSQL volume; changing an environment secret does not rotate the
SQL role. A convenient initialization-role demo is not production least privilege.
No master key or encrypted credentials are generated into the checked-in source.
Generate/stage `SECRET_KEY_BASE` outside the image, e.g. with `openssl rand -hex 64`.

Caddy terminates TLS and forwards `X-Forwarded-Proto`. Rails keeps `force_ssl`
enabled but does not globally assume TLS. Only `/readyz` bypasses SSL redirects
and Host Authorization so internal HTTP probes with container-IP Host headers
work. All other paths retain host checks and HTTP-to-HTTPS redirects. Caddy is the
only public entrypoint; do not publish the app port directly. Configure the exact
routed public hostname through `ALLOWED_HOSTS`.

Puma is explicitly single-process (`workers 0`, even if `WEB_CONCURRENCY` is set)
with three threads. Queue configuration is one worker process/two execution
threads and one dispatcher under the Solid Queue supervisor; it is **not** a
Puma plugin. Budget up to five DB connections per process: two web replicas,
worker + dispatcher + supervisor, and a release process imply up to 30 before
rollout overlap. Pools grow on demand. Measure memory before selecting generic
resource limits; these are small defaults, not capacity guarantees.

## Release and shared image

The Dockerfile uses `ruby:3.4.10-slim`, a throwaway native-extension build stage,
and a nonroot UID/GID 1000 final stage. It installs only runtime libraries in the
final image and excludes development/test bundles. It declares **no VOLUMEs**.
The Docker build context is this directory, not the repository root. `.dockerignore`
excludes `.env*`, keys, credentials, local bundles, `.soar`, tests, logs, and temp
files. Image build requires gem/package network access, but no DB or app secrets.

Manifest process commands:

```text
web:      bundle exec puma -C config/puma.rb
worker:   bin/jobs
release:  bin/release
readiness: HTTP GET :3000/readyz
```

`bin/docker-entrypoint` only `exec`s its arguments. **Neither web nor worker boot
runs migrations.** `bin/release` first retries connectivity a bounded number of
times (3-second connection timeout, 5-second pool/lock timeout, 15-second SQL
statement timeout, one-second pause between failures), then `exec`s **one**
`bin/rails db:migrate` attempt. Failed migrations are never automatically retried;
their exit status is preserved. Release does not create the database or SQL role.
Do not run releases concurrently; platform-level serialization is still required.

Both application and queue tables are explicit versioned migrations in the same
primary database. The initial queue migration is the installed Solid Queue 1.7.2
schema version 1, with destructive `force: :cascade` creation removed. It includes
the 1.7 batch tables. There is no separate `queue_schema.rb` that `db:migrate`
would silently omit. Upgrade queue schemas with reviewed new migrations; never
rerun or rewrite the initial schema on a live database. A generated `db/schema.rb`
is not required for bootstrap; migrations are the authoritative source.

Integration bootstraps the protected PostgreSQL service/volume, stages secrets,
and uses the captured shared image's operation-scoped release command once before
starting/promoting two real web replicas and a separate private worker. Web and
worker have no persistent mounts; only PostgreSQL owns a volume. A failed release
must leave the previous web route and worker intact. Code rollback does not undo
migrations, queued payloads, or data; maintain old/new schema and job compatibility.
Volumes are not backups.

Suggested acceptance observations: repeated verified-HTTPS requests see both web
markers (not necessarily equal traffic); one entry is readable from both; worker
completion has a different marker; a second release is harmless; replacement
preserves data and the DB volume; a failed release blocks promotion. Platform
integration owns replicas/resource enforcement/release lifecycle assertions.

## Soar deployment and runtime acceptance

Use the disposable guest's `local` server target; this example does not provision
a server. The checked-in hostname assumes its test domain is `soar.localhost`.
For another domain, change `ALLOWED_HOSTS` to the exact generated web hostname
(and any explicit `http.domains` aliases) before deploying. Do not disable host
checks. Run commands from this directory so the Docker build context is correct.

1. Stage `database-password` and `app-password` with the **same** random password,
   and `secret-key-base` with an independently generated value, using
   `soar secrets stage --server local --app rails-ledger --stdin NAME`.
   Supply values through stdin, never manifest literals or command arguments.
2. Create a temporary `bootstrap.yaml` with `app: rails-ledger` and **only the
   unchanged `services.database` block from `soar.yaml`**. Do not include `release`.
   Run `soar deploy --server local -f bootstrap.yaml`. PostgreSQL creates the
   database/role when initializing its empty volume. There are no implicit
   service dependencies. `deploy --service database` with the full manifest is
   invalid because it excludes the declared release runner (`web`).
3. Run `soar deploy --server local` with the full `soar.yaml`. Its release runs
   once for the deployment operation, not once per replica or service. It waits
   for DB connectivity and runs the explicit app/queue migrations before any
   runtime update. Neither web nor worker performs implicit migrations.
4. Inspect `soar status --server local --app rails-ledger --json`. The replica
   worktree's public schema uses `services.web.instances[].ordinal` (1-based),
   `container`, `port`, `running`, and `available`; image is service-level.
   `release.operation` identifies the separate run for
   `soar logs --server local --app rails-ledger --run OPERATION`.
   Use `--service web --replica 1` (or `2`) for `exec`, `logs`, and `console`;
   omitting the selector for a multi-instance service is an error.
5. For a worker-only update, remove the top-level `release` directive from the
   operation's manifest, then use `deploy --server local --service worker`.
   An excluded release runner is rejected, not silently ignored. To update both
   app services while leaving a pending manual database secret pinned, use an
   application-only manifest containing web, worker, and release, omitting DB.
   A full manifest that includes a protected DB with a changed staged secret is
   refused; staging a secret does **not** rotate the SQL password.
6. A failed release leaves existing runtimes and routes intact. Inspect its
   explicit run logs. `resume PARENT_OPERATION` observes the same failed run and
   does not retry it. After fixing the command, use
   `soar deploy --server local --supersede PARENT_OPERATION` with the same service
   scope to authorize a new release attempt. Code rollback cannot undo schema
   or data changes made by a release.

`integration/runtime_scenarios.py` is wired after the existing CLI scenarios and
before recovery preparation, behind the unchanged disposable-guest ownership
gate. It creates fresh app IDs independent of earlier scenarios. It asserts real
Docker limits/IDs, all replica readiness and loopback bindings, verified HTTPS
load balancing/aliases, shared-image deployment, SQL migration versions, durable
Solid Queue processing, replica-selected exec/logs/PTY console, pending manual DB
secret pins, scoped updates, and failed-release non-reexecution (using a durable
SQL counter). A small Python ledger scenario exercises the generic contract too.
It writes **`runtime-summary.json`**, without replacing other suite summaries.
Successful cleanup/destroy retains DB volumes and secret provenance until the
owned guest itself is deleted. Use the integration coordinator's
`--keep-on-failure` option to retain a failed guest for diagnostics; the default
also removes failed guests. See [test ownership and cleanup](../../docs/testing.md#isolation-and-cleanup).

**Schema layout:** this fixture deliberately has one PostgreSQL database, with
both application and Solid Queue tables. `bin/release` runs `db:migrate`, not
`db:prepare`; it does not create databases or use a separate queue connection. Changing that layout would also change the atomic enqueue
guarantee described above and is outside platform wiring.

The runtime scenarios passed on an owned Ubuntu 24.04 arm64 guest, including the
Linux image build, real PostgreSQL migrations and Solid Queue processing,
Docker/kernel limits, two web replicas, selected sessions, failed-release gating,
HTTPS and retained-state cleanup. Local Python helper tests are supplementary,
not a substitute for these live checks.

## Local development and tests

With Ruby 3.4.10, an isolated bundle, and a **disposable PostgreSQL database**:

```sh
bundle install
# Export DB_HOST/DB_PORT/DB_USER/DB_PASSWORD as needed.
RAILS_ENV=development bin/rails db:create
RAILS_ENV=development bin/release
bin/rails server -b 127.0.0.1  # terminal 1
bin/jobs                    # terminal 2

# Only a disposable test DB; never a shared/live DATABASE_URL.
RAILS_ENV=test bin/rails db:create db:migrate
bin/rails test

# These checks need no PostgreSQL server:
bundle exec rails zeitwerk:check
bundle exec ruby test/boot_check.rb
bundle exec ruby test/release_check.rb
bin/jobs check
```

`bin/setup` is a local developer convenience, not an image entrypoint. The DB
suite is serial because readiness tests change schema inside rollback-only
transactions. It covers strict validation, API shape, readiness with missing
tables/columns/versions, repeat job execution, and the **real** Solid Queue adapter
(not just Active Job's in-memory test adapter). `boot_check.rb` boots production
against a deliberately closed loopback port and tests 503 readiness, narrow host/
SSL exemptions (success path uses a schema-check stub), shared-pool configuration,
opaque fork-safe markers, and migration/template parity. `release_check.rb` uses
a fake connection and migration executable to test retry boundaries/status
preservation; it is not a PostgreSQL migration test.

### Validation performed during implementation

On the local arm64 macOS Ruby 3.4.10 runtime: `bundle check`,
`bundle exec rails zeitwerk:check`, and `bin/jobs check` passed.
`bundle exec ruby test/boot_check.rb` passed **6 tests / 20 assertions**;
`bundle exec ruby test/release_check.rb` passed **4 tests / 11 assertions**.
Ruby syntax checks on app/config/db/test/lib files and `bin/release`, `bin/jobs`,
plus `sh -n bin/docker-entrypoint`, passed. A real production-boot negative
release check (`DB_HOST=127.0.0.1 DB_PORT=1 RELEASE_DB_ATTEMPTS=1 bin/release`,
with a test-only `SECRET_KEY_BASE`) exited 1 with “migrations not run”.
`bin/docker-entrypoint ruby -e 'exit 23'` preserved exit status 23.

The initial local DB-test attempt used a deliberately closed endpoint and did
not run database tests. Live runtime acceptance subsequently passed with a real
PostgreSQL database on Ubuntu 24.04 arm64. The final full acceptance run,
`soar-test-f5b6acb3fd56`, passed all seven runtime groups and restart recovery;
its owned guest was deleted. It covered migrations, separate Solid Queue
processing and shared rows through both web replicas. The complete database-backed
Rails suite also passed through a test-only one-off image in a separate
`DB_TEST_NAME`: **12 tests / 86 assertions, zero failures, errors or skips**, with
empty stderr. Setup and tests run in separate processes to close setup connections
before Rails prepares the test schema. See [verification evidence](../../docs/testing.md#runtime-feature-acceptance).
Linux amd64 image execution and production capacity are not established by an
arm64 smoke test.

## Reproducible generation

The installed Rails generator's `new --help`, generated production SSL/entrypoint,
and Solid Queue 1.7.2 template/adapter sources were inspected before customization.
From the repository root, with tooling isolated under `.pi/rails-tooling`:

```sh
GEM_HOME="$PWD/.pi/rails-tooling" GEM_PATH="$PWD/.pi/rails-tooling" \
  "$PWD/.pi/rails-tooling/bin/rails" _8.1.4.1_ new examples/rails-ledger \
  --api --database=postgresql --skip-git --skip-bundle --no-rc \
  --skip-action-mailer --skip-action-mailbox --skip-action-text \
  --skip-active-storage --skip-action-cable --skip-asset-pipeline \
  --skip-javascript --skip-hotwire --skip-jbuilder --skip-system-test \
  --skip-bootsnap --skip-dev-gems --skip-thruster --skip-rubocop \
  --skip-brakeman --skip-bundler-audit --skip-ci --skip-kamal --skip-solid
```

`--skip-solid` avoids unused Solid Cache/Cable and the separate-DB queue defaults;
Solid Queue alone is added explicitly. Generated credentials/master key and stray
CI files were removed. Dependencies were installed only into the repository-local
tooling GEM_HOME, with `BUNDLE_IGNORE_CONFIG=1` and `BUNDLE_USER_HOME` inside that
scratch directory. The lockfile was resolved with
`bundle lock --add-platform aarch64-linux x86_64-linux` (Bundler 2.6.9).
Do not regenerate this app during platform integration tests.
