// PHP EOL calendar (php.net official):
//   - 8.1: 2025-12-31
//   - 8.2: 2026-12-31
//   - 8.3: 2027-12-31
//   - 8.4: 2028-12-31
//   - 8.5: 2029-12-31
// Client-side so the operator sees the countdown even when the CP has not
// enriched the diagnostics blob. The CP version of this table is the source of
// truth for any audit/alert; this is presentation only. Shared by the ribbon
// and the PHP card so the same chip reads identically in both places.

const PHP_EOL: Record<string, string> = {
  "8.1": "2025-12-31",
  "8.2": "2026-12-31",
  "8.3": "2027-12-31",
  "8.4": "2028-12-31",
  "8.5": "2029-12-31",
};

/** Days until the EOL date for `version` (negative when already past). */
export function phpEolDays(version: string): number | null {
  const parts = version.split(".");
  if (parts.length < 2) return null;
  const key = `${parts[0]}.${parts[1]}`;
  const eol = PHP_EOL[key];
  if (!eol) return null;
  const eolTs = Date.parse(`${eol}T00:00:00Z`);
  if (Number.isNaN(eolTs)) return null;
  return Math.round((eolTs - Date.now()) / 86400000);
}

/** Short human label for an EOL countdown, e.g. "EOL in 7mo" / "EOL 12d ago". */
export function eolDaysLabel(days: number): string {
  if (days < 0) return `EOL ${Math.abs(days)}d ago`;
  if (days < 30) return `EOL in ${days}d`;
  if (days < 365) return `EOL in ${Math.round(days / 30)}mo`;
  return `EOL in ${Math.round(days / 365)}y`;
}

export type EolTone = "warning" | "success";

/** Within 90 days (or past) → warning; otherwise success. */
export function eolTone(days: number): EolTone {
  return days < 90 ? "warning" : "success";
}

/** A self-contained PHP-EOL chip used by both the ribbon and the PHP card. */
export function eolChipClasses(tone: EolTone): string {
  return tone === "warning"
    ? "bg-warning-subtle text-warning-subtle-fg"
    : "bg-success-subtle text-success-subtle-fg";
}
