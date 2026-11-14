# Encrypted backup repository setup

**Implemented boundary: repository setup only.** Soar can create or connect to an
encrypted S3-compatible repository and verify its identity. It does **not** yet
capture application data, restore it, schedule backups, prune snapshots, or run a
recovery drill. A repository reporting `ready` is not evidence that your data is
backed up. The [three-method backup design](backups.md) covers the next increments.

## Requirements

- An existing, ready Soar server and an independently provisioned S3-compatible
  bucket, reachable over HTTPS. Soar does not provision cloud resources or buckets.
- A provider endpoint, region (default `us-east-1`), bucket and prefix. Endpoints
  cannot contain credentials, a URL path, query, or fragment. Bucket names use
  lowercase letters, numbers and hyphens; prefixes use letters, numbers,
  underscores, hyphens and slash-separated components.
- Dedicated storage credentials. They are separate from application runtime
  secrets. Restrict access to the intended bucket/prefix; do not grant bucket
  creation. Creation needs object writes as well as bucket/prefix reads. Read-only
  connect/verify needs to read the encrypted config and key objects. This is not
  an append-only credential or protection against a compromised root account.
- A caller-owned recovery directory, not group/world writable. On POSIX the kit
  is exclusively created with mode `0600`; Windows users must secure the directory
  with suitable ACLs themselves. Copy the kit into an independent password manager
  or offline recovery store—not only onto the server being protected.

The backend is restic **0.19.1**, repository format **2**, pinned for Linux amd64
and arm64. The executor installs and verifies a versioned binary on first use;
there is no dependency on a system `restic`, shell PATH, or ambient AWS credentials.
[Artifact hashes and verification provenance](../internal/backupbackend/README.md)
are retained in the repository. Older repository formats are rejected, not upgraded.

## Create a new repository

Flags precede the repository name. All repository commands select a registered
server using `--server` or the default server; app-directory bindings do not select
or scope a repository.

```sh
mkdir -p "$HOME/.config/soar/recovery"
chmod 700 "$HOME/.config/soar/recovery"
soar backup repository add --server home --mode new \
  --endpoint https://s3.example.com --bucket personal-backups \
  --region us-east-1 --prefix soar --lookup path \
  --recovery-file "$HOME/.config/soar/recovery/offsite.json" \
  --credentials-stdin --yes offsite < private-storage-credentials.json
```

`private-storage-credentials.json` is an operator-provided private JSON object:

```json
{
  "access_key_id": "YOUR_STORAGE_ACCESS_KEY",
  "secret_access_key": "YOUR_STORAGE_SECRET_KEY"
}
```

Optional fields are `session_token` and `ca_certificate` (PEM certificates, encoded
as a JSON string). `password` is required when connecting to an existing repository
and must be omitted for new repositories. Do not place secret values in flags,
shell history, app YAML, or source control. Private JSON is bounded to 64 KiB;
unknown/duplicate fields are rejected by the CLI. New passwords are generated from
32 cryptographically random bytes.

On a terminal, omit flags to use public configuration prompts and no-echo
credential input. Use `--credentials-stdin` with complete public flags and `--yes`
for automation; it never consumes that stream as interactive prompt answers.
Without `--yes`, nonterminal add is a preview: it does not read credentials, write
a recovery kit, inspect SSH, or contact storage.

New repositories receive a fresh operation-scoped prefix, for example
`soar/<32-hex-operation>`. The supplied base is not initialized directly. Soar
checks the existing bucket and exact subtree for emptiness independently of restic.
A failed probe never triggers initialization. The random prefix is essential:
restic initialization does not provide an atomic cross-host create-if-absent lock.
Do not arrange for other hosts/tools to initialize the same prefix concurrently.

## Connect to an existing repository

Use its **exact** prefix and the independently known, full 64-hex decrypted
repository ID. This registers a pin; it never initializes or repairs the repository.

```sh
soar backup repository add --server home --mode connect \
  --endpoint https://s3.example.com --bucket personal-backups \
  --region us-east-1 --prefix existing/exact-prefix --lookup path \
  --repository-id FULL_64_HEX_REPOSITORY_ID \
  --recovery-file "$HOME/.config/soar/recovery/existing.json" \
  --credentials-stdin --yes existing < private-existing-credentials.json
```

The private JSON must additionally contain the existing restic `password`.
Interactive connect can observe the ID read-only and asks you to confirm the full
value. Noninteractive connect always requires `--repository-id`. A wrong password,
wrong ID or unsupported format fails verification; none causes initialization.

## Observe and reconcile

```sh
soar backup repository list --server home --json
soar backup repository show --server home offsite
soar backup repository verify --server home offsite
soar backup repository resume --server home offsite ORIGINAL_32_HEX_OPERATION
```

- `list` / `show` read recorded metadata only. They do not contact S3 or prove
  current reachability. Listings are bounded to 32 registered repositories.
- `verify` reads and decrypts the remote config, compares the full pinned ID and
  format, and updates local verification metadata. It uses restic
  `cat config --no-cache --no-lock`: no remote lock, cache, snapshots, or repair.
  It is not a full repository integrity check or restore test.
- `resume` uses the original operation and captured credentials/configuration.
  Before initialization was attempted, it can repeat the read-only preflight and
  continue the approved setup. Once initialization intent has been durably recorded,
  it can **only observe** the result; it never invokes initialization again.

States are `prepared`, `initializing`, `ready`, and `unverified`. `initializing`
may mean the tool stopped, the response was lost, or only some remote objects were
written. If read-only reconciliation cannot recover a valid config, retain the kit
and objects for manual investigation. Do not delete objects or repeat init just
because a probe failed. `unverified` preserves the expected ID even when current
verification fails; a replacement repository cannot silently be adopted.

All remote operations run under the existing host mutation lock and a bounded
systemd worker; read-only recorded listings do not require the worker. Application
journals, release commands, service volumes and applied secret pins are untouched.

## Recovery material and limitations

The client writes and syncs the recovery kit **before** remote setup. It contains
configuration, storage credentials, encryption password, operation identity,
backend/format versions and (when known) the full repository ID. After successful
verification the client atomically finalizes the ID without replacing the key.
An interrupted setup leaves the pending kit intact. A failure to finalize a kit
must not be interpreted as remote setup having failed.

On the server, private host-bound records live under
`/var/lib/soar/backups/repositories/` (`0700` directories, `0600` records). Credentials
travel in a separate private archive entry, never request metadata. Raw backend
output and credential values are not returned by repository commands. The pinned
tool lives under `/usr/local/lib/soar/tools/restic/0.19.1/`; temporary credential
files stay in the root-only worker runtime directory and are removed on completion.

The kit is usable without Soar's local registry: install the independently verified
restic version, provide the exact S3 URL (`s3:ENDPOINT/BUCKET/PREFIX`), password file,
provider credentials and optional CA file, then read config with `--no-cache
--no-lock` and compare its full ID. Use private files/controlled environment rather
than secret command arguments. **There are no application recovery points in this
slice**, so this is repository-access recovery only.

Registration names, captured inputs and repository identities cannot be overwritten
by another setup operation. Credential rotation, removing registrations, scheduling,
pruning and object deletion are not implemented. Retained repository state blocks
host teardown/new host identity creation; preserve independent recovery material
before any manual state reconciliation. Do not use manual deletion as an automated
cleanup or retry mechanism.

## Verification for this slice

- `mise run check`: formatting, vet, the full race suite, native build, and Linux
  amd64/arm64 builds passed. `mise run test:python` passed 54 tests. Windows amd64
  test compilation passed; no Windows live SSH or ACL acceptance is claimed.
- Review and follow-up review completed with no remaining actionable findings.
- The complete owned Ubuntu 24.04 arm64 integration run
  `soar-test-3a69292083eb` passed existing deployment/runtime scenarios, restart
  recovery, and the new repository phase. It collected artifacts and deleted its VM.
- The repository phase used real pinned restic and a digest-pinned guest-local
  RustFS service over HTTPS with a private CA. It verified installation across
  `/run` tmpfs and the tool filesystem, create/connect, full identity pinning,
  repeated read-only verification, list/show/resume, wrong passwords/IDs, missing
  prefixes, private recovery/host files, scratch cleanup and credential-free output.
  Complete bucket object listings remained unchanged across read-only operations.
- Unit tests cover lost initialization results, durable intent before the sole init
  call, read-only resume, immutable captured inputs, unsafe/foreign state and
  credential transport. This is not a live power-loss or application restore drill.

An earlier fresh run refused an unplanned network attachment in the unchanged
failed-service-removal scenario, before repository testing. The final clean run
passed without relaxing ownership checks; the earlier diagnostics were retained.
Commercial S3 providers, DNS-style bucket lookup, amd64 runtime execution, snapshot
publication and application source-loss recovery are not claimed by this evidence.
