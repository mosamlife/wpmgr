// Shared company + billing constants for the Terms of Service, Privacy
// Policy, and Refund Policy pages. Everything entity-specific is centralized
// here so it is edited in exactly one place. The bracketed values below are
// placeholders: they must be filled in with real business details before
// these pages go live. See the review note at the bottom of each legal page.

/**
 * The legal entity that operates the WPMgr hosted service. Jayso Labs, LLC is
 * the seller and merchant of record for card payments (processed by Stripe,
 * see {@link STRIPE}); it also holds the account these pages describe.
 * `address` and `jurisdiction` are placeholders pending confirmation of the
 * entity's registered address and state of formation -- do not publish these
 * pages until an owner or counsel fills them in.
 */
export const COMPANY = {
  legalName: "Jayso Labs, LLC",
  address: "[Jayso Labs, LLC registered address -- confirm before publishing]",
  jurisdiction: "[Jayso Labs, LLC state of formation -- confirm before publishing]",
  supportEmail: "support@wpmgr.app",
} as const;

/**
 * Shared effective date for the Terms, Privacy Policy, and Refund Policy.
 * Bump this single value whenever any of the three documents changes
 * materially so all three stay in sync.
 */
export const LEGAL_EFFECTIVE_DATE = "June 1, 2026";

export const LEGAL_CONTACT_HREF = `mailto:${COMPANY.supportEmail}`;

/**
 * By default, every customer pays by card in US dollars, processed by
 * Stripe. {@link COMPANY} is the seller and merchant of record for these
 * payments; Stripe is the payment processor only and is not the merchant of
 * record.
 *
 * Customers in India can instead choose Razorpay, which charges in Indian
 * rupees and also supports UPI and RuPay. The seller entity for the
 * Razorpay route is confirmed separately and is not yet reflected here.
 */
export const STRIPE = {
  legalName: "Stripe, Inc.",
  shortName: "Stripe",
  role: "Payment processor",
  website: "https://stripe.com",
} as const;

/** Optional payment processor for Indian customers; not the merchant of record. */
export const RAZORPAY = {
  legalName: "Razorpay Software Private Limited",
  shortName: "Razorpay",
  role: "Payment processor",
  website: "https://razorpay.com",
} as const;
