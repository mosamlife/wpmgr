# ADR-039 — Heartbeat cadence + connection-timeout thresholds

**Status:** accepted
**Date:** 2026-05-31
**Phase:** 5.7 — Live enrollment + connection lifecycle

## Context

The agent **already heartbeats** — a 5-minute WP-Cron (`wpmgr_agent_heartbeat`,
`apps/agent/includes/class-scheduler.php`) posts a signed `{site_id, ts}` to
`/agent/v1/heartbeat`, which bumps `last_seen_at` + `health_status='healthy'`.
This is an **extension, not a new build**.

Two realities constrain the timings:
- **WP-Cron only fires on site traffic.** A low-traffic site can legitimately go
  minutes between page loads, so an aggressive timeout would mark a *healthy* site
  as degraded/disconnected.
- The feature spec proposed 30s/90s/180s. On wp-cron that **will false-positive**.

## Decision

1. **Agent heartbeat cadence → 60s.** Add a `wpmgr_60sec` cron schedule; the beat
   posts the existing signed payload plus light metadata (status, wp/php version,
   plugin versions, pending-update count) so the dashboard row stays fresh.

2. **CP timeout sweeper (River cron, every 15s) — generous multiples of cadence:**
   - `connected → degraded` after **180s** missed (3× cadence).
   - `degraded → disconnected` after **360s** missed (6× cadence).
   The 3×/6× margins absorb wp-cron's traffic-gated firing without flapping. The
   sweeper is the **only** writer of the degraded/disconnected transitions.

3. **Immediate post-enroll beat.** On a successful enroll the agent fires **one
   heartbeat synchronously** (does not wait for the first 60s tick) so the
   dashboard transitions `pending_enrollment → connected` within ~1s. This is what
   makes the live-enrollment modal feel instant.

4. **Recovery.** The heartbeat handler resets `last_seen_at` and is the **only**
   thing that transitions `degraded`/`disconnected → connected`. Every recovery is
   recorded in `site_connection_history` (see ADR-041).

5. **No-cron hosts.** Document the system-cron alternative
   (`* * * * * curl -s 'https://site/wp-cron.php?doing_wp_cron'`) for hosts that
   disable WP-Cron or have no traffic; without it, such a site will read
   `disconnected` despite being healthy — a documented, accepted limitation.

## Consequences

- Detection drops from ~5 min (today) to ≤180s/≤360s **without** false-flagging
  low-traffic sites.
- Heartbeat traffic rises 5×（300s→60s); payload is a few hundred bytes, signed,
  and the handler is a single indexed `UPDATE` — negligible.
- River sweep every 15s runs two indexed range scans
  (`idx_sites_last_seen` partial on connected/degraded) — cheap.

## Alternatives considered

- **30s/90s/180s (spec original)** — rejected: false-positives on low-traffic
  wp-cron sites, which would erode trust in the badge.
- **CP→agent "beat now" push** — deferred: the `agentcmd` Ed25519-JWT command
  channel already exists and could force an on-demand fresh beat (e.g. when the
  operator opens a site). A good v2; not needed for the lifecycle to work.
- **Real-time transport (WebSocket ping)** — rejected: WP can't hold a persistent
  outbound socket from PHP; cron/HTTP is the only portable mechanism.
