# ADR-041 — Re-enrollment identity + connection-state model

**Status:** accepted
**Date:** 2026-05-31
**Phase:** 5.7 — Live enrollment + connection lifecycle

## Context

`sites` today carries two **free-text columns with no CHECK constraint**:
`status` (`pending`/`active`) and `health_status` (`unknown`/`healthy`/
`unreachable`), plus `enrolled_at`/`last_seen_at`. There is **no
`connection_state`, no soft-delete, no generation counter, and no transition
history.** Re-enrolling the same URL today silently rotates the agent key and
resets to active — the operator never learns it happened. `agent_public_key`
carries a **unique index**, so a naive re-enroll can collide.

## Decision

### 1. `connection_state` is the new single source of truth
Add (additive migration — no rewrite of existing rows):
- `connection_state TEXT NOT NULL DEFAULT 'pending_enrollment'` with a **CHECK**
  in `{pending_enrollment, connected, degraded, disconnected, revoked,
  archived}`.
- `connection_generation INT NOT NULL DEFAULT 0`.
- `disconnected_at TIMESTAMPTZ`, `disconnected_reason TEXT`, `archived_at
  TIMESTAMPTZ`. (`last_seen_at` already exists.)

The legacy `status`/`health_status` columns **keep being written** (derived from
`connection_state`) for backward-compat with anything still reading them; **all
new UI reads only `connection_state`.** Backfill on migrate: `active → connected`,
`pending → pending_enrollment`. No risky free-text migration.

The **state machine is enforced in Go** (`internal/site/service/connection.go`),
not just by the CHECK: every transition validates its source state, writes the new
state + a `site_connection_history` row + a hash-chained audit action in **one
transaction**, and publishes the SSE event **after commit**.

### 2. Re-enrollment reuses `site_id`
Re-enrolling a previously-connected site **keeps the same `site_id`** (preserving
URL identity, backup history, and all related rows), **increments
`connection_generation`**, and archives the prior generation's key reference in
`site_connection_history`. `BeginReEnrollment` mints a fresh enrollment code bound
to the existing `site_id` and moves the row back to `pending_enrollment`. The
prior generation's window (e.g. "Generation 1 · 2026-01 → 2026-04") is renderable
from history.

### 3. Revoke / archive / restore semantics
- **Revoke** (operator action): transition → `revoked`; queue a `revoke`
  instruction returned on the agent's next heartbeat (agent verifies a signed
  revoke token, then wipes keys + self-deactivates per ADR-040). **NOTE
  (superseded by ADR-040 addendum / Phase 6 finding C):** revoke does NOT null
  `agent_public_key` — the agent must keep a valid key to authenticate the very
  heartbeat that delivers the revoke. Re-enroll overwrites the key on the same
  row (the unique index is partial, `WHERE agent_public_key <> ''`), so there is
  no collision.
- **Archive**: terminal soft-delete (`archived_at` set); hidden from the default
  sites list (default filter `connection_state != 'archived'`); reachable via a
  `state:archived` filter chip.
- **Restore**: un-archive back to its prior non-terminal state (or
  `disconnected`).

### 4. Every transition is auditable
`site_connection_history (from_state, to_state, reason, actor_user_id,
occurred_at, metadata)` + a hash-chained audit action per transition:
`site.connected`, `site.degraded`, `site.disconnected`, `site.revoked`,
`site.archived`, `site.restored`, `site.reenrolled`.

## Consequences

- Full lifecycle history threads across re-enrollment generations under one
  stable `site_id`.
- Additive migration → **zero data-migration risk**; legacy columns become
  secondary/derived.
- The unique `agent_public_key` index is respected across revoke→re-enroll.
- Sweeper (ADR-039) and last-will (ADR-040) are just two of the transition
  callers; the service layer is the single chokepoint.

## Alternatives considered

- **Mint a new `site_id` on re-enroll** — rejected: severs backup/scan/uptime
  history; the spec explicitly wants history to thread back.
- **Migrate `status`/`health_status` in place into one column** — rejected:
  free-text values + external readers make this risky for no benefit; additive is
  safer.
- **Reuse `health_status` as the lifecycle column** — rejected: "health"
  (reachability of a connected agent) and "connection" (is there an enrolled
  agent at all) are distinct axes; conflating them loses the `degraded` vs
  `disconnected` vs `revoked` distinctions.
