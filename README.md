# Soar

A small, Dockerfile-first app platform for one server. The intended stack is a Go CLI/executor, SSH, Docker Engine with BuildKit, and host-managed Caddy—not a new runtime or scheduler.

**Multi-service apps work:** up to eight Dockerfile-built or prebuilt-image public/private services per app, with private DNS, remote builds, readiness-checked public replacement, stop/start private replacement, Caddy routing/automatic HTTPS, and captured-input recovery. Root-only Ubuntu setup/teardown, preview-first app cleanup/destruction, and staged runtime secrets are implemented. Persistent local volumes use single-writer stop/start with durable public 503 routing and retained data. Protected manual updates, scoped deployment and noninteractive one-off commands are implemented; preview-first [code/config rollback](docs/rollback.md) is also implemented. See [the deployment boundary](docs/deployment.md) and [multi-service recovery semantics](docs/multi-service.md) before using it.

## Everyday commands

```sh
soar init notes --port 8080       # Minimal Dockerfile configuration; never overwrites
soar apps --server home           # Recorded app and retained-resource inventory
soar logs --service web --follow  # Follow one verified container, not future replacements
soar exec --service web -- python --version
soar console --service web       # Interactive /bin/sh in the existing service
```

See [CLI commands](docs/cli.md) for target selection, stdin/TTY options, exit codes,
private session staging and disconnect limits. Exec/console use live service data
and secrets; they do not build, deploy, or retry commands automatically.

## Independent service directories

One app can be deployed from several local source folders with unique service
names. Each manifest upserts only its declared selection; omitted services and
pending secrets stay untouched. Existing services require explicit local identity
binding; first new creation saves an acknowledged binding. See
[independent services](docs/services.md) for `service bind`, preview-first
`service remove`, re-add approval, per-service history and compatibility.
[Live acceptance](docs/services.md#live-acceptance) covers four independent
services sharing one database, binding protection, rollback and retained-data re-add.

## Platform scope

| Component | Target operating systems |
| --- | --- |
| Client CLI | macOS, Linux, and Windows |
| Deployment host / remote executor | Ubuntu 24.04 or newer, with systemd; amd64 or arm64 |

The Ubuntu restriction applies **only to deployment hosts**. The client does not require Ubuntu, APT, systemd, or a local Docker daemon; builds will run remotely. Host inspection uses the client's OpenSSH executable. Keep Ubuntu-specific setup in the remote executor, not in shared client code.

Older Ubuntu releases, other server distributions, and Ubuntu derivatives are outside the supported host scope. Host setup uses APT and systemd directly rather than a cross-distribution installation layer. Host inspection checks OS identity/version, architecture, running systemd, and the SSH user's UID. Setup requires root and refuses to adopt pre-existing runtimes, configurations, or data.

CI is configured for native Ubuntu 24.04, macOS, and Windows client checks. These exercise parsing, subprocess lifecycle, protocol handling, and OpenSSH argument acceptance without connecting to a remote server. They do not establish working host provisioning or deployment.

## Try it

Install [mise](https://mise.jdx.dev/getting-started.html) to manage the pinned Go toolchain and project commands. `mise.toml` pins Go 1.27.1; `go.mod` retains Go 1.25 as the minimum language/toolchain requirement. Race-enabled tests also require a system C toolchain (Xcode Command Line Tools on macOS, a C compiler on Linux, or a Go-compatible MinGW-w64 toolchain on Windows). Formatting checks use Bash; Windows contributors need Git Bash on PATH. These are development requirements, not client runtime dependencies. Docker is not required to build or test this scaffold.

```sh
mise trust       # Trust this project's configuration after reviewing it
mise install     # Install the pinned development tools
mise run deps    # Download Go modules (build/test also fetch missing modules)
mise run build
./bin/soar help
./bin/soar version
./bin/soar validate -f examples/dispatch/soar.yaml
```

From an app directory, `soar validate` defaults to `soar.yaml`. Validation checks the YAML schema and local constraints, including:

- Unknown/duplicate fields, multiple documents, and missing required values.
- Exactly one build specification or image per service.
- Explicit `auto`/`manual` update policy, defaulting to `auto` even with volumes.
- Names, environment/secret mappings, HTTP ports/readiness paths, and domain syntax.
- Duplicate domains, shared named volumes, and invalid mount paths.

It does **not** verify Dockerfile existence, image references against a registry, secret availability, DNS/TLS, server-wide hostname ownership, or live storage state. Success is not a deployment-readiness guarantee.

A service can declare one named volume with `volume: {name: data, mount: /data}`. The singular field takes an object, not a list; `volumes` is not accepted. Volume-backed deployment is implemented with single-writer stop/start and retained data. See the [storage lifecycle](docs/volumes.md); the original proposal is superseded.

Build contexts are relative to the configuration directory; Dockerfile paths are relative to their context and cannot escape it lexically. Container mount paths use POSIX syntax, must be clean absolute paths other than `/`, and are never host-directory mounts. Domains are ASCII DNS hostnames without a scheme, port, wildcard, or trailing dot (use punycode for international names). Environment values are passed literally; there is no interpolation.

## Inspect a host

```sh
# Establish and verify the host key separately using your normal SSH workflow.
# An SSH alias can configure the user, port, identity file, and jump host.
./bin/soar server inspect home
./bin/soar server inspect --json --timeout 20s root@server.example.com
```

On Windows, use `bin/soar.exe`. Flags precede the target. Targets accept SSH aliases or `[user@]host`, including unbracketed IPv6. Use SSH configuration for nonstandard ports and keys, not an arbitrary argument tail.

Inspection works on a fresh host without Soar, Docker, or Caddy installed. It runs a fixed shell collector over SSH; it does not upload files, install software, change host configuration, or invoke sudo. Ordinary SSH access can still produce authentication/audit logs and other system-managed side effects.

Host keys must already be trusted; authentication must work noninteractively with keys or an agent. Unknown/changed keys and password prompts fail rather than weakening SSH verification. Forwarding and connection reuse are disabled for the probe. Existing SSH configuration, including proxy/Match rules, remains trusted client configuration.

A compatible **non-root** host can pass inspection: its UID is reported, but privileged setup capability is explicitly unverified. A host-policy failure preserves detected facts and explains the incompatibility. Exit codes are 0 for a compatible host, 1 for incompatible hosts or inspection/connection failures, and 2 for invalid arguments. `--json` writes a report for completed inspections; transport errors go to stderr and do not produce a report.

See [the host inspection boundary](docs/host-inspection.md) for protocol details, limits, tests, and a real-host smoke-test checklist.

## Bootstrap and tear down a test host

```sh
mise run build  # Builds the client plus Linux amd64/arm64 executors in bin/
./bin/soar server add --yes --domain apps.example.com test root@server.example.com
./bin/soar server status test
# Repeating add with the same identity verifies an already-ready installation.
./bin/soar server add --yes --domain apps.example.com test root@server.example.com
./bin/soar server teardown --yes --purge test
```

Use a **fresh, dedicated, disposable host** for initial testing. Keep the Linux executor binaries beside the client, or supply `--executor FILE`. Setup does not change DNS, issue certificates, open firewall ports, or deploy apps. It registers the server locally only after remote setup succeeds; the first registration becomes the default.

Teardown is intentionally conservative: containers, images, volumes, build cache, custom networks, containerd resources, changed configuration, package-version drift, and mounted data block it. `--purge` authorizes deleting owned runtime data/certificates, not arbitrary workloads. A small ownership receipt remains for retry safety. APT/system logs and caches are not erased; this is not a byte-for-byte restoration of the original OS.

Remote operations run in a bounded systemd worker and survive SSH loss. If interrupted, query `server status NAME TARGET` before retrying the same command. An explicit target permits recovery even when setup failed before local registration. See [the lifecycle contract and recovery notes](docs/host-lifecycle.md).

## Deploy a stateless web app

With a bootstrapped server, wildcard DNS pointing at it, and TCP 80/443 allowed:

```sh
./bin/soar deploy -f examples/hello/soar.yaml
./bin/soar status -f examples/hello/soar.yaml
./bin/soar logs -f examples/hello/soar.yaml --tail 100
```

`examples/hello` is a runnable nginx-based Dockerfile app. With base domain `pastime.gg`, its generated URL is `https://hello.pastime.gg`. No local Docker or image registry is required. Caddy route activation is verified; public DNS and certificate readiness must be checked separately.

The first default-target contact creates a gitignored project binding under `.soar/`; `--server` overrides only one invocation. Contexts honor Docker ignore rules and always omit `.git`/`.soar`. The aggregate multibuild bundle is limited to 32 MiB. Exclude secrets explicitly. See [deployment and recovery](docs/deployment.md) for supported configuration, `resume`, `deploy --supersede`, retention limits, and verification results.

## Try SQLite and uploads

`examples/journal` is a runnable Python standard-library **demo, not a production
app; it has no authentication**. SQLite and uploads share one `/data` mount.

```sh
./bin/soar deploy -f examples/journal/soar.yaml
./bin/soar status -f examples/journal/soar.yaml
```

See its [write/read API and persistence exercise](examples/journal/README.md).
Each service may have one owned named local volume; sharing is forbidden and
bindings are immutable. Updates stop/start with public durable HTTP 503 downtime.
Data survives failed startup, supersede, reboot, cleanup and destroy; after
destroy, reuse requires explicit `deploy --reuse-data`. Status shows storage
identities, missing/retained state and possible mutation. There is no data deletion,
backup or restore command. See [the storage boundary](docs/volumes.md), including
legacy journal recovery and the limits of missing-storage checks.

## Add a private backend

`examples/private-backend` is a runnable web/private-backend example. Both services share one build; only `web` gets a public route. The web process connects to `http://backend:9000` through its app network.

```sh
./bin/soar deploy -f examples/private-backend/soar.yaml
./bin/soar status -f examples/private-backend/soar.yaml --service backend
./bin/soar logs -f examples/private-backend/soar.yaml --service backend
```

Deployments may select `--service NAME` and are **not atomic across services**. Private replacements have downtime; completed service changes survive later failure. See [multi-service deployment](docs/multi-service.md).

## Try PostgreSQL and explicit migrations

The [runnable Ledger demo](examples/ledger/README.md) bootstraps a private,
protected PostgreSQL service before building and initializing its API:

```sh
cd examples/ledger
soar secrets stage database-password
soar deploy --service db
soar run --service web -- ./bin/migrate
# Continue only after a successful migration:
soar deploy
```

Initial database creation needs no `--yes`. Ordinary deploy never refreshes or
restarts an existing manual database; protected changes require the exact scoped
command, `soar deploy --service db --yes`. Without approval, selecting an existing
manual service previews the plan. Runs are **noninteractive: no stdin or TTY**.
Inspect interrupted runs with `status --run OP` and `logs --run OP`; disconnects
do not cancel them, and Soar never automatically retries them. See the
[PostgreSQL contract](docs/postgres.md) and [run limits/recovery](docs/deployment.md#one-off-commands).
Ledger is unauthenticated and intended for disposable demo data, not production.

## Stage runtime secrets

Map environment variables to app-scoped names under `services.<name>.secrets`. From that app directory, with the built CLI on your PATH:

```sh
soar secrets stage session-secret  # No-echo prompt; flags precede the name
soar secrets list
soar status                       # Pending versions, never values
soar deploy                       # Apply captured staged versions
```

Use `--stdin` for automation, supplying values from a trusted secret producer—not literal shell arguments. Staging never restarts an app; resume and reboot keep pinned versions. Cleanup/destruction retain secrets, and host teardown refuses retained secret state. See [staged secrets](docs/secrets.md) for input, storage, recovery, and exposure limits.

## Clean up or remove an app

```sh
./bin/soar cleanup -f examples/hello/soar.yaml        # Preview; deletes nothing
./bin/soar cleanup -f examples/hello/soar.yaml --yes  # Apply a freshly computed plan
./bin/soar destroy --server droplet --app temporary-app
./bin/soar destroy --server droplet --app temporary-app --yes
```

Cleanup keeps effective versions, per-service predecessors and the current/previous completed app snapshots and never prunes shared Docker resources or volumes. Destruction proves the app route is gone before removing verified owned containers/network and known captured inputs. Both have durable recovery and explicit confirmation. See [the app lifecycle contract](docs/app-lifecycle.md) for retention limits, reboot verification, and upgrade notes. **Legacy deployments need a replacement deploy to gain pinned ports.**

App status/logs now stage a matching trusted executor temporarily, without replacing the bootstrap-owned installed binary; keep the Linux artifacts beside your client. This remains a root-SSH administration interface, not a restricted read-only account API.

## Layout

```text
cmd/soar/           Process entry point and build version
internal/cli/       Commands, output, and exit codes
internal/config/    Typed configuration, strict loading, validation
internal/host/      Shared read-only collector, parser, and Ubuntu policy
internal/ssh/       System OpenSSH bootstrap transport
internal/process/   Bounded RPC subprocesses and direct streaming execution
internal/session/   Private single-use, identity-pinned exec/log session staging
internal/executor/  Versioned JSON stdin/stdout host-operation protocol
internal/lifecycle/ Ubuntu setup/teardown, ownership journal, and safety checks
internal/registry/  Private, atomically written local server registrations
internal/buildcontext/ Bounded build-context capture with Docker ignore rules
internal/deploy/    Deployment/maintenance journals, Docker lifecycle, Caddy routing
internal/appstate/  Minimal completed-destruction receipt shared with host teardown
internal/secrets/   Private immutable secret versions and metadata-only selection
docs/               Implemented boundary and verification notes
examples/          Three design fixtures plus runnable hello, journal, ledger and private-backend apps
mise.toml          Pinned development tools and project tasks
PLAN.md            Product behavior, workflows, and implementation sequence
```

The Go module is `github.com/nchapman/soar`. No persistent Soar daemon or separate runtime framework is required. The SSH collector and host executor share one host-inspection implementation. Host operations use transient systemd workers, not a persistent Soar service.

## Examples

| Config | Scenario |
| --- | --- |
| [hello](examples/hello/soar.yaml) | Runnable single-service Dockerfile app supported today |
| [private-backend](examples/private-backend/soar.yaml) | Runnable public web/private backend sharing one Dockerfile build |
| [studio](examples/studio/soar.yaml) | Stateless website and multiple domains |
| [journal](examples/journal/soar.yaml) | SQLite and uploads; automatic updates with stop/start |
| [ledger](examples/ledger/soar.yaml) | API and private, manually updated PostgreSQL |
| [dispatch](examples/dispatch/soar.yaml) | Web, worker, PostgreSQL, and Redis |
| [workspace](examples/workspace/soar.yaml) | Separate frontend/API builds and a private renderer |

`hello`, `private-backend`, `journal`, and `ledger` are runnable applications supported by the current deployment implementation. The other three are design/configuration fixtures: Dockerfiles and application code are intentionally absent, and their broader workflows remain proposed in [PLAN.md](PLAN.md). Tests load all seven configurations.

## Development

```sh
mise tasks          # List available commands
mise run fmt        # Format Go source
mise run fmt-check  # Check formatting without modifying files
mise run vet        # Static analysis
mise run test       # Full Go test suite with race detection
mise run check      # Formatting, vet, Go tests, and build
mise run test:python       # Python example and integration-driver unit tests
mise run test:integration  # macOS: disposable OrbStack Linux checks + real lifecycle tests
```

Tasks run from the project root, including when invoked from a subdirectory. `check` runs its independent tasks in parallel. CI uses the same `mise.toml` and `mise run check`; there is no separate Makefile or Go version pin in the workflow.

Mise manages development tool versions; Go libraries remain in `go.mod`/`go.sum`. Use `mise exec -- go get <module>@<version>` to change a library dependency and `mise exec -- go mod tidy` to reconcile modules. `GOTOOLCHAIN=local` prevents Go from silently selecting a different toolchain than the mise pin. Shell activation is optional when using `mise run` or `mise exec`.

The default Go tests are offline apart from initial Go module downloads; no Docker daemon, SSH server, or credentials are needed. Native Unix tests run the read-only collector locally. If OpenSSH is installed, a test runs `ssh -G` with an empty SSH configuration to validate options without connecting. Subprocess tests exercise real child processes, cancellation, output limits, and inherited pipes. SSH boundary tests supply recorded probe results; they do not claim to test remote authentication or network behavior.

For real SSH, Docker, Caddy, HTTPS, and database lifecycle testing without a remote
droplet, use the [disposable OrbStack integration suite](docs/testing.md). It runs
in an isolated Linux machine, preserves diagnostics, and tears the machine down.
CI also runs the Python example and integration-driver unit tests.

To build a versioned binary:

```sh
mise exec -- go build -trimpath -ldflags '-X main.version=0.1.0-dev' -o bin/soar ./cmd/soar
```

Staged secrets, multi-service apps and [persistent local volumes](docs/volumes.md) are implemented. The original volume proposal is superseded; backup/restore/preparation remain deferred, and new storage full-suite/live acceptance results are not claimed here; broader workflows remain proposed in [PLAN.md](PLAN.md). Verification evidence and outstanding platform cases are tracked in [inspection](docs/host-inspection.md#observed-real-host-results), [host lifecycle](docs/host-lifecycle.md#verification), [deployment](docs/deployment.md#verification), and [app lifecycle](docs/app-lifecycle.md#verification).

## Custom domains

Public services support [custom-domain aliases](docs/domains.md) through
`http.domains`: up to 8 per service and 32 per app. Edit YAML, `soar deploy`,
then `soar status`; there is no separate `soar domains` command. Generated URLs
are always retained, and aliases do not redirect. Operators configure DNS and
open ports 80/443; Caddy manages public certificates automatically. Status
separates route state, DNS matching and locally verified TLS; it does not prove
Internet reachability or application health. Captured aliases participate in
[rollback](docs/rollback.md), subject to fresh server-wide ownership checks.
