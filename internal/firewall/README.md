# Managed firewall core

Core API used by the lifecycle manager and exposed through the SSH executor and CLI.

```go
m := firewall.Manager{} // production paths and process.Run
s, err := m.Enable(ctx, hostID)
// Present s.Token and require another, non-multiplexed SSH connection.
s, err = m.Confirm(ctx, hostID, token)
s, err = m.Status(ctx, hostID)
s, err = m.Disable(ctx, hostID)
```

`Manager.Run` has the lifecycle Runner signature (no stdin). `Root` rebases files
only, **not subprocesses**; a non-default Root requires an injected Runner.
Commands execute on the managed host, as root. Pass the real SSH session's
`SSH_CONNECTION` environment through the integration layer. The host-side local
port is retained even when an upstream router translates the public SSH port.
Fresh means a different SSH connection tuple, not another multiplexed channel.
Root can forge this environment; it is an operational confirmation, not a
security boundary against the administrator.

`Status` reports Enabled, Pending, Degraded, Token, SSHPorts and Problems.
Enabled is true only when owned rules and hooks verify in **both** families.
Pending enables return a token; confirmed status omits it. Degraded status also
returns an error; callers should retain/display the returned Status on error.
Repeated Enable neither changes the generation nor resets its deadline.

## Ownership and safety

- State: `/var/lib/vamos/firewall.json`, independent of host State. A 256-bit
  generation token binds the state, script, confirmation marker and rollback.
- Lock: `/run/lock/vamos-firewall.lock`, shared by Go operations and the independent
  shell using flock. Operations have a two-minute timeout. State/confirmation
  writes use rename, file sync and directory sync.
- Script: `/usr/local/lib/vamos/firewall.sh`; systemd units:
  `vamos-firewall.service`, `vamos-firewall-rollback.service` and
  `vamos-firewall-rollback.timer`; Docker drop-in `50-vamos-firewall.conf`.
- Enable validates before staging, enables boot protection, starts and verifies
  the five-minute rollback timer **before** installing rules. Caller cancellation
  does not cancel the timer. Install failure attempts immediate rollback with a
  separate bounded context. A failing rollback service retries independently.
- A synced confirmation marker makes a late timer a no-op. An unconfirmed boot
  rolls back rather than restoring rules. A confirmed boot restores rules before
  `network-pre.target` and `docker.service`. Docker requires this service, with
  an ExecStartPre verification on every start/restart.
- Only filter `VAMOS-FW-IN`/`VAMOS-FW-FWD`, mangle `VAMOS-FW-FWD` and their exact hooks are removed. There is no
  global flush/restore, builtin-policy change, or Docker NAT/isolation rewrite.
  Disable verifies ownership and drift before mutation. Rollback permits a
  partial install but rejects foreign contents/references in either family
  before touching either. Successful removal retires the identity to
  `firewall.retired.json`; a subsequent Enable/Disable validates and cleans the
  exact leftover artifacts. This supports re-enable after autonomous rollback.

## Packet policy

INPUT first drops INVALID and non-loopback-interface packets destined for loopback.
It then allows loopback, established/related, ICMP/ICMPv6, DHCP client replies,
actual sshd listeners (and configured SSH ports with live systemd-owned sockets),
the active SSH connection's local port, TCP 80/443 and UDP 443. Everything else
is dropped. No application publish is added to INPUT. Host-network applications
therefore are not a way to expose arbitrary ports.

FORWARD and DOCKER-USER both enter the owned gate. Established replies and
container-origin traffic on approved bridges RETURN to Docker, **not ACCEPT**.
New IPv4 Docker DNAT TCP/UDP traffic to approved bridges RETURNs only in the
conntrack ORIGINAL direction and if its original destination is not loopback.
Application IPv6 forwarding is denied; host IPv6 is protected and usable. Other new forwarding is dropped. Docker still
checks its publish and isolation rules. Public Docker publishes are intentional
exposure; ports require no separate firewall mapping. No protection is claimed
against root or Docker administrators.

An additional owned mangle/FORWARD gate uses the same forwarding policy and runs
**before filter/FORWARD**, after PREROUTING DNAT. Both families' early gates are
installed before filter hooks. This protects forwarding before Docker starts and
during Docker's temporary filter-hook reordering on restart; it does not depend
on a post-restart repair or transient DOCKER-USER ordering. It only DROPs or
RETURNs, never accepts past Docker isolation. Removal validates all four owned
chains first and removes early guards last. The filter DOCKER-USER gate is still
retained and verified, including final traversal order. The exact DROP-only BuildKit mangle guards with the matching 32-hex host identity
are preserved. They may be inserted ahead of the gate when the first build runs
after enable. Other custom mangle policy is unsupported.

## Deliberately narrow support boundary

- Ubuntu/systemd, working IPv4 and IPv6 iptables, a single non-Docker uplink,
  standard routing policy. Provider-specific outbound host routes are preserved. Required tools:
  iptables/ip6tables plus both alternatives' save tools, nft (or Python 3 + libnftables.so.1), ip, ss, sshd,
  systemctl, Docker, flock and standard Ubuntu shell/core utilities.
- iptables-legacy **or** iptables-nft, consistently in both families. The inactive
  backend must be empty. Native nftables Docker backend is unsupported. For the
  nft compatibility backend, table/base-chain metadata and rule inventories must
  match iptables-save; opaque warnings, native sets/maps/flowtables or additional
  chains are rejected. Native rules placed in Docker-owned chains by a root
  administrator are outside the threat model.
- Modern Docker's DOCKER-USER/DOCKER-FORWARD layout; older layouts with builtin
  forwarding accepts are rejected. Standard packaged dockerd command line,
  Vamos NAT bridge mode (`bridge:none`, `log-driver:local`), no swarm, macvlan,
  overlay, IPv6 apps, internal/routed networks, arbitrary custom bridge names,
  direct-routing or firewall overrides. Default `docker0` is not supported. Some otherwise-safe
  customizations are deliberately rejected rather than inferred safe.
- Fixed interface matches `br-+` (iptables prefix wildcard) and `vamos-build0`:
  new app and BuildKit networks work immediately without policy refresh or another
  SSH confirmation. Admission checks existing network/container Vamos labels and
  host ownership, permits owned host-network Caddy, and rejects foreign workloads.
  This is admission, not continuous Docker inventory enforcement. Root/Docker
  administrators and interface naming remain trusted.
  Firewall preflight does not read deployment release captures. A service name
  and Docker labels alone do not establish that host networking was authorized
  for that release. Preflight therefore refuses host-networked application
  containers (the existing Caddy exception is unchanged). No service ports are
  added to INPUT; host-network listeners are subject to existing host policy.
- Existing UFW/firewalld/nftables/persistent iptables configuration files are
  rejected even if inactive. Installing a package can leave such files: remove
  the conflicting configuration deliberately, not through this package.
  Native nft inspection is required even with iptables-legacy: iptables-save alone
  cannot prove the absence of hidden native nftables policy. When `nft` is absent,
  inspection uses the libnftables public API via Python 3's standard ctypes library
  (Ubuntu docker.io supplies libnftables1). No inspection errors are suppressed.
  If neither path is available, lifecycle must supply the dependency; installing
  nftables itself may create `/etc/nftables.conf`, still treated as a conflict.
- Foreign filter rules, custom routing/VPN links, custom policy routing,
  additional physical uplinks and nondefault Docker NAT/raw forms are rejected. Docker's
  standard per-subnet MASQUERADE and inventory-bound endpoint raw drops are
  supported, including inventory-bound published-loopback raw drops. Older
  per-container hairpin MASQUERADE layouts are not.

## Failure/recovery and verification limits

This package fails closed on ambiguity; it does not promise automatic repair of
arbitrary root-administrator changes. A crash partway through *artifact staging*
(before rules change), ownership drift, missing tools, or a failed cleanup can
require administrator inspection. Do not blindly delete state or flush tables.
Preserve the generation and use the saved script's `rollback` action for an
unconfirmed, non-drifted partial installation. An operator must resolve foreign
references before removal can succeed. A confirmed installation is not removed
by the rollback action. Do not invoke the internal `--locked` argument externally.

Tests include regression policy generation, SSH discovery, native nft inventory
rejection, ownership/drift, timer staging, confirmation, expiry, rollback failure,
reenable, disable and IPv6. The generated shell is also executed against a
purpose-built iptables model, including unconfirmed reboot and confirmed restore.
That model is **not** kernel verification. Before release, VM tests must verify
real Ubuntu legacy/nft backends, IPv6 and custom SSH connectivity, DHCP/ND,
Docker DNAT and loopback bindings, isolation/egress, direct-routing denial,
container/network churn, daemon restart ordering, caller termination, reboot
while pending, confirmed reboot, and partial-family rollback failures. These
runtime security properties are not all covered by the focused acceptance suite.
See `integration/FIREWALL.md` for its coverage and limitations. The suite uses
real Vamos deployments and a host-process probe; admission does not exempt
unowned Docker workloads. Ubuntu's libnftables inspection path is exercised
when the nft CLI is absent.
