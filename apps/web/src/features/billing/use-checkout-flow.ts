import { useState } from "react";

import { toast } from "@/components/toast";

import {
  useCreateBillingCheckout,
  useVerifyRazorpayCheckout,
  type BillingCurrency,
  type BillingProvider,
  type CheckoutTierId,
  type RazorpayCheckoutData,
} from "./use-billing";
import { loadRazorpayCheckout, type RazorpayHandlerResponse } from "./razorpay-checkout";

// M16 Phase B/C — the checkout machinery shared by every surface that can
// start a paid-tier checkout (today: /settings/billing's `PlanTiersGrid`;
// Phase C's post-verify upgrade screen reuses this exact hook rather than a
// second copy). Extracted verbatim out of billing.tsx's former
// `PlanTiersGrid` internals — this is a pure lift, not a behavior change; see
// use-checkout-flow.test.ts and the still-green -billing.test.tsx for the
// parity proof.
//
// Encapsulates:
//   - the provider (Stripe/Razorpay) + Razorpay-only currency selection state
//     that feeds `PaymentMethodPicker`;
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
  /**
   * Initial currency selection — e.g. a `?currency=` URL hint carried from
   * `/register` through to `/welcome/checkout` (M16 Phase C2). Only
   * meaningful once the operator picks Razorpay; defaults to "USD", same as
   * every caller that omits this (unregressed `/settings/billing` behavior).
   */
  initialCurrency?: BillingCurrency;
}

export interface UseCheckoutFlowResult {
  provider: BillingProvider;
  currency: BillingCurrency;
  setProvider: (provider: BillingProvider) => void;
  setCurrency: (currency: BillingCurrency) => void;
  /** Starts a checkout for `tier` using the currently selected provider/currency. */
  startCheckout: (tier: CheckoutTierId) => void;
  /** True while the checkout POST (or the Razorpay modal it opens) is in flight. */
  isStarting: boolean;
  /** The checkout POST's error, if the most recent attempt failed. */
  error: Error | null;
}

export function useCheckoutFlow(options: UseCheckoutFlowOptions): UseCheckoutFlowResult {
  const checkout = useCreateBillingCheckout();
  const verify = useVerifyRazorpayCheckout();
  const [provider, setProvider] = useState<BillingProvider>("stripe");
  const [currency, setCurrency] = useState<BillingCurrency>(
    options.initialCurrency ?? "USD",
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
        currency: provider === "razorpay" ? currency : undefined,
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
    currency,
    setProvider,
    setCurrency,
    startCheckout,
    isStarting: checkout.isPending,
    error: checkout.isError ? checkout.error : null,
  };
}
