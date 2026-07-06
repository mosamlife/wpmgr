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
 * Paddle.com Market Ltd is the Merchant of Record for every paid WPMgr
 * subscription. Paddle is the seller on the customer's card statement and
 * handles billing, invoicing, tax collection and remittance, and refunds.
 * WPMgr is the merchant of record for nothing billing-related; Paddle is.
 */
export const PADDLE = {
  legalName: "Paddle.com Market Ltd",
  shortName: "Paddle",
  role: "Merchant of Record",
  website: "https://www.paddle.com",
} as const;
