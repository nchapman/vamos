# Client and executor versioning

Soar currently brings a Linux executor with each app operation (as the Phase 1
foundation toward an installed `soar-agent` coordinator; see
[the agent architecture proposal](agent-proposal.md)). A **matching local bundle
is enforced before uploading or launching that executor**, including when
`--executor FILE` is supplied. Read-only SSH host inspection may happen first
to discover the architecture.

## Build and update the whole bundle

```sh
mise run build
bin/soar version
# Optional human-readable release label:
mise exec -- go run ./cmd/build -version 0.2.0-dev
```

The output directory contains the native `soar` (`soar.exe` on Windows),
`soar-linux-amd64`, and `soar-linux-arm64`. Keep all three together when installing
or updating. `-out DIRECTORY` selects another output directory. Build from the
repository root. The former standalone `build:host-*` tasks are replaced by the
single bundle build; do not update just one artifact.

`soar version` prints the label and full SHA-256 build identity. The default label
is `dev`, but its identity distinguishes different source snapshots. A release
label alone is not used as the compatibility check.

The builder copies `cmd/`, `internal/`, `go.mod`, and `go.sum` into a private
snapshot and builds all three executables from those captured bytes. The
identity covers normalized relative paths and contents (conservatively including
tests and internal documentation), the selected Go version, version label, and a
versioned build recipe. It includes embedded shell scripts and uncommitted files;
it does not depend on Git metadata, timestamps, or checkout location. Exported
source archives work without `.git`.

The recipe disables CGO, VCS stamping, workspaces, ambient compiler flags and
experiments, and fixes architecture tuning. It uses readonly module mode and
rejects local module replacements whose source lies outside the captured inputs.
The toolchain and module cache remain trusted build inputs. Symlink/special-file
source inputs are refused. The builder verifies all output stamps before
publishing any executables.

Publishing the three files is **not an atomic directory replacement**. An
interrupted or concurrent update can temporarily leave a mixed bundle; operations
then fail safely at the pairing check. Reinstall/rebuild the whole bundle. Existing
remote workers retain their already-staged executable; updating local binaries
does not upgrade or interrupt them.

## Mismatched and unversioned artifacts

The client checks the selected artifact's Linux ELF architecture and requires one
valid embedded bundle marker matching its own. Validation and upload use the
same bounded bytes. It never executes the foreign-architecture artifact locally.
`--executor FILE` chooses a different location, **not a mismatch bypass**.

A raw `go build ./cmd/soar` or `go run ./cmd/soar` is unversioned and cannot issue
commands requiring an executor. Such builds can still validate manifests and use
host inspect/status/connect. Use the bundle builder for app operations (including
app status/logs) and host setup/teardown. The old `-X main.version=...` recipe does
not produce a supported bundle.

Missing, ambiguous, or mismatched markers fail with a rebuild/reinstall message.
The marker is a pairing guard, **not a signature or authenticity proof**. Install
artifacts only from trusted builds. The SSH payload's separate checksum verifies
transfer integrity; SSH host-key validation is unchanged.

## Protocol, state, and installed server tools

Build identity is separate from wire-protocol versions and persisted-state schema
versions. Their existing strict validation remains in force. Matching a bundle
ensures the client and its uploaded executor declare the same build inputs; it
does not guarantee an older bundle can read state written by a newer one. Do not
downgrade clients against newer state. Coordinate operator/automation upgrades
and retain the original bundle when diagnosing incomplete operations.

This build identity supplies the deterministic `agent_build` / `agent_version`
metadata used by the installed `soar-agent` API handshake. `soar server add`
installs the paired executor and coordinator under `/usr/local/lib/soar/`;
`soar server upgrade --yes NAME` replaces that pair only after strict ownership
and health checks. Host inspect/status/connect still require no executor artifact.
See [the agent protocol](agent-protocol.md) for compatibility and reattachment
semantics.
