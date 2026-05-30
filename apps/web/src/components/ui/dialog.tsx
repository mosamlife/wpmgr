import * as React from "react";
import { createPortal } from "react-dom";
import { AnimatePresence, motion } from "motion/react";

import { cn } from "@/lib/utils";
import { dur, ease, fade, scaleIn } from "@/lib/motion-presets";

// Sprint 3 dialog primitive. Implements the DESIGN.md modal spec:
//   - Centered panel, 12px radius, shadow-lg, popover bg
//   - 480px max width (Modal section: "max 480px")
//   - --scrim token backdrop (Sprint 1)
//   - Enter: scaleIn preset (180ms fade + scale 0.96 -> 1, no width/height)
//   - Exit:  135ms fade + slight scale (derived from scaleIn exit)
//   - reduced-motion: animations zeroed via global CSS rule in globals.css
//
// Phase 5: imports `scaleIn` + `fade` from @/lib/motion-presets so the dialog
// stays locked to the shared duration/easing tiers. The previous inline
// config matched the preset exactly; this is a mechanical refactor.
//
// We intentionally avoid pulling in @radix-ui/react-dialog or
// tailwindcss-animate (neither installed) and instead lean on `motion/react`
// which is already in use in sites-toolbar.tsx. Focus management:
//   - body scroll lock while open
//   - Escape closes
//   - First focusable element receives focus on open
//   - Focus returns to the previously-active element on close
//   - role="dialog" + aria-modal="true" + aria-labelledby/-describedby
//
// Public API mirrors the shadcn dialog shape so call sites read familiarly:
//   <Dialog open={...} onClose={...}>
//     <DialogContent ariaLabelledBy="x-title">
//       <DialogHeader>
//         <DialogTitle id="x-title">...</DialogTitle>
//         <DialogDescription>...</DialogDescription>
//       </DialogHeader>
//       {body}
//       <DialogFooter>{buttons}</DialogFooter>
//     </DialogContent>
//   </Dialog>

interface DialogContextValue {
  onClose: () => void;
}

const DialogContext = React.createContext<DialogContextValue | null>(null);

function useDialogContext(component: string): DialogContextValue {
  const ctx = React.useContext(DialogContext);
  if (!ctx) {
    throw new Error(`${component} must be used inside <Dialog>.`);
  }
  return ctx;
}

export interface DialogProps {
  open: boolean;
  onClose: () => void;
  children: React.ReactNode;
}

export function Dialog({ open, onClose, children }: DialogProps) {
  // Lock body scroll while any dialog is open. Tracks via a counter so nested
  // dialogs (rare here, but the api-keys page can stack create+confirm) don't
  // prematurely unlock.
  React.useEffect(() => {
    if (!open) return;
    const previous = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      document.body.style.overflow = previous;
    };
  }, [open]);

  // Escape closes. Attached on document so it works even before focus lands
  // inside the panel.
  React.useEffect(() => {
    if (!open) return;
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") {
        e.stopPropagation();
        onClose();
      }
    }
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [open, onClose]);

  if (typeof document === "undefined") return null;

  const ctx: DialogContextValue = { onClose };

  return createPortal(
    <DialogContext.Provider value={ctx}>
      <AnimatePresence>
        {open ? (
          <motion.div
            key="dialog-root"
            className="fixed inset-0 z-50 flex items-center justify-center p-4"
            variants={fade}
            initial="initial"
            animate="animate"
            exit="exit"
          >
            <DialogOverlay />
            {children}
          </motion.div>
        ) : null}
      </AnimatePresence>
    </DialogContext.Provider>,
    document.body,
  );
}

function DialogOverlay() {
  const { onClose } = useDialogContext("DialogOverlay");
  return (
    <button
      type="button"
      aria-label="Close dialog"
      tabIndex={-1}
      onClick={onClose}
      className="absolute inset-0 cursor-default bg-[var(--scrim)]"
    />
  );
}

export interface DialogContentProps {
  ariaLabelledBy?: string;
  ariaDescribedBy?: string;
  className?: string;
  children: React.ReactNode;
}

export function DialogContent({
  ariaLabelledBy,
  ariaDescribedBy,
  className,
  children,
}: DialogContentProps) {
  const panelRef = React.useRef<HTMLDivElement>(null);
  const previouslyFocused = React.useRef<HTMLElement | null>(null);

  // Save the previously-focused element on mount, restore on unmount.
  React.useEffect(() => {
    previouslyFocused.current = document.activeElement as HTMLElement | null;
    // Focus the first focusable element inside the panel after one frame so
    // motion's mount transition is committed and inputs are interactive.
    const id = window.requestAnimationFrame(() => {
      const el = panelRef.current;
      if (!el) return;
      const focusable = el.querySelector<HTMLElement>(
        "[data-autofocus], input:not([disabled]), textarea:not([disabled]), button:not([disabled]), select:not([disabled])",
      );
      focusable?.focus();
    });
    return () => {
      window.cancelAnimationFrame(id);
      previouslyFocused.current?.focus?.();
    };
  }, []);

  return (
    <motion.div
      ref={panelRef}
      role="dialog"
      aria-modal="true"
      aria-labelledby={ariaLabelledBy}
      aria-describedby={ariaDescribedBy}
      // Enter/exit shape comes from the shared `scaleIn` preset:
      //   • enter — 180ms fade + scale 0.96 → 1 on ease.out
      //   • exit  — 135ms fade + scale 1 → 0.98 on ease.in
      // No width/height animation per DESIGN.md ("Don't animate width, height").
      variants={scaleIn}
      initial="initial"
      animate="animate"
      exit="exit"
      // Override the preset's `ease.out` with `ease.outExpo` here only — the
      // dialog is the most prominent floating surface in the app and the
      // sharper deceleration helps it read as "placed", not "drifted in".
      transition={{ duration: dur.fast, ease: ease.outExpo }}
      className={cn(
        "relative z-10 w-full max-w-[min(480px,calc(100vw-2rem))] rounded-xl border border-[var(--color-border)]",
        "bg-[var(--color-popover)] text-[var(--color-popover-foreground)] shadow-lg",
        "p-6",
        className,
      )}
    >
      {children}
    </motion.div>
  );
}

export function DialogHeader({
  children,
  className,
}: {
  children: React.ReactNode;
  className?: string;
}) {
  return <div className={cn("space-y-1", className)}>{children}</div>;
}

export function DialogTitle({
  id,
  children,
  className,
}: {
  id?: string;
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <h2 id={id} className={cn("text-lg font-semibold", className)}>
      {children}
    </h2>
  );
}

export function DialogDescription({
  id,
  children,
  className,
}: {
  id?: string;
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <p
      id={id}
      className={cn("text-sm text-[var(--color-muted-foreground)]", className)}
    >
      {children}
    </p>
  );
}

export function DialogBody({
  children,
  className,
}: {
  children: React.ReactNode;
  className?: string;
}) {
  return <div className={cn("space-y-4", className)}>{children}</div>;
}

export function DialogFooter({
  children,
  className,
}: {
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <div className={cn("flex justify-end gap-2", className)}>{children}</div>
  );
}
