import type { Transition, Variants } from "motion/react";

// Phase 5 / Motion presets — the single source of truth for UI motion across
// the WPMgr dashboard. Surfaces import from here so duration tiers, easing
// curves, and enter/exit shapes stay locked across the app and a future
// audit can grep one file.
//
// Hard rules (DESIGN.md "Motion" + PRODUCT.md "Banned visual patterns"):
//   • Animate ONLY transform (x/y/scale/rotate) + opacity.
//   • NEVER width, height, top, left, margin, padding.
//   • NEVER bounce, elastic, or spring easings on UI surfaces.
//   • Duration tiers: 100 / 180 / 240 / 340 / 500ms.
//   • Exit ≈ 75% of enter — destinations slide off faster than they slid in.
//   • Reduced-motion fallback is handled GLOBALLY via the
//     `prefers-reduced-motion` CSS rule in styles/globals.css (transition +
//     animation durations are forced to ~0ms). Wrap with `useReducedMotion()`
//     ONLY when the conditional logic itself needs to know.
//   • Stagger total cap: 500ms. NEVER re-stagger on re-fetch — initial mount
//     only.
//
// Surface mapping:
//
//   Surface                           Preset
//   ─────────────────────────────────────────────
//   Dialog / modal enter+exit         scaleIn
//   Bulk action drawer slide          drawerUp  (currently CSS-driven; see note)
//   Toolbar mode swap                 motion.div layout + dur.base + ease.out
//   Sticky save bar slide-up          drawerUp  (subset: y only)
//   Sites row initial mount stagger   stagger (parent) + fadeUp (children)
//   Skeleton → content crossfade      skeletonToContent (opacity only, 500ms)
//   Status-dot one-shot pulse         statusPulse (scale + opacity, 600ms)
//
// Intentional exception — the command palette uses CSS keyframes driven by
// cmdk's data-state attribute (see globals.css `.wpmgr-cmdk-*`). The motion
// shape matches `scaleIn` (scale 0.96 → 1, 180ms ease-out-quint) but CSS is
// the right tool there because cmdk owns the state machine. Do not migrate
// the palette to motion; it works and the shape is documented for parity.
//
// Intentional exception — Sonner (toast surface) keeps its own slide-in
// motion. The global reduced-motion CSS rule collapses Sonner's keyframes
// to ~0ms, so the contract is honoured without an override. If we ever
// want to portal toasts through a custom motion component we'd wire
// `fadeUp` here; right now sonner's defaults read identically.

// ---------------------------------------------------------------------------
// Tokens
// ---------------------------------------------------------------------------

/**
 * Easing curves. Mirrors the `--ease-*` CSS custom properties declared in
 * styles/globals.css so JS-driven motion lands on the same curves as CSS
 * transitions.
 */
export const ease = {
  /** Default UI easing. Smooth deceleration; works for almost everything. */
  out: [0.25, 1, 0.5, 1] as const,
  /** Slightly snappier than `out`. Reserved for slower hero motions. */
  outQuint: [0.22, 1, 0.36, 1] as const,
  /** Most decisive deceleration. Reserved for the largest entrances. */
  outExpo: [0.16, 1, 0.3, 1] as const,
  /** Inverse of `out`. Used for exits so destinations accelerate offscreen. */
  in: [0.5, 0, 0.75, 0] as const,
  /** Symmetrical for state changes that read as a swap rather than entrance. */
  inOut: [0.65, 0, 0.35, 1] as const,
};

/**
 * Duration tiers, in seconds (the unit `motion/react` expects). Matches the
 * `--duration-*` CSS custom properties in styles/globals.css.
 *
 *   instant = 100ms  — feedback (button press, hover tone shift)
 *   fast    = 180ms  — state changes (menu open, dialog enter)
 *   base    = 240ms  — default UI transitions (toolbar mode swap)
 *   slow    = 340ms  — layout-class changes (drawer slide-up)
 *   slower  = 500ms  — entrance/crossfade hero motions
 */
export const dur = {
  instant: 0.1,
  fast: 0.18,
  base: 0.24,
  slow: 0.34,
  slower: 0.5,
};

// ---------------------------------------------------------------------------
// Variants
// ---------------------------------------------------------------------------

/**
 * fadeUp — opacity 0 + 6px y-translation on enter; reverses on exit.
 *
 * Default for content that "lands" into view (list items on first mount,
 * toasts via a motion wrapper, single-row reveals). Y-distance is
 * intentionally small so the motion reads as confirmation, not theatre.
 */
export const fadeUp: Variants = {
  initial: { opacity: 0, y: 6 },
  animate: {
    opacity: 1,
    y: 0,
    transition: { duration: dur.base, ease: ease.out },
  },
  exit: {
    opacity: 0,
    y: 4,
    transition: { duration: dur.base * 0.75, ease: ease.in },
  },
};

/**
 * fade — opacity only. Use when no spatial relationship implies direction
 * (e.g. cross-fading between two siblings of the same bounds).
 */
export const fade: Variants = {
  initial: { opacity: 0 },
  animate: {
    opacity: 1,
    transition: { duration: dur.fast, ease: ease.out },
  },
  exit: {
    opacity: 0,
    transition: { duration: dur.fast * 0.75, ease: ease.in },
  },
};

/**
 * scaleIn — opacity 0 + scale 0.96 → 1. The signature for modal / palette /
 * popover entrances. The scale lift is what makes a floating surface read as
 * separate from the page beneath, replacing the dropped shadow that other
 * motion would require.
 */
export const scaleIn: Variants = {
  initial: { opacity: 0, scale: 0.96 },
  animate: {
    opacity: 1,
    scale: 1,
    transition: { duration: dur.fast, ease: ease.out },
  },
  exit: {
    opacity: 0,
    scale: 0.98,
    transition: { duration: dur.fast * 0.75, ease: ease.in },
  },
};

/**
 * drawerUp — translateY(100%) → 0. Slide-up surfaces (bulk action drawer,
 * sticky save bar). `outQuint` so the panel decelerates decisively at rest.
 */
export const drawerUp: Variants = {
  initial: { y: "100%" },
  animate: {
    y: 0,
    transition: { duration: dur.slow, ease: ease.outQuint },
  },
  exit: {
    y: "100%",
    transition: { duration: dur.base, ease: ease.in },
  },
};

/**
 * stagger — parent variant for list-mount choreography. 40ms between
 * children with a 20ms head start means 10 items finish in 420ms (under
 * the 500ms cap). Use ONLY on the initial mount of a list, NEVER on
 * re-fetch.
 */
export const stagger: Variants = {
  animate: {
    transition: { staggerChildren: 0.04, delayChildren: 0.02 },
  },
};

// ---------------------------------------------------------------------------
// Transitions
// ---------------------------------------------------------------------------

/**
 * skeletonToContent — the 500ms opacity crossfade from skeleton placeholders
 * to real content. Long enough to read as "the page settled" rather than a
 * flicker. `ease.outExpo` so the content layer lands rather than fades.
 *
 * Consumed by both CSS (via `transition` style) and motion components.
 */
export const skeletonToContent: Transition = {
  duration: dur.slower,
  ease: ease.outExpo,
};

/**
 * statusPulse — a ONE-SHOT scale + opacity pulse used when a StatusDot
 * transitions tone (e.g. site goes Down). Read as "something just
 * happened here", not as a perpetual attention magnet. 600ms total because
 * the operator's eye needs to register the change without it dragging.
 *
 * NOT a perpetual loop — drive it via a key prop that increments on tone
 * change so motion runs it from start to finish once per change.
 *
 * Returned as a factory so each call hands motion a fresh, mutable object —
 * `motion/react`'s `animate` target rejects `as const` keyframes because
 * it mutates the input internally. The factory keeps the source-of-truth
 * shape readable while still satisfying the type.
 */
export const statusPulse = (): {
  scale: number[];
  opacity: number[];
  transition: Transition;
} => ({
  scale: [1, 1.15, 1],
  opacity: [1, 0.85, 1],
  transition: { duration: 0.6, ease: [...ease.out] },
});
