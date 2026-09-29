import { useState } from "react";

import { toast } from "@/components/toast";

import {
  useCreateBillingCheckout,
  useVerifyRazorpayCheckout,
  type BillingProvider,
  type CheckoutTierId,
  type RazorpayCheckoutData,
} from "./use-billing";
import type { BillingCheckoutError } from "./billing-checkout-error";
import { loadRazorpayCheckout, type RazorpayHandlerResponse } from "./razorpay-checkout";

// M16 Phase B/C, S0.4 (stripe-design-v9.md 5.10) — the checkout machinery
// shared by every surface that can start a paid-tier checkout (today:
// /settings/billing's `PlanTiersGrid`; /welcome/checkout's post-verify
// screen reuses this exact hook rather than a second copy).
//
// Encapsulates:
//   - the provider (Stripe/Razorpay) selection state that feeds
//     `PaymentMethodPicker`. Razorpay is INR-only (decision 17) — there is no
//     currency choice any more; `startCheckout` always sends `currency:
//     "INR"` once Razorpay is selected;
//   - POST /billing/checkout via `useCreateBillingCheckout`;
//   - the Stripe path: a hosted-redirect `{ url }` -> `window.location.href`;
//   - the Razorpay path: opening the Checkout.js modal, then on a successful
//     payment POSTing /billing/checkout/verify (UX-confirmation only) and
//     calling the caller's `onCheckoutSuccess` regardless of verify's own
//     outcome — the caller's own poll (`useBillingCheckoutReturn`) remains
//     the sole source of truth for the actual plan flip;
//   - the "Razorpay failed to load" fallback toast.

export interface UseCheckoutFlowOptions {
  /**
   * Called once the Razorpay verify call SETTLES (success or failure) after
   * a successful in-modal payment. Never called on the Stripe path — Stripe's
   * own hosted-redirect return URL is what lands the browser back on
   * `?checkout=success`, so there is no in-page event to hook there. The
   * caller is expected to flip whatever "finalizing your subscription" state
   * it drives off of (see billing.tsx's `markCheckoutSuccess`).
   */
  onCheckoutSuccess: () => void;
  /** Preselect Razorpay as the initial provider (e.g. the decline banner's "Pay with Razorpay" action, or a tenant already pinned to Razorpay). Defaults to "stripe" for everyone (5.10: `:70`), even for a likely-Indian user. */
  initialProvider?: BillingProvider;
}

export interface UseCheckoutFlowResult {
  provider: BillingProvider;
  setProvider: (provider: BillingProvider) => void;
  /** Starts a checkout for `tier` using the currently selected provider. */
  startCheckout: (tier: CheckoutTierId) => void;
  /** True while the checkout POST (or the Razorpay modal it opens) is in flight. */
  isStarting: boolean;
  /** The checkout POST's error, if the most recent attempt failed. Carries `code`/`reason` — see mapBillingCheckoutError. */
  error: BillingCheckoutError | null;
}

export function useCheckoutFlow(options: UseCheckoutFlowOptions): UseCheckoutFlowResult {
  const checkout = useCreateBillingCheckout();
  const verify = useVerifyRazorpayCheckout();
  const [provider, setProvider] = useState<BillingProvider>(
    options.initialProvider ?? "stripe",
  );

  function openRazorpayCheckout(data: RazorpayCheckoutData) {
    loadRazorpayCheckout()
      .then((Razorpay) => {
        const instance = new Razorpay({
          key: data.key_id,
          subscription_id: data.subscription_id,
          amount: data.amount,
          currency: data.currency,
          name: "WPMgr",
          description: "WPMgr subscription",
          handler: (response: RazorpayHandlerResponse) => {
            // Verify is a UX confirmation only — the poll the caller drives
            // off of `onCheckoutSuccess` is what actually observes the plan
            // flip, so it must start regardless of whether verify itself
            // succeeds or fails.
            verify.mutate(response, {
              onSettled: () => options.onCheckoutSuccess(),
            });
          },
          modal: {
            // The operator closed the modal without paying — a normal
            // cancel, never an error. No toast, no navigation.
            ondismiss: () => {},
          },
        });
        instance.open();
      })
      .catch((err: unknown) => {
        toast.error("Could not open the Razorpay payment window", {
          description:
            err instanceof Error
              ? err.message
              : "Please try again, or choose Stripe instead.",
        });
      });
  }

  function startCheckout(tier: CheckoutTierId) {
    checkout.mutate(
      {
        tier,
        provider,
        // Razorpay is INR-only (decision 17) — no user-facing currency
        // choice any more (5.10, Z5).
        currency: provider === "razorpay" ? "INR" : undefined,
      },
      {
        onSuccess: (result) => {
          if (result.razorpay) {
            openRazorpayCheckout(result.razorpay);
          } else if (result.url) {
            window.location.href = result.url;
          }
        },
      },
    );
  }

  return {
    provider,
    setProvider,
    startCheckout,
    isStarting: checkout.isPending,
    error: checkout.isError ? checkout.error : null,
  };
}
