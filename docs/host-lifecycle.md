# Host bootstrap and teardown

## Supported boundary

Root SSH access to a **dedicated Ubuntu 24.04+ host with systemd**, amd64 or arm64. The client runs on macOS, Linux, or Windows and needs OpenSSH, not Docker or APT. Trust the SSH host key separately. No sudo escalation, distribution-independent installer, external package repository, or adoption of existing Docker/Caddy installations is implemented. The separate [deployment slice](deployment.md) can subsequently manage app routes; host teardown still refuses app state/workloads.

```sh
mise run build
bin/soar server add --yes --domain apps.example.com test root@host
bin/soar server status --json test
bin/soar server teardown --yes --purge test
```

Flags precede positional arguments. Setup requires both name and target. Status and teardown resolve a registered name, or accept `NAME TARGET` for recovery when no registration exists. An explicit target cannot override a conflicting local registration. Names are lowercase DNS labels; the base domain is a lowercase hostname without a scheme, port, or wildcard.

`mise run build` bundles `soar-linux-amd64` and `soar-linux-arm64` beside the native client. `--executor FILE` overrides artifact selection; ELF architecture must match the inspected host. Artifacts are built locally, not downloaded from an unsigned release URL. The root executor is installed at `/usr/local/lib/soar/executor`; repeat setup verifies its recorded digest rather than silently upgrading it.

## Setup

1. Validate identity, authorization, and local registration conflicts; inspect the host read-only.
2. Send a fixed-name tar archive containing the Linux executable, checksum, and structured JSON request over strict OpenSSH. Request fields never become shell source.
3. Start `soar-host-operation.service`, a transient systemd worker. A fixed unit name rejects concurrent launches; a separate kernel-backed lock at `/run/lock/soar-host.lock` serializes actual mutations, including direct executor use.
4. Refuse existing runtime packages/binaries, managed directories, service overrides, service accounts, busy web/admin ports, or unhealthy dpkg state.
5. Refresh Ubuntu's configured APT indexes and simulate installing `docker.io`, `docker-buildx`, and `caddy` without recommendations. Refuse changes to pre-existing packages. Record every newly installed dependency and exact version **before** installing anything.
6. Mask the new services so package scripts cannot start an unconfigured default Caddy site. Install the recorded package versions; reject package-plan drift. Installer logs are drained with bounded diagnostic tails, not killed for being verbose.
7. Write a Caddy configuration with its admin listener on `127.0.0.1:2019` and no app routes. Configure Docker with no shared default bridge and the bounded `local` log driver. Record managed file digests and service-account identities, unmask/enable/start services, and check Docker, Buildx, Caddy, and Caddy's live configuration.
8. Mark the host ready, then register it locally. The first local registration becomes the default. A local write failure does not undo a successful remote installation; rerun the same add command to register it.

State lives in the private `/var/lib/soar/host.json` ownership manifest/journal. Writes use a synced temporary file and atomic replacement. The local registry is `soar/registry.json` under Go's platform user-config directory (`~/Library/Application Support` on macOS, `$XDG_CONFIG_HOME` or `~/.config` on Linux, `%AppData%` on Windows), protected by a stable advisory lock. It is not app configuration and is not stored in the project.

A repeated add with the same name/domain verifies a ready host without reinstalling packages or restarting healthy services. Changing the identity, overwriting drifted configuration, adopting existing resources, and upgrading package/executor versions are not implicit operations.

## Teardown

Both `--yes` and `--purge` are required. The latter authorizes removal of owned runtime data and certificates; it **does not** authorize deleting workloads. Teardown checks the journal and name, then refuses:

- Missing ownership evidence, unexpected Soar app/state files, symlinked managed paths, or mounts at/under owned data directories.
- Changed managed configuration/executor, systemd overrides, service-account/group changes, or changed package versions.
- Docker containers (including stopped ones), images, volumes, custom networks, or build cache.
- Unmanaged containerd namespaces or containers/images/content in Docker's namespaces.
- API-loaded Caddy configuration that differs from its managed file.
- An APT purge plan that would remove anything outside the recorded package set.

After approval it masks/stops owned services, purges only recorded packages **without autoremove**, removes fixed owned configuration/runtime directories, deletes recorded service accounts/groups, and removes its masks. Local registration is forgotten only after the host reports `removed`.

The host receipt and runtime lock remain so a repeated teardown is safe. Reboot naturally clears `/run` locks. A subsequent add performs fresh-host checks again, creates a new installation identity, and re-registers the server. OS package indexes/caches, system journals, and authentication logs are retained. Kernel networking artifacts can outlive daemon removal until reboot; teardown never flushes the host firewall globally. This is installation removal, not a pristine-OS snapshot restore.

Do not run other installers, administrators' runtime changes, or workloads concurrently with setup/teardown. Soar's lock coordinates Soar operations, not arbitrary root activity or all system package managers.

## Interruption and recovery

The client defaults to a 25-minute wait, while the executor has a 20-minute operation deadline and its systemd worker a 21-minute hard limit. Request reading is separately bounded to 15 seconds. Ctrl-C, SSH loss, or a short client `--timeout` does not imply rollback or stop an already-launched worker.

```sh
bin/soar server status test root@host
# If active=true, let the worker settle. Then repeat the same authorized operation.
bin/soar server add --yes --domain apps.example.com test root@host
# Or explicitly remove a partial installation:
bin/soar server teardown --yes --purge test root@host
```

Status is read-only and shows the saved phase/step, active-worker flag, and last recorded error. A lost response after successful setup/teardown is recoverable through the receipt. Worker staging directories normally disappear on completion. A hard crash during upload or before worker launch can leave a private `/run/soar-stage.*` directory until reboot; do not remove a stage belonging to an active worker.

Partial package transactions are not repaired by a broad `apt --fix-broken`, `dpkg --configure -a`, autoremove, or prune command. Retry uses the captured package list; broken dpkg state or unavailable pinned versions may require operator diagnosis. Resolve the exact reported problem before retrying. If a stopped/unhealthy daemon prevents proving that data is unused, restore it and inspect resources rather than bypassing the guard. A deadline during APT can leave pending dpkg work; the receipt is not a promise of transactional OS rollback.

The internal JSON protocol adds `bootstrap` and `teardown` operations with `config`, `confirm`, and `purge` fields. Unauthorized/invalid requests fail before inspection or mutation. Success returns `host_state`; failure returns the versioned error envelope with `lifecycle_failed`. There are no arbitrary remote path or command fields.

## Verification

Offline tests cover initial setup/repeat/teardown/rebootstrap, interrupted install/start, ownership/refusal paths, package simulations, workload/config/account/mount drift, locks, registry atomicity and concurrency, authorization before remote contact, tar/checksum boundaries, and verbose installer output. External APT/systemd/Docker/Caddy commands are mocked; tests do not install packages or contact hosts.

Native host-lifecycle filesystem tests run on Unix; Windows tests exercise client/registry/protocol/process behavior. Cross-compilation is not native Windows/Linux execution and unit mocks are not real APT validation.

### Observed real-host results

The macOS client exercised a disposable **Ubuntu 26.04 / amd64 / systemd** droplet as root:

- Fresh setup installed six recorded packages: Caddy 2.6.2, Docker 29.1.3, Buildx 0.30.1, containerd 2.2.2, runc 1.4.0, and libnss3-tools. All services/health checks passed.
- Status during setup reported `active=true` even before the ownership manifest existed. Repeat setup returned the same installation ID without reinstalling/restarting services.
- Caddy's live config contained only its loopback admin listener. No HTTP/HTTPS app listener or certificate was created. The manifest was root-owned mode 0600 and the installed executable mode 0755 inside a root-only directory.
- Teardown removed all six packages, managed data/configuration/executor directories, service accounts/groups, and service masks. Units reported `not-found`, dpkg audit was clean, and no staging directory remained. The receipt remained; repeat teardown succeeded with an explicit target after local deregistration.
- Docker's kernel firewall chains remained after teardown. No host firewall flush was attempted; this observed limitation is part of the cleanup boundary above.
- Rebootstrap with a deliberately short **10-second client timeout** exited 1 with an unknown-outcome diagnostic. The independent worker continued and reached `ready` with a new installation ID. Rerunning add verified that installation and restored local registration; no staging directory remained.
- Creating a labeled, empty test Docker volume caused teardown to refuse before stopping services. Both Docker and Caddy remained active. The test volume was then explicitly removed by name.
- A final teardown → bootstrap cycle with the reviewed build passed. The host was left ready with a third installation identity, no containers/images/volumes, only `host`/`none` Docker networks, a clean dpkg audit, and no staging directory. The installed executor's digest matched the local Linux amd64 artifact.

Local `mise run check` (format, vet, full race-enabled tests, native client build, and Linux amd64/arm64 executor builds) passed. Windows test binaries cross-compiled successfully. Review findings about failed systemd queries and output-write errors have failing-before/fixed-after regression coverage.

Still unverified: Ubuntu 24.04 specifically, arm64 execution, native Windows/Linux client test suites, reboot/power-loss recovery, and killing the worker during an APT transaction. Client disconnection was tested; destructive interruption of dpkg was not.
