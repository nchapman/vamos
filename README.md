# Soar

A small, Dockerfile-first app platform for one server. The intended stack is a Go CLI/executor, SSH, Docker Engine with BuildKit, and host-managed Caddy—not a new runtime or scheduler.

**This is a scaffold, not a deployment tool yet.** Local configuration validation works. Server setup, Docker builds/runs, SSH transport, domains, secrets, deployment, and recovery are described in [PLAN.md](PLAN.md) but are not implemented. Current commands never contact a server or modify Docker state.

## Try it

Install [mise](https://mise.jdx.dev/getting-started.html) to manage the pinned Go toolchain and project commands. `mise.toml` pins Go 1.27.1; `go.mod` retains Go 1.25 as the minimum language/toolchain requirement. Race-enabled tests also require a system C toolchain (Xcode Command Line Tools on macOS or a C compiler on Linux). Docker is not required to build or test this scaffold.

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

## Layout

```text
cmd/soar/           Process entry point and build version
internal/cli/       Commands, output, and exit codes
internal/config/    Typed configuration, strict loading, validation
examples/          Five configurations matching the design document
mise.toml          Pinned development tools and project tasks
PLAN.md            Product behavior, workflows, and implementation sequence
```

The Go module is `github.com/nchapman/soar`. There is intentionally no speculative runtime interface, daemon, or empty deployment framework. Add SSH, Docker, and host-executor packages with the first working deployment slice.

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

Tests are offline apart from initial Go module downloads; no Docker daemon, SSH server, or credentials are needed. CLI tests use temporary directories and captured output. Configuration tests cover invalid inputs and storage/network declarations without mocking runtime infrastructure.

To build a versioned binary:

```sh
mise exec -- go build -trimpath -ldflags '-X main.version=0.1.0-dev' -o bin/soar ./cmd/soar
```

The next milestone is a narrow end-to-end deployment: transfer a build context over system SSH, build with Docker, start one stateless service, and serve it through Caddy. Implement the associated failure-path tests with that behavior rather than pretending a scaffold deploys anything today.
