# Give a laptop or CI runner its own deploy key

Use one named key per runner or laptop. The key can operate **one app**, including deploying code, executing commands, and changing that app's secrets and services. It cannot list all apps, deploy another app, grant keys, or administer the host. This is access for trusted app operators and code, not a sandbox for untrusted tenants.

## Grant access (administrator)

Have the runner generate a dedicated SSH key (e.g. `ssh-keygen -t ed25519 -f ~/.ssh/vamos-ledger`) and give you **only the `.pub` file**. Keep the private key on the runner or in its CI secret store. Use your existing root or passwordless-sudo server registration and matching Vamos bundle. On an older installation, upgrade the CLI and run `vamos server upgrade --yes prod` first. Run key administration when the host is idle:

```sh
vamos server principal grant --server prod --app ledger --key ./vamos-ledger.pub --yes ledger-ci
vamos server principal list --server prod
```

`ledger-ci` is a key name, not an SSH username. `ledger` is the exact app name in `vamos.yaml` (not a wildcard). `list` shows the name, app, and SHA-256 fingerprint; add `--json` for automation. It lists only keys managed by these commands (up to 256 per server). The original bootstrap key and manually configured principals are not replaced or listed.

## Connect and deploy (runner)

Provision the server's **independently verified SSH host key** in `known_hosts` and select the dedicated identity explicitly in `~/.ssh/config`:

```sshconfig
Host ledger-prod
    HostName server.example.com
    User vamos
    IdentityFile ~/.ssh/vamos-ledger
    IdentitiesOnly yes
```

From the checkout, with a compatible `vamos` CLI installed:

```sh
vamos server connect --bridge --domain apps.example.com prod ledger-prod
vamos status --server prod --app ledger
# Existing service: check its identity, then bind this checkout before deploying.
vamos service bind web --server prod -f vamos.yaml
vamos deploy --service web --server prod -f vamos.yaml
```

For a new app, skip status and binding and deploy from its manifest. Bind each existing service you intend to update; [service bindings](services.md#explicit-identity-bindings) are local to the checkout. Don't copy the administrator's `.vamos/` directory or private key. `connect --bridge` checks the installed identity and protocol via the bridge and writes only the local server registry. It does not enroll a key, invoke sudo, or bootstrap the host. Use `status --app ledger` rather than `apps` with a scoped key.

## Rotate or revoke (administrator)

For rotation, grant the replacement key **under a new name** and test it from the runner before revoking the old key:

```sh
vamos server principal revoke --server prod --yes ledger-ci
vamos server principal list --server prod
```

Revocation stops new requests; it does not cancel previously admitted operations, disconnect an active console, or undo changes the key already made. Other keys, including the bootstrap operator's, continue to work. If a response is lost, inspect `list` and repeat the same intended action before issuing a different grant.
