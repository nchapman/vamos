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

## CLI visual harness

With Node.js 20+, npm, the pinned Go toolchain, and locally installed Google Chrome:

```sh
mise run test:visual
```

This opt-in task runs `npm ci --ignore-scripts`, `npm test`, and `npm run capture`
in `integration/visual`. Dependency installation may need registry access; capture
uses only an ephemeral loopback HTTP server, which it closes afterward. It does
not contact deployment servers. Set `VAMOS_VISUAL_CHROME` to an installed Chrome
executable if the default Playwright `chrome` channel is unavailable; no browser
is downloaded by this harness. Fixture generation has a five-minute process
limit (and a two-minute Go test limit); browser startup, capture, and cleanup
steps have 30-second deadlines so stalled rendering can fail and clean up.

Capture runs `TestPresentationVisualFixtures` in `internal/cli` with
`VAMOS_VISUAL_DIR` pointing to `.pi/visual`. That test supplies actual production
renderer output in `fixtures.json`: an array of `{case, theme, width, ansi}`
objects, not hand-written HTML replicas. Missing fixtures fail capture. The matrix
covers 48/80/120 columns and light/dark/mono/terminal modes. Mono and inherited
terminal foreground modes are rendered on both controlled light and dark
backgrounds in xterm.js and installed Chrome. The 144-fixture, 216-capture matrix
includes successful replica groups and blocked release commands as well as the
existing help, status, inventory, maintenance and recovery views.

Generated bundles, fixtures, screenshots (`shots/`), and `evaluation.json` stay in
Git-ignored `.pi/visual/`. Review the screenshots as well as the evaluation: this
is not a pixel-baseline regression test. Evaluation checks logical text (SGR
removed, soft wraps joined, ASCII tabs expanded at eight-column stops, right
space padding ignored), visible-cell contrast of at least 4.5:1 on the controlled
backgrounds, and browser errors. Eligible standalone, unindented `vamos` command
lines are selected through xterm into a non-executing textarea. This does not test
the system clipboard, execute suggestions, or validate every help example.

For just the dependency-free model tests, run `npm test` in `integration/visual`.
They cover fixture validation, control rejection, logical-line normalization,
selection eligibility, and contrast calculations. Native-terminal rendering,
custom palettes/fonts, real clipboard behavior, and screen-reader usability need
separate manual checks; browser screenshots do not establish those properties or
guarantee contrast on arbitrary backgrounds. See [terminal presentation](cli.md#terminal-presentation)
for environment controls and unchanged plain/stream output contracts.

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
  `go.sum`, and `mise.toml` are archived. `.git`, `.vamos`, `.pi`, `.env*`, and Python
  caches are excluded; symlinks and special files are rejected. This captures
  current working files, including uncommitted code—not just HEAD.
- The Linux suite runs the client as an unprivileged guest user against a real
  SSH server bound to guest loopback. The guest generates its own keys and pins
  its SSH host key using the trusted VM control channel. Mac SSH configuration,
  keys, Vamos registry, service bindings and Docker context are not used.
- Vamos's real bootstrap installs Docker and runs its pinned Caddy container inside the guest.
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
   resources. Tests do not modify retention journals or bypass production safeguards
   to make teardown pass.

The runtime scenarios additionally exercise a real Rails/PostgreSQL/Solid Queue
app and the existing Python Ledger, writing a separate `runtime-summary.json`:

- Inspect actual Docker limits and kernel cgroup-v2 CPU, memory and swap ceilings.
- Bootstrap a protected database separately; run one captured release before any
  selected runtime creation, then start two web replicas and a separate worker
  from the same immutable Rails image.
- Run the Rails database-backed test suite through a separate one-off test image
  and disposable `DB_TEST_NAME`, never the application's live database. Restore
  the production Docker ignore rules before further deployments.
- Read shared data from every loopback replica and verified HTTPS upstream;
  observe a distinct worker processing a durable Solid Queue job.
- Require explicit replica selection for logs, exec and PTY console, and check
  selected container identity and log-stream separation.
- Preserve web/DB identities and manual secret pins across a scoped worker update.
- Fail a release with a durable SQL attempt counter, prove all old runtimes and
  routes unchanged, and prove resume does not run the command again. Explicitly
  supersede with a new successful release, then verify retained-data cleanup.
- Repeat the release/replica/resource contract with the Python Ledger rather
  than relying on framework-specific platform behavior.

These are single-host acceptance checks, not throughput, multi-host availability,
production database-role hardening or full power-loss guarantees.

A second app exercises recovery and diagnostics:

- Failing Docker builds preserve useful, bounded diagnostics for deploy and one-off
  run status/logs. A staged test sentinel is absent from responses and journals;
  a failed build never executes its requested command or replaces a serving app.
- Disconnect the client and SSH process group while the independent worker is
  paused at a build boundary. Release it and verify completion from captured
  source/configuration, despite subsequent edits to the local directory.
- Kill the worker after Caddy has reloaded the new route but before promotion is
  committed. Resume must keep the same candidate and ignore unsubmitted edits.
- Kill service removal after route withdrawal and before container removal. The
  exact operation resumes, and ordinary deploy cannot resurrect the service.
- Kill a private-service replacement after stopping its retired container. The Mac
  coordinator restarts the **same owned machine**, then the guest verifies that
  the old process stays stopped, app identities and PostgreSQL data survive,
  staged database credentials remain pending, and explicit resume completes the
  captured replacement.

Checkpoints use a guest-only Docker wrapper (Caddy runs through `docker exec`), private Unix sockets and Linux
pidfds. They do not modify the production executor, inject timing-dependent sleeps,
change Vamos journals, or disable machine isolation. Nonmatching commands execute
the original binaries. A gate is one-shot and fails closed without its explicit
release; both sides have bounded socket deadlines, and killing the worker also
kills its blocked gate process. Restart is
coordinated outside the guest, with ownership/isolation checked again before and
after. Restart and its identity checks have a three-minute deadline; post-restart
verification has a ten-minute deadline. Expiry still enters fresh-context cleanup.
The guest verifies that PID 1's start identity actually changed; this is
an OrbStack machine restart, not a claim of full shared-kernel reboot or power loss.
The complete run deletes the machine after post-restart verification.

The CLI acceptance app additionally checks:

- `init` creates a valid, default-bound manifest and refuses to overwrite it.
- `apps --json` lists live recorded state and destroyed apps with retained data.
- `exec` preserves literal argv, 256 KiB of binary stdin/output, separate stderr,
  the image user and working directory, and known exit codes; SSH 255 is ambiguous.
- `console` rejects pipes, then runs through a real Linux PTY. Ctrl-C interrupts
  a foreground program without losing the shell; window resize reaches the inner
  terminal; shell exit status and local terminal modes are preserved.
- `logs --follow` returns both the initial tail and subsequent request logs and
  respects its explicit deadline. Sessions leave the service identity unchanged;
  ordinary completion removes private session staging.

The DX checks additionally exercise quoted manifest paths from another working
directory, context-preserving binding/approval suggestions, inventory attribution,
snapshot stdout/stderr separation, explicit-target recovery hints, nested help, and
a real value-free secret-store permission refusal. The permission fault affects only
the disposable CLI test app and is restored in `finally`; no live service replacement
is authorized by the preview check.

Artifacts include `runner.log`, `manifest.json`, the exact `source.tar`, and
`guest-artifacts.tar`. The guest archive contains numbered command/stdout/stderr
files, `result.json`, scenario progress in `summary.json` and
`recovery-summary.json`, `cli-summary.json`, `runtime-summary.json`, CLI terminal/follow transcripts,
checkpoint-client output, and bounded systemd/Docker/Caddy
diagnostics. Successful preparation is not overall success: `result.json` only
sets `passed` after post-restart verification. `coverage.out` and `coverage.html` report the Go test
suite's statement coverage; the separately built CLI/executor integration paths
are not instrumented, so this is not an end-to-end coverage percentage. Secret staging input and process environments are not
logged; SSH private keys and database files are not collected. Artifacts are kept
in a private, Git-ignored directory and can still contain source and test output;
inspect them before sharing. Remove old run directories when no longer needed.

### Verified run

The original eight-scenario flow passed on **OrbStack 2.2.5, Ubuntu 24.04.5, native
arm64**, including automatic machine deletion.
The run took approximately 7½ minutes. The generated Go test report measured
**78.0% statement coverage**; this is a baseline, not a threshold or a claim of
end-to-end instrumented coverage. Failure cleanup and explicit keep-on-failure
were also exercised while bringing up the suite; the retained debugging machine
was subsequently deleted.

Ubuntu 26.04 and emulated amd64 are selectable but were not run through this
complete suite. Ordinary runner-signal cleanup has deterministic unit coverage.
The expanded suite also passed all six recovery/diagnostics groups on the same
platform, including real client disconnection, three worker-kill boundaries and
machine restart. The complete base-plus-recovery run took about ten minutes,
and its owned machine was automatically deleted. Restart preserved the exact
PostgreSQL volume and row, left newer database credentials pending, and did not
restart the retired worker. A review-driven gate timeout regression was then
added and verified, followed by explicit coordinator restart/verification deadlines
and timeout-cleanup regression tests.

The subsequent CLI-expanded flow passed on Ubuntu 24.04.5 arm64, including all
seven CLI groups above, the base/recovery scenarios, restart verification and
owned-machine deletion. Its real terminal evidence is for a **Linux client in
the guest**, not native macOS or Windows terminal execution.

The DX-extended flow also passed on Ubuntu 24.04.5 arm64: quoted-path binding and
approval guidance, inventory attribution, snapshot stream separation, recovery/help
output, and the real secret-store refusal followed by successful metadata listing.
The full base/CLI/recovery flow and owned-machine deletion passed together.

### Runtime feature acceptance

The resource/release/replica-expanded suite passed on **Ubuntu 24.04.5 arm64**
in owned run `vamos-test-f5b6acb3fd56` (about 16 minutes). All seven runtime groups
in `runtime-summary.json` passed alongside the existing base, CLI and recovery
scenarios, including coordinator-owned restart and deletion of the guest.
The run built the real Rails image, migrated PostgreSQL, processed a Solid Queue
job, exercised both web upstreams and replica-selected sessions, checked Docker
and kernel resource ceilings, and proved a failed release could not change old
runtimes or be restarted by resume. The Python Ledger passed the same generic
release/replica contract. The standalone Rails database-backed suite passed in
an isolated test database: **12 tests, 86 assertions, zero failures/errors/skips**,
with empty stderr. Database setup and tests run in separate processes so Rails
can prepare its test schema without a setup connection blocking database purge;
the harness also rejects aborted tasks even when Rails returns zero.

The current visual matrix also passed: **216 captures from 144 production Go
renderer fixtures**, including replica groups and failed release commands at
48/80/120 columns. Mac race-enabled Go/Python checks and Windows compilation are
separate from this Linux runtime evidence; Windows terminal behavior is not
established by compilation.

### Limits

This does not replace public-DNS/ACME smoke tests or cloud-network testing. The
`.localhost` checks establish real local TLS and proxy routing, not publicly
reachable HTTPS or certificate-renewal behavior. OrbStack uses its own Linux
kernel and virtualization environment; results do not establish identical cloud
kernel, firewall, reboot or power-loss behavior. In particular, the tested isolated
guest rejects BuildKit's nested overlay mount. The suite creates a guest-only
`docker-container` Buildx builder with the **native copying snapshotter**, retaining
machine isolation rather than weakening it. Vamos's managed Docker configuration
and the Mac Docker daemon are unchanged. This tests real builds and containers,
but not the droplet's default BuildKit snapshotter; the extra builder resources
are disposed of with the VM.

APT interruption, full kernel reboot/power loss, registry credentials,
backup/restore and SQL password rotation are not covered here. The selected real
worker interruptions supplement—not replace—the wider deterministic Go recovery
coverage. CI still provides
native macOS/Linux/Windows client checks; the OrbStack suite is a Linux client and
host integration test, not a Windows execution test.
