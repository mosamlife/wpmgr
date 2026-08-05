// GH #349. The Sites list order-by axis.
//
// The wire values are pinned by the contract for GET /api/v1/sites:
//
//   name | -name | created_at | -created_at | last_seen | -last_seen
//
// A leading "-" is DESCENDING. Absent behaves exactly as the list always has:
// -created_at. The server 422s an unrecognised value rather than silently
// falling back, so the web side validates before it sends: the URL parser
// (searchSchema in routes/_authed/sites/index.tsx) rejects anything outside
// this list, which means an invalid value can never reach the request.
//
// These are wire values, not labels. Nothing an operator reads comes from this
// list directly; SITE_SORT_LABELS below owns the copy, and it deliberately
// speaks about sites ("Newest first") rather than about columns.

export const SITE_SORT_VALUES = [
  "name",
  "-name",
  "created_at",
  "-created_at",
  "last_seen",
  "-last_seen",
] as const;

export type SiteSort = (typeof SITE_SORT_VALUES)[number];

/**
 * What the list does when no order is chosen. Matches the server's own
 * default, so an absent `sort` and an explicit `-created_at` return the same
 * rows in the same order.
 */
export const DEFAULT_SITE_SORT: SiteSort = "-created_at";

/**
 * Operator-facing labels. "Recently active" reads off `last_seen_at`, the
 * timestamp of the site's last agent check-in.
 */
export const SITE_SORT_LABELS: Record<SiteSort, string> = {
  "-created_at": "Newest first",
  created_at: "Oldest first",
  name: "Name (A to Z)",
  "-name": "Name (Z to A)",
  "-last_seen": "Recently active",
  last_seen: "Least recently active",
};

/** Menu order: the two most-reached-for orders first, then name, then activity. */
export const SITE_SORT_MENU: readonly SiteSort[] = [
  "-created_at",
  "created_at",
  "name",
  "-name",
  "-last_seen",
  "last_seen",
];

/**
 * Both activity orders put sites that have never checked in at the END.
 *
 * A site with no `last_seen_at` has no activity to compare, so it is not
 * "least recently active", it is unknown. Sorting unknowns to the top of
 * either direction would bury the rows the operator opened the order for.
 * The menu says so out loud (ORDER_HINT below) rather than leaving the
 * operator to work it out from a screen of blanks.
 */
export const NEVER_SEEN_HINT =
  "Sites that have never checked in are listed last.";

/** Is this an order the contract accepts? Guards URL and stored values. */
export function isSiteSort(value: unknown): value is SiteSort {
  return (
    typeof value === "string" &&
    (SITE_SORT_VALUES as readonly string[]).includes(value)
  );
}

/** The label for the applied order, falling back to the default's label. */
export function siteSortLabel(sort: SiteSort | undefined): string {
  return SITE_SORT_LABELS[sort ?? DEFAULT_SITE_SORT];
}
