// Analytics configuration for the marketing site ONLY. The dashboard
// (apps/web) is deliberately not instrumented here.
//
// EVERY VALUE IS READ FROM THE ENVIRONMENT AND EVERY INTEGRATION NO-OPS WHEN
// ITS VALUE IS ABSENT. That is not defensive coding, it is the requirement:
// this repository is public and self-hostable, so anyone can build and deploy
// this marketing site. If the measurement IDs were hardcoded, every fork would
// report into our property and the numbers we make decisions from would be
// somebody else's traffic. Absent config means no script is loaded at all.
//
// These are NEXT_PUBLIC_ values, so they are baked in at BUILD time and are
// visible in the page source. That is expected: a GA4 measurement ID and a
// PostHog project key are public identifiers, not secrets. They are still kept
// out of the repo so that only our build produces a reporting bundle.
// See infra/Dockerfile.marketing for how they are passed in.

/** GA4 measurement ID, e.g. G-XXXXXXXXXX. */
export const GA_MEASUREMENT_ID = process.env.NEXT_PUBLIC_GA_MEASUREMENT_ID ?? "";

/** PostHog project API key (public), e.g. phc_xxx. */
export const POSTHOG_KEY = process.env.NEXT_PUBLIC_POSTHOG_KEY ?? "";

/**
 * PostHog ingestion host. EU cloud is https://eu.i.posthog.com.
 *
 * `||` and not `??` on purpose. The Docker build arg defaults to an EMPTY
 * STRING rather than being unset, and `?? ` only falls back on null or
 * undefined, so `??` would leave the host as "" and break init on every build
 * that does not pass one explicitly.
 */
export const POSTHOG_HOST =
  process.env.NEXT_PUBLIC_POSTHOG_HOST || "https://us.i.posthog.com";

/**
 * Google Search Console verification token, the `content` value from the
 * HTML-tag verification method. Only needed until verification completes, but
 * removing it afterwards un-verifies the property, so it stays.
 */
export const GSC_VERIFICATION = process.env.NEXT_PUBLIC_GSC_VERIFICATION ?? "";

export const analyticsEnabled = {
  ga: GA_MEASUREMENT_ID.length > 0,
  posthog: POSTHOG_KEY.length > 0,
};

/**
 * Where a signup click came from. One value per surface, so a report answers
 * "which page earns signups" rather than "the dashboard got some traffic".
 *
 * Keep these stable: renaming one splits its history in the analytics UI.
 */
export type SignupSource =
  | "header"
  | "mobile-nav"
  | "hero"
  | "cta-band"
  | "pricing-free"
  | "pricing-starter"
  | "pricing-agency"
  | "pricing-scale"
  | "feature-hero"
  | "feature-cta"
  | "solution-hero"
  | "solution-cta"
  | "blog-post"
  | "blog-inline"
  | "compare"
  | "plugin-stack"
  | "self-host"
  | "guide"
  | "guides-index"
  | "features-index"
  | "solutions-index"
  | "about"
  | "product-hunt";
