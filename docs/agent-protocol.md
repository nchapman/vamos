# Agent RPC and Handshake Protocol

**Status: implemented — `internal/agentproto` frames, `internal/agent` daemon, `internal/agentclient`, and the restricted SSH bridge in `internal/ssh`.**

## Transport and framing

- Requests and responses use strict single-document newline-delimited JSON frames bounded to `128 KiB` (`MaxEnvelopeBytes`) so a `64 KiB` executor response fits inside an outer `OperationRecord` / `RPCResponse` envelope.
- Every frame decoder sets `DisallowUnknownFields()` and rejects trailing JSON documents.
- Large inputs (build context archives, private secret payloads, repository credentials) remain bounded out-of-band stream payloads alongside structured metadata—never inlined into `RPCRequest` JSON. A `submit` declares `payload_bytes` and `payload_sha256`; after the accepted response the client writes exactly those bytes before the next frame. The agent stores them mode `0600`, verifies the digest, and fences an incomplete transfer as `uncertain`.

## Handshake (`hello`)

Every client connection begins with `hello` before submitting mutations:

1. Client sends `HelloRequest` with `api_min`, `api_max`, `client_version`, and `client_build_id`.
2. Agent responds with `HelloResponse` containing:
   - `agent_version` and `agent_build_id` (from `internal/buildmeta`)
   - `api_min..api_max`
   - `capabilities` (`app-deploy`, `durable-queue`, `per-app-concurrency`, `scoped-acl`, `secrets`, `backups`, `sessions`)
   - `host_id`, `host_name`, `domain`
   - `concurrency` (`max_queue_depth`, `max_staged_bytes`, `max_concurrent_builds`, `max_concurrent_deploys`)
3. Compatibility check:
   - Client older than agent `api_min`: fail before sending payloads and instruct upgrading the local CLI.
   - Agent older than client `api_min` or missing a required capability: fail before mutation and instruct `vamos server upgrade`.

## Methods

- `hello` — capability handshake; required first.
- `submit` — mutating operations only. Persists admission, accepts the declared payload stream, and returns a reconnectable operation ID. Read-only actions are refused.
- `read` — authorized non-mutating requests (status, logs, previews, listings) executed directly without queue records, so observation never fences teardown.
- `status` / `wait` — reattach to a recorded operation; `wait` blocks server-side until a terminal phase.
- `session` — one interactive exec or followed log stream. After the validated preparation response the connection switches to typed length-prefixed frames (`0x01` stdout, `0x04` stderr, `0x05` client stdin, `0x03` stdin EOF, `0x06` TTY window resize); the terminal exit status is a distinct `0x02` control frame, so workload output cannot forge it. Interactive TTY sessions allocate a host PTY and forward initial window size and `SIGWINCH` resizes over `0x06`. A session consumes the connection.

## Durable queueing and idempotency

- `submit` carries a 32–64 hex `idempotency_key` alongside a validated `deploy.Request` and, when needed, the size and SHA-256 of its following out-of-band payload.
- Retrying `submit` with the same `idempotency_key` returns the existing `OperationRecord` (`deduplicated: true`) rather than creating a duplicate operation.
- Allowed lifecycle transitions:
  - `queued` → `running` | `cancelled`
  - `running` → `succeeded` | `failed` | `uncertain`
- `uncertain`, `succeeded`, `failed`, and `cancelled` are terminal for automatic scheduling; `uncertain` operations remain fenced on that app until explicit `resume` or `supersede`.

## Concurrency policy and ACLs

- Default host concurrency is stored in root-owned `/etc/vamos/agent.json`: `max_queue_depth=32`, `max_staged_bytes=512 MiB`, `max_concurrent_builds=1`, `max_concurrent_deploys=1`. The coordinator enforces deploy/build semaphores, one mutation per app, and a short ingress transaction lock; restart the agent after an administrator changes this validated file.
- `admin` principals may execute host-administration actions (`server-add`, `server-upgrade`, `server-configure`, `server-teardown`, `acl-manage`).
- `deployer` principals are confined to explicitly allowed apps and actions and cannot list all host apps (`apps`) without explicit wildcard scope.
- Scoped CI access is mapped by root-owned `/etc/vamos/authorized_keys`: every nonempty key must use `restrict,command="/usr/local/lib/vamos/executor internal-agent bridge --principal <id>"` (legacy `vamos-agent` path also accepted) and name a principal in `/etc/vamos/principals.json`. The daemon refuses unsafe or unmatched mappings at startup, so the client cannot choose its own principal.
- `vamos server add` installs the operator's own key (SSH agent, `~/.ssh`, or `--deploy-key PATH`) as the initial `default-deployer` bridge key with wildcard app scope. Ordinary CLI app traffic (`deploy`, `status`, `logs`, `run`, `rollback`, `resume`, `cleanup`, `destroy`, `service`, `secrets`, `backup`, `apps`, `exec`, `console`) connects as `vamos@host` through this bridge; only host administration (`server add/upgrade/teardown`) still uses root SSH. An explicit `--executor` override is rejected on the agent path; recover version skew with `vamos server upgrade`.
