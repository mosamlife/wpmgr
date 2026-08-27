# Architecture Decision Records — index

This is the live index for every ADR that has a file in this directory —
ADR-037 onward. It is not a clean "044 and later" split: ADR-038 through
ADR-043 have a file here **and** are also archived in
[`DECISIONS.md`](../../DECISIONS.md) at the repo root — the same six
decisions, recorded in both places. ADR-034 through ADR-036 have neither; see
Numbering below for what is known about each. `DECISIONS.md` covers ADR-001
through ADR-033 and ADR-038 through ADR-043; it is historical and is not
appended to any more.

**This file is the only place a new ADR gets registered.** To add one: take
the next free number below, write `docs/adr/ADR-<NNN>-<slug>.md`, then add its
row to the table.

## Index

One row per ADR file in this directory. Status is transcribed as written in
each file (see "Status line formats" below — the tree is not normalized).

| # | Title | Status | Date | File |
|---|-------|--------|------|------|
| 037 | WPMgr site-management roadmap | Superseded by ADR-060 | 2026-05-29 | [ADR-037-wpmgr-feature-parity-roadmap.md](./ADR-037-wpmgr-feature-parity-roadmap.md) |
| 038 | SSE channel scoping + cross-instance fan-out | accepted | 2026-05-31 | [ADR-038-sse-channel-scoping.md](./ADR-038-sse-channel-scoping.md) |
| 039 | Heartbeat cadence + connection-timeout thresholds | accepted | 2026-05-31 | [ADR-039-heartbeat-cadence-timeouts.md](./ADR-039-heartbeat-cadence-timeouts.md) |
| 040 | Agent-side last-will (disconnect) mechanism | accepted | 2026-05-31 | [ADR-040-agent-last-will-disconnect.md](./ADR-040-agent-last-will-disconnect.md) |
| 041 | Re-enrollment identity + connection-state model | accepted | 2026-05-31 | [ADR-041-reenrollment-identity-connection-state.md](./ADR-041-reenrollment-identity-connection-state.md) |
| 042 | CP-Driven WordPress Agent Self-Update | Accepted | 2026-05-31 | [ADR-042-cp-agent-self-update.md](./ADR-042-cp-agent-self-update.md) |
| 043 | Media Optimizer: architecture, encode topology & transport | Accepted | 2026-05-31 | [ADR-043-media-optimizer-architecture.md](./ADR-043-media-optimizer-architecture.md) |
| 044 | Automatic image optimization on upload | Proposed | 2026-06-01 | [ADR-044-auto-optimize-on-upload.md](./ADR-044-auto-optimize-on-upload.md) |
| 045 | UI-Configured SMTP Email, Self-Serve Auth Flows, and Alert Channels | Accepted (2026-06-02) — building | 2026-06-02 | [ADR-045-email-auth-alerts.md](./ADR-045-email-auth-alerts.md) |
| 045 | Appendix A — Brand HTML Email Templates | *(no Status line — companion doc)* | — | [ADR-045-email-templates.md](./ADR-045-email-templates.md) |
| 045 | Appendix B — Pluggable Alert Channels (Phase 4, LATER) | DESIGN ONLY. Phase 4 implements this. Do not build now. | — | [ADR-045-alert-channels-design.md](./ADR-045-alert-channels-design.md) |
| 046 | Performance Suite: caching, optimization & pure-Go RUCSS topology | Accepted | 2026-06-03 | [ADR-046-performance-suite-architecture.md](./ADR-046-performance-suite-architecture.md) |
| 047 | Hand-written Gin routes for the Performance Suite (OpenAPI exception) | Accepted | 2026-06-04 | [ADR-047-hand-written-gin-routes-perf-suite.md](./ADR-047-hand-written-gin-routes-perf-suite.md) |
| 051 | Archive-delta incremental backups | Accepted (2026-06-07) | 2026-06-07 | [ADR-051-archive-delta-incremental.md](./ADR-051-archive-delta-incremental.md) |
| 052 | Font transcoding to WOFF2 | Accepted (2026-06-08) | 2026-06-08 | [ADR-052-font-transcoding-woff2.md](./ADR-052-font-transcoding-woff2.md) |
| 053 | Font subsetting (Phase 2) | Accepted (2026-06-09) | 2026-06-09 | [ADR-053-font-subsetting.md](./ADR-053-font-subsetting.md) |
| 054 | Real User Monitoring (Performance Tracker) | Accepted (2026-06-09) | 2026-06-09 | [ADR-054-rum-performance-tracker.md](./ADR-054-rum-performance-tracker.md) |
| 055 | Autologin 2FA Bypass + 502 Hardening | Accepted | 2026-06-10 | [ADR-055-autologin-2fa-bypass.md](./ADR-055-autologin-2fa-bypass.md) |
| 056 | Dashboard Two-Factor Authentication | Accepted | 2026-06-16 | [ADR-056-dashboard-2fa.md](./ADR-056-dashboard-2fa.md) |
| 057 | Security Suite Foundation: Per-Site Policy Model | Accepted | 2026-06-20 | [ADR-057-security-suite-foundation.md](./ADR-057-security-suite-foundation.md) |
| 059 | Site-User Authentication Policy (2FA + Password Policy) | Accepted — 2026-06-20 | 2026-06-20 | [ADR-059-site-user-auth-policy.md](./ADR-059-site-user-auth-policy.md) |
| 060 | Phase order: safety and truth before capability | Accepted | 2026-08-18 | [ADR-060-phase-order-safety-before-capability.md](./ADR-060-phase-order-safety-before-capability.md) |
| 061 | Assistant surface, Phase 1: control-plane server and out-of-band approval | Accepted | 2026-08-22 | [ADR-061-assistant-surface-phase-1.md](./ADR-061-assistant-surface-phase-1.md) |
| 062 | Assistant surface, Phase 2: governed fleet content operations | Proposed | 2026-08-27 | [ADR-062-assistant-surface-phase-2-content-operations.md](./ADR-062-assistant-surface-phase-2-content-operations.md) |

Not part of the numbering: [`font-subsetting-phase2-plan.md`](./font-subsetting-phase2-plan.md)
is a build plan, not a decision record. It lives in this directory because it
extends ADR-052, but it was never assigned a number and shouldn't be.

### Status line formats

Three formats coexist in the files above, transcribed as-is rather than
normalized:

1. **Bold inline**, its own line near the top — `**Status:** <value>`, e.g.
   ADR-037 through ADR-047 (except the ADR-045 appendices), ADR-055–057,
   ADR-059, ADR-060.
2. **Plain inline**, no bold — `Status: <value> (<date>)`, e.g.
   ADR-051–054.
3. **Bold bullet**, inside a metadata list — `- **Status:** <value> (<date>) —
   <note>`, e.g. ADR-045-email-auth-alerts.md.

Two files don't fit any of the three and are findings, not table artifacts:

- `ADR-045-alert-channels-design.md` has a `Status:` line (plain, no bold,
  line 7 rather than the top) but its value, "DESIGN ONLY. Phase 4 implements
  this. Do not build now.", isn't one of Proposed/Accepted/Superseded.
- `ADR-045-email-templates.md` has no `Status:` line at all.

## Numbering

Numbers are allocated in this file and are never reused, including the ones
below that have no file. **The next free number is 063.**

| Number(s) | What happened |
|---|---|
| 034 | Reserved, no `docs/adr/` file and no entry in `DECISIONS.md`. Subject: the M5.6 restore engine (staged-prefix DB restore, atomic file/directory swap, the restore state-machine driver and its watchdog). Cited from: `apps/api/internal/backup/service.go:797`, `apps/api/internal/agentcmd/backup_contract.go:347`, `apps/agent/includes/commands/class-restore-command.php:3`, `apps/agent/includes/backup/class-restore-runner.php:3`, and dozens more across `apps/agent/includes/backup/**`, `apps/api/internal/backup/**` and their tests — run `git grep -n 'ADR-034'` for the complete set. Reserved, never reused. |
| 035 | Reserved — a gap with no file, no `DECISIONS.md` entry, and (like 058) no citation found anywhere: not in the tracked tree (`git grep -n "ADR-035"`), not in any commit message (`git log --all --grep="ADR-035"`), and not in any historical diff (`git log --all -S"ADR-035"`) — all three came back empty at the time this index was written. Reserved and never reused regardless. |
| 036 | Reserved, no `docs/adr/` file and no entry in `DECISIONS.md`. Subject: the M7 storage-adapter / backup-destination foundation and the URL rewriter. Cited from: `apps/api/migrations/20260530000001_m7_site_destinations.sql:1`, `apps/api/migrations/20260530010000_m7_url_rewriter.sql:1`, `apps/agent/includes/backup/destinations/class-backup-destination.php:3`, `docs/adr/ADR-037-wpmgr-feature-parity-roadmap.md:5,17,98` (ADR-037 itself cites it as a separate doc), and dozens more across `apps/agent/includes/backup/**`, `apps/api/internal/backup/**`, `apps/api/internal/sitedestination/**` and generated OpenAPI code — run `git grep -n 'ADR-036'` for the complete set. Reserved, never reused. |
| 038–043 | Not a gap — the opposite: each of these six numbers has both a file under `docs/adr/` and an entry in `DECISIONS.md`. Same six decisions, recorded twice. This file is the live index; the `DECISIONS.md` entries are historical and are not updated to match. |
| 045 | Allocated three times: `ADR-045-email-auth-alerts.md`, `ADR-045-alert-channels-design.md`, `ADR-045-email-templates.md`. These are three distinct, related decisions that collided on one number. All three keep their existing filenames. 045 is closed — no fourth file is ever filed under it. |
| 048 | Reserved, no file was ever written. Subject: the first incremental-backup engine (per-file content-addressed chunking), later superseded by ADR-051 (per ADR-051's own text, `docs/adr/ADR-051-archive-delta-incremental.md:4,8`). Cited from: `apps/api/migrations/20260611000000_m48_schedule_incremental.sql:1,9`, `CHANGELOG.md:1931`, `apps/api/internal/backup/enqueuer.go:32`, and dozens more sites across `apps/api/internal/backup/**`, `apps/api/internal/db/sqlc/**` and its tests — run `git grep -n 'ADR-048'` for the complete set. Reserved, never reused. |
| 049 | Reserved, no file was ever written. Subject: chain / point-in-time restore (the read side of the incremental engine). Cited from: `apps/api/migrations/20260608000000_m45_incremental_restore.sql:1`, `CHANGELOG.md:1931,1933`, `apps/api/internal/backup/worker.go:513`, `apps/api/internal/agentcmd/backup_contract.go:399,415`, and more across `apps/api/internal/backup/**` and its tests — run `git grep -n 'ADR-049'` for the complete set. Reserved, never reused. |
| 050 | Reserved, no file was ever written. Subject: mark-and-sweep retention GC / object-storage reclaim. Cited from: `apps/api/cmd/wpmgr/main.go:1369,3281`, `apps/api/migrations/20260609000000_m46_chain_id_base_stamp.sql:1`, `apps/api/migrations/20260610000000_m47_chunk_last_referenced.sql:1`, `apps/api/migrations/20260815000000_m113_site_object_reclaim.sql:18`, `CHANGELOG.md:1935`, and dozens more across `apps/api/internal/backup/**` (`gc.go`, `reclaim_worker.go`, `tenant_reclaim_worker.go`), generated sqlc code and tests — run `git grep -n 'ADR-050'` for the complete set. Reserved, never reused. |
| 058 | Reserved — the gap between ADR-057 and ADR-059. No file exists, and unlike 048/049/050, no citation of `ADR-058` was found anywhere: not in the tracked tree (`git grep -n "ADR-058"`), not in any commit message (`git log --all --grep="ADR-058"`), and not in any historical diff (`git log --all -S"ADR-058"`) — all three came back empty at the time this index was written. Reserved and never reused regardless of whether a citation ever existed. |

