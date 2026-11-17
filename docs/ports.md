# Published TCP and UDP service ports

Use `ports` for protocols that should go directly to a container rather than
through Caddy:

```yaml
app: gateway
services:
  gateway:
    image: example/gateway:1.0
    ports:
      - "9000:8080"             # TCP by default
      - "3478:3478/udp"
      - "49160-49200:49160-49200/udp"
    http: {port: 8080, health: /healthz} # optional; HTTP and ports can coexist
```

Each quoted string is `HOST:CONTAINER[/tcp|udp]`. Both sides may be inclusive,
ascending ranges of **equal size**. Ports must be **1–65535**. Only TCP and UDP
are supported. There is no host-IP syntax, automatic host-port assignment, host
networking option, or per-application firewall rule configuration.

Vamos passes these bindings to Docker as `--publish 0.0.0.0:…` on the existing
app bridge network. **These are public IPv4 bindings on every host interface,
not loopback or IPv6 bindings.** They bypass Caddy: no automatic HTTPS, WAF,
cache, HTTP routing, or authentication protects them. Configure authentication
and encryption in the service itself. If a container port is also served by
`http`, its direct binding still bypasses those HTTP protections. An image's
`EXPOSE` alone does not publish a port.

You must deliberately allow the required protocols/ports in your cloud/network
firewall. Docker manages its own publish/NAT rules; do not assume a host firewall
front end such as UFW blocks published container ports. The optional
[managed host firewall](firewall.md) automatically allows intentional Docker public
publishing; it does not make these bindings private. Without that opt-in policy,
Vamos does not change host firewall policy for published ports.

## Validation and ownership

- Services with `ports` require **one replica** (omission or `replicas: 1`).
- At most **4096 published bindings per service**. Ranges count individually;
  TCP and UDP bindings count separately. This bounds Docker's per-port work.
- Duplicate and overlapping host bindings are rejected per protocol, including
  across services. TCP and UDP may use the same host number.
- TCP **80, 443, 2019** and UDP **443** are reserved for Vamos's HTTP ingress,
  loopback Caddy admin API, and HTTP/3 respectively. A range cannot include them.
- Deploy preflight checks other tracked apps, pending desired bindings, current
  and in-flight releases, and Vamos's allocated loopback HTTP upstream ports.
  A wildcard binding conflicts with a loopback binding too. Claims are published
  under the existing host-wide ingress lock before runtime changes.
- A service may replace its own bindings. To transfer a binding to another
  service/app, first deploy its removal from the original service (or explicitly
  remove/destroy that service/app). An all-at-once port swap is rejected while
  the old owners still hold the bindings. Retained historical snapshots alone
  do not reserve ports; a later rollback must pass conflict checks again.

Docker, not Vamos, detects conflicts with unmanaged containers and host
processes. A socket probe would not reserve a port, so there is no speculative
probe or automatic eviction of another process.

## Updates, health, and recovery

Fixed-port replacement uses the existing stop-first lifecycle:

1. Build/pull and validate captured inputs and tracked bindings.
2. Create the candidate **stopped**, with restart disabled; inspect its exact
   bindings, including Docker's expanded ranges.
3. If `http` is present, withdraw HTTP routes with durable 503 responses.
4. Disable restart on the old container, stop it, and prove it stopped.
5. Enable restart on the candidate and start it. With `http`, run the usual HTTP
   readiness and route activation. Without `http`, use the usual process/startup
   observation and service-name DNS, not a TCP/UDP protocol health probe.

Updates have downtime, including removal of ports. Unchanged services do not
restart. Direct TCP/UDP bindings become reachable when Docker starts the
candidate, **before HTTP readiness or Caddy activation**. Existing connections,
TURN allocations, and other protocol sessions are not migrated. Status labels
services without HTTP but with direct bindings as `ports`, rather than `private`.

Docker may discover an unmanaged bind conflict only at step 5, after the old
container has stopped. Errors do not expose Docker output or environment values.
The operation remains resumable, the old container stays retired, and any HTTP
route stays unavailable. Resolve the external conflict and run `vamos resume
OPERATION_ID`; or use the existing explicit supersede/rollback workflow. Vamos
does not automatically restart the old container or steal a binding.

Rollback restores captured ports/configuration and uses the same stop-first
path. Its preview reports downtime. Normal manual-service and volume rollback
restrictions still apply. Cleanup removes unused retired containers; service
removal/destroy retire and remove owned containers and release their claims.
One-off `run` and release commands do **not** inherit published ports.

## Inspecting published ports

`vamos status --server NAME --app APP` lists **current** and **candidate**
published mappings separately, including on services that also have HTTP.
These come from captured releases on the server, not the local manifest. The
existing `port` field is the loopback HTTP upstream port, not a public binding.

JSON status adds optional `published_ports` and `candidate_published_ports`
objects to each service:

```json
{
  "published_ports": {
    "mappings": ["3478:3478/udp", "49160-49200:49160-49200/udp"],
    "total": 2,
    "observed": true
  }
}
```

- Mappings retain `HOST:CONTAINER[/protocol]` syntax and compact ranges; an
  omitted protocol means TCP. The recorded bind address is always `0.0.0.0`.
- `observed: true` means an owned running container's effective Docker bindings
  matched its captured configuration at inspection time. It does **not** verify
  cloud/host firewall reachability, application protocol health, or authentication.
- Stopped, missing, restarting, detached, or unverifiable runtimes retain their
  recorded mappings with `observed: false`. False does not prove ports are closed.
- Absent objects mean no direct-port mappings are recorded for that release (or
  an older server does not provide this metadata); never infer that the host has
  no other listeners. A candidate is not presented as the current release.
- Each object lists up to 32 sorted mapping entries; `total` is the exact count
  before truncation. A range counts as one entry here, unlike the expanded-binding
  validation limit. Human output explicitly identifies truncated lists.

## Coturn example

Keep relay ranges small enough for your capacity needs: Docker expands each
published range into individual bindings, so large ranges can be expensive.
Use identical host/container relay numbers for coturn's advertised allocations.
This example supports authenticated TURN over TCP/UDP 3478 and UDP media relay;
it deliberately does not configure TLS/DTLS or TCP relay allocations.

```yaml
app: turn
services:
  turn:
    image: coturn/coturn:4.19.1
    ports:
      - "3478:3478"
      - "3478:3478/udp"
      - "49160-49200:49160-49200/udp"
    # The upstream image declares this VOLUME; Vamos requires a matching mount.
    volume: turn-data:/var/lib/coturn
    secrets:
      TURN_SHARED_SECRET: turn-shared-secret
    command:
      - "-n"
      - "--listening-port=3478"
      - "--external-ip=203.0.113.10" # replace with the server's public IPv4
      - "--realm=turn.example.com" # replace with your realm
      - "--min-port=49160"
      - "--max-port=49200"
      - "--use-auth-secret"
      - "--static-auth-secret=$TURN_SHARED_SECRET"
      - "--userdb=/tmp/turnserver.db"
      - "--no-tls"
      - "--no-dtls"
      - "--no-tcp-relay"
      - "--no-cli"
      - "--no-multicast-peers"
      - "--log-file=stdout"
```

Stage `turn-shared-secret` with `vamos secrets stage` before deployment; use a
strong random secret and generate short-lived TURN credentials in your trusted
application backend. The **coturn image's entrypoint** expands the environment
reference in the command; Vamos itself does not expand command arguments. Pin a
vetted image digest for production.

This shared-secret example puts the incidental SQLite user database in `/tmp`
(no durable user accounts), avoiding ownership changes to a new root-owned
volume when the upstream image runs as `nobody`. The declared image volume is
still retained and follows [volume lifecycle/rollback rules](volumes.md). For
durable coturn user storage, build an image that initializes the named mount's
ownership appropriately; Vamos does not chown application data.

Point your TURN DNS name at the server, configure its actual external address
and any upstream NAT correctly, and permit TCP/UDP 3478 plus UDP 49160–49200 in
your network firewall. Validate allocation and media relay from an external
client; process observation alone cannot prove TURN credentials, NAT, or relay
reachability. TLS certificates and TURN-specific security/capacity policy remain
application responsibilities.
