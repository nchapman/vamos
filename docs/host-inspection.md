# Host inspection boundary

This document covers the read-only inspection boundary. Provisioning and removal are documented separately in [host lifecycle](host-lifecycle.md); app deployment is not implemented.

## Fresh-host path

`soar server inspect [--json] [--timeout 15s] TARGET` invokes the client's system OpenSSH directly, without a local shell. The remote command is the constant `/bin/sh -s`; stdin contains the fixed embedded `internal/host/inspect.sh` collector. No target or other user input is interpolated into remote shell code.

This bootstrap path needs no remote Soar binary. It is deliberately separate from the installed executor's JSON transport: sending a binary to a fresh machine would require a write, which inspection does not need.

The collector gathers:

- Kernel and machine architecture from `uname`.
- Effective UID from `id -u`.
- PID 1 identity from `/proc/1/comm`.
- The presence of `/run/systemd/system` and `systemctl`.
- Up to 16 KiB plus one truncation-detection byte from `/etc/os-release`.

It never sources OS metadata, runs sudo, installs packages, uploads files, or changes configuration. SSH authentication logs, access times, shell startup scripts, and administrator-configured hooks may still have side effects; this is not a literal zero-filesystem-write guarantee.

The Go parser requires exact versioned framing rather than searching through banners. Ubuntu identity must be `ID=ubuntu`, not merely `ID_LIKE=ubuntu`. `VERSION_ID` is parsed as numeric year/month components and must be 24.04 or newer. Quoted/unquoted identity values are supported; malformed, duplicated, missing, or unrecognized identity fields fail compatibility. No shell expansion is performed.

Compatible architectures are `x86_64` → `amd64` and `aarch64`/`arm64` → `arm64`. A systemctl executable alone is not sufficient: systemd must also be PID 1 with its runtime directory present. Inspection does not test individual units or reject an otherwise valid host just because an unrelated unit is degraded.

Non-root inspection is allowed. The report describes the UID and does not infer setup permissions from group membership or a sudo probe. Docker/Caddy presence and port conflicts are checked by provisioning, not by this read-only compatibility probe.

## SSH behavior

- Existing SSH aliases, identities, agents, and jump hosts remain usable.
- Host key checking is strict; inspection never accepts a new key or updates trusted keys automatically.
- Authentication is noninteractive. Load encrypted keys into an agent beforehand.
- No PTY, agent/X11/port forwarding, LocalCommand, or multiplexed session reuse.
- The target is validated before spawning SSH; no extra SSH arguments are accepted.
- The connection timeout is 10 seconds; the overall command deadline defaults to 15 seconds and is configurable. Ctrl-C (and SIGTERM on Unix) cancels the local SSH process.
- Stdout and stderr are separately capped at 64 KiB. Overflow cancels the process. Shutdown waits for inherited pipe handles for at most one additional second.
- Remote stderr is truncated to 4 KiB and quoted before human display, preventing terminal control sequences from being replayed.

Killing SSH does not prove that arbitrary proxy or remote descendants terminated. The collector is short-lived and foreground-only; user SSH configuration remains trusted executable configuration. Inspecting a host does not register it locally.

## Installed executor protocol

The same executable exposes `soar internal-executor`. A caller sends one JSON document and **closes stdin**:

```json
{"version":1,"operation":"inspect"}
```

The executor runs the same collector locally on Linux and emits only one JSON response on stdout:

```json
{
  "version": 1,
  "result": {
    "kernel": "Linux",
    "architecture": "amd64",
    "os": "ubuntu",
    "os_version": "24.04",
    "systemd": true,
    "uid": 1000,
    "root": false,
    "supported": true,
    "problems": []
  }
}
```

The protocol version is independent of the CLI build version. Request size is limited to 4 KiB. Unknown fields, multiple documents, unsupported versions, and unsupported operations are rejected before any collector runs. There is no arbitrary command, path, or script field. Request reading and collection share a 15-second deadline. A peer that never sends EOF cannot keep the executor waiting indefinitely; cancellation closes its request input.

Error responses use `{"version":1,"error":{"code":"…","message":"…"}}`. Codes are `invalid_request`, `unsupported_version`, `unsupported_operation`, and `inspection_failed`.

Executor exit status differs from the public inspection command:

- **0:** collection and parsing completed, even if `result.supported` is false.
- **1:** collection failed or the response could not be written.
- **2:** invalid/unsupported request.

The public command exits 1 for an incompatible host and preserves the report. Its `--json` output is the report itself, not the executor envelope. The same protocol also supports explicitly authorized `bootstrap` and `teardown` requests. Their longer deadlines, installed artifact, transient-worker transport, and `host_state` responses are described in the lifecycle document; they do not change inspection's read-only behavior.

## Verification

Automated tests cover:

- Ubuntu versions, derivatives, malformed/missing OS metadata, architectures, systemd evidence, and root/non-root users.
- Exact collector framing and oversized/truncated output.
- Protocol validation before execution and consistent executor/bootstrap reports.
- Destination rejection, fixed SSH argv/stdin, quoted errors, and installed OpenSSH option acceptance via `ssh -G` without a connection.
- Real local subprocess success/failure, stdin/stdout/stderr separation, deadlines, output floods, inherited pipe handles, and Unix shutdown signals. Executor stdin cancellation and both JSON/text output failures have regression coverage.
- Native collector execution on Unix test machines. Windows clients never need to run the Linux collector locally.

CI is configured to run these on Ubuntu 24.04, macOS, and Windows. Linux/macOS use the native collector; Windows exercises the client and subprocess boundary. CI configuration is not proof that all those runs have completed successfully.

### Observed real-host results

A DigitalOcean droplet running **Ubuntu 26.04 / amd64 / systemd**, accessed as root from the macOS client, passed both JSON and human-readable inspection. An independent SSH check confirmed the reported OS, architecture, PID 1, and UID. No Soar, Docker, or Caddy executable was installed, and `/var/lib/soar` was absent after inspection.

The initial connection correctly failed because the ED25519 host key was unknown. After the operator confirmed the displayed fingerprint, that exact key was added to the client's `known_hosts` and inspection succeeded. This changed local SSH trust only; no software was installed or host configuration changed. An already-expired CLI deadline also returned exit 1 with no success report; this does not establish cancellation of an in-flight remote command.

Still unverified on a real remote host: Ubuntu 24.04 specifically, arm64, non-root access, incompatible hosts, authentication/network failures beyond the unknown-key case, and cancellation during an active connection. Native Windows/Linux client execution also remains separate from this macOS smoke test.

### Real-host smoke-test checklist

Use a disposable Ubuntu 24.04 amd64 or arm64 machine with systemd, SSH access, and no Soar binary installed. Do not use production infrastructure as the first test target.

1. Verify its SSH host key through a trusted channel and configure ordinary noninteractive SSH access.
2. Run `soar server inspect --json <alias>` as a non-root user. Expect supported=true and root=false, without a sudo prompt.
3. Repeat as root if available. Expect the same OS facts with UID 0.
4. Use a separate known older-Ubuntu or non-systemd test host to confirm incompatibility produces facts and exit 1.
5. With isolated SSH aliases, exercise an untrusted key, invalid identity, unreachable port, and a short overall timeout. Expect actionable errors, never automatic trust acceptance.
6. Confirm Soar has not created app directories, uploaded binaries, or changed Docker/Caddy/systemd configuration. Authentication/audit logs are expected.

No real remote host is part of the offline test suite. Record native CI and smoke-test results separately before relying on this boundary for provisioning.
