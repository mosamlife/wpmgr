# GH #291: uptime probe cannot see past a page cache

**Date:** 2026-07-27
**Status:** Design locked, ready to build. Phased, each phase independently shippable.
**Method:** 3-agent workflow (codebase audit + field research with citations + design synthesis).

---

## 0. The headline: we already had the answer and threw it away

During the reporter's outage the control plane **knew the site was broken** and rendered it as a clean green "Up".

A fatal-on-every-request outage means the agent's WP-Cron heartbeat cannot run, so `sites.last_seen_at` goes stale. `Sweeper.Sweep` hits the 900s `disconnectAfter`, sends a **signed, uncacheable** POST to `/wp-json/wpmgr/v1/command/ping`, gets a 5xx, and marks the site `connection_state='disconnected'`. The site was almost certainly disconnected for most of those hours.

Then `deriveFleetStatus` (`apps/api/internal/uptime/repo.go:361-376`) special-cases **only** the literal string `"degraded"`. `disconnected` falls through to `return FleetStatusUp`.

That is a roughly 3-line bug in existing code. Fixing it would have caught this exact incident **with zero new HTTP requests**.

A second existing asset was also already in place: `CronKicker` (`internal/uptime/cron_kick.go:127-137`) already makes a PHP-booting request to `/wp-cron.php` every 5 minutes for every site, and its own doc comment states the premise of this bug ("a fully page-cached site never boots PHP for organic traffic"). It records nothing.

---

## 1. What "up" means: decided

**`up` keeps its exact current meaning forever.** A cached 200 is `up`, and that is the honest answer: visitors are being served. We add a **second orthogonal signal**. No existing number moves.

| Signal | Question | Owns |
|---|---|---|
| `up` (reachability) | Did a visitor's request succeed? | `site_uptime_probes.up`, `sites.health_status`, `site_incidents`, uptime %, SLA, client portal, white-label PDFs. **FROZEN.** |
| `app_up` (application health) | Did PHP and WordPress actually execute? | The new operator state and the new alert kind. Three-valued: true / false / **unknown**. |

**No new enum value.** The scenario maps onto the existing `degraded` value of `UptimeStatusKind`. API enum, counters and every FE consumer keep their shape; only the label and reason are new.

The operator-facing copy is the product:

> "Visitors are being served (HTTP 200), but WordPress is not responding. The agent has been unreachable since 14:07 and /wp-json/ returned HTTP 500. Cached pages will keep working until the cache expires; wp-admin, logins, forms and checkout are likely already broken."

Honest in both directions: it does not say the site is down, and it does not say it is fine. Where app health is **unknown** (`cache_bypass_defeated`, `edge_blocked`, `rest_disabled`) the chip stays green and a neutral marker appears. **Unknown is never dressed up as either healthy or broken.**

---

## 2. Verdict on the reporter's four suggestions

**1. Cache-busting query param: ADAPT, demote from mechanism to belt-and-braces, never on the primary probe.**

It works on nginx FastCGI cache, on the common mod_rewrite-based page caches, and on Cloudflare APO, all of which skip the cache when a query string is present. It is defeated by **one click** on Cloudflare "Ignore Query String", by Varnish configured to ignore query strings, and by any nginx keyed on `$uri` rather than `$request_uri`. A live test against one managed host showed it **stores** the busted response, so each probe mints a new cache object there.

Two hard costs rule it out on the 60s primary probe:
- 1,440 uncached full theme renders per site per day, exactly the traffic the cache exists to eliminate, on hosts often metered by PHP worker or CPU-second.
- It would silently redefine `dns_ms/connect_ms/tls_ms/ttfb_ms/total_ms` from "what a visitor experiences" to "worst-case cold render", corrupting the fleet perf dashboard, the `slowThresholdMs` degraded classification, and white-label client PDFs, with no migration and no explanation.

Param name `wpmgr_hc`, deliberately not `utm`-adjacent: WP Engine strips those server-side before PHP sees them, so a buster named `utm_probe` would be silently neutralised.

**Adopt the corollary, which is worth more than the buster:** you can *detect* that your bypass was ignored, via `cf-cache-status`, `x-litespeed-cache`, `x-kinsta-cache`, `x-proxy-cache`, `x-cache`, or `Age > 0`. That is what turns a silent false-healthy into an honest `unknown`.

**2. Probe `/wp-json/`: ADOPT as the app probe, with fallbacks.** Not as a replacement for `/`.

**3. Per-site health path: ADOPT as an override**, surfaced when the probe reports `unknown`.

**4. Agent cross-check: ADOPT, and promote it to first-class.** It is the cheapest and most reliable signal we have, and it is the one thing a pure uptime monitor cannot do.

---

## 3. Probe strategy

**Probe A (reachability) is FROZEN**, bit for bit unchanged. The one change is adding test assertions that the requested path is exactly `/` with an empty query, because **none of the 26 existing probe tests assert the requested path**. That gap must close in the same PR that introduces a second probe, or the two will drift.

**Probe B (application health), new**, default 300s (not 60s), timeout 10s:

- **B0, agent ground truth:** if `sites.last_seen_at` is fresher than the interval, a heartbeat already proves PHP booted. Record `app_up=true, kind='agent'` and make **zero network requests**. On a healthy fleet this is the common case, so steady-state added traffic is near zero. The HTTP probe only fires when the heartbeat is already suspect, which is exactly when you want it.
- **B1:** `GET <site>/wp-json/?wpmgr_hc=<random>`, bounded 16 KB read.
- **B2 fallback** (404 or non-JSON 200): `GET <site>/?rest_route=/&wpmgr_hc=<random>`, WP core's permalinks-off REST entrypoint. Skipping it would misclassify every plain-permalink install.
- **B3 override:** per-site `app_probe_path`.

**4xx is not app-down.** A 401 `rest_not_logged_in` is extremely common (security plugins, including WPMgr's own suite) and 404 is normal on REST-disabled installs. Treating those as down would produce a fleet-wide false-alarm storm.

---

## 4. The sibling bug is more dangerous than the reported one

`update.runApply` (`apps/api/internal/update/worker.go:212-243`) probes the site root after an update and, on a cached site, reads the **pre-update** HTML. A plugin update that fatals PHP therefore **passes its health check, is not rolled back**, and the operator is told "updated and healthy". `probeHealthWithRetry`'s ~21s retry schedule makes it strictly worse: it waits longer, giving the cache more time to serve a warm copy.

**Fix, in priority order:**
1. **Agent-ping-first.** The CP has *just* spoken to the agent over a signed POST. A signed `VerifyReachable` is a guaranteed-uncacheable, PHP-booting check that **already exists** and is already used by the connection sweeper. Call it first, before the public GET. A cache cannot fake a signed POST response, because the route does not exist unless PHP booted and the plugin loaded.
2. Fix `ProbeResult.Healthy()` (`client.go:802`), where 401/403/404/410 currently count as healthy, but **do not** make 4xx mean rollback.
3. Cache-buster on the post-update GET.
4. Share the extracted `scanFatal` and cache-detection helpers.

---

## 5. Rollout: measure first, alert later

The new probe **will** immediately find real, previously-invisible breakage across the whole fleet. If that arrives as pages, operators disable the feature within an hour and we have shipped nothing.

- `app_probe_enabled` = **true** (collect and display from day one).
- `app_alerts_enabled` = **false** on any deployment that already has sites. Nobody's phone rings on upgrade.
- **Fresh installs get alerting on**, decided deterministically *in the migration*, not by a runtime guess:
  `INSERT INTO instance_settings ... SELECT 'uptime.app_alerts_default', (count(*) = 0)::text FROM sites`
- Threshold 5 (~25 min), requires `ever_app_up = true` so a never-healthy site can never fire, plus a fleet circuit-breaker collapsing >25% simultaneous app-down into one aggregate alert.
- **Existing sites' behaviour on upgrade is bit-identical**, enforced by a golden test written before the feature: a site serving cached 200s with a dead backend must still report `up=true`, unchanged `uptime_pct_30d`, unchanged `health_status`.

---

## 6. Phases

| Phase | Ships | Why |
|---|---|---|
| **0** | ClickHouse hardening: idempotent `ADD COLUMN IF NOT EXISTS` in `ensureSchema`, column-explicit `InsertChecks`. | **Fixes a live P0.** `ensureSchema` has no ALTER path and `CREATE TABLE IF NOT EXISTS` is a no-op on an existing table, so appending new values later breaks inserts outright. CORRECTION (verified during the Phase 0 build): the earlier claim that `tls_issuer`/`tls_subject` are "already drifted" in ClickHouse is wrong. Those two columns exist and are written only in the **Postgres** implementation; they were never declared in the ClickHouse DDL, so there is nothing to wire through. Phase 0 stands on the missing ALTER path alone, which is sufficient. |
| **1** | Fix `deriveFleetStatus` to stop rendering `disconnected` as green Up; typed reasons from `VerifyReachable`. | **Zero new HTTP requests, no metrics schema change, and it would have caught this incident.** Highest value per line in the whole plan. **AS BUILT:** `revoked`/`archived` were deliberately left at today's behaviour rather than given a distinct state, because the only way to express one is a 5th enum value, which this design forbids. Follow-up, not a gap. The disconnected check is gated on a **whitelist** of the two CP-authored sweeper reasons (`agent_unreachable`, `heartbeat_timeout`), so a signed agent last-will (deactivate or uninstall, where the reason string is agent-supplied and not enum-validated) can never raise a false Degraded. |
| **2** | m107 additive nullable columns, the app prober (B0-B3), two-signal recording. Alerting still off. | The actual new capability. |
| **3** | App-health alerting, per-site override, fleet circuit-breaker. | Opt-in, conservative. |
| **4** | The post-update rollback probe fix (agent-ping-first). | Independently shippable; the more dangerous bug. |

---

## 7. Risks the build must respect

- **ClickHouse arity trap is a live P0** that Phase 2 would trigger. Phase 0 exists to defuse it first.
- **Never record app health as a second row** per sweep: `site_uptime_daily` has no kind dimension (PK `site_id, day`), so `total_checks` would double and `up_checks` would blend two meanings, silently corrupting every uptime percentage in the product.
- The 300s app probe against the 60s reachability probe means **4 of every 5 rollup upserts carry `app_up = NULL`**. A naive `latest_app_up = EXCLUDED.latest_app_up` clobbers a known value with NULL 80% of the time. The upsert must be COALESCE-guarded.
- Cache-busting is defeated one click away, and Cloudflare Always Online defeats everything. A design that silently reports healthy in those cases is worse than the current bug, which is why cache-HIT detection maps to `unknown`, not to healthy.
- `VerifyReachable` today collapses a 404 into a bare `alive=false` and discards the reason, so an *uninstalled* agent looks like a *broken* one. Typed reasons are a prerequisite for the cross-check.
