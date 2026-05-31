import { AnimatePresence, motion } from "motion/react";
import { RotateCcw, Sparkles, Trash2, X } from "lucide-react";

import { Button } from "@/components/ui/button";
import { drawerUp } from "@/lib/motion-presets";

// BulkActionBar — the sticky action toolbar that slides up (drawerUp subset)
// when one or more assets are selected. Shows the selection count and the three
// bulk actions: Optimize, Restore, Delete originals. Every action label shows
// the count (DESIGN: "Show counts in every bulk action"). Delete-originals is
// admin-gated + destructive (only shown when canDelete).

export interface BulkActionBarProps {
  selectedCount: number;
  /** Operator+: optimize/restore. */
  canOperate: boolean;
  /** Admin+: delete originals. */
  canDelete: boolean;
  onClear: () => void;
  onOptimize: () => void;
  onRestore: () => void;
  onDeleteOriginals: () => void;
}

export function BulkActionBar({
  selectedCount,
  canOperate,
  canDelete,
  onClear,
  onOptimize,
  onRestore,
  onDeleteOriginals,
}: BulkActionBarProps) {
  const show = selectedCount > 0;

  return (
    <AnimatePresence>
      {show ? (
        <motion.div
          key="bulk-bar"
          variants={drawerUp}
          initial="initial"
          animate="animate"
          exit="exit"
          role="toolbar"
          aria-label={`${selectedCount} assets selected`}
          className="sticky bottom-4 z-30 mx-auto flex w-fit max-w-full items-center gap-2 rounded-lg border border-[var(--color-border)] bg-[var(--color-card)] px-3 py-2 shadow-lg"
        >
          <div className="flex items-center gap-2 pr-1">
            <Button
              type="button"
              variant="ghost"
              size="icon"
              onClick={onClear}
              aria-label="Clear selection"
              className="size-7"
            >
              <X aria-hidden="true" className="size-4" />
            </Button>
            <span className="text-sm font-medium tabular-nums text-[var(--color-foreground)]">
              {selectedCount.toLocaleString()} selected
            </span>
          </div>

          <div className="h-5 w-px bg-[var(--color-border)]" aria-hidden="true" />

          {canOperate ? (
            <>
              <Button type="button" size="sm" onClick={onOptimize}>
                <Sparkles aria-hidden="true" className="size-4" />
                Optimize {selectedCount.toLocaleString()}
              </Button>
              <Button
                type="button"
                size="sm"
                variant="outline"
                onClick={onRestore}
              >
                <RotateCcw aria-hidden="true" className="size-4" />
                Restore {selectedCount.toLocaleString()}
              </Button>
            </>
          ) : null}

          {canDelete ? (
            <Button
              type="button"
              size="sm"
              variant="ghost"
              onClick={onDeleteOriginals}
              className="text-[var(--color-destructive)] hover:bg-[var(--color-destructive)]/10 hover:text-[var(--color-destructive)]"
            >
              <Trash2 aria-hidden="true" className="size-4" />
              Delete originals
            </Button>
          ) : null}
        </motion.div>
      ) : null}
    </AnimatePresence>
  );
}
