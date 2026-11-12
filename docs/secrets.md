# Staged runtime secrets

Supported for the single Dockerfile-built, volume-free `web` service. Staging never builds an image, changes a route, or restarts a container. Values are applied only through deployment.

## Stage, inspect, deploy

Add environment-name to secret-name mappings under the service:

```yaml
services:
  web:
    # Existing build and http configuration remains required.
    secrets:
      SESSION_SECRET: session-secret
```

```sh
soar secrets stage session-secret    # No-echo terminal prompt
soar secrets list                   # Names, retained version IDs, latest flags
soar status                         # Pending versions and affected services
soar deploy                         # Capture staged versions and deploy
```

Flags precede the name. `-f`, project/server binding, `--server`, and `--executor` work as for deployment. Outside a project:

```sh
soar secrets stage --server home --app notes session-secret
soar secrets list --server home --app notes --json
```

For automation, pipe a value from a trusted secret producer into `soar secrets stage --stdin NAME`. Do not put literal credentials in shell commands, command arguments, environment assignments, or shell history. `--stdin` reads a bounded stream and removes exactly one final LF or CRLF as input framing; it preserves other spaces and tabs. Additional line endings are rejected. Without `--stdin`, a terminal is required. Input is not echoed or added to terminal-editor history; ordinary cancellation restores terminal settings. A forced process kill cannot guarantee restoration.

Values must be nonempty, single-line UTF-8 without CR, LF, or NUL, at most **8192 bytes**. The interactive editor conservatively limits total typed/pasted input to fewer than 4096 value bytes; use `--stdin` for larger values. Multiline PEM material and binary values are not supported. Quotes, backslashes, dollar signs, equals signs, and whitespace are literal, not shell syntax.

`secrets list --json` emits a metadata response including all retained versions, not just the latest. Version IDs are random operation IDs, not hashes of values; list order is by name/version, not creation time. `status --json` includes `secrets` entries with staged/applied version IDs, `pending`, and affected `services`. A staged but unused secret has no affected services. Secret-management output does not display values.

## Selection and recovery

- Staging before first deployment registers the app's secret identity without creating a deployment journal, container, network, or route. Status reports `staged`.
- Deploy preflight resolves all required secrets **before** saving new deployment inputs, building, or superseding a failed candidate. Missing secrets fail without disturbing the existing deployment.
- The deployment journal captures immutable references by environment key. Candidate, current, and previous releases retain their own mappings and version IDs, never the values.
- Resume uses the captured versions. Newer staging during a stopped incomplete deployment does not repin that operation. Missing/corrupt pinned versions cause failure; there is no fallback to latest.
- A failed deployment leaves the old current release and route intact. Its selected versions remain pending until successful promotion. A successful deployment can still have pending changes if newer values were staged after it captured its inputs.
- Docker restart and host reboot use the existing container's pinned environment, not the latest staged values. Recreating a candidate also uses its captured references.
- Staging shares the host mutation lock and refuses an active worker or pending maintenance. It is allowed during a stopped incomplete deployment.

A lost staging response means the outcome is unknown. Record the printed operation/version ID and run `secrets list` for that target; the retained version list can confirm it even after another staging operation. Repeating the same internal operation with identical content is idempotent and never moves latest backward. Reusing that identity with different content fails. A new CLI staging invocation always creates a new version, even for identical plaintext. `resume` is for deployment/maintenance, not staging.

## Storage and exposure boundary

Values are stored in `/var/lib/soar/secrets/<app>.json`, separate from release history. The store is executor-owned (root in production), with a private `0700` directory and regular `0600` files. Unsafe ownership/permissions, symlinks, malformed records, and mismatched app/host identities are refused. Each staging commit atomically replaces a synced file containing immutable versions and the latest pointers together; the directory is synced as well. Interrupted-write temporary files remain private and are not authoritative state.

The SSH payload contains a separate private `secret.value` input, never a value in request JSON or a source context. During container creation Docker receives a private `--env-file` in the worker's `/run` staging directory. Values are not passed in process argv or injected into the Docker client's own environment, so keys such as `DOCKER_HOST` affect only the application. The env file is removed after the invocation; the worker wrapper removes its private directory on ordinary exit. A hard kill or an interruption during worker handoff can leave private `/run` material until explicit operator cleanup or reboot.

Runtime Docker command and inspection failures use value-free diagnostics before reaching responses or release journals. Local Docker inspection has an **8 MiB per-stream limit** to accommodate escaped secret environments; oversized responses still fail closed. The metadata-only SSH protocol retains its **64 KiB per-stream limit**. Build diagnostics remain available because runtime secrets are not supplied to builds. This does **not** detect credentials already present in source files: explicitly exclude `.env` and other sensitive files with the selected Docker ignore file. Dockerfile-specific ignore files take precedence over `.dockerignore`.

This is permission-restricted plaintext storage, not an encrypted vault or a defense against the host administrator. Root, Docker access, application code, Docker runtime metadata, and backups can expose values. Application/build logs can contain operator- or application-supplied secrets and are not arbitrarily redacted. Go/transport buffers are not guaranteed to be zeroized. Use trusted clients, executor artifacts, Dockerfiles, and applications.

## Retention and limits

Cleanup and app destruction retain **all secret versions**, including unreferenced ones. A destroyed receipt and retained store coexist; redeploy can reuse the staged values. Cleanup/destroy on a secret-only app has no deployment resources to remove and leaves the store unchanged.

There is no secret deletion or automatic secret garbage collection in this slice. Per app, staging is bounded to **64 names, 256 retained versions, and 1 MiB of serialized storage**; deployments support at most 64 secret environment mappings across the desired app configuration. Multi-service status reports each service's applied pins; partially completed operations can have mixed effective versions. Resume preserves all captured pins, and newer stages remain pending. At capacity, staging fails without modifying authoritative state. This is a finite rotation budget, not a hidden pruning policy.

Host teardown/purge refuses retained secret state. A new bootstrap cannot silently adopt it under a different host identity. Removing retained secrets requires separate deliberate operator action after checking backups and required version references; `destroy --yes` and host `--purge` do not authorize it. Backups must protect the secret store along with deployment state. Future rollback must use retained references and may restore revoked credentials; rollback is not implemented yet.

For multi-service deployment/recovery and metadata-response limits, see [multi-service apps](multi-service.md).

## Verification

Offline tests cover value framing/limits, literal preservation, private ownership and filesystem boundaries, malformed-record redaction, atomic version selection, idempotent retries after rotation, storage budgets, secret-only identity, mutation fences, preflight before supersede, lost-create adoption with pinned versions, missing-version refusal, remapping, maintenance retention, and separation of values from argv, contexts, status, and journals.

On the Ubuntu 26.04 amd64 droplet (Docker 29.1.3, Caddy 2.6.2), using only synthetic values:

- Missing-secret deployment failed before creating app deployment state. Staging then produced a secret-only identity, and first deployment applied version A.
- Staging B left A live. Container restart and a full orderly host reboot kept A pinned and B pending. Hello recovered on its unchanged port and stayed healthy during the other lifecycle tests.
- A B deployment deliberately failed readiness while A continued serving. C was staged during the stopped operation. Once the candidate's delayed readiness condition was satisfied, resume promoted captured B, not C. Status correctly kept C pending; a fresh deployment then applied C.
- Values containing leading/trailing spaces, tabs, quotes, backslashes, dollar signs, equals signs, and Unicode matched their exact expected runtime digests. Mapping `DOCKER_HOST` affected the application only, not the Docker client.
- A maximum-mapping deployment with **64 mappings to an 8192-byte value**, whose escaped Docker inspection exceeded 3 MiB, passed readiness, status, and exact runtime digest checks.
- App journals/archives and the probe's logs were checked for the synthetic values. Store permissions were root-owned `0700`/`0600`. Cleanup and destruction removed runtime resources but retained all versions. The final host keeps only hello's current/previous containers; the destroyed probe has its receipt and a private store with four synthetic versions across two names. No worker staging/env files remained.

A macOS pseudo-terminal test verified no echo and restored terminal modes on success, Ctrl-C, SIGTERM, and timeout. Full `mise run check` passed (formatting, vet, race tests, native client and Linux amd64/arm64 builds), as did Windows cross-compilation. Review findings were triaged and the follow-up found no actionable issues.

Native Windows/Linux client and prompt execution, arm64 host execution, forced power-loss recovery, and exhaustive worker-kill injection remain unverified. Host teardown refusal with retained secrets was tested offline, not by tearing down the live hello host.
