# ADR-040 — Agent-side last-will (disconnect) mechanism

**Status:** accepted
**Date:** 2026-05-31
**Phase:** 5.7 — Live enrollment + connection lifecycle

## Context

When a user removes the agent from a WordPress site, the control plane should
learn quickly. Two removal paths exist with different guarantees:
- **Deactivate** (and the deactivate step of an update/reinstall) — fires
  `register_deactivation_hook`; the agent is still alive and can make one
  outbound request.
- **Uninstall / hard file delete** — `register_uninstall_hook` *may* fire (only on
  a clean WP "Delete" flow); a manual `rm -rf` of the plugin fires nothing.

There is currently **no agent→CP disconnect signal**; removal is invisible until
the (old, 5-min) health sweep eventually marks the site unreachable.

## Decision

1. **Deactivation last-will.** `register_deactivation_hook` posts a **signed**
   (Ed25519 signed-request — the same scheme used for heartbeat, *not* an
   unauthenticated call) `POST /agent/v1/disconnect` with
   `{reason: "deactivated"|"uninstalled"|"user_initiated"}`, a **3-second
   timeout**, **best-effort** (failure is swallowed), and then clears the
   heartbeat cron.

2. **Uninstall last-will.** `register_uninstall_hook` posts the same disconnect
   with `reason:"uninstalled"` (best-effort), then wipes the keystore and drops
   the agent's tables/options.

3. **Signature required — anti-spoof.** The disconnect endpoint verifies the
   agent's signature over the canonical request and binds it to the calling
   site. **Possession of a `site_id` alone cannot disconnect a site** — only the
   holder of that site's agent private key can. This closes a trivial DoS where
   one tenant could disconnect another's site by id.

4. **Timeout fallback is the safety net.** If no last-will arrives (hard delete,
   dead host, or a network blip during deactivate), the heartbeat-timeout sweeper
   (ADR-039) marks the site `disconnected` within ≤360s. The last-will is a
   *latency optimisation* for the common, clean removal — never the sole path.

5. **CP handling.** A valid disconnect transitions the site to `disconnected`
   with `disconnected_reason` set, writes `site_connection_history` + a
   `site.disconnected` audit action, and publishes a `site.state_changed` SSE
   event (ADR-038). It does **not** archive — archive stays an explicit operator
   action so history is never silently hidden.

## Consequences

- Clean deactivate/uninstall → dashboard shows `disconnected` in seconds.
- Hard delete / dead host → `disconnected` in ≤360s via timeout — graceful
  degradation, no lost state.
- The 3s best-effort cap means a deactivate never hangs the WP admin on a slow
  network; it just falls through to the timeout path.

## Alternatives considered

- **Unauthenticated disconnect by site_id** — rejected: cross-tenant disconnect
  DoS.
- **Timeout-only (no last-will)** — rejected: the common deactivate case would
  take up to 6 minutes to reflect, which feels broken.
- **WP `shutdown`/`wp_loaded` hooks** — rejected: unreliable timing, fire on every
  request, would generate spurious disconnects.

## Addendum (2026-05-31) — Signed revoke instruction (Phase 6 security review)

The Phase-6 security review found that **dashboard-initiated revoke** (the
CP→agent direction) had two coupled problems:

- **Finding C — the revoke instruction was unreachable.** `MarkSiteRevoked` nulled
  `agent_public_key`, but agent auth resolves the site *by* that key, so the
  agent's next heartbeat 401'd and never received the `["revoke"]` instruction —
  the agent kept running locally and never tore down. **Fix:** revoke no longer
  nulls the key (a re-enroll overwrites it on the same row, so there's no
  unique-index collision); the agent can authenticate its heartbeat and receive
  the instruction.
- **Finding B — the instruction was unauthenticated (TLS-only).** Acting on a
  `revoke` from the plain heartbeat *response body* let a MITM with a trusted
  root CA on the WP host force destructive self-deactivation. **Fix:** the CP now
  returns a **signed revoke token** alongside the instruction — a short-lived
  Ed25519 JWT minted by the **existing agentcmd signer** (ADR-031; `cmd="revoke"`,
  `aud=<site_id>`, `exp` = the standard command TTL, single-use `jti`). The agent
  MUST verify it with the stored control-plane public key (signature + `exp` +
  `aud == own site_id` + `cmd == "revoke"` + `jti` freshness, via the existing
  `Connector::verify`) **before** wiping keys + self-deactivating. An absent or
  invalid token → the agent ignores the instruction (no teardown). This reuses an
  ADR-backed mechanism — no new crypto primitive.

**Residual (accepted):** between revoke and the agent acting on the next beat
(≤60s) the agent's key is still valid, so the agent could still make other signed
calls in that window. Bounded + small; the agent self-deactivates on the next
heartbeat. A future tightening could reject non-heartbeat agent calls for a
`revoked` site. **Follow-up:** finding D (per-principal SSE stream cap) is
implemented; the best-effort post-commit audit write (finding #9 nit) should log
failures loudly so a chain gap is detectable.
