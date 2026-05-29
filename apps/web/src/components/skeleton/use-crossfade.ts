import { useEffect, useRef, useState, type CSSProperties } from "react";

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

export const CROSSFADE_DURATION_MS = 500;
const CROSSFADE_EASING = "cubic-bezier(0.16, 1, 0.3, 1)"; // --ease-out-expo

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

    clearAll();

    if (isLoading) {
      setPhase("loading");
      return clearAll;
    }

    // Loading just finished. Mount the content layer at opacity 0
    // ("entering"), then on the next animation frame flip to "fading" so
    // the CSS transition has a "from" value to interpolate from. After
    // 500ms unmount the skeleton.
    setPhase("entering");
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
