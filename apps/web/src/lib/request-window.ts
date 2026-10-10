// How a request card says when a request was asked and when it closes.
//
// Both timestamps are RFC 3339 instants from the control plane (created_at and
// expires_at on a request), shown in the viewer's own time zone and locale. A
// window can run past midnight, and a bare time of day cannot tell a close at
// this hour tomorrow from one at this hour today, so the close names its day
// (the weekday and the date) unless it falls on the same calendar day as the
// ask. An unparsable timestamp renders as unparsable, never as "now" or blank.

export interface RequestWindowText {
  /** The ask, as a time of day. */
  readonly asked: string;
  /** The close: a time of day on the ask's day, the day and the time on any other. */
  readonly closes: string;
}

const UNREADABLE = "an unreadable time";

const TIME_OF_DAY: Intl.DateTimeFormatOptions = { hour: "2-digit", minute: "2-digit" };

const DAY_AND_TIME: Intl.DateTimeFormatOptions = {
  weekday: "short",
  month: "short",
  day: "numeric",
  ...TIME_OF_DAY,
};

function parse(iso: string): Date | null {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? null : d;
}

function sameLocalDay(a: Date, b: Date): boolean {
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
}

/**
 * The text of "Asked X, closes Y" for one request. When the ask cannot be read
 * the close cannot be placed on the ask's day, so it names its own.
 */
export function formatRequestWindow(createdIso: string, expiresIso: string): RequestWindowText {
  const asked = parse(createdIso);
  const closes = parse(expiresIso);
  let closesText = UNREADABLE;
  if (closes) {
    closesText =
      asked && sameLocalDay(asked, closes)
        ? closes.toLocaleTimeString([], TIME_OF_DAY)
        : closes.toLocaleString([], DAY_AND_TIME);
  }
  return {
    asked: asked ? asked.toLocaleTimeString([], TIME_OF_DAY) : UNREADABLE,
    closes: closesText,
  };
}
