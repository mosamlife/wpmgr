// Typed sub-value pickers for the diagnostics payload. Cards read fields off
// the raw `SiteDiagnosticsCard.payload` (an open-ended object) through these so
// the access is null-safe and type-narrowed in one place.
//
// Kept in their OWN module (not co-located with DiagnosticCard) so the
// react-refresh fast-refresh boundary stays clean: a file that exports a
// component should export ONLY components.
//
// We use an en-dash for "no value" (DESIGN.md bans em-dashes in prose; the
// data-absent glyph reads as "no value").
export const ABSENT = "–";

/** Get a string field from the raw payload, falling back to ABSENT. */
export function pickString(
  payload: unknown,
  key: string,
  fallback = ABSENT,
): string {
  if (
    payload &&
    typeof payload === "object" &&
    !Array.isArray(payload) &&
    key in (payload as Record<string, unknown>)
  ) {
    const v = (payload as Record<string, unknown>)[key];
    if (v === null || v === undefined || v === "") return fallback;
    if (typeof v === "string") return v;
    if (typeof v === "number" || typeof v === "boolean") return String(v);
  }
  return fallback;
}

/** Get a numeric field from the raw payload, falling back to `fallback`. */
export function pickNumber(payload: unknown, key: string, fallback = 0): number {
  if (
    payload &&
    typeof payload === "object" &&
    !Array.isArray(payload) &&
    key in (payload as Record<string, unknown>)
  ) {
    const v = (payload as Record<string, unknown>)[key];
    if (typeof v === "number") return v;
    if (typeof v === "string" && v.length > 0 && !Number.isNaN(Number(v))) {
      return Number(v);
    }
  }
  return fallback;
}

/** Get a boolean field from the raw payload, falling back to `fallback`. */
export function pickBool(
  payload: unknown,
  key: string,
  fallback = false,
): boolean {
  if (
    payload &&
    typeof payload === "object" &&
    !Array.isArray(payload) &&
    key in (payload as Record<string, unknown>)
  ) {
    const v = (payload as Record<string, unknown>)[key];
    if (typeof v === "boolean") return v;
    if (typeof v === "string") {
      return v === "true" || v === "1" || v === "on" || v === "yes";
    }
    if (typeof v === "number") return v !== 0;
  }
  return fallback;
}
