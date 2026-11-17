# Focused managed-firewall acceptance

Run from the repository on macOS with OrbStack installed:

```sh
mise run test:integration --scenario firewall
```

This uses the existing runner's source snapshot, isolated Ubuntu 24.04 VM,
identity/ownership checks, guest-only SSH keys, artifact collection, machine
restart and exact-ID deletion. It does **not** run the full integration or
repository suites. No host firewall or live server is contacted. A failure is
not skipped and no Docker/firewall configuration is rewritten to bypass
unsupported-baseline checks. Use `--keep-on-failure` only when deliberately
retaining the owned guest for diagnosis.

The prepare phase bootstraps with `--firewall=false`, proves an external host
listener reachable, enables and confirms through the CLI, then checks fresh SSH
and actual inbound filtering. A new Vamos application bridge and a real
Dockerfile build are created **after** enable. Public Docker TCP and UDP mappings
on that bridge, Caddy HTTP/HTTPS, loopback upstream privacy and direct-container
IP denial are tested. The probes run inside a Linux network namespace connected
by a veth pair (198.18.0.0/30); conflicting guest routes cause failure, not route
replacement. Local HTTP requests are only destination-side positive controls.
A denied TCP probe must time out, not just fail with connection refused.

The same packet checks run after Docker restart and after the runner restarts
the owned machine. Disable must preserve administrator sentinel chains in both
families and all non-owned iptables rules/policies, as well as working Docker
publishing and Caddy. Finally, an SSH executor enable request deliberately never
sends confirmation: the real five-minute systemd timer must restore access.
This test does not shorten the timer, edit state, or invoke rollback manually.

Artifacts include every command/stdout/stderr, the prepare checkpoint and (only
on complete success) `firewall-summary.json`, collected under
`.pi/integration/<owned-machine>/guest-artifacts.tar`. Allow at least five
minutes for rollback, plus VM bootstrap, compilation and image pulls.

Fast driver tests (no machine or packet mutation):

```sh
python3 -m unittest discover -s integration -p 'test_*.py' -v
mise exec -- go test ./integration/orbstack
```

Host filtering probes cover IPv4 and IPv6; container publishing probes cover
IPv4 (the supported app-network mode). HTTPS is TCP, not QUIC. OrbStack machine
restart need not reboot the shared kernel. Post-restart checks prove settled
policy, not absence of a transient startup exposure window. Docker's raw rules
can also enforce direct-container denial, so that denial alone is not attributed
to Vamos. The focused stage does not run the separate comprehensive build
isolation scenario; it verifies actual HTTPS dependency fetching during a build
with managed firewall enabled.
