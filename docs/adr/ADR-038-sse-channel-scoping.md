# ADR-038 — SSE channel scoping + cross-instance fan-out

**Status:** accepted
**Date:** 2026-05-31
**Phase:** 5.7 — Live enrollment + connection lifecycle
**Supersedes:** nothing (extends the per-resource Hub pattern in `internal/backup/hub.go`, `internal/update/hub.go`)

## Context

The existing SSE infrastructure is **two independent in-process `Hub` structs** (a
`sync.Mutex` + a map of buffered channels) keyed on a **resource id** the client
already holds (`snapshot_id`, `run_id`). There is **no cross-process fan-out, no
event ids, and no replay** (`?since`/`Last-Event-ID`). The control plane runs on
Cloud Run with **potentially many instances**, so an event produced on instance A
never reaches an `EventSource` pinned to instance B.

Live enrollment breaks both assumptions:
- At "Add site" time the client holds only a pairing code — **the `site_id` may
  not exist yet** — so it cannot subscribe to a per-resource channel.
- The enroll event is produced by the public `POST /enroll` handler, which may
  land on a different instance than the one holding the operator's SSE stream.

## Decision

1. **Shared bus = Postgres `LISTEN/NOTIFY`.** A new `site_events` table is the
   durable record; on insert (inside the same tx as the state transition, via a
   trigger or an explicit `pg_notify` after commit) the producer emits
   `NOTIFY wpmgr_site_events, '<tenant_id>:<event_id>'` (ids only — NOTIFY's 8 KB
   payload cap means we never ship the body on the wire). Every API instance runs
   **one dedicated `LISTEN` connection**; on a notification it loads the event row
   and fans it out to its locally-connected SSE subscribers for that tenant.
   - **Postgres over Redis:** Redis (Memorystore) is currently disabled in our
     stack (TLS/auth follow-up pending); Postgres LISTEN/NOTIFY needs **no new
     infrastructure** and is more than sufficient for site-lifecycle event volume
     (a handful of events per site per minute, not a firehose).

2. **Tenant-scoped channel, client-side site filter.** Clients open a single
   `GET /api/v1/sites/events` stream scoped to their active tenant (`tenant:<id>`)
   and filter by `site_id` in the browser. The enrollment modal subscribes
   *before* a site exists and simply watches for the first `site.state_changed`
   carrying its own `site_id`.

3. **Event envelope + replay.**
   ```json
   { "id": "<ULID>", "type": "site.state_changed", "tenant_id": "...",
     "site_id": "...", "ts": "...", "data": { "from": "...", "to": "...",
     "site": { /* full row */ } } }
   ```
   `id` is a **ULID** (monotonic per tenant), persisted in `site_events`
   (ring-buffered, ~5 min retention via a periodic prune). On (re)connect the
   client sends `?since=<last-id>` (or `Last-Event-ID`); the server replays missed
   rows from `site_events`, then attaches the live stream.

4. **Delivery contract.** At-least-once **within** the 5-minute replay window;
   best-effort beyond it. As a backstop, on every (re)connect the client also
   invalidates `["sites","list"]` (reconcile-on-connect) so a gap longer than the
   window self-heals on the next render.

## Consequences

- One extra long-lived `LISTEN` connection per API instance (cheap).
- New `site_events` table + a prune job (River cron). RLS-scoped by tenant.
- The two existing resource-scoped hubs (backup/update progress) are **left as-is**
  — only the new `site.*` events ride the shared bus. A later cleanup could fold
  them in, but that is out of scope here.
- SSE holds a connection open; fine at current scale. Cloud Run request timeout
  must exceed the stream lifetime (already configured high for existing SSE).

## Alternatives considered

- **Redis pub/sub** — rejected: disabled infra; would re-open the Memorystore
  TLS/auth work first.
- **In-process only** — rejected: silently broken across instances (the actual
  root cause of "I have to refresh").
- **WebSockets** — rejected: SSE is already in the stack and the flow is strictly
  server→client; bidirectional adds no value here.
- **Full event-sourcing / durable queue** — rejected: over-built for lifecycle
  notifications; the 5-min replay window + reconcile-on-connect is enough.
