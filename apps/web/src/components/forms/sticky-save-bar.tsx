import { AnimatePresence, motion } from "motion/react";

import { Button } from "@/components/ui/button";
import { useShellState } from "@/components/layout/app-shell-context";
import { cn } from "@/lib/utils";
import { dur, ease } from "@/lib/motion-presets";

// Phase 4 / Sprint 4 surface 4.14 - Forms.
//
// Sticky "Save changes" / "Discard changes" bar that appears whenever a form
// has a dirty react-hook-form state. Replaces per-section save buttons across
// the app (backup schedules, alert config, future settings surfaces) so a
// single, predictable affordance covers every editable surface.
//
// Behavior (from DESIGN.md "Components" + the Sprint 4 spec):
//   - Pinned to the viewport bottom, offset by the 240px sidebar (or 64px
//     when collapsed). Mobile (<768px) hides the sidebar entirely so the bar
//     spans the full width.
//   - Slides up from y:100 -> y:0 on dirty, slides back down on clean. Uses
//     motion's `out-quart`-like ease ([0.25, 1, 0.5, 1]) over 240ms to match
//     the toolbar/dialog feel used elsewhere in the app.
//   - `prefers-reduced-motion` collapses the slide to a plain opacity fade
//     via the global CSS rule in globals.css; no custom branching needed.
//   - Sits at `shadow-md` because it literally floats above the page
//     (DESIGN.md "Elevation & Depth" - shadows reserved for floaters).
//   - Left side: a small primary dot + "Unsaved changes" muted label.
//     Optional inline error message sits between the dirty label and the
//     buttons in `text-destructive`.
//   - Right side: `[Discard changes] [Save changes]`. Verb-first labels per
//     DESIGN.md "Do's and Don'ts".

export interface StickySaveBarProps {
  /** react-hook-form `formState.isDirty`. The bar is hidden when false. */
  isDirty: boolean;
  /** Server mutation in flight. Disables both buttons and swaps Save label. */
  isPending?: boolean;
  /** Inline mutation error, surfaced between label and buttons. */
  errorMessage?: string | null;
  /** Save handler (usually `form.handleSubmit(onSubmit)`). */
  onSave: () => void | Promise<void>;
  /** Discard handler (usually `() => form.reset()`). */
  onDiscard: () => void;
  /** Override the default "Save changes" label (e.g. "Apply policy"). */
  saveLabel?: string;
  /** Override the default "Discard changes" label. */
  discardLabel?: string;
}

/**
 * Sticky bottom save bar. Render once at the form root and pass the
 * react-hook-form state. The bar handles its own enter/exit animation via
 * `AnimatePresence`.
 */
export function StickySaveBar({
  isDirty,
  isPending = false,
  errorMessage,
  onSave,
  onDiscard,
  saveLabel = "Save changes",
  discardLabel = "Discard changes",
}: StickySaveBarProps) {
  const { collapsed } = useShellState();

  // Static class strings so Tailwind extracts both at build time. The bar
  // spans full width on mobile (sidebar hidden behind a drawer) and offsets
  // by the sidebar's resting width on >=md screens.
  const offsetClass = collapsed
    ? "md:left-[64px]"
    : "md:left-[240px]";

  return (
    <AnimatePresence>
      {isDirty ? (
        <motion.div
          role="region"
          aria-label="Unsaved changes"
          // Phase 5: same shape as the `drawerUp` preset but expressed in
          // pixels (the bar is already pinned to the bottom edge so the
          // translation is the bar's own 64px height, not 100% of viewport).
          // dur.base + ease.out lock it to the same tier as the toolbar and
          // dialog so unrelated surfaces don't race each other on screen.
          initial={{ y: 100 }}
          animate={{ y: 0 }}
          exit={{ y: 100 }}
          transition={{ duration: dur.base, ease: ease.out }}
          className={cn(
            "fixed bottom-0 left-0 right-0 z-30 flex h-16 items-center justify-between gap-4 border-t border-border bg-card px-6 shadow-md",
            "motion-reduce:transition-opacity motion-reduce:duration-150",
            offsetClass,
          )}
        >
          <div className="flex min-w-0 items-center gap-3 text-sm">
            <span
              aria-hidden="true"
              className="inline-block size-2 rounded-full bg-[var(--color-primary)]"
            />
            <span className="text-muted-foreground">Unsaved changes</span>
            {errorMessage ? (
              <span
                role="alert"
                className="truncate text-destructive"
                title={errorMessage}
              >
                {errorMessage}
              </span>
            ) : null}
          </div>

          <div className="flex shrink-0 items-center gap-2">
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={onDiscard}
              disabled={isPending}
            >
              {discardLabel}
            </Button>
            <Button
              type="button"
              size="sm"
              onClick={() => void onSave()}
              disabled={isPending}
            >
              {isPending ? "Saving…" : saveLabel}
            </Button>
          </div>
        </motion.div>
      ) : null}
    </AnimatePresence>
  );
}
