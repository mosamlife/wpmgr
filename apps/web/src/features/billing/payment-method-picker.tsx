import { SegmentedControl } from "@/components/ui/segmented-control";

import type { BillingProvider } from "./use-billing";

// S0.4 (stripe-design-v9.md 5.10) — the provider picker shared by every
// checkout surface (`/settings/billing`'s `PlanTiersGrid` and
// `/welcome/checkout`'s post-verify screen).
//
// The Razorpay currency segment is gone: Razorpay is INR-only now (decision
// 17) — see use-checkout-flow.ts. The provider option itself renders only
// when `availableProviders` includes razorpay AND (`likelyIndian` OR the
// tenant is already pinned to razorpay) — everyone else sees no picker at
// all and pays by card.

export function PaymentMethodPicker({
  provider,
  onProviderChange,
  availableProviders,
  likelyIndian,
}: {
  provider: BillingProvider;
  onProviderChange: (provider: BillingProvider) => void;
  availableProviders: readonly BillingProvider[];
  likelyIndian: boolean;
}) {
  const showRazorpay =
    availableProviders.includes("razorpay") &&
    (likelyIndian || provider === "razorpay");

  if (!showRazorpay) return null;

  return (
    <div className="flex flex-wrap items-center gap-2 rounded-lg border border-border bg-muted/20 px-4 py-3 text-sm">
      <span className="font-medium text-foreground">Pay via</span>
      <SegmentedControl
        aria-label="Payment provider"
        value={provider}
        onChange={onProviderChange}
        options={[
          { value: "stripe", label: "Card (US$)" },
          { value: "razorpay", label: "UPI / RuPay / ₹ (Razorpay)" },
        ]}
      />
    </div>
  );
}
