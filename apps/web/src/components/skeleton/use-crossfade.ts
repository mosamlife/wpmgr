import { useEffect, useRef, useState, type CSSProperties } from "react";

import { dur, ease } from "@/lib/motion-presets";

// Surface 4.13 — Crossfade helper.
//
// Manages the 500ms crossfade from skeleton to real content. The caller
// renders both layers (skeleton + content) simultaneously while the
// transition runs, then unmounts the skeleton once its opacity hits zero.
//
// Timing
// ------
// • Total crossfade window: 500ms (DESIGN.md --duration-slower).
// • Curve: ease-out — the new content "lands" rather than races in.
// • The skeleton fades to 0 while content fades to 1 over the same window,
//   so the two layers visually swap rather than wipe.
//
// Phase machine
// -------------
//   "loading"  → only skeleton mounted, full opacity
//   "entering" → both mounted, content opacity 0 (one paint)
//   "fading"   → both mounted, content opacity 1 → triggers transition
//   "loaded"   → only content mounted, full opacity
//
// The "entering" → "fading" hop is what actually drives the CSS transition.
// We need a paint at opacity 0 before flipping to 1, otherwise the browser
// has no "from" value to interpolate from and the layer just snaps in.
//
// Usage
// -----
//   const { showSkeleton, showContent, skeletonStyle, contentStyle } =
//     useCrossfade(isLoading);
//   return (
//     <div className="relative">
//       {showSkeleton ? (
//         <div className="absolute inset-0" style={skeletonStyle}>
//           <SitesTableSkeleton />
//         </div>
//       ) : null}
//       {showContent ? (
//         <div style={contentStyle}><SitesTable {...} /></div>
//       ) : null}
//     </div>
//   );

// Phase 5: the crossfade values come from @/lib/motion-presets so the timing
// matches the `skeletonToContent` transition every other surface imports. The
// CSS transition string is computed from the same numbers — one source of
// truth, no drift between JS and CSS layers.
export const CROSSFADE_DURATION_MS = dur.slower * 1000;
const CROSSFADE_EASING = `cubic-bezier(${ease.outExpo.join(", ")})`;

type CrossfadePhase = "loading" | "entering" | "fading" | "loaded";

export interface UseCrossfadeResult {
  /** True while the skeleton layer should be rendered. */
  showSkeleton: boolean;
  /** True while the content layer should be rendered. */
  showContent: boolean;
  /** Inline style for the skeleton layer (opacity + transition). */
  skeletonStyle: CSSProperties;
  /** Inline style for the content layer (opacity + transition). */
  contentStyle: CSSProperties;
}

export function useCrossfade(isLoading: boolean): UseCrossfadeResult {
  const [phase, setPhase] = useState<CrossfadePhase>(
    isLoading ? "loading" : "loaded",
  );
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const rafRef = useRef<number | null>(null);

  // Store the previous isLoading value in state (stored-prev-in-state pattern,
  // per React docs). Reading/writing refs during render is forbidden by
  // react-hooks/refs; using a state variable avoids that entirely. When
  // isLoading flips we call setPhase during the render pass — this is allowed;
  // the lint rule only bans synchronous setState inside effect bodies.
  const [prevIsLoading, setPrevIsLoading] = useState<boolean>(isLoading);
  if (prevIsLoading !== isLoading) {
    setPrevIsLoading(isLoading);
    // Apply the phase transition immediately in this render pass so the DOM
    // is already in the right shape when the effect fires.
    if (isLoading) {
      // Flipped back to loading (e.g. a refetch). The effect will cancel
      // any in-flight timers/rafs on its next run.
      setPhase("loading");
    } else {
      // Loading just finished. Mount the content layer at opacity 0
      // ("entering") so the DOM node exists before we start the CSS transition.
      setPhase("entering");
    }
  }

  useEffect(() => {
    const clearAll = () => {
      if (timerRef.current) {
        clearTimeout(timerRef.current);
        timerRef.current = null;
      }
      if (rafRef.current !== null) {
        cancelAnimationFrame(rafRef.current);
        rafRef.current = null;
      }
    };

    // Always cancel previous timers before starting new ones.
    clearAll();

    // When loading is true the render pass already set phase to "loading";
    // there is nothing async to schedule.
    if (isLoading) {
      return clearAll;
    }

    // The render pass already set phase to "entering". Schedule the rAF
    // double-pump to flip to "fading" so the CSS transition has a "from"
    // value to interpolate from. After CROSSFADE_DURATION_MS, unmount the
    // skeleton by transitioning to "loaded".
    rafRef.current = requestAnimationFrame(() => {
      rafRef.current = requestAnimationFrame(() => {
        setPhase("fading");
        timerRef.current = setTimeout(() => {
          setPhase("loaded");
          timerRef.current = null;
        }, CROSSFADE_DURATION_MS);
      });
    });

    return clearAll;
  }, [isLoading]);

  const transition = `opacity ${CROSSFADE_DURATION_MS}ms ${CROSSFADE_EASING}`;
  const showSkeleton = phase !== "loaded";
  const showContent = phase !== "loading";

  const skeletonOpacity = phase === "loading" || phase === "entering" ? 1 : 0;
  const contentOpacity = phase === "fading" || phase === "loaded" ? 1 : 0;

  const skeletonStyle: CSSProperties = {
    opacity: skeletonOpacity,
    transition,
    pointerEvents: phase === "loading" ? "auto" : "none",
  };

  const contentStyle: CSSProperties = {
    opacity: contentOpacity,
    transition,
    pointerEvents: phase === "loaded" ? "auto" : "none",
  };

  return { showSkeleton, showContent, skeletonStyle, contentStyle };
}
