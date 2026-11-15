# Isolated remote BuildKit

This package is for a **trusted root worker**, not a hostile-tenant sandbox.
The caller must hold Vamos's global worker lock throughout `Manager.Build`.
It does not change Docker's daemon configuration, restart Docker, install a
service, or migrate existing resources. The expected daemon policy remains
`{"bridge":"none","log-driver":"local"}`; Docker live restore must be disabled.

## API

```go
type Runner func(context.Context, string, []string, []byte) (process.Result, error)
type Manager struct {
    Root        string // default /; rebases private files, NOT the Docker socket
    Run         Runner // default process.Run; must honor context cancellation
    Snapshotter string // "auto" (initial default), or "native"
}
func (Manager) Build(ctx context.Context, hostID, dockerfile, iidfile string,
    archive []byte) (process.Result, error)
```

`hostID` must be exactly 32 lowercase hexadecimal characters. Empty Snapshotter
reuses the receipt's choice; an explicit conflicting choice is rejected. Native
is an administrative/isolated-guest choice, not an automatic fallback.

Only the build command's stdout/stderr are returned (also on build or cleanup
failure). The caller must redact them. Infrastructure errors contain no command,
path, daemon response, or caller-provided value. The default runner's output
limit still applies; callers needing larger build logs should supply a suitable
bounded runner without silently truncating infrastructure JSON inventories.

## Execution and ownership

* A private, atomically replaced and fsynced receipt lives at
  `/var/lib/vamos/buildkit/state.json`. It records a random creation nonce and
  intent **before** creating Docker resources, then captures immutable IDs,
  image defaults and cache creation metadata.
* Receipt and namespace ownership, permissions, symlinks, unknown JSON fields,
  duplicate JSON keys, host identity and receipt version are checked. A
  pre-existing namespace without a valid receipt is never adopted. A crash
  between directory creation and initial receipt persistence needs operator
  inspection; an empty namespace is deliberately not treated as intent.
* Creation interruptions can resume only with matching intent labels and
  validated shape. Captured resources that disappear or are replaced are not
  recreated/adopted. Successful inventories establish absence; CLI errors never
  mean "not found". Configuration changes are rejected, not migrated.
* The image trust anchor is exported as `Image`:
  `moby/buildkit:v0.27.1@sha256:1e110c71d389d6d24f67b9438e2f7b8da749a6ff407b22a1631e025c95599368`.
  It is pulled only during initial image setup and is never deleted.
* One privileged container runs `buildkitd --oci-worker-snapshotter=auto` (or
  `native`), with restart disabled, local logging and only its explicit cache
  volume. There are no insecure entitlements, published ports, socket mounts,
  host binds or host network mode.
* The Docker network is an IPv4 bridge with automatic default IPAM, IPv6 disabled
  and ICC disabled. Its allocated subnet and gateway are captured. Foreign endpoints,
  altered bridge options and additional container network attachments fail
  validation. IPv6 firewall guards are nevertheless mandatory.
* Buildx uses the **remote** driver, never `docker-container`, with the endpoint
  `docker-container://CONTAINER`. That helper executes `buildctl dial-stdio`
  through Docker. The explicit build command is:

  ```text
  docker --host unix:///var/run/docker.sock buildx build --builder BUILDER \
    --load --network default --iidfile IIDFILE --file DOCKERFILE -
  ```

  The archive is stdin, not a shell argument. Dockerfile/iidfile paths are trusted
  caller inputs. Successful loads retain their images in the local daemon.

Commands retain the trusted root worker's HOME, PATH, Docker registry config,
proxy settings and credential-helper environment. `/usr/bin/env` removes
`DOCKER_CONTEXT`, `DOCKER_TLS`, `DOCKER_TLS_VERIFY`, `DOCKER_CERT_PATH`,
`BUILDKIT_HOST` and `BUILDX_BUILDER`, and sets the fixed local `DOCKER_HOST` and
private `BUILDX_CONFIG=/var/lib/vamos/buildkit/buildx`. Docker also receives explicit
`--host`; its stdio helper inherits the constrained endpoint. Credentials are not
copied into private metadata or mounted inside the builder. Registry helpers and
token acquisition may run in the host client, outside the bridge guard.
`--config` is not used to select Buildx metadata.

Local CLI help was checked against Buildx v0.37.2: `create --driver remote
--name NAME --node NODE ENDPOINT` is supported; `inspect` has no JSON/format
option. Consequently the package validates Buildx's private `store.NodeGroup`
instance JSON (name, remote driver, one exact node/endpoint, no options, flags,
files or platforms). Unknown schemas fail closed. Missing metadata is
reconstructible; changed metadata is not silently replaced.

## Firewall and stopping

Before **every** start, both `/usr/sbin/iptables` and `/usr/sbin/ip6tables` must
successfully list and verify these leading mangle rules, each with comment
`vamos-buildkit:HOSTID`:

```text
PREROUTING: -i vamos-build0 -m addrtype --dst-type LOCAL ... -j DROP
FORWARD:    -i vamos-build0 -o br-+ ... -j DROP
FORWARD:    -i vamos-build0 -o docker0 ... -j DROP
FORWARD:    -i vamos-build0 -o vamos-build0 ... -j DROP
```

PREROUTING precedes DNAT, including host published-port DNAT. FORWARD blocks
standard Vamos `br-HASH` application bridges and same-bridge peers. Only exact
owned rules may be moved/deleted during repair; unknown rules are preserved.
Both effective leading placement and installation results are checked. The
package does not flush chains or install permanent firewall services.

A verified existing container is stopped **before** guard inspection/repair.
Every exit after identifying a verifiable owned container attempts cleanup
using its immutable ID and an independent 20-second context, including a
canceled caller, uncertain start, failed build, and failed inventories for an
already-captured ID. Cleanup revalidates the container, stops it with a 5-second
Docker stop timeout, falls back to kill if stop fails, and verifies it is stopped.
Cleanup failure is returned, retaining any build output. Unverifiable resources
are never stopped, and an unavailable Docker daemon cannot be forced to stop;
those cases fail closed and require operator recovery. A runner that ignores
its context defeats the cleanup time bound.

Root firewall changes while a build is active are outside supported
coordination. Reboot safety relies on restart=no and no Docker live restore;
there is no persistent in-progress-build watchdog. Builds retain general
outbound access: this policy isolates the worker's local services and standard
application bridges, not arbitrary remote networks or nonstandard app bridges.

## Exact names and retention

For host ID `HOSTID`:

| Resource | Name |
| --- | --- |
| Container | `vamos-buildkit-HOSTID` |
| Docker network | `vamos-buildkit-HOSTID-net` |
| Linux bridge interface | `vamos-build0` (one host-bound receipt per worker) |
| Cache volume | `vamos-buildkit-HOSTID-cache` |
| Buildx instance | `vamos-HOSTID` |
| Buildx node | `vamos-HOSTID0` |

Docker resources carry `org.vamos.buildkit.host`, `org.vamos.buildkit.nonce`, and
`org.vamos.buildkit.role` labels (`container`, `network`, `volume`). Containers and
networks are bound by exact IDs; volumes are bound by name, nonce labels,
creation timestamp, mountpoint and local-driver shape. Docker has no immutable
volume ID: a root admin who fabricates identical metadata is outside this
ownership model.

Cache, container, bridge, receipt, guards and metadata are retained between
builds. No API prunes or deletes Docker resources. For deliberate operator
removal while no host worker is active, inspect the receipt and compare IDs,
nonce labels and volume creation identity. Stop/remove the exact captured
container ID, remove the exact captured network ID, then explicitly remove the
verified cache volume if its loss is intended. Do not delete shared images or
remove the receipt first: it is authority for remaining resources and guards.

`ReceiptGone` uses successful inventories to prove the named resources absent;
errors and replacements block host teardown. `RemoveReceipt` additionally removes
only exact owned guards and disposable Buildx metadata/receipt. Its caller must
hold the host lock, persist host-purge authorization and verify Docker is otherwise
empty. Host lifecycle calls it before stopping/removing Docker. An authorized
purge can reconcile the final empty receipt directory after an interrupted unlink;
an ordinary build or preflight never adopts an empty namespace.

## Validation

Run `go test ./internal/buildkit` (and `go test -race ./internal/buildkit`). Tests
use persistent external CLI models with Docker inventory/inspect JSON and
ordered firewall chains, not real Docker, SSH, hosts or cloud resources.
Parent integration/disposable-VM tests must verify actual Docker/Buildx versions,
image defaults, iptables backend behaviour, bridge isolation, image loading and
snapshotter support. Those live proofs are not claimed by these unit tests.
