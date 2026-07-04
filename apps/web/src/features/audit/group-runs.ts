import type { AuditEntry } from "@wpmgr/api";

import { actionLabel, classifySeverity } from "./labels";

// Collapses consecutive same-actor + same-action + same-target "read"
// entries into a single summary run, so a burst of 14 file reads renders as
// one row instead of drowning the risk events around it (redesign point 6).
//
// Hard rule: only entries classified "read" (see labels.ts) are ever
// collapsible. A denied, sensitive, or write entry always breaks the current
// run and renders standalone — "Read x8 -> Edited file -> Read x6" never
// becomes "Read x15". Collapsing is purely a rendering concern: entries are
// never reordered (day-group order is preserved) and each entry id appears
// in exactly one run.

export interface AuditRun {
  kind: "single" | "run";
  entries: AuditEntry[];
}

/** Consecutive reads within this many milliseconds of each other still count
 * as "the same burst" even though a fast file-manager session isn't
 * perfectly back-to-back. */
const RUN_GAP_MS = 5 * 60_000;

/** Caps a single collapsed run so one degenerate burst can't blow up render
 * cost; anything beyond this starts a new run. */
const RUN_CAP = 200;

function withinGap(aIso: string, bIso: string): boolean {
  const a = Date.parse(aIso);
  const b = Date.parse(bIso);
  if (Number.isNaN(a) || Number.isNaN(b)) return false;
  return Math.abs(a - b) <= RUN_GAP_MS;
}

function sameGroup(a: AuditEntry, b: AuditEntry): boolean {
  return (
    a.actor_type === b.actor_type &&
    a.actor_id === b.actor_id &&
    a.action === b.action &&
    a.target_type === b.target_type &&
    a.target_id === b.target_id
  );
}

/** Groups one day-bucket's (already time-ordered) entries into runs. */
export function groupRuns(entries: AuditEntry[]): AuditRun[] {
  const out: AuditRun[] = [];
  const seen = new Set<string>();
  let current: AuditEntry[] = [];

  const flush = () => {
    if (current.length === 0) return;
    out.push({ kind: current.length > 1 ? "run" : "single", entries: current });
    current = [];
  };

  for (const entry of entries) {
    if (seen.has(entry.id)) continue; // defensive de-dupe
    seen.add(entry.id);

    const collapsible = classifySeverity(entry.action) === "read";
    if (!collapsible) {
      flush();
      out.push({ kind: "single", entries: [entry] });
      continue;
    }

    const last = current[current.length - 1];
    if (
      last &&
      current.length < RUN_CAP &&
      sameGroup(last, entry) &&
      withinGap(last.created_at, entry.created_at)
    ) {
      current.push(entry);
    } else {
      flush();
      current = [entry];
    }
  }
  flush();
  return out;
}

// Per-action templates for the collapsed run's summary verb, so "Read file"
// becomes "Read 14 files" rather than the generic "Read file (14x)". Only
// the actions realistically likely to burst are worth a bespoke plural; any
// other action falls back to the generic template.
const RUN_TEMPLATES: Record<string, (count: number) => string> = {
  "site.files.read": (n) => `Read ${n} file${n === 1 ? "" : "s"}`,
  "site.files.search": (n) => `Searched files (${n}x)`,
  "site.files.versions.list": (n) => `Viewed version history (${n}x)`,
  "auth.login.success": (n) => `Signed in (${n}x)`,
  "site_diagnostics.refresh": (n) => `Refreshed diagnostics (${n}x)`,
  "site_vuln.rescan": (n) => `Rescanned for vulnerabilities (${n}x)`,
};

export function runSummaryLabel(action: string, count: number): string {
  const template = RUN_TEMPLATES[action];
  if (template) return template(count);
  return `${actionLabel(action)} (${count}x)`;
}
