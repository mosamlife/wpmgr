// S0.4 (stripe-design-v9.md 3.1, 5.10, 5.14) — the fixed billing copy
// strings quoted verbatim in the design so a render test can assert the
// exact text without re-deriving it, and so a future wording change touches
// exactly one place per string.

/** 3.1 — shown above the pay button for likely-Indian users. */
export const INDIAN_CARD_GUIDANCE_ITEMS: readonly string[] = [
  "Turn on international online payments in your bank app.",
  "Use Visa or Mastercard for automatic renewals.",
  "Your bank notifies you before each renewal.",
  "Your bank may add its own foreign-currency fee.",
];

/** 5.14 — appended to the guidance when Scale is the plan being purchased (or is one of the offered plans). */
export const INDIAN_SCALE_GUIDANCE_LINE =
  "Scale may be above India's ₹15,000 auto-debit limit, so your bank may ask you to approve each renewal through its notice before the charge.";

/** 3.1, under decision 21(a) — shown to every checkout visitor. */
export const TAX_ID_HINT =
  "A business tax ID (VAT or GST number) is required in the UK, EU, India and many other countries. In the EU, enter your EU VAT number, not a national tax number.";

export const PRICES_EXCLUDE_TAX = "Prices exclude tax.";

/** 5.14 — shown beside "Change plan" for likely-Indian users on Stripe with a live subscription. */
export const INDIAN_UPGRADE_COPY =
  "Paying with an Indian card? When you first subscribed, your bank approved automatic payments up to the price of the plan you started on. If you upgrade to a plan that costs more than that, every renewal of the new plan needs your approval, and today's prorated charge may too. Your bank asks for it through its notice, about a day before each charge. An unapproved payment fails and isn't retried. To avoid this, cancel in Manage billing instead of upgrading. Your plan stays active until the end of the billing period, then your workspace moves to Free. Subscribe to the new plan after that. The new subscription sets up a new approval.";

/** 3.3 — the Indian-card decline banner, shown on `checkout=cancel` for likely-Indian users. */
export const INDIAN_DECLINE_MESSAGE_WITH_RAZORPAY =
  "Checkout canceled. Indian banks often block international payments by default. Turn on international use in your bank app and try again, or pay with Razorpay (UPI, RuPay, ₹).";

export const INDIAN_DECLINE_MESSAGE_NO_RAZORPAY =
  "Checkout canceled. Indian banks often block international payments by default. Turn on international use in your bank app and try again.";
