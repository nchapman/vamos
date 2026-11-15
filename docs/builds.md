# Isolated remote builds

Vamos builds captured source on the deployment host, but not in the host's network
namespace. This applies to ordinary/scoped deployments, captured-build recovery,
and builds for one-off commands. Prebuilt-image deployments do not need the build
service.

## Boundary

A pinned BuildKit daemon runs in its own container on a dedicated Docker bridge.
Both Dockerfile `RUN` commands **and daemon-side fetches**, such as HTTP/Git `ADD`,
use this outer network boundary. Protecting only `RUN` would leave source fetches
in the host-side Docker daemon outside the boundary.

- Public dependency downloads, image pulls and DNS resolution remain available.
- Build traffic to host-local addresses is dropped before Docker port translation.
  This includes the build gateway and the host's other interface addresses, not
  just host loopback. Caddy's loopback admin listener is not exposed to builds.
- Forwarding from the build bridge to Vamos's application bridges is blocked,
  including direct container IPs and translated published-port paths.
- IPv4 and IPv6 guards are installed and verified before the builder starts.
- The builder has no published API port or mounted Docker socket. The host client
  connects through `docker exec` and BuildKit's stdio transport.
- Neither host-network nor insecure-execution entitlements are enabled. Source
  configuration cannot request them through Vamos.
- Buildx uses explicit private builder metadata and a fixed local Docker endpoint,
  not the administrator's currently selected builder or Docker context. Root's
  existing Docker registry configuration, credential helpers and their environment
  remain available. Credentials are not copied into a new configuration or mounted
  inside the builder; private registries must still be reachable through the guard.

The Docker daemon retains its existing Vamos configuration, including the disabled
shared default bridge. First use does not rewrite daemon configuration or restart
Docker or application containers. Existing installations need no host-network
migration for this build path.

## Ownership and recovery

The first build records private creation intent under
`/var/lib/vamos/buildkit/`, then creates the labeled network, cache volume and
builder container. Subsequent work verifies the captured identities and runtime
configuration. Unknown resources, replacements, changed endpoints, unexpected
attachments and failed inventories are not adopted or interpreted as absence.
Docker-injected proxy environment is captured at initial builder creation, not
silently changed on reuse. Treat the private receipt as sensitive: proxy URLs can
contain credentials. Changing captured builder settings requires planned
infrastructure replacement, not journal edits.

The global host operation lock serializes setup and builds. The builder uses
`restart=no`; each build verifies its guards before startup and attempts to stop
the exact builder afterward, including on ordinary failure or cancellation.
A hard worker kill may leave it running. The next build reconciles that exact
recorded builder before repairing guard state. No uncertain build is silently
converted into a successful deployment.

Cache stays in the builder's dedicated volume between builds. App cleanup and
app destruction do not delete it. Monitor its disk usage separately from captured
source and Docker runtime images. The builder is pinned by image digest; it is
not silently replaced by an upstream tag update.

Host teardown refuses retained builder resources. If retiring the entire host,
inspect the private `state.json` receipt and compare its container ID, network ID,
cache-volume name, nonce labels and volume creation identity with Docker's actual
objects. Explicitly remove only those verified resources, including the cache if
its loss is intended. Do not use broad prune or remove the receipt to hide drift.
After all normal teardown fences pass, an authorized host purge removes only the
exact recorded firewall rules and disposable Buildx metadata. It never deletes
root's Docker configuration or credentials.

`VAMOS_BUILDKIT_SNAPSHOTTER=native` is a privileged **server-worker environment**
setting for hosts that cannot mount nested overlays. Normal initialization uses
`auto`; the selection is captured in the builder receipt. This is not an
application manifest option, and changing a receipt's configuration by hand is
not a supported migration. The isolated integration guest uses the native
snapshotter while exercising the same network boundary.

## Limits and operator responsibilities

This is network hardening, **not a sandbox for hostile Dockerfiles**. The rootful
BuildKit container is privileged infrastructure; the kernel, Docker, BuildKit,
host administrators and application build inputs remain trusted. Builds can
consume host CPU, memory and disk. Cache is shared by builds on the host, not a
hostile-tenant confidentiality boundary.

The forwarding guard covers Vamos's supported Docker bridge topology. It is not
an internet-only egress allowlist: LAN/VPN destinations, other networking drivers,
cloud metadata services and externally reachable application URLs are not all
excluded. An external proxy or relay can provide indirect access beyond a direct
network boundary. Existing root proxy configuration is trusted and remains
available. Registry authentication can execute credential helpers and acquire
tokens in the host client; these authentication requests are not covered by the
builder's bridge firewall. This is not a guarantee that every build-related
client request originates inside the builder.

Coordinate firewall and daemon administration with deployments. Do not flush or
reload away Vamos's guard rules while a builder is running. A pre-start check cannot
protect an already running build from a later privileged firewall change. Docker
live restore is outside the accepted daemon policy; do not enable it to keep the
builder alive across a daemon restart. Root administrators can always bypass or
replace these controls.

Root SSH remains required for the current Vamos deployment protocol. Restricting
routine deployment credentials and replacing client-uploaded privileged executors
are separate work; this change does not claim to implement either.
