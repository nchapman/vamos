# Ledger: PostgreSQL and an explicit migration

A small **unauthenticated public demo**, not a financial system. Anyone with its
URL can read or insert entries. Use disposable data. Python's stdlib HTTP server
is intentionally minimal; production authentication, rate limiting, database
roles, backups and restore tooling are not provided.

## Empty host to running API

Install the Soar CLI locally and have SSH access to a fresh Ubuntu 24.04+ host.
Replace the example host/domain below with yours; point wildcard DNS
`*.apps.example.com` at that host and allow TCP 80/443 (plus your SSH port).
No local Docker or YAML edits are needed. Run from this directory:

```sh
cd examples/ledger
soar server add --yes --domain apps.example.com home root@203.0.113.10
soar secrets stage database-password
# Enter a strong password at the hidden prompt; never put it in argv.
soar deploy --service db
soar run --service web -- ./bin/migrate
# Continue only after migration exits successfully:
soar deploy
soar status

# Use the web URL printed by deploy (with your actual base domain):
URL=https://ledger.apps.example.com
curl --fail "$URL/readyz"
curl --fail "$URL/entries" -H 'Content-Type: application/json' \
  --data '{"amount_cents":1250,"description":"First deposit"}'
curl --fail "$URL/entries"
```

If the server is already configured, omit `server add`. First contact binds this
project to the selected server; check the printed target before every mutation.
The stable app name is `ledger`: do not use this recipe on an existing valuable
app. The same staged `database-password` supplies `POSTGRES_PASSWORD` to db and
`PASSWORD` to web. Literal `HOST=db`, `DB=ledger`, `USER=ledger` avoid URL encoding
or copying credentials into a connection URL.

Bootstrap deploy selects only db: it requires only db's secrets and does not
build web. PostgreSQL 17 initializes an empty named `data` volume mounted at
`/var/lib/postgresql/data`. Private-service startup proves running, **not database
readiness**. The explicit migration makes at most 20 connection attempts, with
3-second connection timeouts and 1-second gaps; SQL has a 5-second statement
timeout. Schema execution is transactional and is never retried automatically.
`CREATE TABLE IF NOT EXISTS` makes this initial migration repeatable, not a general
schema upgrade system. The web process never migrates at startup. `/readyz`
queries the initialized table's required columns and returns 503 if unavailable.

## App update, then explicit database acknowledgement

After editing application code, run:

```sh
# Only if schema work is needed, and compatible with the still-running API:
soar run --service web -- ./bin/migrate
soar deploy
curl --fail "$URL/entries"

# Intentional existing protected db update; inspect the plan and accept downtime:
soar deploy --service db --yes
soar status
curl --fail "$URL/readyz"
```

The final command explicitly acknowledges updates to db, including intentionally
refreshing `postgres:17`; it does not update web. No configuration edits are
needed to exercise this sequence. Ordinary deploy leaves an unchanged protected
db alone and never pulls its mutable tag. Configured image, environment, command,
secret-version or protection-policy changes to an existing manual service cause
ordinary deploy to refuse rather than silently apply or ignore them. Production
should choose a vetted PostgreSQL patch tag/digest and plan upgrades separately.
Scoped database replacement uses stop/start and may cause downtime; web requests
can return 503 during the interruption.

**Staging an initialization password does not rotate PostgreSQL's SQL role
password.** PostgreSQL initialization variables apply only to an empty data
directory. Do not stage a new password as an upgrade exercise: both services
would select a credential that does not match the existing role. Actual rotation
requires an operator-controlled SQL password change and matching staged values,
then explicit db acknowledgement and web deployment; this demo does not automate
that process.

## One-off outcomes and recovery

`run` builds/pulls the selected app image and uses its latest selected staged
secrets, configured environment, and existing app network. It needs no running
web instance. It starts no dependencies, mounts no volumes, publishes no ports,
and replaces CMD while retaining ENTRYPOINT. This image uses CMD only and the
same web build for HTTP serving and migration (no separate migration service).
Each invocation may build; there is no promise of cross-command image reuse.

Commands are noninteractive, with no stdin or TTY. Output and exit code are
captured in a durable operation without changing the active release. If a run
fails or SSH disconnects, use the operation ID printed by Soar:

```sh
soar status --run OP
soar logs --run OP
```

Replace `OP` with that ID. Inspect the outcome before deciding whether to rerun;
Soar never automatically retries a command. This initializer is idempotent, but
future migrations or other administrative commands may not be. An interrupted
client is not proof that database changes were cancelled.

## API and storage boundary

- `GET /readyz`: schema/connectivity check.
- `POST /entries`: JSON object with exactly `amount_cents` (signed 64-bit integer,
  not float/bool) and `description` (1–500 characters, not blank or NUL).
  Returns the inserted entry with an ID and HTTP 201.
- `GET /entries`: newest 100 entries, descending ID. No pagination in this demo.
- Request bodies are bounded to 4096 bytes. SQL values are parameterized;
  database errors return generic 503 responses, not credentials or driver details.
  Inserts have no idempotency key: blindly retrying a lost response can duplicate
  an entry.

Only web has a public HTTP route. `db:5432` is app-local DNS with no published
host port; unrelated app networks cannot connect. Private does not mean air-gapped:
containers retain outbound access, and host/Docker administrators remain trusted.
The app uses the initialization role for simplicity, not least-privilege production
roles. See [PostgreSQL lifecycle notes](../../docs/postgres.md).

Entries survive container replacement on the named volume. A deployment is not a
database transaction; failure, code rollback, or command failure cannot undo
committed SQL or restore data. Volumes are retained on app destruction; reuse is
an explicit lifecycle choice, not a fresh database. Backups/restores are deferred:
a local named volume is not a backup, and a failed server can lose everything.

## Code rollback is not database recovery

```sh
soar rollback --service web        # preview captured web code/config
soar rollback --service web --yes # apply after checking schema compatibility
```

[Rollback](../../docs/rollback.md) restores captured local images, configuration and secret
versions, not SQL or schema state. The manual `db` service is never rolled back,
even with `--service db`; intentional database changes require a vetted scoped
deploy. Use backward-compatible migrations so old web code can read current data.
Captured credentials may have been revoked, and staged secrets are not selected.
`soar run` remains noninteractive and is never automatically retried or undone.

## Local tests (no Docker or PostgreSQL needed)

Python 3.10+ suffices for the unit suite; it deliberately mocks the database
boundary and lazily imports the driver, so missing psycopg never silently skips
tests or prevents discovery:

```sh
cd examples/ledger  # from repository root
python3 -m unittest discover -s tests -v
```

Runtime dependency: `psycopg[binary]==3.2.6`; the container uses `python:3.13-slim`
and runs web/migrations as UID/GID 10001. To install locally:

```sh
python3 -m venv .venv
. .venv/bin/activate
python -m pip install -r requirements.txt
python -m unittest discover -s tests -v
```

For a separately provisioned local PostgreSQL, set HOST, DB, USER and PASSWORD in
your environment (do not commit them), run `./bin/migrate` twice, then
`python ledger.py` and exercise `http://localhost:8080` with the curls above.
This optional manual integration check is not part of the unit suite.
