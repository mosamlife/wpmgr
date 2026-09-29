import { PRICES_EXCLUDE_TAX, TAX_ID_HINT } from "./billing-copy";

// S0.4 (stripe-design-v9.md 3.1, 5.10) — the tax copy shown on both checkout
// surfaces: "Prices exclude tax", the 21(a) tax-ID line, with the EU VAT
// hint inline (bolded in the design doc; a plain sentence here since this
// page has no rich-text renderer for the copy string).

export function BillingTaxCopy() {
  return (
    <p className="text-xs text-muted-foreground">
      {PRICES_EXCLUDE_TAX} {TAX_ID_HINT}
    </p>
  );
}
