# Runtime secrets: stage or set

Supported across Dockerfile-built and prebuilt-image public and private services, including volume-backed services. `stage` stores values without changing running services. `set` uses that same staging path, then replaces the deployed consumers using their existing images and configuration—without a source upload, build, image pull, or release command. Ordinary deployment also applies staged values; one-off runs independently capture latest selected staged versions without changing running services.

## Stage, inspect, deploy

Add environment-name to secret-name mappings under the service:

```yaml
services:
  web:
    # Either build or image is required; http is optional (public services only).
    secrets:
      SESSION_SECRET: session-secret
```

```sh
vamos secrets stage session-secret    # No-echo terminal prompt
vamos secrets list                   # Names, retained version IDs, latest flags
vamos status                         # Pending versions and affected services
vamos deploy                         # Capture staged versions and deploy
```

Flags precede the name. `-f`, project/server binding, and `--server` work as for deployment. The normal client uses the installed agent; omit `--executor`. Outside a project:

```sh
vamos secrets stage --server home --app notes session-secret
vamos secrets list --server home --app notes --json
```

For automation, prefer piping a value from a trusted secret producer into `vamos secrets stage --stdin NAME`, rather than putting credentials in shell commands or command arguments. Inline arguments are supported below, but expose values to shell history and local process inspection. Single-name `--stdin` reads a bounded stream and removes exactly one final LF or CRLF as input framing; it preserves other spaces and tabs. Additional line endings are rejected. For bare names, omit `--stdin` on a terminal to get hidden prompts. Explicit `--stdin` refuses a terminal before reading. For example, redirect a private, owner-readable file **outside every Docker build context**:

```sh
vamos secrets stage --stdin session-secret < "$HOME/.config/vamos/private/session-secret"
```

Prompt input is not echoed or added to terminal-editor history; ordinary cancellation restores terminal settings. A forced process kill cannot guarantee restoration.

Values must be nonempty, single-line UTF-8 without CR, LF, or NUL, at most **8192 bytes**. The interactive editor conservatively limits total typed/pasted input to fewer than 4096 value bytes; use `--stdin` for larger values. Multiline PEM material and binary values are not supported. In prompt and stdin input, quotes, backslashes, dollar signs, equals signs, and whitespace are literal, not shell syntax.

`secrets list --json` emits a metadata response including all retained versions, not just the latest. Version IDs are random operation IDs, not hashes of values; list order is by name/version, not creation time. `status --json` includes `secrets` entries with staged/applied version IDs, `pending`, and affected `services`. A staged but unused secret has no affected services. Secret-management output does not display values.

## Set new values and replace consumers immediately

`set` takes **new input**; it does not apply a previously staged value by name. Replacement can interrupt service, and the rollout across containers is not atomic.

```sh
vamos secrets set llm-api-key
# Hidden prompt, then replacement of the deployed services using this secret.
vamos status
```

`set` discovers consumers from their **deployed** secret mappings, not the local manifest. It changes only the named secret pins, preserving deployed configuration, per-service image identities, and all other secret pins—even if other secrets have newer staged values. It updates all replicas of each consumer through the normal public-readiness/private-stop-start machinery. This replaces containers; a Docker restart alone cannot change their environment.

The app must have a completed multi-service deployment, and affected consumers and their captured local images must be available and ownership-valid. Incomplete deployments must be recovered first. A legacy single-service v1 app must first be deployed with a current client. A manual-service consumer causes the entire request to refuse before staging: use `stage`, then preview a scoped `deploy --preview --service NAME` with the intended server and manifest before approving it. `set` never starts dependencies or silently approves a protected database update. Unused secrets, including those staged before the first deployment, are stored without starting any services.

**Setting a database initialization secret does not rotate a SQL role password.** PostgreSQL initialization environment variables only affect an empty data directory. Coordinate SQL-side credential changes separately. For provider API keys, keep the old key valid until all consumers have transitioned and their availability and applied versions have been checked.

## Multiple secrets in one operation

Both commands accept several names:

```sh
vamos secrets stage llm-api-key webhook-secret
vamos secrets set llm-api-key webhook-secret
```

Each value gets its own hidden prompt. Both commands also accept inline pairs:

```sh
vamos secrets set llm-api-key=EXAMPLE_ONLY webhook-secret=EXAMPLE_ONLY
vamos secrets stage llm-api-key=EXAMPLE_ONLY webhook-secret=EXAMPLE_ONLY
```

Use either bare names (prompts) or `NAME=VALUE` pairs, not a mixture; inline pairs cannot be combined with `--stdin`. Split occurs only at the first `=`, and the remaining value is passed literally after normal shell parsing. Quote the whole argument when needed for spaces or shell metacharacters. Inline values have the same nonempty, single-line UTF-8 and 8192-byte limits as stdin.

**Inline values can appear in shell history, local process listings, and external command logging.** Vamos warns without echoing values and keeps them out of request metadata and remote command arguments, but cannot erase that local exposure. Prefer hidden prompts or stdin for real credentials.

All inputs are validated before sending, and all versions are staged in **one atomic secret-store write**. An invalid value or exhausted store budget does not stage a partial batch. `set` then replaces each affected service once, even when it consumes several changed secrets.

For automation, pipe records from a trusted producer or redirect a private batch file outside every Docker build context:

```sh
vamos secrets set --stdin llm-api-key webhook-secret < "$HOME/.config/vamos/private/new-secret-values"
```

With multiple names, stdin contains literal `NAME=value` records, one per line, exactly once for each requested name. Record order does not matter; the first `=` separates the name from the value. Values are not shell-expanded, unquoted, or whitespace-trimmed. CRLF record endings are accepted; blank records, extra names, duplicates, missing names, and invalid values are refused. The existing **single-name** `--stdin` behavior remains raw value input, not `NAME=value` parsing. Stdin input cannot be combined with inline pairs.

The CLI prints the operation ID and every version ID before dispatch. A singleton retains its operation ID as the version; batch members have distinct, deterministic version IDs bound to the operation and secret name. Replaying an internal batch requires the same complete name set and values, never moves latest pointers backward, and cannot reuse that operation for a different batch. Ordinary CLI invocations still create new operations, even for identical values.

If the request was not submitted, it did not stage these values. If access was revoked after admission, ask the administrator to verify the original app and key before retrying; loss of access does not undo earlier work.

Staging is atomic; application across containers is **not**. If `set` loses its response or fails after staging, inspect `secrets list` and `status` before retrying. If status records the printed operation as incomplete, use `vamos resume OP`: it reuses the captured images and secret versions, including after newer values are staged. A failure before publishing the deployment journal can leave the values staged but no resumable deployment. Do not assume every failure left the store untouched, or rerun `set` blindly.

## Selection and recovery

- Staging before first deployment registers the app's secret identity without creating a deployment journal, container, network, or route. Status reports `staged`.
- Scoped deploy and `run` require only the selected service’s secrets. Unselected services retain actual configuration and secret pins. Ordinary deploy refuses staged-secret or policy changes to an existing manual service and points to a scoped preview pinned to the original manifest and server; the preview gives the approval command.
- Deploy preflight resolves all required secrets **before** saving new deployment inputs, building, or superseding a failed candidate. Missing secrets fail without disturbing the existing deployment.
- The deployment journal captures immutable references by environment key. Candidate, current, and previous releases retain their own mappings and version IDs, never the values.
- Resume uses the captured versions. Newer staging during a stopped incomplete deployment does not repin that operation. Missing/corrupt pinned versions cause failure; there is no fallback to latest.
- A failed deployment preserves already-completed service transitions, not the entire old app. Public services may already use new secret versions while others retain older pins. A failed private replacement leaves its old version retired; it is not restarted implicitly. Check each service's applied version **and availability** before revoking an old credential.
- Staging a corrected value does not repair a captured operation. Resume keeps that operation's pins; applying corrected inputs requires explicit supersede where permitted, or a fresh deployment after completion. A successful deployment can still have pending changes if newer values were staged after it captured its inputs.
- Docker restart and host reboot use the existing container's pinned environment, not the latest staged values. Recreating a candidate also uses its captured references.
- Staging shares the host mutation lock and refuses an active worker or pending maintenance. It is allowed during a stopped incomplete deployment, but pending/running/unknown one-off runs block mutations. See [run recovery](deployment.md#one-off-commands).

A lost staging response means the outcome is unknown. Record the printed operation/version ID and run `secrets list` for that target; the retained version list can confirm it even after another staging operation. Repeating the same internal operation with identical content is idempotent and never moves latest backward. Reusing that identity with different content fails. A new CLI staging invocation always creates a new version, even for identical plaintext. `resume` is for deployment/maintenance, not staging.

A **not submitted** diagnostic means the request was not admitted; check target and access rather than attempting to resume it. If the coordinator recorded a failure, inspect retained versions and, for `set`, deployment status: a failed queue record alone does not establish whether staging or replacement occurred.

Known refusals have value-free explanations: retained-version/name/size capacity,
conflicting operation content, an active host mutation, mismatched ownership/identity,
and unsafe or invalid secret-store metadata. These are accepted only from a complete,
supported response and rendered from a local allowlist, never arbitrary remote text.
Staging reports the operation/version and an inspection command. Missing, malformed,
unsupported or otherwise unrecognized failure responses remain **outcome unknown**;
a persistence error is not presented as proof that no value was committed. Unclassified
list failures remain generic. No refusal authorizes automatic pruning or repair.

## Storage and exposure boundary

Values are stored in `/var/lib/vamos/secrets/<app>.json`, separate from release history. The store is executor-owned (root in production), with a private `0700` directory and regular `0600` files. Unsafe ownership/permissions, symlinks, malformed records, and mismatched app/host identities are refused. Each staging commit atomically replaces a synced file containing immutable versions and the latest pointers together; the directory is synced as well. Batch commits also retain operation membership in secret-store format v2. Existing v1 stores remain readable; their first batch write upgrades them, and older executors that do not understand v2 refuse rather than modify them. Interrupted-write temporary files remain private and are not authoritative state.

The SSH payload contains a separate private `secret.value` input (raw singleton value or bounded batch JSON), never a value in request JSON or a source context. During container creation Docker receives a private `--env-file` in the worker's `/run` staging directory. On the executor, values are not passed in Docker process argv or injected into the Docker client's own environment, so keys such as `DOCKER_HOST` affect only the application. The env file is removed after the invocation; the worker wrapper removes its private directory on ordinary exit. A hard kill or an interruption during worker handoff can leave private `/run` material until explicit operator cleanup or reboot.

Runtime Docker command and inspection failures use value-free diagnostics before reaching responses or release journals. Local Docker inspection has an **8 MiB per-stream limit** to accommodate escaped secret environments; oversized responses still fail closed. The metadata-only SSH protocol retains its **64 KiB per-stream limit**. Build diagnostics remain available because runtime secrets are not supplied to builds. Persisted build-failure tails additionally redact exact known app-secret values across all retained versions before bounding the output. An unreadable required secret store withholds the diagnostic rather than exposing raw output. This does **not** detect arbitrary, encoded/transformed, unknown, or other-app credentials already present in source files: explicitly exclude `.env` and other sensitive files with the selected Docker ignore file. Dockerfile-specific ignore files take precedence over `.dockerignore`.

This is permission-restricted plaintext storage, not an encrypted vault or a defense against the host administrator. Root, Docker access, application code, Docker runtime metadata, and backups can expose values. Application/build logs can contain operator- or application-supplied secrets and are not arbitrarily redacted. The exact-value filtering of persisted build failures is a limited safeguard, not a guarantee that arbitrary logs are safe to publish; runtime container logs retain their existing behavior. Go/transport buffers are not guaranteed to be zeroized. Use trusted clients, executor artifacts, Dockerfiles, and applications.

## Retention and limits

Cleanup and app destruction retain **all secret versions**, including unreferenced ones. A destroyed receipt and retained store coexist; redeploy can reuse the staged values. Cleanup/destroy on a secret-only app has no deployment resources to remove and leaves the store unchanged.

There is no secret deletion or automatic secret garbage collection in this slice. Per app, staging is bounded to **64 names, 256 retained versions, and 1 MiB of serialized storage**; deployments support at most 64 secret environment mappings across the desired app configuration. Multi-service status reports each service's applied pins; partially completed operations can have mixed effective versions. Resume preserves all captured pins, and newer stages remain pending. At capacity, staging fails without modifying authoritative state. This is a finite rotation budget, not a hidden pruning policy.

Host teardown/purge refuses retained secret state. A new bootstrap cannot silently adopt it under a different host identity. Removing retained secrets requires separate deliberate operator action after checking backups and required version references; `destroy --yes` and host `--purge` do not authorize it. Backups must protect the secret store along with deployment state. [Rollback](rollback.md) uses captured immutable secret-version references, not staged values. Missing captured versions fail closed; there is no fallback to newer credentials. Rollback warns that restored credentials may have been revoked: restoring their values does not restore validity.

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
