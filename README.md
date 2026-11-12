# Soar

A small, Dockerfile-first app platform for one server. The intended stack is a Go CLI/executor, SSH, Docker Engine with BuildKit, and host-managed Caddy—not a new runtime or scheduler.

**This is not a deployment tool yet.** Local configuration validation and read-only host inspection over SSH work. The internal host-executor inspection protocol is also implemented. Server setup, Docker builds/runs, domains, secrets, deployment, and recovery remain planned in [PLAN.md](PLAN.md). No current command provisions a host or modifies Docker state.

## Platform scope

| Component | Target operating systems |
| --- | --- |
| Client CLI | macOS, Linux, and Windows |
| Deployment host / remote executor | Ubuntu 24.04 or newer, with systemd; amd64 or arm64 |

The Ubuntu restriction applies **only to deployment hosts**. The client does not require Ubuntu, APT, systemd, or a local Docker daemon; builds will run remotely. Host inspection uses the client's OpenSSH executable. Keep Ubuntu-specific setup in the remote executor, not in shared client code.

Older Ubuntu releases, other server distributions, and Ubuntu derivatives are outside the supported host scope. Host setup will use APT and systemd directly rather than a cross-distribution installation layer. Host inspection checks OS identity/version, architecture, running systemd, and the SSH user's UID. Server setup does not exist yet.

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

## Layout

```text
cmd/soar/           Process entry point and build version
internal/cli/       Commands, output, and exit codes
internal/config/    Typed configuration, strict loading, validation
internal/host/      Shared read-only collector, parser, and Ubuntu policy
internal/ssh/       System OpenSSH bootstrap transport
internal/process/   Bounded, cancellable subprocess execution
internal/executor/  Versioned JSON stdin/stdout inspection protocol
docs/               Implemented boundary and verification notes
examples/          Five configurations matching the design document
mise.toml          Pinned development tools and project tasks
PLAN.md            Product behavior, workflows, and implementation sequence
```

The Go module is `github.com/nchapman/soar`. There is no custom daemon, runtime abstraction, or deployment framework yet. The SSH bootstrap collector and installed executor share one host-inspection implementation; Docker integration and provisioning come next.

## Examples

| Config | Scenario |
| --- | --- |
| [studio](examples/studio/soar.yaml) | Stateless website and multiple domains |
| [journal](examples/journal/soar.yaml) | SQLite and uploads; automatic updates with stop/start |
| [ledger](examples/ledger/soar.yaml) | API and private, manually updated PostgreSQL |
| [dispatch](examples/dispatch/soar.yaml) | Web, worker, PostgreSQL, and Redis |
| [workspace](examples/workspace/soar.yaml) | Separate frontend/API builds and a private renderer |

These are configuration fixtures, not runnable sample applications: Dockerfiles and application code are intentionally absent. The proposed first-deploy and day-2 workflows are in [PLAN.md](PLAN.md). Tests load all five fixtures.

## Development

```sh
mise tasks          # List available commands
mise run fmt        # Format Go source
mise run fmt-check  # Check formatting without modifying files
mise run vet        # Static analysis
mise run test       # Full test suite with race detection
mise run check      # Formatting, vet, tests, and build
```

Tasks run from the project root, including when invoked from a subdirectory. `check` runs its independent tasks in parallel. CI uses the same `mise.toml` and `mise run check`; there is no separate Makefile or Go version pin in the workflow.

Mise manages development tool versions; Go libraries remain in `go.mod`/`go.sum`. Use `mise exec -- go get <module>@<version>` to change a library dependency and `mise exec -- go mod tidy` to reconcile modules. `GOTOOLCHAIN=local` prevents Go from silently selecting a different toolchain than the mise pin. Shell activation is optional when using `mise run` or `mise exec`.

Tests are offline apart from initial Go module downloads; no Docker daemon, SSH server, or credentials are needed. Native Unix tests run the read-only collector locally. If OpenSSH is installed, a test runs `ssh -G` with an empty SSH configuration to validate options without connecting. Subprocess tests exercise real child processes, cancellation, output limits, and inherited pipes. SSH boundary tests supply recorded probe results; they do not claim to test remote authentication or network behavior.

To build a versioned binary:

```sh
mise exec -- go build -trimpath -ldflags '-X main.version=0.1.0-dev' -o bin/soar ./cmd/soar
```

The next milestone is idempotent Ubuntu host setup, followed by one end-to-end stateless deployment: transfer a build context over SSH, build with Docker, start one service, and serve it through Caddy. Real-host inspection should be smoke-tested on a disposable Ubuntu 24.04 machine before provisioning is built on it.
