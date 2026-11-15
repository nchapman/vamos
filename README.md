# Vamos

Vamos is a small, Dockerfile-first tool for deploying and running apps on a single server.

You write a `Dockerfile` and a short `vamos.yaml`, and Vamos handles remote builds, readiness-checked rollouts, automatic HTTPS via Caddy, staged secrets, persistent volumes, and one-off commands over SSH. There is no cluster, no Kubernetes, and no local Docker daemon required on your laptop.

Under the hood, Vamos pairs a local Go CLI with a lightweight coordinator (`vamos-agent`) and executor on an Ubuntu 24.04+ server running Docker Engine, BuildKit, and Caddy.

## Quickstart

### 1. Install Vamos

On macOS or Linux, install the latest release (`vamos` plus the matching Linux `amd64`/`arm64` executor binaries) into `~/.local/bin`:

```sh
curl -fsSL https://raw.githubusercontent.com/nchapman/vamos/main/install.sh | sh
```

Or download an archive from [GitHub Releases](https://github.com/nchapman/vamos/releases) (macOS, Linux, or Windows) and place all three binaries (`vamos`, `vamos-linux-amd64`, and `vamos-linux-arm64`) together in a directory on your `PATH`.

To upgrade later, run:

```sh
vamos upgrade
```

### 2. Inspect and bootstrap a server

Start with a **fresh Ubuntu 24.04+ server** (amd64 or arm64) with root SSH access and an already-trusted host key. Point a wildcard DNS record (e.g. `*.apps.example.com`) at the server and allow TCP ports `80` and `443`.

```sh
# Read-only compatibility check
vamos server inspect root@server.example.com

# Install Docker, Caddy, and vamos-agent, and register the server locally as "prod"
vamos server add --yes --domain apps.example.com prod root@server.example.com
```

Once bootstrapped, `vamos server add` installs a restricted non-root SSH bridge key for `vamos-agent` so day-to-day app commands (`deploy`, `logs`, `exec`, `secrets`, etc.) do not require root SSH.

### 3. Deploy an app

You can try the included `hello` example right away (from a clone of this repo):

```sh
vamos deploy -f examples/hello/vamos.yaml
vamos status -f examples/hello/vamos.yaml
vamos logs -f examples/hello/vamos.yaml --tail 100
```

Or initialize a new app in your own project directory:

```sh
vamos init myapp --port 8080
vamos validate
vamos deploy
```

With base domain `apps.example.com`, `myapp` is served at `https://myapp.apps.example.com` with automatic TLS.

### 4. Inspect, debug, and clean up

```sh
vamos apps                                # List deployed apps on the server
vamos exec --service web -- sh -c 'id'    # Run a command in a live container
vamos console --service web               # Open an interactive shell
vamos cleanup -f examples/hello/vamos.yaml --yes   # Prune superseded releases
vamos destroy -f examples/hello/vamos.yaml --yes   # Remove the app and its routes
```

## What Vamos supports

- **Multi-service apps** — Up to 8 services per app (Dockerfile-built or prebuilt image) sharing a private Docker network, with public HTTP routing on selected services, private service-to-service DNS, and per-service `replicas` (up to 8 stateless instances behind Caddy load balancing) and `resources` (`cpus`, `memory_mb`).
- **Zero-downtime public rollouts** — New containers must pass HTTP readiness checks before Caddy switches traffic; failed rollouts leave the previous release serving.
- **Persistent volumes** — One named local volume per service (`volume: {name: data, mount: /data}`) with single-writer stop/start replacement, durable 503 maintenance routing during updates, and retained data across cleanup/destroy.
- **Staged runtime secrets** — Stage app-scoped secrets without restarting (`vamos secrets stage`), then apply them on the next `vamos deploy` or `vamos secrets set`. Secret values are never printed in status output or logs.
- **Release & one-off commands** — Run pre-rollout release hooks (`release: {service: web, command: [...]}`) or ad-hoc noninteractive jobs (`vamos run --service web -- ...`) inside the captured image and network.
- **Preview-first rollbacks** — Inspect and apply code/config rollbacks with `vamos rollback`.
- **Custom domains** — Add `http.domains` aliases (up to 8 per service) alongside the generated app URL; Caddy manages TLS certificates automatically.
- **Independent service directories** — Deploy services from separate repositories/folders into one app using `--service` and `vamos service bind` / `vamos service remove`.
- **Encrypted backup repositories & plans** — Configure S3-compatible restic repositories (`vamos backup repository`) and shared backup plans (`vamos backup configure` / `vamos backup plan`). *(Application capture, restore, and scheduling are not yet implemented.)*
- **Crash-safe remote execution** — Mutations run inside systemd units supervised by `vamos-agent`. Disconnecting SSH does not abort an in-progress deploy; inspect with `vamos status` and continue with `vamos resume <operation-id>` or replace with `vamos deploy --supersede <operation-id>`.

## Everyday commands

```sh
vamos init myapp --port 8080          # Create a minimal vamos.yaml
vamos validate                        # Check vamos.yaml syntax and constraints
vamos deploy                          # Build, push/load, and roll out
vamos status                          # Show service health, routes, TLS, and pending work
vamos logs --service web --follow     # Stream logs from a verified container
vamos exec --service web -- CMD...    # Run a command in a live container
vamos console --service web           # Interactive shell in a live container
vamos run --service web -- CMD...     # One-off noninteractive container run
vamos secrets stage NAME              # Prompt (no-echo) and stage a secret
vamos rollback                        # Preview rollback to a previous operation
vamos apps                            # List apps and retained resources on the server
vamos upgrade                         # Upgrade vamos and Linux executors from GitHub Releases
vamos server upgrade --yes prod       # Upgrade vamos-agent on a managed server
```

Run `vamos help` or `vamos <command> --help` for full options and flags.

## Platform scope

| Component | Supported OS |
| --- | --- |
| Local CLI (`vamos`) | macOS, Linux, Windows |
| Target server (`vamos-agent` & executor) | Ubuntu 24.04+ with systemd (`amd64` or `arm64`) |

The local CLI does not require Docker, systemd, or Linux. Server lifecycle commands (`server add`, `server upgrade`, `server teardown`, `server connect`) require root SSH; normal app commands use the restricted `vamos` SSH bridge configured by `server add`.

## Examples

| Example | Description |
| --- | --- |
| [hello](examples/hello/vamos.yaml) | Single-service Dockerfile web app |
| [private-backend](examples/private-backend/vamos.yaml) | Public web service + private backend sharing one build |
| [journal](examples/journal/vamos.yaml) | Python app with SQLite and file uploads on a persistent `/data` volume |
| [ledger](examples/ledger/vamos.yaml) | API + private manually updated PostgreSQL with `vamos run` migrations |
| [rails-ledger](examples/rails-ledger/README.md) | Rails + PostgreSQL + Solid Queue worker with `release` migrations |
| [studio](examples/studio/vamos.yaml), [dispatch](examples/dispatch/vamos.yaml), [workspace](examples/workspace/vamos.yaml) | Multi-service configuration fixtures (YAML validation targets) |

## Development

Prerequisites: [mise](https://mise.jdx.dev/getting-started.html) (pins Go `1.27.1`) and a C toolchain for `-race` tests.

```sh
mise trust && mise install # Install pinned Go toolchain
mise run build             # Build vamos + Linux amd64/arm64 executors into bin/
mise run check             # Format check, go vet, go test -race ./..., and build bundle
mise run lint              # Run golangci-lint (if installed)
mise run fmt               # Format Go source
mise run test:python       # Run Python example and integration-driver unit tests
mise run test:integration  # macOS: full OrbStack Linux VM acceptance suite
```

All default Go and Python tests run offline without Docker or a remote server.

## Documentation

Detailed contracts and architecture notes live in `docs/`:

- [CLI reference](docs/cli.md)
- [Host inspection](docs/host-inspection.md) & [Host lifecycle (`add`, `upgrade`, `connect`, `teardown`)](docs/host-lifecycle.md)
- [Deployment & recovery](docs/deployment.md), [Multi-service apps](docs/multi-service.md), & [Rollback](docs/rollback.md)
- [Volumes](docs/volumes.md), [Secrets](docs/secrets.md), [Custom domains](docs/domains.md), [Resources](docs/resources.md), & [Release commands](docs/release-commands.md)
- [Independent services](docs/services.md) & [PostgreSQL](docs/postgres.md)
- [Backup repositories](docs/backup-repositories.md) & [Backup plans](docs/backup-plans.md)
- [Operations checklist](docs/operations.md) & [Testing](docs/testing.md)
