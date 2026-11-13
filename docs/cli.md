# Everyday CLI commands

These commands complement deployment and recovery. They use the existing
registered servers and project bindings; they do not introduce another daemon,
registry requirement, or deployment state model.

## CLI conventions

Use `soar help COMMAND` (or `soar COMMAND --help`) for options, for example
`soar help deploy` and `soar help service remove`. Group help also accepts `-h`.
Successful help goes to stdout; usage errors and diagnostics go to stderr.

Flags normally precede positional arguments. `init` accepts flags on either side
of its app name; `service bind/remove` accepts the service name before flags.
Use `--` before literal command arguments for `run` and `exec`. There is no implicit
shell. Printed command suggestions quote arguments for POSIX shells, or PowerShell
on Windows; they are not `cmd.exe` command strings.

### Targets and recovery guidance

Project commands select the saved server binding, or bind to the registered default
on first contact. An explicit `--server` overrides only this invocation. Commands
that support `--server NAME --app APP` can operate without loading a local manifest.
`apps` is deliberately different: it ignores project bindings and uses the registered
default unless `--server` is supplied.

If missing or invalid YAML blocks observation or recovery, use an explicit target:

```sh
soar status --server home --app notes
soar logs --server home --app notes --service web
soar resume --server home --app notes OPERATION
```

These hints do not trust partially parsed YAML or choose an app identity for you.
Deploy and `run` still require valid local inputs. Recovery hints preserve observation
selectors but do not carry destructive approval into a placeholder-target command.

Deploy approval suggestions include the resolved server and absolute manifest path,
plus supplied executor, deadline, supersede and retained-data options. Binding advice
is emitted only for a classified adoption refusal, not arbitrary build/SSH failures;
it uses `-f` so the binding is saved beside the original manifest, not the current
directory. Inspect the intended service identity before adopting it. An approval
invocation rechecks current inputs and state; a preview is not an immutable transaction.

### Output and exit contracts

| Command | Machine-readable output |
| --- | --- |
| `apps --json` | Inventory object containing `entries` |
| `status --json` | Status object, including recorded run metadata when selected |
| `cleanup/destroy/service remove --json` | Full response envelope, including the plan |
| `secrets stage/list --json` | Metadata response envelope; never secret values |
| `server inspect --json` | Inspection report |
| `server status --json` | Host status response |

JSON goes to stdout without human headings. Errors go to stderr, not a JSON error
object. Human inventory/status formats are intended for people, not stable parsers.
Ordinary exit 0 means the requested operation, observation, or preview succeeded;
it does not establish service health or mean that a preview was applied. Ordinary
failures return 1 and usage errors return 2. A one-off `run` propagates its recorded
command exit; exec/console exit and disconnect rules are described below.

Snapshot service logs put workload records on stdout and target metadata on stderr.
Streaming logs preserve Docker's stdout/stderr streams, with target diagnostics on
stderr. `logs --run` is a bounded result/diagnostic view, not a raw workload stream.

## List apps

```sh
soar apps
soar apps --server home
soar apps --server home --json
```

`apps` queries the selected registered server, or the registered default. It does
not require a manifest, inspect the current project's binding, or bind a project.
Human output identifies the selected server name/SSH target for both empty and
populated inventories; JSON output retains its existing shape.
The inventory includes deployed apps, secret-only staged apps, and destroyed apps
with retained metadata. JSON contains an `entries` array with app name, recorded
phase, service names/count, retained-secret presence, volume-record count, and
whether volume records remain outside the declared services.

This is **recorded inventory, not a live health check**. `ready` does not establish
that the containers are still running. Use `status --server home --app NAME` for
live observations. Volume records likewise do not prove a physical volume exists.
No secret values or application environment values are returned.

Enumeration is limited to 256 apps/entries per store and the normal 64 KiB RPC
response. Oversized, unsafe, corrupt or foreign metadata produces an explicit
error, not an incomplete list presented as complete. Concurrent metadata changes
may require another explicit read. `--timeout` defaults to 15 seconds;
`--executor FILE` selects a matching Linux artifact as for other remote commands.

## Create a minimal configuration

```sh
soar init notes --port 8080
soar init notes --port 8080 --health /readyz --server home
soar init dispatch --private --service worker
```

`init` writes `soar.yaml` (or `-f FILE`) for a Dockerfile build with context `.`.
The default service is `web`, and the default public health path is `/`.
Public services require an explicit port. `--private` omits HTTP and cannot be
combined with `--port` or `--health`. Flags may appear before or after the app name.

There is no framework detection, Dockerfile creation, dependency installation or
remote contact. The output is validated before writing. Existing files, symlink
output paths and incompatible local bindings are refused; there is no force flag.
Output parent directories must already exist. A write/binding failure can leave
new files behind and reports that partial result without removing unrelated work.

With a registered default, `init` creates a Git-ignored `.soar/` binding. An explicit
`--server` must be registered. With no registered server, it creates an unbound
configuration; register a server before deployment. It does not adopt or bind an
existing remote service incarnation: use `service bind` deliberately for that.

## Follow service logs

```sh
soar logs --follow
soar logs --service worker --follow --tail 25
soar logs --server home --app notes --candidate --follow --timeout 5m
```

`-f` still selects the configuration file; only `--follow` means streaming.
Without `--follow`, workload content remains a bounded snapshot. Its target metadata
is written to stderr, separately from the workload records on stdout.
Following emits the selected container's last 1–1000 lines (default 100), then
new timestamped output without the snapshot byte cap. Workload output goes to
stdout/stderr; target and transport diagnostics go to stderr.

Selection is `--service`, otherwise `web`, otherwise the sole deployed service.
Ambiguous apps need an explicit selection. `--candidate` selects a recorded
candidate. The stream is pinned to one verified immutable container ID: a deploy
may end it, but it never silently jumps to another release. Run the command again
to select the new current container. Following has no default session deadline;
an explicit positive `--timeout` bounds it, and Ctrl-C interrupts the client.

`--follow --run OP` is not supported. Recorded one-off/build diagnostics remain
available through ordinary `logs --run OP`, including failures without containers.
Runtime logs are not arbitrarily secret-redacted; inspect output before sharing it.

## Execute in an existing service

```sh
soar exec --service web -- python --version
soar exec --server home --app notes --service web -- sh -c 'printf "%s\n" "$HOME"'
printf 'input\n' | soar exec --service web -i -- cat
soar exec --service db -i -t -- psql -U app
```

`exec` uses the current running service container, its configured user and working
directory, and its existing environment, secret pins and mounted volumes. It does
not build an image, create a container, restart a service or apply staged secrets.
This differs from [`run`](deployment.md#one-off-commands), which starts a separate,
noninteractive, volume-free one-off container from project inputs.

Arguments are preserved individually, including empty arguments, spaces, Unicode
and shell metacharacters. There is **no implicit shell**. Use an explicit `sh -c`
only when shell interpretation is intended. NUL and invalid UTF-8 arguments are
rejected, as are commands above 256 arguments, 8 KiB per argument or 32 KiB total.

By default stdin is closed and stdout/stderr remain separate. `-i`/`--interactive`
forwards stdin, including binary pipes. `-t`/`--tty` additionally requests a terminal
and requires `-i` plus real local input/output terminals. TTY output combines the
remote terminal streams. Use separate `-i -t` flags, not the combined `-it` spelling.
There are no user/environment overrides, privileged mode, detached execution,
arbitrary container IDs, or candidate execution options.

Service selection matches logs (`web`, sole service, or `--service`). Existing
project service-identity bindings are checked for the selected service; a stale
binding cannot silently attach to a removed/recreated incarnation. Outside a
project, specify both `--server` and `--app`. An explicit server override does not
rewrite a project's binding. All remote commands still require root SSH access
and a matching Linux executor artifact.

## Open a console

```sh
soar console
soar console --service worker
soar console --server home --app notes --service web --shell /bin/bash
```

`console` is interactive exec of `/bin/sh`, not framework-specific REPL detection.
`--shell` selects a single alternate executable. A missing shell fails without
trying another one. For a framework or database console, use `exec -i -t -- COMMAND`.
Console requires a terminal and rejects redirected input/output; use non-TTY exec
for automation. OpenSSH owns local raw mode and terminal-size propagation, and
Docker owns the container terminal. SSH escape sequences are disabled so typed
input is not interpreted as a hidden local SSH command.

## Concurrency, exits and disconnection

Sessions verify full recorded ownership/runtime identity and pin an immutable
container ID. Preparation and attachment resolve separately; if host, service
incarnation or release changes between them, attachment fails rather than retargets.
Exec refuses admission during an active host mutation and rejects retired,
unavailable, paused or stopped containers. Log following remains observational.

The host mutation lock is **not held for the session lifetime**. A later deployment,
rollback or removal can stop its container and end the session. These are live
administrative commands, not transactions or isolated snapshots; they can change
application data while the application itself continues running. Code rollback
cannot undo those side effects.

Known remote command exit codes 0–254 are propagated. SSH exit 255 is ambiguous
between transport failure and a command returning 255, so Soar reports an unknown
outcome and exits 1. Cancellation or connection loss also cannot prove an exec
command stopped: a Docker exec process may keep running after its client disappears.
There is no automatic retry, reconnect, cancellation guarantee or `status --run`
record for exec. Inspect the application before deciding whether to repeat a command.

Artifact upload is separate from workload stdin and never runs through a PTY.
Private, root-owned `/run/soar-sessions` staging uses single-use tokens, attach-time
identity checks, a two-minute unclaimed preparation expiry, and a 16-session limit.
Ordinary completion removes staging. The client also attempts cleanup with a fresh
10-second deadline after cancellation. Inactive expired staging is pruned by the
next preparation, and host restart clears `/run`; active sessions are not expired
just because their preparation deadline has passed. Malformed owned records use
the directory age for expiry; unsafe permissions/ownership or unexpected non-token
entries at the private root require explicit operator correction, not automatic
deletion of potentially unrelated state. No persistent worker is added.

## Verification boundary

Unit tests cover CLI parsing/selection, no-overwrite initialization, bounded app
inventory, stale identities, immutable target resolution, session expiry/replay,
private staging, cleanup, literal argv, large binary streams and exit handling.
The disposable Linux acceptance suite exercises the real CLI, SSH, Docker and a
PTY, including shell interruption, resize and terminal restoration. See
[testing](testing.md) for completed-run evidence and platform limits. Cross-compilation
is not proof of native Windows terminal behavior, and guest Linux PTY tests are not
claims of native macOS/Windows interactive acceptance.
