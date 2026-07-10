import { SegmentedControl } from "@/components/ui/segmented-control";

import type { BillingCurrency, BillingProvider } from "./use-billing";

// M16 Phase B/C — the provider (Stripe/Razorpay) + Razorpay-only currency
// picker shared by every checkout surface (`/settings/billing`'s
// `PlanTiersGrid` and `/welcome/checkout`'s post-verify screen). Extracted
// verbatim out of billing.tsx's former private `PaymentMethodPicker` — a pure
// lift, not a behavior change (see -billing.test.tsx, still green).

export function PaymentMethodPicker({
  provider,
  onProviderChange,
  currency,
  onCurrencyChange,
}: {
  provider: BillingProvider;
  onProviderChange: (provider: BillingProvider) => void;
  currency: BillingCurrency;
  onCurrencyChange: (currency: BillingCurrency) => void;
}) {
  return (
    <div className="flex flex-wrap items-center gap-4 rounded-lg border border-border bg-muted/20 px-4 py-3 text-sm">
      <div className="flex items-center gap-2">
        <span className="font-medium text-foreground">Pay via</span>
        <SegmentedControl
          aria-label="Payment provider"
          value={provider}
          onChange={onProviderChange}
          options={[
            { value: "stripe", label: "Stripe" },
            { value: "razorpay", label: "Razorpay" },
          ]}
        />
      </div>
      {provider === "razorpay" ? (
        <div className="flex items-center gap-2">
          <span className="font-medium text-foreground">Currency</span>
          <SegmentedControl
            aria-label="Currency"
            value={currency}
            onChange={onCurrencyChange}
            options={[
              { value: "USD", label: "USD ($)" },
              { value: "INR", label: "INR (₹)" },
            ]}
          />
          <span className="text-xs text-muted-foreground">
            Prices below are shown in USD; the exact INR amount is confirmed
            in the payment window before you pay.
          </span>
        </div>
      ) : null}
    </div>
  );
}
