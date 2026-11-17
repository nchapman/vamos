# Managed host firewall

Managed firewall is an **opt-in, automatic policy for a dedicated Vamos host**.
It is not a general-purpose firewall editor: there are no custom rule, port,
interface, or backend settings. Existing unsupported firewall configurations
(including UFW/firewalld and foreign filtering policy) are refused rather than
silently replaced. Review the dedicated-host support boundary before enabling.

## Setup and commands

```sh
vamos server add --yes --firewall=true --domain apps.example.com prod root@host
# Or bootstrap without touching firewall rules:
vamos server add --yes --firewall=false --domain apps.example.com prod root@host

vamos server firewall enable --yes prod
vamos server firewall status prod
vamos server firewall status --json prod
vamos server firewall disable --yes prod
```

`server add` is server setup; `vamos init` only creates an application manifest.
On interactive stdin, add prompts for a firewall choice if the option is absent.
Noninteractive setup requires an explicit `--firewall=true` or `--firewall=false`;
`--yes` still authorizes bootstrap, but does not answer the firewall question.
False leaves firewall rules untouched, including any existing managed policy.
The choice is not saved in application configuration or host identity.

Standalone firewall commands use the registered default server when NAME is
omitted. Flags precede NAME. All require root or passwordless-sudo SSH access, a ready matching owned
installation, and the matching Linux executor artifact (including status).
`--executor FILE` selects an artifact; `--timeout DURATION` bounds client waiting
(default two minutes). Normal app operations still use the restricted SSH bridge.

After bootstrap and local registration, enable first stages the policy and arms
an independent five-minute rollback. The client then makes a **fresh,
non-multiplexed SSH connection** to confirm the returned token. The server never
auto-confirms enable. A failed confirmation is not reported as success: leave the
rollback timer to restore previous rules and query `server firewall status`
before retrying. Losing the response can leave the outcome unknown even if the
server accepted confirmation. Registration is retained if firewall setup fails.

## Policy and boundaries

- Preserve discovered SSH TCP listeners, including nonstandard SSH ports.
- Allow host HTTP/HTTPS ingress and necessary established/related traffic.
- Automatically allow intentional Docker public port publishing through Docker's
  NAT path. Published TCP/UDP services are public; an image's `EXPOSE` alone is not
  publishing. Container application ports are not added as host INPUT permits.
- Deny other unsolicited inbound traffic; outbound connectivity remains usable.
- Support dedicated Ubuntu/systemd hosts using iptables (legacy or iptables-nft)
  and supported Docker NAT bridge networking, not native nftables Docker backend,
  arbitrary routers/VPNs, or custom direct-routing modes.
- New Vamos app bridges and the BuildKit bridge work automatically, including when
  first created after enable. The policy matches `br-+` and `vamos-build0`; no
  disable/enable refresh is needed. Admission validates existing workloads using
  Vamos labels and host identity, allowing owned host-network Caddy but rejecting
  foreign containers/networks. Default `docker0` and IPv6 app networks are not
  supported. Host IPv6 ICMP, SSH and Caddy remain protected and usable.
- Public forwarding requires IPv4 TCP/UDP DNAT in conntrack's ORIGINAL direction
  with a non-loopback original destination. The firewall returns to Docker's own
  publish/isolation policy, never accepts around it. BuildKit's host-owned mangle
  guards are preserved. INVALID traffic and non-loopback INPUT to loopback
  destinations are dropped.
- Native nftables inspection is required even with iptables-legacy. It uses `nft`
  or, when that CLI is absent, Python 3 and the libnftables shared library supplied
  by Ubuntu docker.io. Missing both inspection paths fails admission;
  iptables-save alone is not a safe substitute. Installing the nftables package
  can leave `/etc/nftables.conf`, which is also a conflict and must be handled
  explicitly during dependency provisioning.
- Cloud/provider firewalls remain separate: allow necessary ports there too.
  Root and Docker administrators remain trusted and can change host networking.

Status reports enabled/disabled, pending confirmation, degraded state and detected
SSH allowances. JSON exposes the same state for automation. Degraded means the
recorded policy cannot be treated as healthy; investigate rather than assuming
protection. Runtime state is maintained independently in
`/var/lib/vamos/firewall.json`, not in app YAML or the local registry. The shared
operation/rollback lock lives at `/run/lock/vamos-firewall.lock`; querying status
on a never-enabled host does not create a Vamos data directory.

**Disable removes managed protection only.** It does not stop workloads, close
Docker published ports, or install a replacement policy. It refuses unsafe drift
rather than flushing unrelated firewall rules. Coordinate host networking changes
with deployments and build networking guards; do not manually flush managed
chains. See [published ports](ports.md) for application exposure semantics.

Unit/transport tests do not establish kernel, reboot, or real SSH rollback safety;
those behaviors require isolated VM validation, never live production firewall
experiments.
