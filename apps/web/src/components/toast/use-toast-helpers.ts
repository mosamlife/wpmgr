import { toast as sonner } from "sonner";
import { AlertCircle } from "lucide-react";
import { createElement } from "react";

// Surface 4.15 — Verb-first toast helpers.
//
// Sonner is general-purpose; WPMgr is opinionated. Every toast in this app
// follows three rules from DESIGN.md and PRODUCT.md:
//
//   1. The action is a VERB. "Undo", "View details", "Open snapshot",
//      "Try again". Never "OK" or "Dismiss". The action is what to DO next,
//      not an acknowledgement.
//   2. Duration is severity-tiered. Success vanishes in 5s; errors linger 8s
//      so operators can read them; destructive notifications stay until the
//      operator acts (Infinity).
//   3. Destructive toasts REQUIRE an action — this is enforced by the
//      TypeScript type, not by convention. A backup that failed mid-restore
//      MUST offer "Try again" or "Open snapshot"; never a silent dead-end.
//
// Call sites import { toast } from "@/components/toast/use-toast-helpers" and
// pick the right severity. The underlying Sonner state machine handles
// stacking, hover-pause, swipe-to-dismiss, and reduced-motion.

/** An action label MUST start with a verb. */
export interface ToastAction {
  /** Verb-first label, e.g. "Undo", "View details", "Try again". */
  label: string;
  onClick: () => void;
}

interface CommonOpts {
  description?: string;
  action?: ToastAction;
}

interface DestructiveOpts {
  description?: string;
  /** Required: destructive toasts MUST offer a next step. */
  action: ToastAction;
}

interface PromiseOpts {
  loading: string;
  success: string;
  error: string;
}

function toAction(a: ToastAction | undefined) {
  // Sonner's Action.onClick signature includes the MouseEvent; we adapt our
  // verb-action callbacks to that shape without leaking the event detail to
  // call sites (which never care about it).
  if (!a) return undefined;
  return { label: a.label, onClick: () => a.onClick() };
}

export const toast = {
  /** Auto-dismisses in 5s. Action optional (e.g. "Undo", "View details"). */
  success: (title: string, opts: CommonOpts = {}) => {
    return sonner.success(title, {
      description: opts.description,
      action: toAction(opts.action),
      duration: 5000,
    });
  },

  /** Auto-dismisses in 8s. Action recommended (e.g. "Try again"). */
  error: (title: string, opts: CommonOpts = {}) => {
    return sonner.error(title, {
      description: opts.description,
      action: toAction(opts.action),
      duration: 8000,
    });
  },

  /**
   * Manual-close only. Used for irreversible operations that need explicit
   * acknowledgement — e.g. "Backup restore failed; site may be in a partial
   * state". The action is REQUIRED at the type level so we can never ship a
   * destructive toast without a next step.
   */
  destructive: (title: string, opts: DestructiveOpts) => {
    // We render destructive on top of the error type so it picks up the same
    // red XCircle icon. The custom AlertCircle icon would conflict with the
    // existing destructive token palette (destructive uses XCircle elsewhere
    // in the shell). Duration: Infinity, so the operator must engage.
    return sonner.error(title, {
      description: opts.description,
      action: toAction(opts.action),
      duration: Infinity,
      // Inline AlertCircle icon swap so destructive reads visually distinct
      // from a transient error. Coloured destructive token, not error red.
      icon: createElement(AlertCircle, {
        "aria-hidden": "true",
        className: "size-4 text-[var(--color-destructive)]",
      }),
    });
  },

  /** Auto-dismisses in 5s. Neutral; used for "Opening site..." style notices. */
  info: (title: string, opts: CommonOpts = {}) => {
    return sonner.message(title, {
      description: opts.description,
      action: toAction(opts.action),
      duration: 5000,
    });
  },

  /**
   * Promise-bound toast. Shows `loading` immediately, swaps to `success` or
   * `error` based on resolution. Use for mutations where the operator
   * benefits from a single thread of feedback (e.g. an update run).
   */
  promise: <T,>(promise: Promise<T>, opts: PromiseOpts) => {
    return sonner.promise(promise, opts);
  },

  /** Manual dismiss. Pass the id returned from any helper above. */
  dismiss: (id?: number | string) => sonner.dismiss(id),
};

export type Toast = typeof toast;
