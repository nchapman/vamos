"""Real guest-only acceptance scenarios, invoked by linux_suite.run's harness.

No retries, sleeps, journal edits, global Docker prune, or secret-bearing argv.
Failure intentionally leaves the disposable VM intact for the parent to diagnose.
"""
import copy
import json
import secrets
import shutil
from pathlib import Path


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def identities(status):
    result = {}
    for name, service in status.get("services", {}).items():
        value = tuple(service.get(key) for key in ("container", "image", "incarnation"))
        require(all(value), f"incomplete identity for {name}")
        require(service["running"] and service["available"], f"unavailable {name}")
        result[name] = value
    return result


def run(h):
    """Bootstrap, exercise real SSH/Docker/HTTPS lifecycle, leave retained data."""
    summary = {"completed": [], "skipped": [
        "full kernel reboot/power loss and APT interruption (separate recovery suite covers selected worker kills and machine restart)",
        "registry/CI images, backup/restore, SQL password rotation",
        "deletion of retained data/secret provenance (no supported purge API)",
    ]}

    def done(name):
        summary["completed"].append(name)
        h.log("scenario passed: " + name)
        (h.artifacts / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")

    from build_network import configure_native, run as build_network
    configure_native(h)

    def bootstrap():
        h.cli("server", "add", "--yes", "--domain", "vamos.localhost", "local", "root@127.0.0.1")
        report = json.loads(h.cli("server", "status", "--json").stdout)
        require(not report["active"] and report["state"]["phase"] == "ready", "host did not settle")

    bootstrap()
    # Exercise a real Dockerfile build, deploy, default-server binding, and app
    # destroy before proving host teardown cleans up BuildKit, Caddy, and receipts.
    hello = h.work / "hello"
    shutil.copytree(h.root / "examples" / "hello", hello)
    h.command(["chown", "-R", "vamostest:vamostest", hello], user="root")
    h.hosts(["hello.vamos.localhost"])
    h.cli("deploy", cwd=hello)
    require((hello / ".vamos" / "server.json").is_file(), "deploy did not bind unbound project")
    require(b"ok" in h.https("hello.vamos.localhost", "/healthz"), "hello healthz did not respond")
    h.cli("destroy", "--yes", cwd=hello)
    h.cli("server", "teardown", "--yes", "--purge", "local")
    bootstrap()
    done("fresh host bootstrap/status, stateless app deploy/destroy, successful teardown and rebootstrap")
    build_network(h)
    done("isolated build service blocks host/app access and insecure entitlements")
    from secrets_set import run as secrets_set
    secrets_set(h)
    done("atomic multi-secret stage and runtime-only set for deployed consumers")

    app = "ledger-" + secrets.token_hex(4)
    base = h.work / app
    base.mkdir(parents=True, exist_ok=False)
    summary["app"] = app
    source = h.root / "examples" / "ledger"
    files = ("Dockerfile", "ledger.py", "migrate.py", "requirements.txt", "bin/migrate", ".dockerignore")

    def fixture(name, services, application=app):
        directory = base / name
        directory.mkdir()
        for filename in files:
            target = directory / filename
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(source / filename, target)
        manifest = {"app": application, "services": services}
        save(directory, manifest)
        return directory, manifest

    def save(directory, manifest):
        # JSON is a YAML subset; avoid a third-party YAML dependency in the runner.
        (directory / "vamos.yaml").write_text(json.dumps(manifest, indent=2) + "\n")
        h.command(["chown", "-R", "vamostest:vamostest", str(directory)], user="root")

    def cli(directory, operation, *args, error=None):
        return h.cli(operation, "--server", "local", *args, cwd=directory, expect_error=error)

    def service(operation, name, *args, directory=base, error=None):
        return h.cli("service", operation, name, "--server", "local", "--app", app,
                     *args, cwd=directory, expect_error=error)

    def stage(name, value):
        h.cli("secrets", "stage", "--server", "local", "--app", app,
              "--stdin", name, input=value)

    def stable(before, names):
        after = identities(h.status(app))
        for name in names:
            require(after[name] == before[name], f"unselected {name} changed")
        return after

    web = {"build": {"context": ".", "dockerfile": "Dockerfile"},
           "env": {"HOST": "database", "DB": "ledger", "USER": "ledger"},
           "secrets": {"PASSWORD": "app-password"},
           "http": {"port": 8080, "health": "/readyz"}}
    db = {"image": "postgres:17", "update": "manual",
          "env": {"POSTGRES_DB": "ledger", "POSTGRES_USER": "ledger"},
          "secrets": {"POSTGRES_PASSWORD": "database-password"},
          "volume": {"name": "data", "mount": "/var/lib/postgresql/data"}}
    database, _ = fixture("database", {"database": db})
    orders, om = fixture("orders", {"orders": copy.deepcopy(web)})
    billing, bm = fixture("billing", {"billing": copy.deepcopy(web)})
    duplicate, _ = fixture("duplicate", {"orders": copy.deepcopy(web)})
    password = secrets.token_urlsafe(36).encode()
    stage("database-password", password)
    stage("app-password", password)
    cli(database, "deploy")
    initial_db = identities(h.status(app))
    require(set(initial_db) == {"database"}, "bootstrap created extra services")
    cli(orders, "run", "--service", "orders", "--", "./bin/migrate")
    stable(initial_db, initial_db)
    cli(orders, "deploy")
    cli(billing, "deploy")
    h.trust_local_ca()
    hosts = {name: f"{app}-{name}.vamos.localhost" for name in ("orders", "billing")}
    alias = f"{app}-alias.vamos.localhost"
    h.hosts(list(hosts.values()) + [alias])
    row = json.loads(h.https(hosts["orders"], "/entries", {"amount_cents": 173, "description": app}))

    def rows():
        for host in hosts.values():
            require(row in json.loads(h.https(host, "/entries")), "shared ledger row lost")

    rows()
    original = identities(h.status(app))
    volume = h.status(app)["volumes"][0]["id"]
    require(volume, "missing volume identity")
    cli(duplicate, "deploy", error="explicitly bind")
    stable(original, original)
    service("bind", "orders", directory=duplicate)
    stable(original, original)
    done("partial directories, initial migration, shared rows, duplicate refusal and read-only binding")

    stage("database-password", secrets.token_urlsafe(36).encode())
    pending = h.status(app)
    db_secret = next(s for s in pending["secrets"] if s["name"] == "database-password")
    require(db_secret["pending"], "database secret not pending")
    for directory, manifest, name in ((orders, om, "orders"), (billing, bm, "billing")):
        before = identities(h.status(app))
        # A real image change, not just a manifest change.
        with (directory / "ledger.py").open("a") as stream:
            stream.write("\n# integration release two\n")
        if name == "orders":
            manifest["services"][name]["http"]["domains"] = [alias]
            save(directory, manifest)
        cli(directory, "deploy")
        after = stable(before, set(before) - {name})
        require(after[name][1] != before[name][1], "update did not build a distinct image")
    require(next(s for s in h.status(app)["secrets"] if s["name"] == "database-password") == db_secret,
            "omitted database secret pins changed")
    require(row in json.loads(h.https(alias, "/entries")), "alias did not reach orders")
    bm["services"]["billing"]["http"]["domains"] = [alias]
    save(billing, bm)
    before = identities(h.status(app))
    cli(billing, "deploy", error="already assigned")
    stable(before, before)
    del bm["services"]["billing"]["http"]["domains"]
    save(billing, bm)
    cli(orders, "cleanup", "--yes")
    cli(orders, "rollback", "--service", "orders")
    stable(before, before)
    cli(orders, "rollback", "--service", "orders", "--yes")
    after = stable(before, ("billing", "database"))
    require(after["orders"][1] == original["orders"][1], "rollback did not restore exact image")
    rows()
    done("pending omitted DB secret, interleaved updates, alias collision, cleanup and scoped rollback")

    before = identities(h.status(app))
    service("remove", "database")
    stable(before, before)
    service("remove", "database", "--yes", error="allow-protected")
    stable(before, before)
    service("remove", "database", "--yes", "--allow-protected")
    require("database" not in h.status(app).get("services", {}), "database removal failed")
    stable(before, ("orders", "billing"))
    stage("database-password", password)  # Initialization values do not rotate SQL roles.
    cli(database, "deploy", "--re-add", "--service", "database", "--yes", error="reuse-data")
    stable(before, ("orders", "billing"))
    cli(database, "deploy", "--re-add", "--service", "database", "--yes", "--reuse-data")
    # Private running != SQL-ready: use the example's bounded connection initializer.
    cli(orders, "run", "--service", "orders", "--", "./bin/migrate")
    after = stable(before, ("orders", "billing"))
    require(after["database"][2] != before["database"][2], "database incarnation reused")
    require(h.status(app)["volumes"][0]["id"] == volume, "retained volume identity changed")
    rows()
    done("protected removal approval, exact retained volume reuse and row retention")

    before = identities(h.status(app))
    service("remove", "orders")
    stable(before, before)
    service("remove", "orders", "--yes")
    require("orders" not in h.status(app).get("services", {}), "orders removal failed")
    cli(orders, "deploy", error="--re-add")
    cli(orders, "deploy", "--re-add", "--service", "orders", "--yes")
    after = stable(before, ("database", "billing"))
    require(after["orders"][2] != before["orders"][2], "orders incarnation reused")
    cli(duplicate, "deploy", error="stale")
    stable(after, after)
    rows()
    done("ordinary removal, explicit re-add and stale incarnation refusal")

    failed, _ = fixture("failed", {"broken": {
        "build": {"context": ".", "dockerfile": "Dockerfile"},
        "command": ["python", "-c", "raise SystemExit(3)"],
    }})
    before = identities(h.status(app))
    cli(failed, "deploy", error="candidate")
    # Incomplete broken service has no current tuple; compare only survivors.
    failed_status = h.status(app)
    require("broken" in failed_status["services"], "failed creation not recorded")
    require(not failed_status["services"]["broken"]["available"], "failed candidate presented as available")
    for name, identity in before.items():
        s = failed_status["services"][name]
        require(tuple(s[k] for k in ("container", "image", "incarnation")) == identity,
                "failed creation changed survivor")
    service("remove", "broken", "--yes")
    stable(before, before)
    require("broken" not in h.status(app)["services"], "failed service not abandoned")
    done("failed initial private service abandonment leaves survivors unchanged")

    foreign_app = app + "-foreign"
    foreign, _ = fixture("foreign", {"probe": {
        "build": {"context": ".", "dockerfile": "Dockerfile"},
        "command": ["python", "-m", "http.server", "8080"],
    }}, application=foreign_app)
    cli(foreign, "deploy")
    db_container = identities(h.status(app))["database"][0]
    networks = json.loads(h.command([
        "docker", "inspect", "--format", "{{json .NetworkSettings.Networks}}", db_container
    ], user="root").stdout)
    require(len(networks) == 1, "database has unexpected network membership")
    address = next(iter(networks.values()))["IPAddress"]
    require(address, "database IP missing")
    probe = ("import socket\n"
             "try: socket.getaddrinfo('database',5432)\n"
             "except socket.gaierror: pass\n"
             "else: raise SystemExit('foreign DNS leaked')\n"
             f"try: s=socket.create_connection(({address!r},5432),timeout=2)\n"
             "except OSError: pass\n"
             "else: s.close(); raise SystemExit('foreign TCP connected')\n"
             "print('isolation verified')\n")
    cli(foreign, "run", "--service", "probe", "--", "python", "-c", probe)
    stable(before, before)
    rows()
    cli(foreign, "destroy", "--yes")
    require(h.status(foreign_app)["phase"] == "destroyed", "foreign app not destroyed")
    done("foreign app cannot resolve database DNS or connect directly to database IP")
    summary["skipped"].append(
        "rollback refusal for removed membership and removed alias negative HTTPS check")
    # Destroy is explicitly not a data purge; never edit production journals to
    # trick teardown into passing. The parent disposes of the isolated guest.
    before = identities(h.status(app))
    h.cli("server", "upgrade", "--yes", "local")
    stable(before, before)
    cli(orders, "destroy")
    stable(before, before)
    cli(orders, "destroy", "--yes")
    destroyed = h.status(app)
    require(destroyed["phase"] == "destroyed", "destroy receipt missing")
    require(any(v["id"] == volume and not v["missing"] for v in destroyed["volumes"]),
            "destroy lost retained volume")
    require(destroyed.get("secrets"), "destroy lost retained secrets")
    # Both retained stores and retained volume provenance are legitimate guards;
    # their checking order is an implementation detail.
    h.cli("server", "teardown", "--yes", "--purge", "local", expect_error="retained")
    summary["retained_volume"] = volume
    summary["final_cleanup"] = "parent destroys disposable VM; final host teardown correctly refused"
    done("destroy retains data/secrets; final host teardown refuses retained state")
