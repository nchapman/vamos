# Common failures and safe recovery

## SSH and setup permissions

Use a dedicated supported Ubuntu host and either root SSH access or a non-root
SSH account with passwordless sudo. Root SSH login can remain disabled.
Administrative commands require permission to run a root shell with `sudo -n`;
password-required sudo and sudo policies requiring a TTY are not supported.
Vamos never reads a sudo password from your deployment/session input.

```sh
ssh ubuntu@server.example.com 'sudo -n true'
vamos server inspect ubuntu@server.example.com
vamos server add --yes --firewall=true --domain apps.example.com prod ubuntu@server.example.com
```

Explicit `server inspect` does not invoke sudo; a non-root UID in that report is
not itself a setup failure. Setup performs its own privileged inspection and
reports OS/systemd/architecture failures separately from privilege failures.
Normal app commands continue to use the restricted coordinator bridge, not sudo.

SSH needs an independently verified host key and noninteractive key/agent
authentication. Unlock encrypted keys in your SSH agent. If a host key changes,
verify its replacement through a trusted channel before updating `known_hosts`;
do not disable host-key checking to bypass the error.

Ports, identities, and jump hosts belong in SSH config, not `host:port` syntax:

```sshconfig
Host prod-vamos
    HostName server.example.com
    User ubuntu
    Port 2222
    IdentityFile ~/.ssh/id_ed25519
```

Then use `prod-vamos` as TARGET. `ssh -G prod-vamos` shows its effective settings.

## Partial setup and firewall changes

If setup reports **server ready and registered, firewall unconfirmed**, do not
start setup again. Check `vamos server firewall status prod`, then use
`vamos server firewall enable --yes prod` if protection needs enabling. An
unconfirmed installation has an independent five-minute rollback timer; a failed
SSH response alone does not prove whether a remote operation completed.

Disable has no automatic rollback. Inspect status before retrying
`vamos server firewall disable --yes prod`. Cleanup can resume after removal of
some owned artifacts, but modified/foreign files or rule references still cause
refusal. Do not flush tables or remove unrelated files to bypass ownership checks.

## Deployment failures

A request the agent did **not admit** has no deployment to resume. For an access refusal, check the selected server, manifest app name, and SSH key/grant with the administrator. If the connection was lost and admission is uncertain, inspect the original app on the same server; only resume an operation that status reports as incomplete. Do not retry a one-off command solely because SSH disconnected.

Recognized Docker failures expose static, secret-free categories for occupied
ports, unavailable Docker, inaccessible images, and full storage. Check the
reported category and app status before retrying. Unknown runtime diagnostics
remain withheld because Docker output can contain runtime secrets. Error
classification does not authorize automatic retries or cleanup.

When readiness fails, a candidate may remain for diagnosis. Check status and its
candidate logs (select the failed service for multi-service apps):

```sh
vamos status --server prod --app APP
vamos logs --server prod --app APP --service SERVICE --candidate
vamos resume --server prod --app APP OPERATION
```

Resume uses the captured inputs. To replace them, deploy your changed project
with explicit `--supersede OPERATION`. Do not assume the previous release is
serving: volume-backed and directly published services can use stop-first
replacement. No automatic data rollback occurs.

## Package-manager contention

APT install/purge waits up to 60 seconds for its dpkg lock, bounded further by
the command deadline. If cloud-init, unattended upgrades, or another package
manager still holds the lock, allow it to finish and retry. `apt-get update` has
a separate package-list lock; Vamos reports that contention without retrying it.
Never delete lock files or kill package processes to force setup through.
