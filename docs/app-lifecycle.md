# app lifecycle

See [persistent local volumes](volumes.md) for single-writer replacement, durable 503 routing, retained data and explicit reuse after destroy.

Soar supports app-scoped cleanup and destruction for public/private multi-service apps, including legacy single-service journals. These commands never run Docker prune, remove volumes, or force-delete images/containers.

## Preview, then explicitly apply

```sh
# Inside a project, -f and the existing project/server binding work as for deploy.
./bin/soar cleanup -f examples/hello/soar.yaml
./bin/soar cleanup -f examples/hello/soar.yaml --yes

# Outside a project, name both the server and app.
./bin/soar destroy --server droplet --app temporary-app
./bin/soar destroy --server droplet --app temporary-app --yes
```

Without `--yes`, both commands return a preview and delete nothing. `--yes` authorizes a **freshly recomputed plan** under the host mutation lock, not an earlier preview. Preview and apply therefore may list different resources if the host changed in between. `--json` emits the full response (plan plus status); `status --json` continues to emit only the status object.

Only a ready deployed app can begin maintenance. A secret-only app has no recorded deployment resources to remove; cleanup/destroy leave its store unchanged. Finish or supersede an incomplete deployment first. An app already destroyed is an idempotent no-op. No implicit stop, data purge, or automatic retention policy is added to deployment.

## Removing one service

Use `soar service remove NAME --app APP --server SERVER` to preview, then add
`--yes` to apply. Manual services additionally require `--allow-protected`.
The app must be settled and retain at least one service; use `destroy` for the
last service. Removal retains data/secrets and never allows rollback to resurrect
membership. See [independent services](services.md) for bindings, re-add, retained
data approval, interrupted removal and the pending live-verification boundary.

## Cleanup policy

Cleanup preserves:

- Effective service versions, pending work, v7 per-service predecessors, current/previous completed app snapshots, and their source archives. A partial operation is not a completed snapshot; cleanup does not discard the last successful app state.
- Any running historical container, even if it is otherwise old.
- Images referenced by any Docker container, including stopped/foreign containers, or another app's effective slots, completed snapshots, candidates, captured builds, or pending maintenance state.
- Tagged images and images with repository digests.
- Image-ID provenance files, so a later cleanup can reconsider safely retained build outputs.
- All [secret versions](secrets.md), in a separate private store outside release history.

It can remove stopped older containers, obsolete captured contexts, and unused untagged/undigested images proven by private app-local Buildx IID records. Docker 29's containerd image store distinguishes requested OCI manifest/index IDs from actual container image/config IDs; reference checks protect both. Image inventory includes untagged/intermediate outputs with `--all`, but **only proven app build outputs** are deletion candidates.

Image references/tags are checked again during apply. An image that becomes shared is retained rather than forcing removal. The returned plan is the authorized plan, not a per-object deletion receipt; newly shared images may remain. Cleanup does not discard their provenance. Shared base images, BuildKit cache, certificates, and unknown resources are outside this command's ownership scope.

Public preview details are bounded to 1,024 resource/retention entries and 32 KiB
of JSON, independently of captured deletion authority. Large previews explicitly
report `details_incomplete: true` and exact category `totals`; the displayed
arrays are only a subset, not an exhaustive list. Approval covers the **entire
freshly recomputed plan**, including omitted details. Small previews retain their
complete detail arrays. The returned plan is still authorization, not a deletion
receipt. Docker inventories remain bounded; failed or truncated inventory is
never interpreted as an empty host.

## Destruction and recovery

Destruction removes the app's generated route, verified owned containers, its empty owned bridge network, and known captured contexts/IID files. It does **not** remove the local project binding or configuration, DNS, another app, volumes, secrets, build cache, or Caddy's shared certificate storage.

The sequence is:

1. Capture exact container/network IDs, immutable image IDs, and hashes of known files in a durable maintenance record. New operations publish private hash-linked pages under `/var/lib/soar/maintenance/<app>/` before installing their bounded journal reference. Historical inline maintenance records remain resumable.
2. Persist old/new whole-host Caddy route intent.
3. Validate/reload and prove the route-free live Caddy configuration.
4. Disable restart, stop, and remove only recorded, reverified containers; remove safe images and the empty recorded network. Persist completion of this resource phase before unlinking hash-matching recorded files, so interrupted provenance deletion cannot cause another image/container deletion attempt.
5. Replace app state with a minimal completed-destruction receipt.

Containers with unverified mounts, unexpected network attachments, replaced identities, unknown files, and unrecognized Caddy drift block the operation. No recursive app-directory removal is used. Shared/tagged images survive destruction; their IID records are intentionally discarded, so subsequent cleanup of those images is the operator's responsibility.

An interruption leaves a resumable operation. While maintenance is pending, other app mutations are fenced even after the original worker exits. Host setup/teardown also refuses active or incomplete app state. Status and logs remain available (logs require a surviving current container).

```sh
./bin/soar status --server droplet --app temporary-app
./bin/soar resume --server droplet --app temporary-app OPERATION_ID
```

The private page chain binds the host, app, operation, route intent and full
captured resource list. Missing, substituted or corrupt pages fail closed.
Publication that completed before the app-journal write still fences mutations;
the refusal identifies the app and exact operation to resume. A never-published
preparation grants no deletion authority and may be rebuilt only against the
unchanged journal. Completed operations clean up their private pages; a crash
during that cleanup can be reconciled by exact-operation resume or subsequent
maintenance. Leftover maintenance state can block host teardown; do not remove it
manually to bypass the guard.

Resume replays only the published plan and rechecks route removal before continuing destructive work. Missing objects count as completed only after a successful inventory query; a replacement with the same name is not adopted. Successful cleanup and destruction can be resumed idempotently until a later operation replaces their completion record.

A destroyed app can be deployed again normally. The small receipt remains for status/retry safety. Explicit host teardown/purge accepts only strictly decoded, matching-host completed receipts with no extra app files; existing workload, image, build-cache, mount, and ownership checks remain intact. It does not treat a receipt as proof that Docker is empty, and retained secret stores still block host teardown/purge.

## Restart-stable ports and upgrade notes

New deployments select an explicit loopback host port, record it before Docker creation, and verify both configured and effective bindings. Bind conflicts fail without silently changing an already recorded endpoint. A missing candidate with explicit port intent may be recreated only with that same port; legacy missing-endpoint recovery remains conservative.

**Redeploy apps created by the first deployment slice before relying on reboot recovery.** Those containers used Docker's automatic host-port assignment, which can change on container restart. Merely resuming an already-ready legacy operation does not pin its Docker configuration.

All app commands now use a matching trusted local Linux executor artifact. Status/logs upload it to a private temporary directory, execute it directly, and remove that staging directory; they do not use the mutation worker or replace the bootstrap-owned installed executor. This avoids older strict state decoders rejecting newer deployment/maintenance fields. `server inspect` remains unchanged and does not upload an executor.

This is a **root-SSH administration interface**, not a restricted read-only credential API. Status/logs are read-only with respect to application/runtime state but stage executable files. Use trusted client binaries and executor artifacts; the payload checksum checks transfer integrity, not software authenticity. Older clients may reject newer journal or summarized-preview fields; update the client and companion Linux executor artifacts together before managing these operations. An older strict decoder fails closed rather than interpreting a partial preview as complete.

## Verification

Historical evidence below predates storage lifecycle acceptance unless explicitly noted. See [persistent volumes](volumes.md) for the storage boundary; no new live/full-suite results are implied.

Offline tests cover preview immutability, maintenance fencing, strict receipts and teardown refusal, same-name replacement detection, network identity changes, image sharing/tag retention, archive hash drift, replay of missing resources, Caddy route proof, cross-app route isolation, port intent/recreation, and bounded journals. Capacity regressions exercise actual planning with 500, 1,024 and 4,096 keyed IID receipts, repeated cleanup and interrupted destruction, plus a 20,000-file intent beyond the old journal ceiling. They also cover page corruption/substitution, interrupted publication, scoped removal with unrelated history, run-removal proofs and preserved replay fences. These are filesystem-backed tests with simulated runtime boundaries, not thousands of live deployments. External Docker/Caddy boundaries are simulated.

On the Ubuntu 26.04 amd64 droplet with Docker 29.1.3 and Caddy 2.6.2:

- Deployed `lifecycle-probe.pastime.gg` alongside `hello.pastime.gg` with distinct content and separate bridge networks.
- Reproduced the legacy bug: restarting the probe changed its port from 32772 to 32773 and yielded HTTP 502, while hello stayed healthy.
- Replaced both apps with pinned-port releases (hello 44161, probe 45211).
- Restarted the probe container and Docker, then fully rebooted the droplet. Both ports remained unchanged; only current containers restarted. Public TLS-verifying HTTP responses matched each app's source HTML after reboot.

- Cleanup preview/apply removed two retired hello containers, three unused build images, and four obsolete contexts. Current/previous containers, their images/contexts, and IID provenance remained. Both apps continued returning `ok`.
- Destroy preview/apply removed the probe route, both probe containers/images, its bridge network, and captured inputs. Hello's public HTML remained byte-for-byte unchanged. Status returned `destroyed` with the same JSON shape as normal status.
- Resuming completed cleanup and destruction was idempotent. Redeploying the probe from its minimal receipt and destroying it again both succeeded.
- Final inspection found only hello's running current container and stopped previous container, their two images, the `soar-hello` network, and only hello in Caddy's live route configuration. The probe retained only its minimal receipt. No `/run/soar-stage.*` directories remained.

Local `mise run check` (formatting, vet, race tests, native client and Linux amd64/arm64 builds) passed, and Windows test binaries cross-compiled. Native Windows/Linux client test execution and arm64 host execution remain unverified. The reboot was orderly: forced power-loss and remote worker termination at every individual destruction boundary were not exercised on the droplet. Host teardown acceptance/refusal for completed receipts was tested offline; the live hello host was deliberately not torn down.
