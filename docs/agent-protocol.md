# Agent RPC and Handshake Protocol

**Status: Phase 1 protocol specification implemented in `internal/agentproto`; `soar-agent` daemon scheduled for Phase 2.**

## Transport and framing

- Requests and responses use strict single-document newline-delimited JSON frames bounded to `64 KiB` (`MaxEnvelopeBytes`) for control metadata.
- Every frame decoder sets `DisallowUnknownFields()` and rejects trailing JSON documents.
- Large inputs (build context archives, private secret payloads, repository credentials) remain bounded out-of-band stream payloads alongside structured metadata—never inlined into `RPCRequest` JSON.

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
   - Agent older than client `api_min` or missing a required capability: fail before mutation and instruct `soar server upgrade`.

## Durable queueing and idempotency

- `submit` carries a 32–64 hex `idempotency_key` alongside a validated `deploy.Request`.
- Retrying `submit` with the same `idempotency_key` returns the existing `OperationRecord` (`deduplicated: true`) rather than creating a duplicate operation.
- Allowed lifecycle transitions:
  - `queued` → `running` | `cancelled`
  - `running` → `succeeded` | `failed` | `uncertain`
- `uncertain`, `succeeded`, `failed`, and `cancelled` are terminal for automatic scheduling; `uncertain` operations remain fenced on that app until explicit `resume` or `supersede`.

## Concurrency policy and ACLs

- Default host concurrency: `max_queue_depth=32`, `max_staged_bytes=512 MiB`, `max_concurrent_builds=1`, `max_concurrent_deploys=1`.
- `admin` principals may execute host-administration actions (`server-add`, `server-upgrade`, `server-configure`, `server-teardown`, `acl-manage`).
- `deployer` principals are confined to explicitly allowed apps and actions and cannot list all host apps (`apps`) without explicit wildcard scope.
