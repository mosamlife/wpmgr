import { CircleCheck, Loader2 } from "lucide-react";

// M16 Phase B/C — the checkout-return status banner shared by every surface
// that drives `useBillingCheckoutReturn` (`/settings/billing` and
// `/welcome/checkout`). Extracted verbatim out of billing.tsx's former
// private `CheckoutReturnBanner` — a pure lift, not a behavior change (see
// -billing.test.tsx, still green).

export function CheckoutReturnBanner({
  status,
  finalizing,
  timedOut,
}: {
  status: "success" | "cancel" | undefined;
  finalizing: boolean;
  timedOut: boolean;
}) {
  if (!status) return null;

  if (status === "cancel") {
    return (
      <div
        role="status"
        className="rounded-lg border border-border bg-muted/40 px-4 py-3 text-sm text-muted-foreground"
      >
        Checkout was canceled. No changes were made to your plan.
      </div>
    );
  }

  if (finalizing) {
    return (
      <div
        role="status"
        aria-live="polite"
        className="flex items-center gap-2 rounded-lg border border-border bg-muted/40 px-4 py-3 text-sm text-foreground"
      >
        <Loader2 aria-hidden="true" className="size-4 animate-spin" />
        Finalizing your subscription…
      </div>
    );
  }

  if (timedOut) {
    return (
      <div
        role="status"
        className="rounded-lg border border-border bg-muted/40 px-4 py-3 text-sm text-muted-foreground"
      >
        Payment received. It can take a few minutes for your plan to update
        here.
      </div>
    );
  }

  return (
    <div
      role="status"
      className="flex items-center gap-2 rounded-lg bg-[var(--color-success-subtle)] px-4 py-3 text-sm text-[var(--color-success-subtle-fg)]"
    >
      <CircleCheck aria-hidden="true" className="size-4" />
      Subscription updated.
    </div>
  );
}
