import type { AgentMirrorStatus, FleetAgentVersions } from "@wpmgr/api";
import { relativeTime } from "@/lib/utils";

// GH #322: the fleet Agent column's "current" classification is only ever
// as fresh as the reference version it was computed against, and on a
// self-hosted install that reference comes from a periodic upstream mirror
// (WPMGR_UPDATE_AGENT_MIRROR_ENABLED). There was previously no signal
// anywhere that the reference itself might be stale.
//
// This module turns `FleetAgentVersions.agent_mirror` (the generated
// `AgentMirrorStatus`, see @wpmgr/api) into the exact operator-facing copy
// for the Agent column header popover. Two rules make this honest rather
// than a fresher version of the same lie:
//
//   1. LAST ATTEMPT vs LAST SUCCESS are different facts (C1). An age is
//      NEVER rendered from `last_attempt_at`, only `last_success_at`
//      answers "when did we last actually confirm what upstream publishes".
//      A run that failed 10 minutes ago must never be described as
//      "checked 10 minutes ago" when the last real confirmation was hours
//      earlier; see describeEnabledMirrorState below for how the two are
//      kept separate in every branch.
//
//   2. `rate_limited` is explicitly NOT a failure (C5). The mirror waits at
//      least 30 minutes between upstream requests, and that is the normal,
//      quiet, expected result most of the time. Its copy says so plainly
//      and its tone is only ever elevated by a stale success underneath it,
//      never by the rate limit itself.
//
// `status` is computed server-side (one threshold, one place, so every
// client agrees) and trusted verbatim here. This module never re-derives
// staleness or the roll-up from raw timestamps.
//
// NOTE ON NULLS (GH #479, GH #508): the spec expresses nullability in the 3.1
// union form, so the generated AgentMirrorStatus tells the truth for every
// "never recorded" field, `last_attempt_outcome` included, and this module
// reads all of them directly with no cast and no shim. Each one is filled by
// agentrelease/handler.go's formatMirrorTime or nonEmptyMirrorString, both of
// which return nil for "never happened", so the wire genuinely carries null
// there before the first mirror run.

export type ReferenceCheckTone = "info" | "warn";

export interface ReferenceCheckMessage {
  title: string;
  body: string;
  tone: ReferenceCheckTone;
  /**
   * Rendered above the title only when the fleet-wide classification has no
   * reference version at all (FleetAgentVersions.reference_source ===
   * "none") and the mirror is on: turns "everything is Unknown" from a dead
   * end into a cause.
   */
  lead?: string;
}

/** relativeTime() is null-safe but this module only ever calls it once a
 *  caller has confirmed the ISO string is present; the fallback is
 *  defensive only and should not be reachable with real data. */
function ago(iso: string | null): string {
  return relativeTime(iso) ?? "recently";
}

const SIX_HOUR_CYCLE_NOTE =
  "That is longer than the usual 6 hour cycle, so a newer release may exist and WPMgr would not know.";

function describeEnabledMirrorState(
  mirror: AgentMirrorStatus,
  hasPublishedReference: boolean,
): ReferenceCheckMessage {
  const lastAttemptAt = mirror.last_attempt_at;
  const lastAttemptOutcome = mirror.last_attempt_outcome;
  const lastAttemptDetail = mirror.last_attempt_detail;
  const lastSuccessAt = mirror.last_success_at;
  const lastSuccessVersion = mirror.last_success_version ?? "an earlier release";

  // The mirror could not run at all (object storage/HTTP client not wired).
  // Never self-heals, so this warns immediately regardless of staleness.
  if (mirror.status === "misconfigured") {
    const successNote = lastSuccessAt
      ? ` The reference is still ${lastSuccessVersion}, last confirmed ${ago(lastSuccessAt)}.`
      : "";
    return {
      title: "Upstream mirror is misconfigured",
      body: `The mirror is enabled but cannot run${lastAttemptDetail ? `: ${lastAttemptDetail}` : ""}. This will not resolve on its own; an operator needs to fix the control plane configuration.${successNote}`,
      tone: "warn",
    };
  }

  // This install publishes its own agent releases; the mirror is correctly,
  // permanently standing down. Nothing is broken.
  if (mirror.status === "standing_down") {
    return {
      title: "Mirroring stood down",
      body: "This install publishes its own agent releases, so the mirror will not overwrite them. The reference comes from your own release channel.",
      tone: "info",
    };
  }

  // The mirror was just turned on (or this install just booted) and has
  // never run at all.
  if (mirror.status === "pending") {
    return {
      title: "First check has not run yet",
      // Two things this must NOT say, both of which were wrong before.
      // First, the periodic job is registered with RunOnStart false, so the
      // first attempt lands six to six and a half hours after boot, not
      // "shortly". Second, an install that has been mirroring for months
      // still has its published reference sitting in object storage: the
      // state row is new and starts empty, which says nothing about whether
      // a reference exists. Claiming there is none would contradict the
      // version shown on the same screen.
      body: hasPublishedReference
        ? "Release mirroring is on, but no check has completed since this control plane started, so the reference below may predate the newest upstream release. The first scheduled check runs within about six hours of startup."
        : "Release mirroring is on, but no check has completed yet, so WPMgr has no published reference version. The first scheduled check runs within about six hours of startup.",
      tone: "info",
    };
  }

  // status is "ok" or "stale" from here on: the mirror has attempted at
  // least once (see agentmirror.State.Status's ordering).
  const stale = mirror.status === "stale";

  // It has tried, but has never once reached a confirmed answer. This is
  // the most severe form of the reported bug, so it warns immediately
  // rather than waiting for a staleness window an established channel
  // would need but an unestablished one does not.
  if (!lastSuccessAt) {
    return {
      title: "No check has ever succeeded",
      // Same rule as the pending branch: only claim there is no published
      // reference when there genuinely is not one.
      body: `Release mirroring is on, but no check has completed on this install. Last try ${lastAttemptAt ? ago(lastAttemptAt) : "recently"}: ${lastAttemptDetail ?? "no result was recorded"}.${hasPublishedReference ? " The reference below comes from a previously published release and may predate the newest upstream one." : " Until one succeeds there is no published reference version."}`,
      tone: "warn",
    };
  }

  const successAge = ago(lastSuccessAt);
  const attemptAge = lastAttemptAt ? ago(lastAttemptAt) : successAge;

  switch (lastAttemptOutcome) {
    case "mirrored":
    case "current":
    case "unchanged":
      return stale
        ? {
            title: `Reference checked ${successAge}`,
            body: `${SIX_HOUR_CYCLE_NOTE} Sites shown as current are compared against ${lastSuccessVersion}.`,
            tone: "warn",
          }
        : {
            title: `Reference checked ${successAge}`,
            body: `The newest agent release upstream was ${lastSuccessVersion}. WPMgr checks again about every 6 hours.`,
            tone: "info",
          };

    case "rate_limited": {
      // S4: expected and quiet (C5). Tone follows the underlying success's
      // own staleness, never the rate limit itself: two consecutive skips
      // over a healthy reference is still fine, an old reference is not.
      const base = `Upstream limits how often WPMgr may ask, so the last run did not get an answer. This is expected and not an error. The reference is ${lastSuccessVersion}, last confirmed ${successAge}.`;
      return stale
        ? { title: "Last check was skipped", body: `${base} ${SIX_HOUR_CYCLE_NOTE}`, tone: "warn" }
        : { title: "Last check was skipped", body: base, tone: "info" };
    }

    case "refused":
      // Upstream answered and this install KNOWS a mismatch exists, which
      // is a stronger and different claim than mere uncertainty, so it
      // always warns regardless of `stale`.
      return {
        title: "Upstream release not mirrored",
        body: `The last check reached upstream and did not mirror what it found: ${lastAttemptDetail ?? "the release was not accepted"}. Sites are compared against ${lastSuccessVersion}, last confirmed ${successAge}.`,
        tone: "warn",
      };

    case "upstream_unavailable":
    case "storage_error":
    default:
      // The one string this whole issue is about, two ages, two labels,
      // never merged into one number. "failed" carries the attempt, "last
      // confirmed" carries the version, and the tone escalates only once
      // the confirmed version is itself stale.
      return {
        title: `Last check failed ${attemptAge}`,
        body: `The reference is still ${lastSuccessVersion}, last confirmed ${successAge}. A newer release may exist. WPMgr tries again on its next scheduled run.`,
        tone: stale ? "warn" : "info",
      };
  }
}

/**
 * Turns a `FleetAgentVersions.agent_mirror` (GH #322) into the operator
 * facing message for the Agent column header popover, or `undefined` when
 * nothing should be shown.
 *
 * Renders nothing for:
 *   - `mirror` absent (rollup not loaded yet).
 *   - `mirror.enabled === false && referenceSource === "published"`: the
 *     reference is current by construction (hosted release pipeline, or a
 *     self-host's own channel with mirroring off) and there is no periodic
 *     check to time. Inventing a "checked Nh ago" line here would be a fact
 *     this install never measured (C2).
 *   - `mirror.enabled === false` while `referenceSource !== "published"`:
 *     mirroring is off and there is no other source either, so only a
 *     plain, informational note is shown that the setting exists.
 */
export function describeReferenceCheck(
  mirror: AgentMirrorStatus | undefined,
  referenceSource: FleetAgentVersions["reference_source"] | undefined,
): ReferenceCheckMessage | undefined {
  if (!mirror) return undefined;

  if (!mirror.enabled) {
    if (referenceSource === "published") return undefined;
    return {
      title: "Release mirroring is off",
      body: "WPMgr never checks upstream for a newer agent release on this install. Set WPMGR_UPDATE_AGENT_MIRROR_ENABLED to turn it on.",
      tone: "info",
    };
  }

  const message = describeEnabledMirrorState(mirror, referenceSource === "published");
  if (referenceSource !== "none") return message;

  // Pairs "everything is Unknown" with the reason, rather than leaving it
  // as an unexplained dead end.
  return { ...message, lead: "WPMgr has no reference agent version yet." };
}
