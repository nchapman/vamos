# Disposable guest repository acceptance

The OrbStack coordinator runs `acceptance.py` **after** successful reboot recovery
and the earlier Docker ownership/host teardown scenarios. It uses the existing
`local` registry target (`root@127.0.0.1`) and real CLI → SSH → host manager path.
No new server alias or ownership bypass is introduced.

Parent invocation, from the repository root on macOS:

```sh
go run ./integration/orbstack --keep-on-failure
# Optional: --arch arm64|amd64 --ubuntu 24.04|26.04
```

Do not invoke the guest script on a workstation. The coordinator passes its
ownership token and imposes a 15-minute deadline. It collects the existing
private guest artifacts and `repository-summary.json`; success of the earlier
`result.json` alone does not certify this final phase. Coordinator exit status is
authoritative. This implementation has not been run against a VM by its author.

Fixture:

* RustFS real S3 service, multiarchitecture OCI index pinned to
  `sha256:b81d16a90ee25bd9511280127ca1471788c6e874cf0f0da759645f46b2dc1d74`.
  Metadata source: `https://hub.docker.com/v2/repositories/rustfs/rustfs/tags/latest`,
  retrieved 2026-11-13. The tag is **not** used at execution time. MinIO's public
  image metadata was unavailable (401/404), so this uses RustFS instead.
* Container port is bound only to guest loopback 19000. A separate Caddy process
  (admin disabled, no mutation of the managed Caddy config) exposes HTTPS on
  loopback 19001 using a generated one-day synthetic certificate.
* AWS SDK v2 helper creates the bucket as fixture administrator, independently
  of the backend. Every comparison lists the entire bucket, paginated, including
  locks, with object names, ETags, sizes and modification times.
* Synthetic credentials enter commands through stdin or a guest-private Docker
  env file, never secret-bearing argv. State stays in the disposable guest.
  Recovery kits are not included in collected artifacts.
* First use requires the pinned restic to be absent and `/run` to be on a
  different filesystem from `/usr/local`. Real config/key objects, full 64-hex
  identity, version 0.19.1 and format 2 are asserted.
* Repeated verify, successful connect, settled resume, wrong password/ID refusal,
  and a direct backend read-only missing-prefix probe must leave object listings
  unchanged. The probe helper uses the production backend, not an S3 mock.
* Kit/host record modes, retained credential equality, root-only host access,
  scratch cleanup and absence of credentials in output/command artifacts are
  checked. Listing equality detects persistent remote changes, not transient
  create/delete pairs; the backend's `--no-lock` unit tests complement this.

Cleanup only removes the exact returned fixture container ID (and its anonymous
volumes) and stops its own TLS process. Repository records deliberately remain;
no production journal is edited to make teardown pass. The coordinator deletes
only its verified owned VM. Unknown-init outcome replay remains the parent/unit
suite's responsibility; there is no live init fault injection here.

Deterministic checks (no Docker/VM needed):

```sh
go test ./integration/orbstack ./integration/repository
python3 -m unittest discover -s integration/repository -p 'test_*.py' -v
```

Integration constraints: requires the parent CLI/backend work to be present,
Docker Hub access to the pinned RustFS image, restic release download access,
free guest loopback ports 19000/19001, and the existing built CLI/SSH/Go/Caddy
installation. Bucket creation uses fixture admin credentials, also intentionally
used as the synthetic repository credentials; IAM least-privilege policy testing
is not covered. No cloud resources are provisioned.
