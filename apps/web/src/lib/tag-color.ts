import type { CSSProperties } from "react";

// GH #230 "rich tags" — deterministic tag coloring.
//
// A tag's color is either:
//   - "" (auto): the client derives a stable color from the tag's NAME via a
//     12-slot hash bucket. Two tenants (or two tags) with the same name always
//     land on the same hue, and the same tag never visibly "shuffles" color
//     across a re-render or a re-fetch.
//   - "#rrggbb": an explicit hex the operator picked from the 12-swatch picker
//     (UI never offers free hex entry). Rendered as a translucent chip with
//     the exact hue, identical in both themes (no light/dark swap needed
//     since alpha-over-surface already reads correctly on either background).
//
// The 12 hues are Tailwind's curated palette names, in a fixed order so the
// hash bucket -> hue mapping never changes across releases (changing the
// order would reshuffle every "auto" tag's color on next load).
//
// IMPORTANT: `tagChipClasses` returns STATIC, fully-spelled Tailwind class
// strings (never template-interpolated like `bg-${hue}-100`) so the Tailwind
// v4 JIT scanner can see every class name as a literal in this file's source
// and include it in the build. A template-interpolated class name is invisible
// to the scanner and would silently produce no styles in production.

export const TAG_HUES = [
  "red",
  "orange",
  "amber",
  "lime",
  "green",
  "teal",
  "cyan",
  "blue",
  "indigo",
  "violet",
  "fuchsia",
  "rose",
] as const;

export type TagHue = (typeof TAG_HUES)[number];

/**
 * FNV-1a 32-bit hash of the raw tag name, bucketed into one of the 12 curated
 * slots (0-11, indexing into TAG_HUES). Deterministic and pure: the same name
 * always maps to the same slot, in any browser, forever.
 */
export function tagSlot(name: string): number {
  let hash = 0x811c9dc5; // FNV offset basis
  for (let i = 0; i < name.length; i++) {
    hash ^= name.charCodeAt(i);
    // FNV prime multiplication, kept in 32-bit range via Math.imul.
    hash = Math.imul(hash, 0x01000193);
  }
  // Force unsigned before the modulo so the bucket index is never negative.
  return (hash >>> 0) % TAG_HUES.length;
}

/** The hue name for a given slot index (0-11). */
export function tagHue(slot: number): TagHue {
  return TAG_HUES[((slot % TAG_HUES.length) + TAG_HUES.length) % TAG_HUES.length]!;
}

// Chip surface classes per hue — light + dark in one static string. amber and
// lime use a darker `text-*-900` in light mode (rather than the `-800` every
// other hue uses) to hold AA contrast against their lighter-reading 100-level
// background.
const CHIP_CLASSES: Record<TagHue, string> = {
  red: "bg-red-100 text-red-800 border-red-200 dark:bg-red-950/40 dark:text-red-300 dark:border-red-900",
  orange:
    "bg-orange-100 text-orange-800 border-orange-200 dark:bg-orange-950/40 dark:text-orange-300 dark:border-orange-900",
  amber:
    "bg-amber-100 text-amber-900 border-amber-200 dark:bg-amber-950/40 dark:text-amber-300 dark:border-amber-900",
  lime: "bg-lime-100 text-lime-900 border-lime-200 dark:bg-lime-950/40 dark:text-lime-300 dark:border-lime-900",
  green:
    "bg-green-100 text-green-800 border-green-200 dark:bg-green-950/40 dark:text-green-300 dark:border-green-900",
  teal: "bg-teal-100 text-teal-800 border-teal-200 dark:bg-teal-950/40 dark:text-teal-300 dark:border-teal-900",
  cyan: "bg-cyan-100 text-cyan-800 border-cyan-200 dark:bg-cyan-950/40 dark:text-cyan-300 dark:border-cyan-900",
  blue: "bg-blue-100 text-blue-800 border-blue-200 dark:bg-blue-950/40 dark:text-blue-300 dark:border-blue-900",
  indigo:
    "bg-indigo-100 text-indigo-800 border-indigo-200 dark:bg-indigo-950/40 dark:text-indigo-300 dark:border-indigo-900",
  violet:
    "bg-violet-100 text-violet-800 border-violet-200 dark:bg-violet-950/40 dark:text-violet-300 dark:border-violet-900",
  fuchsia:
    "bg-fuchsia-100 text-fuchsia-800 border-fuchsia-200 dark:bg-fuchsia-950/40 dark:text-fuchsia-300 dark:border-fuchsia-900",
  rose: "bg-rose-100 text-rose-800 border-rose-200 dark:bg-rose-950/40 dark:text-rose-300 dark:border-rose-900",
};

// Solid -500 swatch classes, used by the color dot in the tag picker list and
// by the 12-swatch color picker's option buttons.
const DOT_CLASSES: Record<TagHue, string> = {
  red: "bg-red-500",
  orange: "bg-orange-500",
  amber: "bg-amber-500",
  lime: "bg-lime-500",
  green: "bg-green-500",
  teal: "bg-teal-500",
  cyan: "bg-cyan-500",
  blue: "bg-blue-500",
  indigo: "bg-indigo-500",
  violet: "bg-violet-500",
  fuchsia: "bg-fuchsia-500",
  rose: "bg-rose-500",
};

// The literal hex each hue's swatch writes when an operator picks it from the
// 12-swatch picker. These are Tailwind's standard -500 values so the swatch
// button (DOT_CLASSES) and the persisted hex always visually match.
const SWATCH_HEX: Record<TagHue, string> = {
  red: "#ef4444",
  orange: "#f97316",
  amber: "#f59e0b",
  lime: "#84cc16",
  green: "#22c55e",
  teal: "#14b8a6",
  cyan: "#06b6d4",
  blue: "#3b82f6",
  indigo: "#6366f1",
  violet: "#8b5cf6",
  fuchsia: "#d946ef",
  rose: "#f43f5e",
};

/** Ordered list of {hue, hex} for the 12-swatch color picker UI. */
export const TAG_SWATCHES: ReadonlyArray<{ hue: TagHue; hex: string }> =
  TAG_HUES.map((hue) => ({ hue, hex: SWATCH_HEX[hue] }));

// Reverse map: the 12 hex values the picker can ever WRITE, back to their
// hue. The UI never offers free hex entry — every "color" a tag can carry is
// either "" (auto) or one of exactly these 12 literal strings, OR a hex set
// by some OTHER API caller outside this UI (a genuinely foreign value).
// Adversarial-verify fix: rendering a KNOWN swatch hex via the inline alpha-
// wash path produced light-mode contrast trouble for lime/amber/cyan (the
// same reason CHIP_CLASSES special-cases their text shade to -900). Since we
// already have an AA-safe, per-theme class recipe for every one of the 12
// hues, a known hex should render through THAT recipe, identically to the
// "auto" path for a tag that happens to hash to the same hue — not through
// the inline style. The inline-style path is reserved for a hex that is NOT
// one of our 12 (set via the API directly, bypassing the picker).
const HEX_TO_HUE: ReadonlyMap<string, TagHue> = new Map(
  TAG_HUES.map((hue) => [SWATCH_HEX[hue].toLowerCase(), hue]),
);

/** Static Tailwind classes for a chip surface in the given slot. */
export function tagChipClasses(slot: number): string {
  return CHIP_CLASSES[tagHue(slot)];
}

/** Static Tailwind classes for a solid color dot in the given slot. */
export function tagDotClasses(slot: number): string {
  return DOT_CLASSES[tagHue(slot)];
}

export interface TagVisualStyle {
  /** Static Tailwind classes to apply (auto/hue path, or a known-swatch hex). */
  className?: string;
  /** Inline style to apply (a genuinely foreign hex, not one of the 12 swatches). */
  style?: CSSProperties;
}

const HEX_RE = /^#[0-9a-f]{6}$/i;

/**
 * Resolve a tag's chip surface styling. `color === ""` (or anything that
 * isn't a valid "#rrggbb") falls back to the deterministic auto-hue path
 * derived from the tag's name. A hex matching one of the 12 swatches renders
 * through that hue's AA-safe class recipe (identical to the auto path for
 * the same hue). Only a genuinely foreign hex (not one of the 12 — set via
 * the API, since this UI never offers free hex entry) renders as an inline
 * translucent style, identical in both themes.
 */
export function resolveTagStyle(tag: {
  name: string;
  color?: string | null;
}): TagVisualStyle {
  const color = tag.color?.trim();
  if (color && HEX_RE.test(color)) {
    const knownHue = HEX_TO_HUE.get(color.toLowerCase());
    if (knownHue) return { className: CHIP_CLASSES[knownHue] };
    return {
      style: {
        backgroundColor: `${color}26`, // ~15% alpha
        color,
        borderColor: `${color}4d`, // ~30% alpha
      },
    };
  }
  return { className: tagChipClasses(tagSlot(tag.name)) };
}

/**
 * Resolve a tag's solid dot color (used in the tag picker list and anywhere
 * else a small swatch preview is needed, distinct from the pale chip
 * surface). Same known-hex-vs-foreign-hex split as `resolveTagStyle`.
 */
export function resolveTagDot(tag: {
  name: string;
  color?: string | null;
}): TagVisualStyle {
  const color = tag.color?.trim();
  if (color && HEX_RE.test(color)) {
    const knownHue = HEX_TO_HUE.get(color.toLowerCase());
    if (knownHue) return { className: DOT_CLASSES[knownHue] };
    return { style: { backgroundColor: color } };
  }
  return { className: tagDotClasses(tagSlot(tag.name)) };
}
