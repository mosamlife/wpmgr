import type { SiteDiagnosticsCard } from "@wpmgr/api";

// ADR-037 Site-Health-Full — typed accessors for the `wp_native` card.
//
// The agent ships the verbatim WP_Debug_Data::debug_data() output under the
// `wp_native` category. WP's shape is open-ended (a third-party plugin may
// drop in extra sections via the `debug_information` filter) so we use a
// permissive Record<...> shape and treat unknown sections as optional.
//
// The shape WP emits is consistent though:
//
//   {
//     "wp-core": {
//       "label": "WordPress",
//       "description": "...",                // optional
//       "show_count": true,                  // optional
//       "private": false,                    // optional
//       "fields": {
//         "version": {
//           "label": "Version",
//           "value": "6.6.1",
//           "debug": "6.6.1"                 // optional; copy-friendly variant
//         },
//         ...
//       }
//     },
//     "wp-paths-sizes": { ... },
//     ...
//   }
//
// Some `fields[].value` are themselves nested name/value arrays (WP renders
// those as a sub-dl). We surface them as `Record<string, string>` when that
// shape is detected.

export interface WpNativeField {
  label?: string;
  value?: string | number | boolean | Record<string, unknown> | null;
  debug?: string | number | boolean | null;
  private?: boolean;
}

export interface WpNativeSection {
  label?: string;
  description?: string;
  show_count?: boolean;
  private?: boolean;
  fields?: Record<string, WpNativeField>;
  // The agent annotates wp-paths-sizes with a status tag describing the dirsize
  // walk. "ok" = fresh + complete; "partial" = at least one size timed out;
  // "unavailable" = get_sizes() missing/threw; "timeout" kept for back-compat
  // with v0.9.14 agents. v0.9.30+ (decoupled cron walk + last-good cache) adds
  // "stale" = showing the last successful scan while a background refresh runs,
  // and "pending" = first scan still computing (sizes not yet available).
  directory_size_status?:
    | "ok"
    | "partial"
    | "timeout"
    | "unavailable"
    | "stale"
    | "pending";
  // v0.9.30+: how the sizes were computed ("du" | "php" | "disk" | "cached")
  // and when (unix seconds), for a freshness hint.
  directory_size_method?: string;
  directory_size_computed_at?: number;
}

// WP_Debug_Data returns several strings HTML-entity-encoded (e.g. the
// directory-size placeholder is the literal "Loading&hellip;", and labels can
// carry "&amp;" / "&#8230;"). The agent ships them verbatim, so the UI must
// decode before rendering — otherwise the operator sees raw "Loading&hellip;"
// (the v0.9.14 bug). A tiny named-entity + numeric-entity decoder; we avoid a
// DOM round-trip so this is SSR-safe and dependency-free.
const NAMED_ENTITIES: Record<string, string> = {
  "&hellip;": "…",
  "&amp;": "&",
  "&lt;": "<",
  "&gt;": ">",
  "&quot;": '"',
  "&#039;": "'",
  "&#39;": "'",
  "&nbsp;": " ",
};

function decodeEntities(input: string): string {
  let out = input;
  for (const [entity, char] of Object.entries(NAMED_ENTITIES)) {
    out = out.split(entity).join(char);
  }
  // Numeric entities: &#8230; (decimal) and &#x2026; (hex). The replace
  // callback capture groups are typed `string` explicitly so the radix-parse
  // arguments are not `any`.
  out = out.replace(/&#(\d+);/g, (_m: string, dec: string) =>
    String.fromCodePoint(Number(dec)),
  );
  out = out.replace(/&#x([0-9a-fA-F]+);/g, (_m: string, hex: string) =>
    String.fromCodePoint(parseInt(hex, 16)),
  );
  return out;
}

// Stringify one leaf of a nested WP field value. WP nested values are flat
// strings in practice, but the type is `unknown`, so coerce safely instead of
// String()-ing a possible object into "[object Object]".
function leafToString(v: unknown): string {
  if (v == null) return "";
  if (typeof v === "string") return v;
  if (typeof v === "number" || typeof v === "boolean") return String(v);
  return JSON.stringify(v);
}

// A value WP emitted as a not-yet-computed placeholder. After the agent's
// get_sizes() merge these should never reach the UI, but if an old agent or a
// genuine timeout leaks one through we render an em-dash instead of the raw
// placeholder text.
function isPlaceholder(value: string): boolean {
  const v = value.trim().toLowerCase();
  return v === "loading…" || v === "loading..." || v === "loading&hellip;";
}

export type WpNativePayload = Record<string, WpNativeSection> & {
  _error?: string;
  _message?: string;
};

/** Pull the `wp_native` card payload, typed. Null when not yet ingested. */
export function asWpNative(
  card: SiteDiagnosticsCard | undefined,
): WpNativePayload | null {
  if (!card || card.payload == null) return null;
  if (typeof card.payload !== "object" || Array.isArray(card.payload)) {
    return null;
  }
  return card.payload as WpNativePayload;
}

/** Get a section by id (e.g. "wp-paths-sizes"). */
export function section(
  payload: WpNativePayload | null,
  id: string,
): WpNativeSection | null {
  if (!payload) return null;
  const v = payload[id];
  return v && typeof v === "object" && !Array.isArray(v) ? v : null;
}

/**
 * Pull a field's user-facing `value`. WP_Debug_Data sometimes nests the value
 * (e.g. wp-server.curl_version.value = "7.79.1, OpenSSL/1.1.1k"); we resolve
 * the common patterns and fall back to the field's `debug` if present.
 */
export function fieldValue(
  sec: WpNativeSection | null,
  key: string,
): string | null {
  if (!sec?.fields) return null;
  const f = sec.fields[key];
  if (!f) return null;
  if (f.value == null) {
    if (f.debug != null) return decodeEntities(String(f.debug));
    return null;
  }
  if (typeof f.value === "object") {
    // Nested name/value array — render as one line per pair. f.value is
    // narrowed to Record<string, unknown> here (the null case returned above),
    // so no assertion is needed; each leaf is coerced safely.
    const entries = Object.entries(f.value);
    return entries
      .map(([k, v]) => `${k}: ${decodeEntities(leafToString(v))}`)
      .join("; ");
  }
  const decoded = decodeEntities(String(f.value));
  // A leaked placeholder ("Loading…") means the size never resolved; show a
  // clean en-dash (data-absent glyph) rather than the raw marker text.
  return isPlaceholder(decoded) ? "–" : decoded;
}

/** Pull a numeric debug value when WP populated it (e.g. wordpress_size). */
export function fieldDebugNumber(
  sec: WpNativeSection | null,
  key: string,
): number | null {
  if (!sec?.fields) return null;
  const f = sec.fields[key];
  if (!f) return null;
  if (typeof f.debug === "number") return f.debug;
  if (typeof f.debug === "string" && !Number.isNaN(Number(f.debug))) {
    return Number(f.debug);
  }
  if (typeof f.value === "number") return f.value;
  return null;
}

/** Enumerate every field as { label, value } for tabular rendering. */
export function fieldRows(
  sec: WpNativeSection | null,
): Array<{ key: string; label: string; value: string }> {
  if (!sec?.fields) return [];
  return Object.entries(sec.fields).map(([key, f]) => ({
    key,
    label: f.label ?? key,
    value: fieldValue(sec, key) ?? "–",
  }));
}
