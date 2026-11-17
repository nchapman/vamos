# Sudo-only administrative acceptance

Run `mise run test:integration --scenario sudo` on macOS with OrbStack.
The existing owned-VM runner creates an isolated Ubuntu guest, snapshots the
current source, collects artifacts, restarts the machine, and deletes its exact
verified VM ID. Existing registered servers are never contacted.

The scenario disables root SSH login and creates two test accounts with the
same fixture public key:

- `vamosadmin`: a non-root user permitted to run a root shell with `sudo -n`.
- `vamosdenied`: sudo requires a password. Setup must fail without prompting or
  creating the host ownership receipt.

It verifies non-root inspection, setup with firewall enablement and fresh-SSH
confirmation, a real deployment, restricted-coordinator exec, uploaded-executor
status and binary-stdin sessions, safe classification of real Docker port-bind
and missing-image failures, upgrade with app routes and firewall enabled,
restart recovery, firewall disable/re-enable, app removal, and host teardown.
The test removes only its pinned pulled fixture image before host teardown;
Vamos intentionally does not adopt arbitrary pulled images as owned builds.

It also simulates interruption after the firewall's real rule-removal script
has retired the identity and one owned artifact has been unlinked. Retrying
normal `server firewall disable` must complete without stopping Docker/Caddy.
Unit tests additionally cover interruption after each artifact unlink, reload
and stop failures, and refusal of changed files, symlinks, and foreign rules.

Commands and results are retained in
`.pi/integration/<owned-machine>/guest-artifacts.tar`; successful runs include
`sudo-summary.json`. These are real SSH/sudo/systemd tests, not mocked sudo.
OrbStack machine restart need not restart its shared kernel.
