import type { Site } from "@wpmgr/api";

import { relativeTime } from "@/lib/utils";
import { siteUptimeBadge } from "@/features/sites/uptime-badge";

// GH #414 phase 4b — the interface for "pause monitoring".
//
// Every decision this phase makes lives here as a pure function so it can be
// tested without mounting a tree. The components below it are thin.
//
// THE RULE THE WHOLE FEATURE IS BUILT ON: pause means "do not tell me", never
// "lie to me". That is a display constraint as much as a scheduling one.
//
// `health_status` has THREE writers, not one: the uptime prober, the
// connection sweep (sweeper.go -> connection_service.go's
// MarkSiteDegraded/MarkSiteDisconnected) and site.HealthCheckWorker
// (health.go, over the unfiltered ListEnrolled). Phase 2 stops only the
// prober for a paused site — the other two keep writing, deliberately, so
// `health_status` stays LIVE and TRUE the whole time a site is paused. It is
// never muted, dated or overridden here, and nothing below should touch it.
//
// What phase 2 DOES stop is the prober's own result: `site.up`,
// `site.uptime_pct` and the "as of" stamp all freeze at whatever they were
// when the pause landed. `health_checked_at`, despite its name, is
// `site_uptime_status.last_probed_at` (apps/api/internal/site/model.go:120)
// — the uptime prober's own clock, not health's. `uptimeBadgeFor` below is
// what mutes and dates THAT result; a confident green "Up" on a site nobody
// has probed since Tuesday is the interface telling the lie the backend
// refused to.

/**
 * Paused when `monitoring_paused_at` is present. There is no boolean on the
 * wire on purpose (phase 4a): the flag and the since-when are one column, so
 * nothing can disagree with the timestamp.
 */
export function isMonitoringPaused(site: Site): boolean {
  return Boolean(site.monitoring_paused_at);
}

/** Split a selection into its paused and active halves, preserving order. */
export function splitByPauseState(sites: Site[]): {
  paused: Site[];
  active: Site[];
} {
  const paused: Site[] = [];
  const active: Site[] = [];
  for (const site of sites) {
    if (isMonitoringPaused(site)) paused.push(site);
    else active.push(site);
  }
  return { paused, active };
}

/** How many of the loaded sites currently have monitoring paused. */
export function pausedCount(sites: Site[]): number {
  return sites.reduce((n, site) => (isMonitoringPaused(site) ? n + 1 : n), 0);
}

/**
 * The fleet count line. "34 sites, 2 paused" rather than a quiet "34": a pause
 * nobody can see from the roster is a pause the operator forgets, and then
 * wonders why an outage went unreported.
 *
 * `paused` is appended only when non-zero, so an unpaused fleet reads exactly
 * as it did before this feature.
 */
export function fleetCountLabel(total: number, paused: number): string {
  const noun = total === 1 ? "site" : "sites";
  if (paused <= 0) return `${total} ${noun} enrolled`;
  return `${total} ${noun} enrolled, ${paused} paused`;
}

// ── The uptime badge ────────────────────────────────────────────────────────

export type UptimeBadgeTone = "success" | "destructive" | "muted";

export interface UptimeBadgeView {
  label: string;
  tone: UptimeBadgeTone;
  pulse: boolean;
  /** Rendered after the label as a separated suffix; null when there is none. */
  time: string | null;
  /** Full sentence for the title/aria description. */
  description: string;
}

/**
 * THE DECISION, defended in one sentence: a paused site keeps its last uptime
 * verdict but loses its colour and gains an explicit "as of" stamp, because
 * hiding the value would throw away the last true thing we know and keeping
 * the green would assert a freshness the prober stopped supplying.
 *
 * `health_status` is deliberately NOT read here — it has two other writers
 * that never stop under pause (see the file header), so it stays live and
 * unmuted wherever it is already shown. This reads `site.up` /
 * `site.uptime_pct` (via `siteUptimeBadge`) instead, dated by
 * `health_checked_at` — the uptime prober's own `last_probed_at`, which stops
 * advancing at exactly the moment this badge freezes. Absent means the site
 * was never probed, and that renders as "not checked", never as now.
 */
// siteUptimeBadge's tone is the wider StatusTone (it also covers "warning" /
// "info" for other callers); GH #414's paused/unpaused states only ever need
// this three-way palette, so status maps down to it explicitly rather than
// widening UptimeBadgeTone to match every StatusTone member.
const activeToneOf: Record<ReturnType<typeof siteUptimeBadge>["status"], UptimeBadgeTone> = {
  up: "success",
  down: "destructive",
  unknown: "muted",
};

export function uptimeBadgeFor(site: Site, now: number = Date.now()): UptimeBadgeView {
  const base = siteUptimeBadge(site.up);
  const pulse = base.status === "up";

  if (!isMonitoringPaused(site)) {
    return {
      label: base.label,
      tone: activeToneOf[base.status],
      pulse,
      time: null,
      description: `Uptime: ${base.label.toLowerCase()}`,
    };
  }

  const checked = relativeTime(site.health_checked_at ?? null, now);

  if (!checked) {
    return {
      label: "Not checked",
      tone: "muted",
      pulse: false,
      time: null,
      description:
        "Monitoring is paused and this site has never been probed for uptime, so there is no result.",
    };
  }

  return {
    label: base.label,
    tone: "muted",
    pulse: false,
    time: `as of ${checked}`,
    description: `Monitoring is paused. Last probed ${checked}; this result is not being refreshed.`,
  };
}

// ── The paused badge ────────────────────────────────────────────────────────

export interface PausedBadgeView {
  /** Short label on the chip itself. */
  label: string;
  /** The hover/title text: who, since when, why, and when it comes back. */
  title: string;
}

/**
 * The chip and its hover text. `resolveActor` turns a user id into a display
 * name; it returns null when the id is unknown, which includes the deliberate
 * case of a deleted user (the FK is ON DELETE SET NULL, so `paused_by` is also
 * simply absent for an API-key actor).
 */
export function pausedBadgeFor(
  site: Site,
  resolveActor: (userId: string) => string | null = () => null,
  now: number = Date.now(),
): PausedBadgeView | null {
  if (!isMonitoringPaused(site)) return null;

  const since = relativeTime(site.monitoring_paused_at ?? null, now);
  const parts: string[] = [];

  const actor = site.monitoring_paused_by
    ? resolveActor(site.monitoring_paused_by)
    : null;

  const who = actor ? ` by ${actor}` : "";
  parts.push(since ? `Monitoring paused${who} ${since}.` : `Monitoring paused${who}.`);

  const reason = site.monitoring_paused_reason?.trim();
  if (reason) parts.push(`Reason: ${reason}`);

  const resume = relativeTime(site.monitoring_resume_at ?? null, now);
  parts.push(
    resume
      ? `Resumes automatically ${resume}.`
      : "Stays paused until someone resumes it.",
  );

  parts.push(MONITORING_PAUSE_SCOPE_SENTENCE);

  return { label: "Monitoring paused", title: parts.join(" ") };
}

// ── The single most important string in this phase ──────────────────────────

/**
 * What a pause actually touches, in one plain sentence.
 *
 * This is the difference between a useful feature and a support ticket six
 * weeks later. Someone pausing before a migration reasonably assumes
 * EVERYTHING stops; backups silently stopping is the one failure people do not
 * recover from, so the sentence names backups explicitly on the "continues"
 * side rather than leaving it to be inferred from silence.
 *
 * It matches what phases 1 through 3 actually built. FOUR things stop:
 * - uptime checks — ProbeWorker.Sweep enumerates
 *   ListEnrolledForMonitoringProbe (m117's `AND monitoring_paused_at IS
 *   NULL`).
 * - uptime alerts — fire()/fireApp() re-read the pause state fresh before
 *   dispatch.
 * - SCHEDULED screenshots only — screenshot_weekly_fanout filters on
 *   monitoring_paused_at IS NULL (sitelister.go); a person clicking Refresh,
 *   and an enroll capture, still run on a paused site.
 * - SCHEDULED vulnerability rescans only, and their alert dispatch —
 *   Repo.ListUnpausedSiteIDsForRescan gates the fan-out and
 *   ListTenantsWithUnnotifiedFindings/ClaimUnnotifiedFindings exclude paused
 *   sites from dispatch; the operator's per-site "rescan now" route is
 *   unfiltered and always runs.
 *
 * Update checks (RefreshInventoryArgs, `cmd/wpmgr/siteadapter.go:167` and
 * `internal/update/worker.go:971`) and scans (ScanRunArgs,
 * `internal/scan/service.go:58`) have NO scheduled producer at all — both are
 * always operator-triggered — so pause has nothing to filter them from and
 * they belong on the "continues" side, not "stops". Backups, the cron kick
 * that keeps WP-Cron draining (and therefore keeps backups running), the
 * connection sweep, RUM ingestion and retention are all unfiltered too.
 */
export const MONITORING_PAUSE_SCOPE_SENTENCE =
  "Backups and connection tracking keep running.";

/** The long form, used in the confirmation dialog. */
export const MONITORING_PAUSE_STOPS =
  "Uptime checks, uptime alerts, scheduled screenshots and scheduled vulnerability rescans stop.";

export const MONITORING_PAUSE_CONTINUES =
  "Backups, connection tracking, update checks and scans keep running, and anything else you click yourself still runs too.";

// ── Menu labelling ──────────────────────────────────────────────────────────

export interface MonitoringMenuView {
  /** Render a "Pause monitoring on N sites" item. */
  pause: { label: string; count: number } | null;
  /** Render a "Resume monitoring on N sites" item. */
  resume: { label: string; count: number } | null;
}

/**
 * A mixed selection offers BOTH items, each labelled with its own count, so
 * neither action is ambiguous about what it touches. Selecting 5 sites of
 * which 2 are paused offers "Pause monitoring on 3 sites" and "Resume
 * monitoring on 2 sites" — never one item over 5.
 */
export function monitoringMenuFor(selected: Site[]): MonitoringMenuView {
  const { paused, active } = splitByPauseState(selected);
  const noun = (n: number) => (n === 1 ? "site" : "sites");
  return {
    pause:
      active.length > 0
        ? {
            label: `Pause monitoring on ${active.length} ${noun(active.length)}...`,
            count: active.length,
          }
        : null,
    resume:
      paused.length > 0
        ? {
            label: `Resume monitoring on ${paused.length} ${noun(paused.length)}`,
            count: paused.length,
          }
        : null,
  };
}

// ── Error codes ─────────────────────────────────────────────────────────────

/**
 * Top-level failures, from the Go handler.
 *
 * `request_too_large` and `too_many_sites` and `site_ids_required` come from
 * `apps/api/internal/site/monitoring_handler.go` (422); `principal_required`
 * from `apps/api/internal/site/monitoring.go:192` and `:229` (403);
 * `resume_at_in_past` is named in the published 422 description on
 * `PauseSiteMonitoringErrors`. Each gets its own sentence rather than a
 * generic "Request failed", because the remedy differs for every one of them.
 */
const REQUEST_ERROR_MESSAGES: Record<string, string> = {
  request_too_large:
    "That selection is too large to send in one request. Select up to 200 sites and try again.",
  too_many_sites:
    "Monitoring can be changed on at most 200 sites per request. Narrow the selection and try again.",
  site_ids_required: "No sites were selected.",
  principal_required:
    "Your session has expired. Sign in again, then retry the change.",
  resume_at_in_past:
    "The resume time has to be in the future. Pick a later time and try again.",
};

/** Message for a whole-request failure, or null when the code is unknown. */
export function monitoringRequestErrorMessage(code: string): string | null {
  return REQUEST_ERROR_MESSAGES[code] ?? null;
}

/**
 * Per-site outcomes, from `monitoringResultDTO.Detail` in
 * `apps/api/internal/site/monitoring_handler.go:60-63`, which names all nine.
 *
 * `site_archived` and `site_revoked` arrive on a 200 alongside `ok:false`, so
 * they are NOT request failures: the other sites in the same call succeeded,
 * and reporting "the request failed" would be wrong. The parameter is a bare
 * string rather than the generated union because the published `detail` enum
 * on `MonitoringResult` is missing those two values even though the handler
 * returns them (pinned by
 * `apps/api/tests/gh414_monitoring_guards_test.go:214-215`).
 */
const SITE_DETAIL_MESSAGES: Record<string, string> = {
  site_archived: "archived, so monitoring was left as it is",
  site_revoked: "revoked, so monitoring was left as it is",
  site_not_found: "no longer exists",
  invalid_site_id: "could not be identified",
  forbidden: "is outside your site access",
};

export function monitoringDetailMessage(detail: string): string | null {
  return SITE_DETAIL_MESSAGES[detail] ?? null;
}

export interface MonitoringOutcomeSummary {
  /** Sites this request actually moved. */
  changed: number;
  /** Accepted but already in the requested state. */
  unchanged: number;
  /** Refused, with a reason worth naming. */
  refused: { siteId: string; detail: string; message: string }[];
}

/** Fold a bulk result into the counts a toast needs. */
export function summarizeMonitoringResult(
  results: { site_id: string; ok: boolean; changed: boolean; detail: string }[],
): MonitoringOutcomeSummary {
  const summary: MonitoringOutcomeSummary = {
    changed: 0,
    unchanged: 0,
    refused: [],
  };
  for (const r of results) {
    if (r.ok && r.changed) summary.changed += 1;
    else if (r.ok) summary.unchanged += 1;
    else {
      summary.refused.push({
        siteId: r.site_id,
        detail: r.detail,
        message: monitoringDetailMessage(r.detail) ?? "could not be changed",
      });
    }
  }
  return summary;
}
