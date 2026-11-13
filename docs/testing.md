# Testing

## Fast checks (no VM)

```sh
mise run check        # Go formatting, vet, race-enabled tests, native and Linux builds
mise run test:python  # Python 3: integration-driver safety tests and both example suites
```

The Go tests include the OrbStack runner's ownership, cleanup, cancellation,
archive-selection and failure-path tests, using a fake external command boundary.
They never start a machine. Python driver tests also run without root, Docker,
SSH, or a VM. CI runs both sets; real OrbStack tests are opt-in on macOS.

## Disposable Linux integration tests

Install and start [OrbStack](https://orbstack.dev/), then run from the repository:

```sh
mise run test:integration
```

This creates a fresh **Ubuntu 24.04** machine using the Mac's native architecture,
runs the checks and real lifecycle scenarios, saves diagnostics, and deletes the
machine. The runner requires macOS, `orbctl` on PATH, and the pinned Go toolchain.
Python and the Linux build/SSH dependencies are installed inside the guest; no
Mac Python, Docker CLI, registry credentials, or droplet is needed for this task.

The default machine has a 4 GiB memory limit, four CPU cores, and a 20 GiB disk
limit. Internet access is required for the Ubuntu image, APT, the checksum-verified
pinned Go distribution, Go modules, container base images and Python packages.
This is local execution, **not an offline test**. Application images are built
from the copied source inside the guest; they are not published to a registry.

For another supported Ubuntu release or architecture:

```sh
mise exec -- go run ./integration/orbstack --ubuntu 26.04
mise exec -- go run ./integration/orbstack --arch amd64
```

An architecture different from the Mac uses OrbStack's emulation and is slower;
it is not evidence of execution on native hardware of that architecture.

### Isolation and cleanup

- Each run generates a new random machine name and records the exact machine ID
  in `.pi/integration/<run>/manifest.json`. Existing machines are never adopted.
- Creation uses `--isolated --isolate-network`: no Mac filesystem mounts, no SSH
  agent forwarding, and no communication with other local machines or host IPs.
  The runner verifies these settings before sending source or running tests and
  removes `ORBENV` so ambient Mac variables are not opted into guest commands.
- Only the repository's `cmd`, `internal`, `integration`, `examples`, `go.mod`,
  `go.sum`, and `mise.toml` are archived. `.git`, `.soar`, `.pi`, `.env*`, and Python
  caches are excluded; symlinks and special files are rejected. This captures
  current working files, including uncommitted code—not just HEAD.
- The Linux suite runs the client as an unprivileged guest user against a real
  SSH server bound to guest loopback. The guest generates its own keys and pins
  its SSH host key using the trusted VM control channel. Mac SSH configuration,
  keys, Soar registry, service bindings and Docker context are not used.
- Soar's real bootstrap installs and manages Docker and Caddy inside the guest.
  HTTPS uses `.localhost` names and Caddy's internal CA. Trust installation is
  confined to the guest; curl verifies the certificate and hostname. No public
  DNS, public ACME issuer, or Mac trust-store modification is needed.
- Success, failure and ordinary Ctrl-C/termination all attempt artifact collection
  and deletion by verified machine ID, using a fresh cleanup context. A cleanup
  failure makes the run fail rather than claiming success.

A killed runner (`SIGKILL`), host crash, or unavailable OrbStack daemon can prevent
cleanup. The manifest and console contain the recorded machine ID; inspect it
with `orbctl info -f json ID` before explicitly deleting it with
`orbctl delete --force ID`. Never use `--all`, reset OrbStack, or delete another
machine merely because its name looks like a test run. If creation was interrupted
before ownership was established, the runner deliberately leaves the ambiguous
machine for manual inspection rather than risking someone else's machine.

To preserve a failed run for debugging:

```sh
mise exec -- go run ./integration/orbstack --keep-on-failure
```

Successful runs still delete their machine. A preserved guest contains generated
test credentials and retained test data; remove it when finished. There is no
reuse-existing-machine mode, avoiding accidental execution on a valuable host.

### What runs

The guest performs Go formatting checks, vet, the full race-enabled Go test suite,
native/Linux amd64/arm64 builds, both Python example suites, driver unit tests,
and validation of all example manifests. The Go version comes from `mise.toml`,
with the downloaded distribution checked against Go's official SHA-256 metadata.

The real CLI scenarios exercise:

1. Host bootstrap/status, successful empty-host teardown, and rebootstrap.
2. Independent orders and billing directories sharing a protected PostgreSQL
   database, an explicit migration, and shared rows over verified HTTPS.
3. Refusal to adopt an existing service, explicit local binding, and unchanged
   container/image/incarnation identities for omitted services.
4. A pending database secret excluded from unrelated updates; independent image
   updates, alias routing/collision, cleanup, and scoped rollback.
5. Protected database removal, required retained-volume reuse approval, a new
   incarnation, and preservation of the exact volume and ledger row.
6. Ordinary service removal/re-add, stale binding refusal, and abandonment of a
   failed initial private service without changing survivors.
7. Cross-app private DNS and direct-IP isolation.
8. App destruction retaining data/secrets and host teardown refusing those retained
   resources. The runner then destroys the whole disposable machine; it does not
   modify retention journals or bypass production safeguards to make teardown pass.

Artifacts include `runner.log`, `manifest.json`, the exact `source.tar`, and
`guest-artifacts.tar`. The guest archive contains numbered command/stdout/stderr
files, `result.json`, scenario progress in `summary.json`, and bounded systemd/
Docker/Caddy diagnostics. `coverage.out` and `coverage.html` report the Go test
suite's statement coverage; the separately built CLI/executor integration paths
are not instrumented, so this is not an end-to-end coverage percentage. Secret staging input and process environments are not
logged; SSH private keys and database files are not collected. Artifacts are kept
in a private, Git-ignored directory and can still contain source and test output;
inspect them before sharing. Remove old run directories when no longer needed.

### Verified run

The complete default flow passed on **OrbStack 2.2.5, Ubuntu 24.04.5, native
arm64**, including all eight scenario groups and automatic machine deletion.
The run took approximately 7½ minutes. The generated Go test report measured
**78.0% statement coverage**; this is a baseline, not a threshold or a claim of
end-to-end instrumented coverage. Failure cleanup and explicit keep-on-failure
were also exercised while bringing up the suite; the retained debugging machine
was subsequently deleted.

Ubuntu 26.04 and emulated amd64 are selectable but were not run through this
complete suite. Ordinary signal cleanup has deterministic runner test coverage;
a live forced interruption was not used as an acceptance test.

### Limits

This does not replace public-DNS/ACME smoke tests or cloud-network testing. The
`.localhost` checks establish real local TLS and proxy routing, not publicly
reachable HTTPS or certificate-renewal behavior. OrbStack uses its own Linux
kernel and virtualization environment; results do not establish identical cloud
kernel, firewall, reboot or power-loss behavior. In particular, the tested isolated
guest rejects BuildKit's nested overlay mount. The suite creates a guest-only
`docker-container` Buildx builder with the **native copying snapshotter**, retaining
machine isolation rather than weakening it. Soar's managed Docker configuration
and the Mac Docker daemon are unchanged. This tests real builds and containers,
but not the droplet's default BuildKit snapshotter; the extra builder resources
are disposed of with the VM.

Forced live worker interruption, APT interruption, reboot/power loss, registry
credentials, backup/restore and SQL password rotation are not covered here.
Recovery branches retain the existing deterministic Go coverage. CI still provides
native macOS/Linux/Windows client checks; the OrbStack suite is a Linux client and
host integration test, not a Windows execution test.
