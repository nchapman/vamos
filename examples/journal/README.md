# Journal: SQLite and uploads on one volume

Runnable Python standard-library persistence **demo, not a production app**.
There is **no authentication**; anyone reaching it can read/write data. Use only
throwaway test data. The single-threaded development HTTP server has no abuse
protection, quotas, migration system, or backup/restore workflow.

From the repository root, on a bootstrapped test server:

```sh
./bin/vamos deploy -f examples/journal/vamos.yaml
# Substitute your server's base domain:
URL=https://journal.apps.example.com
curl -fsS "$URL/readyz"
curl -fsS -H 'Content-Type: application/json' \
  --data '{"text":"persistence-marker-1"}' "$URL/entries"
curl -fsS --upload-file - "$URL/uploads/probe" <<'DATA'
upload-marker-1
DATA
curl -fsS "$URL/entries"
curl -fsS "$URL/uploads/probe"
```

`POST /entries` accepts `{"text":"..."}`, returns 201 with integer `id` and
`text`; `GET /entries` returns an ID-ordered JSON array. `PUT /uploads/NAME`
replaces raw bytes and returns 201 with `name` and `bytes`; `GET` returns those
bytes or 404. Names contain 1–64 ASCII letters, digits, underscores or hyphens.
Writes require Content-Length and are limited to 1 MiB each. `GET /readyz`
queries SQLite and returns `{"ready":true}`. These APIs do not implement an
atomic transaction spanning a database entry and an upload.

Change the image (for example add a comment to app.py), deploy again, and repeat
the GETs. An unchanged deploy need not restart the service. For the lifecycle
acceptance exercise, retain the same markers across failed startup, corrected
supersede, reboot, cleanup, destroy, and explicit `deploy --reuse-data` after
destroy. Public replacement has a durable HTTP 503 interval; failed readiness
never authorizes restarting the old binary against possibly changed data.
See [storage lifecycle](../../docs/volumes.md). These are testing instructions,
not a claim that live tests have already passed.

The image deliberately has no Dockerfile `VOLUME`. Vamos supplies `/data` with
no implicit image-data copy. The entrypoint starts as root only to inspect
permissions and initialize a **new empty root-owned** mount root to numeric
UID/GID 10001, mode 0700. It never recursively chowns/chmods existing data.
Existing trees must consist of directories/regular files owned by 10001:10001
with owner rwx/rw respectively; symlinks and incompatible permissions fail.
It drops supplementary groups and UID/GID before SQLite initialization or HTTP.
SQLite uses restart-safe `CREATE TABLE IF NOT EXISTS`; no schema migration,
adoption, copying, or repair is performed. `/data/journal.sqlite` and
`/data/uploads` share the single mount. Existing data is never silently fixed.

Local focused tests (no Docker required):

```sh
cd examples/journal
python3 -m unittest -v
```
