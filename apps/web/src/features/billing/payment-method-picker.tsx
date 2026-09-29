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
//
// "Already pinned to razorpay" reads `pinnedProvider` (the server's
// `summary.provider`, i.e. what this workspace is actually billed through
// today), never `provider` (the picker's own in-progress selection). Those
// two used to be the same field, which hid the picker the moment a
// Razorpay-pinned operator outside India merely clicked "Card ($)" to look
// at it — nothing showed Razorpay again to switch back and pay. A
// server-confirmed pin must stay visible regardless of what the control is
// currently set to.

export function PaymentMethodPicker({
  provider,
  onProviderChange,
  availableProviders,
  likelyIndian,
  pinnedProvider,
}: {
  provider: BillingProvider;
  onProviderChange: (provider: BillingProvider) => void;
  availableProviders: readonly BillingProvider[];
  likelyIndian: boolean;
  /** The workspace's server-confirmed provider (`BillingInfo.provider`), if any — distinct from `provider`, the picker's own current selection. */
  pinnedProvider?: string;
}) {
  const showRazorpay =
    availableProviders.includes("razorpay") &&
    (likelyIndian || pinnedProvider === "razorpay");

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
