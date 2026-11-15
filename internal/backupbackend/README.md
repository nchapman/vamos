# Repository setup backend

This is only the first repository-setup slice. It does not implement backups,
restores, a host registry, an initialization journal, or cross-host locking.

## Caller contract

- Supply canonical validated configuration (including explicit `path` or `dns`
  lookup). This intentionally supports a conservative subset of S3 bucket and
  prefix names. No normalization of prefixes occurs.
- Hold the host mutation lock. For create-new, append a random operation prefix;
  require sole ownership of that prefix across hosts. CheckEmpty is a read-only
  preflight, **not an atomic emptiness reservation**.
- Journal init intent and credentials before calling Init exactly once. Init
  does not preflight, retry, probe, or fall back. A failure can mean partial or
  completed remote initialization. Reconcile with Probe; never infer permission
  to initialize from a Probe failure. Compare the full returned IDs afterwards.
- Restrict IAM credentials to an already-created bucket; deny bucket creation.
  Restic init itself can attempt bucket creation if the bucket disappears after
  the caller's preflight. This adapter cannot eliminate that distributed race.
- Provide an absolute, pre-existing root-owned 0700 runtimeDir. All ancestors
  must be real root-owned directories without group/other write permission.
  The tool root has the same ancestor requirements. Root tampering and races
  performed by root are outside this boundary; there is no ownership repair.
- Installation stages only public release payloads in a fresh private 0700
  directory within the verified tool version directory. The verified executable
  is chmodded and fsynced before atomic no-overwrite hard-link publication;
  the version directory is then fsynced. Staging is removed on ordinary returns,
  including failures. Runtime storage may be a separate filesystem such as
  tmpfs `/run`; credentials remain exclusively in private runtime scratch,
  never in the tool store.
- Cleanup after worker death remains the coordinator's job. Ordinary returns
  remove the private per-call directory; secure erasure is not promised.

Probe and Init have a two-minute upper deadline, further shortened by caller
context. S3 preflight has a 30-second deadline and one SDK attempt per request;
installer HTTP has a 60-second timeout. Process output is capped at 64 KiB per
stream and overflow cancels execution. Unix cancellation kills the process
group; there is no shell or detached execution. Linux amd64/arm64 only are
installed; unsupported operating systems fail closed.

The child receives an explicit environment, not inherited AWS/RESTIC, proxy,
loader, debug or password-command variables. Password/repository/custom CA are
0600 files in a fresh 0700 directory. AWS credentials use direct controlled
process environment variables, deliberately avoiding INI interpretation of
printable credential characters (`;`, `#`, `=`, brackets). Values are bounded,
single-line printable ASCII with no whitespace. This exposes AWS credentials
to privileged process-environment inspection, not argv; host root is trusted.
No credential files are passed to application processes.

`CheckEmpty` uses AWS Go SDK v2 static credentials, HTTPS verification, optional
additional CA roots, no environment configuration, no proxies or redirects,
bounded response bodies and MaxKeys=1. It checks `prefix/`, not similarly named
siblings. An object named exactly `prefix` is outside this subtree. HeadBucket
failure, malformed/ambiguous listing, any object, or truncated listing refuses.
It performs no writes, bucket creation or destructive cleanup.

All returned errors are fixed messages; no raw child, SDK, network, filesystem,
URL or credential diagnostics are wrapped. A verified repository ID is not
proof of repository freshness, application recoverability or restore testing.

## Tool provenance

Pinned version: **restic 0.19.1**, Linux amd64 and arm64, repository format 2.
No PATH discovery, latest resolution or automatic updates exist. `Artifacts()`
returns copies of URL, compressed SHA-256 and executable SHA-256 metadata.
Every use hashes the installed executable and checks mode/owner/type and safe
ancestors, then checks the actual `version` command before providing secrets.
Unsafe, drifted or partially present installed files are never overwritten.

Downloaded from the exact GitHub `v0.19.1` release:

- `SHA256SUMS` and `SHA256SUMS.asc` (retained in `provenance/`)
- `restic_0.19.1_linux_amd64.bz2`
- `restic_0.19.1_linux_arm64.bz2`

Release public keys downloaded from `https://restic.net/gpg-key-alex.asc` are
retained. The bundle's **first key is not the release key**. Selected the key
whose complete fingerprint is
`CF8F18F2844575973F79D4E191A6868BD3F7A907`, as documented in upstream v0.19.1
`doc/020_installation.rst`. With no gpg/gpgv available, used a disposable Python
venv with PGPy 0.6.0 (PyPI, cryptography backend) to:

1. Assert that exact primary-key fingerprint.
2. Cryptographically verify the detached signature over the exact SHA256SUMS
   bytes with that selected key (verification returned true).
3. SHA-256 both downloaded compressed artifacts and match their exact manifest
   entries.
4. Bzip2-decompress each artifact and SHA-256 the executable bytes.

PGPy warns that its certificate self-signature, revocation and key-flag checks
are incomplete. This verification pins the documented primary fingerprint and
verifies the manifest signature; it does **not** claim web-of-trust, revocation,
or gpgv verification. Scratch verifier and downloaded binaries are not committed.

| Linux arch | Compressed SHA-256 | Executable SHA-256 |
|---|---|---|
| amd64 | `f415415624dcc452f2a02b8c33641791a8c6d6d3b65bbb3543fcf9a25151585c` | `20d4142678d0d95ec11a4759def1b73fd9190abc9ca19e4b62d067c0b387e639` |
| arm64 | `a5f64aaab53d51e311fa3829124c5b703f2d14cf187d8640b6be3b2b49376465` | `2fb45ac6f9071b6f20eb883953a188f9e7c7cb6bbe43c67a2e47ada4e85ee7f0` |

Command signatures and initialization JSON fields were checked against upstream
v0.19.1 source. Unit tests exercise SDK requests against a synthetic-credential
httptest TLS endpoint, strict JSON, command signatures, environment isolation,
output limits, cancellation and error suppression. `TestActualPinnedTool` is
opt-in on a disposable Linux root host via `VAMOS_TEST_RESTIC_ROOT`; it downloads
and executes the pinned binary's version/help commands and tests drift refusal.
It deliberately corrupts that disposable root's installed binary. It is not a
real S3 initialization test. No live restic/S3, provider IAM, cross-architecture
execution or restore acceptance is claimed here; those remain parent-owned
Linux/real-S3 acceptance work.
