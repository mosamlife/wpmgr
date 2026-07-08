// Shared company + billing constants for the Terms of Service, Privacy
// Policy, and Refund Policy pages. Everything entity-specific is centralized
// here so it is edited in exactly one place. The bracketed values below are
// placeholders: they must be filled in with real business details before
// these pages go live. See the review note at the bottom of each legal page.

/** The legal entity that operates the WPMgr hosted service. */
export const COMPANY = {
  legalName: "WPMgr - WordPress Manager",
  address: "Ahmedabad, Gujarat, India",
  jurisdiction: "the courts of Ahmedabad, Gujarat, India",
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
 * WPMgr offers three payment providers, chosen by the customer at checkout:
 * Razorpay, Stripe, and Paddle. The merchant-of-record role differs by
 * provider:
 *
 * - Stripe and Razorpay process payments on behalf of {@link COMPANY}, which
 *   is the seller and merchant of record for those sales. WPMgr handles its
 *   own tax collection and invoicing for these payments (Razorpay covers
 *   Indian GST and INR billing; Stripe covers international card payments)
 *   and issues refunds directly to the original payment method.
 * - Paddle.com Market Ltd remains the merchant of record for sales it
 *   processes. Paddle is the seller on the customer's card statement for
 *   those sales and handles billing, invoicing, tax collection and
 *   remittance, and refunds for them.
 *
 * Stripe and Razorpay are payment processors only; they are not merchants of
 * record.
 */
export const PADDLE = {
  legalName: "Paddle.com Market Ltd",
  shortName: "Paddle",
  role: "Merchant of Record",
  website: "https://www.paddle.com",
} as const;

/** Payment processor for Stripe-processed sales; not the merchant of record. */
export const STRIPE = {
  legalName: "Stripe, Inc.",
  shortName: "Stripe",
  role: "Payment processor",
  website: "https://stripe.com",
} as const;

/** Payment processor for Razorpay-processed sales; not the merchant of record. */
export const RAZORPAY = {
  legalName: "Razorpay Software Private Limited",
  shortName: "Razorpay",
  role: "Payment processor",
  website: "https://razorpay.com",
} as const;
