// GH #463 Phase 3 — the operator's view of a scheduled run.
//
// Pure helpers only, so they can be unit-tested without a DOM and imported by
// both components and route files without tripping react-refresh's
// only-export-components rule.
//
// Every bound below mirrors a constant the control plane actually enforces.
// The client re-states them so an operator is told at the point of typing
// rather than by a 422 after submit; the SERVER remains the authority, and
// these are deliberately never stricter than it, or the UI would refuse a
// schedule the API would have accepted.

import type { UpdateRun } from "@wpmgr/api";

/**
 * How late a scheduled run may fire and still be run at all. Beyond this the
 * control plane moves it to `expired` and contacts no site.
 *
 * Mirrors `dispatchGraceWindow` — apps/api/internal/update/dispatch.go:45
 * (`const dispatchGraceWindow = 2 * time.Hour`).
 */
export const GRACE_WINDOW_HOURS = 2;

/**
 * The furthest ahead a run may be scheduled.
 *
 * Mirrors `scheduleMaxLead` — apps/api/internal/update/service.go:280
 * (`scheduleMaxLead = 30 * 24 * time.Hour`). The server rejects anything
 * beyond it with `422 schedule_too_far`.
 */
export const SCHEDULE_MAX_LEAD_DAYS = 30;

/**
 * How far in the PAST a scheduled time may land and still be read as "now".
 *
 * Mirrors `scheduleSkewGrace` — apps/api/internal/update/service.go:289
 * (`scheduleSkewGrace = 2 * time.Minute`). This exists because the instant is
 * computed from a browser clock, so a submission for "now" routinely lands a
 * few seconds behind the server's. The client MUST honour it: rejecting
 * inside this window would refuse a schedule the API accepts, which is the
 * one direction a client-side bound must never fail in.
 */
export const SCHEDULE_SKEW_GRACE_MS = 2 * 60 * 1000;

/**
 * The IANA zone the browser is in, which is the zone a `datetime-local` value
 * is interpreted in. Falls back to the empty string on a runtime with no
 * `Intl` resolution rather than guessing UTC, so callers can drop the label
 * instead of printing something false.
 *
 * This is deliberately the OPERATOR's zone and never a site's: an update run
 * targets many sites (or a whole tag), so there is no single site zone to
 * resolve, and the instant actually submitted is built from this one.
 */
export function browserTimeZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "";
  } catch {
    return "";
  }
}

/**
 * Absolute time, always carrying the zone it is expressed in.
 *
 * The defect this exists to fix: the run page printed `run.scheduled_at` raw
 * (an ISO-8601 UTC string) and the wizard offered a bare `datetime-local`, so
 * an operator in London scheduling a Sydney site was shown neither zone.
 * A time without its zone is not an answer to "when does this run".
 */
export function formatAbsolute(iso: string, timeZone?: string): string {
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return iso;
  const zone = timeZone || browserTimeZone();
  const stamp = new Intl.DateTimeFormat(undefined, {
    day: "numeric",
    month: "short",
    year: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
    ...(zone ? { timeZone: zone } : {}),
  }).format(at);
  return zone ? `${stamp} ${zone}` : stamp;
}

/** Short "02:00 on 19 Aug" form used inside the expired notice's prose. */
export function formatClockAndDay(iso: string, timeZone?: string): string {
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return iso;
  const zone = timeZone || browserTimeZone();
  const opts = zone ? { timeZone: zone } : {};
  const clock = new Intl.DateTimeFormat(undefined, {
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
    ...opts,
  }).format(at);
  const day = new Intl.DateTimeFormat(undefined, {
    day: "numeric",
    month: "short",
    ...opts,
  }).format(at);
  return `${clock} on ${day}`;
}

/**
 * "8h 42m" / "3d 4h" / "45s". Returns null once the instant has passed, which
 * is the caller's signal to stop saying "starts in" — a countdown that runs
 * negative is how an operator learns not to trust the number.
 */
export function formatCountdown(iso: string, now: number = Date.now()): string | null {
  const target = new Date(iso).getTime();
  if (Number.isNaN(target)) return null;
  const ms = target - now;
  if (ms <= 0) return null;
  const totalSeconds = Math.floor(ms / 1000);
  const days = Math.floor(totalSeconds / 86400);
  const hours = Math.floor((totalSeconds % 86400) / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60);
  const seconds = totalSeconds % 60;
  if (days > 0) return `${days}d ${hours}h`;
  if (hours > 0) return `${hours}h ${minutes}m`;
  if (minutes > 0) return `${minutes}m`;
  return `${seconds}s`;
}

/**
 * The full waiting line: "Starts in 8h 42m · 02:00 19 Aug 2026 Europe/London".
 *
 * Relative AND absolute, together, because neither alone is enough: the
 * countdown answers "how long do I have", the stamp answers "did I pick the
 * time I meant", and the zone is what makes the second one true.
 */
export function scheduleSubline(iso: string, now: number = Date.now()): string {
  const countdown = formatCountdown(iso, now);
  const absolute = formatAbsolute(iso);
  return countdown ? `Starts in ${countdown} · ${absolute}` : `Due now · ${absolute}`;
}

export type ScheduleProblem =
  | { code: "schedule_in_past"; message: string }
  | { code: "schedule_too_far"; message: string }
  | { code: "schedule_unparseable"; message: string };

/**
 * Client-side pre-flight for the wizard's `datetime-local` value.
 *
 * The two codes are the server's own, quoted from
 * apps/api/internal/update/service.go:320 and :323 — `schedule_in_past` and
 * `schedule_too_far`, both `domain.Validation`. Matching them keeps one
 * vocabulary across the wire rather than inventing a second client-only one.
 *
 * `value` is a raw `datetime-local` string ("2026-08-19T02:00"), which the
 * browser interprets in its own zone. That is the same reading `new Date()`
 * gives it and the same instant the wizard submits, so validating here
 * validates exactly what will be sent.
 */
export function validateSchedule(
  value: string,
  now: number = Date.now(),
): ScheduleProblem | null {
  if (!value) return null;
  const at = new Date(value).getTime();
  if (Number.isNaN(at)) {
    return {
      code: "schedule_unparseable",
      message: "That is not a valid date and time.",
    };
  }
  if (at < now - SCHEDULE_SKEW_GRACE_MS) {
    return {
      code: "schedule_in_past",
      message:
        "That time has already passed. Pick a future time, or clear the schedule to run now.",
    };
  }
  if (at > now + SCHEDULE_MAX_LEAD_DAYS * 24 * 60 * 60 * 1000) {
    return {
      code: "schedule_too_far",
      message: `A run cannot be scheduled more than ${SCHEDULE_MAX_LEAD_DAYS} days ahead.`,
    };
  }
  return null;
}

/**
 * `min` / `max` for the wizard's `<input type="datetime-local">`, in the
 * local-naive format the control expects. Native bounds are a hint the
 * browser can enforce in its own picker; `validateSchedule` above is the real
 * gate, because a typed value can still land outside them.
 */
export function scheduleBounds(now: number = Date.now()): {
  min: string;
  max: string;
} {
  return {
    min: toLocalInputValue(new Date(now)),
    max: toLocalInputValue(
      new Date(now + SCHEDULE_MAX_LEAD_DAYS * 24 * 60 * 60 * 1000),
    ),
  };
}

/**
 * The numbers the expired notice quotes, taken from the run's own tasks.
 *
 * An expired run's tasks are all `expired` too (the control plane moves them
 * together), so the outstanding count is that set. It falls back to the whole
 * task list, and then to the list-response `task_count`/`site_count`, rather
 * than printing a confident zero: "0 updates are still outstanding" would be
 * the exact reassurance this notice exists to withhold.
 */
export function outstandingWork(run: UpdateRun): {
  updates: number;
  sites: number;
} {
  const tasks = run.tasks ?? [];
  const neverRan = tasks.filter((t) => t.status === "expired");
  const counted = neverRan.length > 0 ? neverRan : tasks;
  const sites = new Set(counted.map((t) => t.site_id)).size;
  return {
    updates: counted.length || run.task_count || 0,
    sites: sites || run.site_count || 0,
  };
}

/** "2026-08-19T02:00" in LOCAL time — `toISOString()` would silently shift to UTC. */
export function toLocalInputValue(at: Date): string {
  const pad = (n: number) => String(n).padStart(2, "0");
  return (
    `${at.getFullYear()}-${pad(at.getMonth() + 1)}-${pad(at.getDate())}` +
    `T${pad(at.getHours())}:${pad(at.getMinutes())}`
  );
}
