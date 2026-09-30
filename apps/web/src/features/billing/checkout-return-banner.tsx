import { CircleCheck, Loader2 } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  INDIAN_DECLINE_MESSAGE_NO_RAZORPAY,
  INDIAN_DECLINE_MESSAGE_WITH_RAZORPAY,
} from "./billing-copy";

// M16 Phase B/C, S0.4 (stripe-design-v9.md 3.2, 3.3, 3.4) — the
// checkout-return status banner shared by every surface that drives
// `useBillingCheckoutReturn` (`/settings/billing` and `/welcome/checkout`).
//
// "Activating…" while finalizing/polling (3.2 step 4), the Indian-card
// decline copy on `checkout=cancel` for a likely-Indian visitor with a "Pay
// with Razorpay" action when the instance offers it (3.3), and the "payment
// received, still activating" copy once the poll times out (3.4).

export function CheckoutReturnBanner({
  status,
  finalizing,
  timedOut,
  likelyIndian = false,
  razorpayAvailable = false,
  onPayWithRazorpay,
}: {
  status: "success" | "cancel" | undefined;
  finalizing: boolean;
  timedOut: boolean;
  /** Show the Indian-card decline copy instead of the generic cancel message. */
  likelyIndian?: boolean;
  /** Whether this instance has Razorpay registered, so the decline banner can offer it. */
  razorpayAvailable?: boolean;
  /** Starts a Razorpay checkout. Omit to hide the action even when `razorpayAvailable`. */
  onPayWithRazorpay?: () => void;
}) {
  if (!status) return null;

  if (status === "cancel") {
    if (likelyIndian) {
      const offerRazorpay = razorpayAvailable && Boolean(onPayWithRazorpay);
      return (
        <div
          role="status"
          className="space-y-2 rounded-lg border border-border bg-muted/40 px-4 py-3 text-sm text-muted-foreground"
        >
          <p>
            {offerRazorpay
              ? INDIAN_DECLINE_MESSAGE_WITH_RAZORPAY
              : INDIAN_DECLINE_MESSAGE_NO_RAZORPAY}
          </p>
          {offerRazorpay ? (
            <Button type="button" size="sm" onClick={onPayWithRazorpay}>
              Pay with Razorpay
            </Button>
          ) : null}
        </div>
      );
    }
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
        Activating…
      </div>
    );
  }

  if (timedOut) {
    return (
      <div
        role="status"
        className="rounded-lg border border-border bg-muted/40 px-4 py-3 text-sm text-muted-foreground"
      >
        Payment received. Activating your plan… You don't need to pay again.
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
