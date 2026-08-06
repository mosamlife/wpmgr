// Pricing page content module. Tier data, FAQ, and supporting copy for
// /pricing. House rules enforced by scripts/check-copy.mjs: no em dashes,
// no en dashes, no competitor plugin names.
//
// Live prices (from GET /api/v1/pricing, fetched at build time in
// app/(marketing)/pricing/page.tsx via lib/pricing-live.ts) override each
// paid tier's `price` fallback below, per currency -- see resolveTierPrices.
import type { Cta, FaqItem } from "./types";
import { SITE_CONFIG, signupHref } from "@/lib/site";
import type { LivePricingResponse, LiveTier } from "@/lib/pricing-live";

export type PricingTier = {
  id: "free" | "starter" | "agency" | "scale";
  name: string;
  /** Fallback USD dollars/month, used whenever live pricing is unresolved. */
  price: number;
  audience: string;
  mostPopular?: boolean;
  features: string[];
  /**
   * The paid tiers' href already carries `?plan=<id>` so the signup app
   * knows which plan to preselect at checkout; the free tier's href stays a
   * bare signup link. Use ctaHrefWithCurrency to also thread the currency
   * toggle's selection through.
   */
  cta: Cta;
};

export const PRICING_TIERS: PricingTier[] = [
  {
    id: "free",
    name: "Free",
    price: 0,
    audience: "For trying it out or a small personal site",
    features: [
      "3 sites",
      "Bring your own backup storage",
      "5-minute uptime checks",
      "The full feature set, nothing gated",
    ],
    cta: {
      label: "Get started for free",
      href: signupHref("pricing-free"),
      variant: "secondary",
      icon: "ArrowRight",
    },
  },
  {
    id: "starter",
    name: "Starter",
    price: 15,
    audience: "For freelancers and small portfolios",
    features: [
      "10 sites",
      "50 GB managed backup storage",
      "Daily backups",
      "The full feature set, nothing gated",
    ],
    cta: {
      label: "Start with Starter",
      href: signupHref("pricing-starter", { plan: "starter" }),
      variant: "secondary",
      icon: "ArrowRight",
    },
  },
  {
    id: "agency",
    name: "Agency",
    price: 59,
    audience: "The core plan for agencies",
    mostPopular: true,
    features: [
      "50 sites",
      "250 GB managed backup storage",
      "Hourly backups",
      "The full feature set, nothing gated",
    ],
    cta: {
      label: "Start with Agency",
      href: signupHref("pricing-agency", { plan: "agency" }),
      variant: "primary",
      icon: "ArrowRight",
    },
  },
  {
    id: "scale",
    name: "Scale",
    price: 169,
    audience: "For large fleets",
    features: [
      "200 sites",
      "1 TB managed backup storage",
      "Hourly backups",
      "The full feature set, nothing gated",
    ],
    cta: {
      label: "Start with Scale",
      href: signupHref("pricing-scale", { plan: "scale" }),
      variant: "secondary",
      icon: "ArrowRight",
    },
  },
];

export const PRICING_NOTE =
  "Self-hosting the control plane is free and unlimited forever under the AGPL-3.0 license, with no site limit and no feature gating. Annual billing on the hosted plans is coming soon; every plan above is billed monthly today.";

export const PRICING_FAQ: FaqItem[] = [
  {
    q: "What counts as a site?",
    a: "A site is one connected WordPress installation, whether it is a production site, a staging copy, or a multisite network root. Each site you enroll in the dashboard counts once against your plan's site limit, regardless of how many features you turn on for it.",
  },
  {
    q: "Can I change plans later?",
    a: "Yes. You can upgrade or downgrade at any time from the dashboard billing page. Upgrades take effect immediately and are prorated for the rest of the billing period. Downgrades take effect at the start of your next billing period, so you keep your current plan's limits until then.",
  },
  {
    q: "Is there a free trial?",
    a: "There is something better than a trial: a permanent free tier. You can run up to 3 sites on the Free plan for as long as you like, with the full feature set, no credit card, and no expiry. Upgrade whenever you need more sites or managed backup storage.",
  },
  {
    q: "What payment methods do you accept?",
    a: "You choose your payment provider at checkout: Razorpay, Stripe, or Paddle. For Razorpay and Stripe, WPMgr is the seller and handles its own invoicing and tax directly (Razorpay covers Indian GST and INR, Stripe covers international cards). For Paddle, Paddle is the merchant of record and handles invoicing and tax for that sale. All three accept major credit and debit cards, and Razorpay and Paddle also support a range of local payment methods.",
  },
  {
    q: "What is your refund policy?",
    a: "Monthly subscriptions can be cancelled at any time and remain active until the end of the paid period. First-time subscribers are also covered by a 14-day money-back guarantee. See the full refund policy for details.",
  },
  {
    q: "Can I self-host instead of paying for a hosted plan?",
    a: "Yes, always. The entire control plane is open source under the AGPL-3.0 license and the WordPress agent is MIT-licensed. Self-hosting has no site limit, no per-site fee, and no feature gating. The hosted plans above are for teams who would rather not run their own server.",
  },
];

export const PRICING_CTAS: Cta[] = [
  { label: "Get started for free", href: signupHref("cta-band"), variant: "primary", icon: "ArrowRight" },
  { label: "Star on GitHub", href: SITE_CONFIG.github, variant: "secondary", icon: "Github" },
];

// ---------------------------------------------------------------------------
// Live pricing: build-time merge of GET /api/v1/pricing (lib/pricing-live.ts)
// with the static fallback amounts above, plus the USD/INR display toggle.
// ---------------------------------------------------------------------------

export type BillingCurrency = "USD" | "INR";

export type CurrencyPrice = {
  /** Major-unit amount (dollars/rupees) -- used for the JSON-LD Offer.price. */
  amountMajor: number;
  /** Human display label with currency symbol, e.g. "$15" or "₹1,249". */
  label: string;
};

export type TierDisplayPrice = {
  usd: CurrencyPrice;
  /**
   * Null when this tier has no live INR price: the free tier (never priced
   * by a payment provider), a Stripe-only paid tier, or the fully-offline
   * static fallback (which is USD-only, mirroring the CP's own
   * staticFallbackJSON in apps/api/internal/pricing/service.go).
   */
  inr: CurrencyPrice | null;
};

const CURRENCY_SYMBOLS: Record<BillingCurrency, string> = {
  USD: "$",
  INR: "₹",
};

function formatMajorAmount(amountMinor: number, currency: BillingCurrency): string {
  const major = amountMinor / 100;
  const hasFraction = !Number.isInteger(major);
  const formatted = major.toLocaleString(currency === "INR" ? "en-IN" : "en-US", {
    minimumFractionDigits: hasFraction ? 2 : 0,
    maximumFractionDigits: hasFraction ? 2 : 0,
  });
  return `${CURRENCY_SYMBOLS[currency]}${formatted}`;
}

function buildCurrencyPrice(amountMinor: number, currency: BillingCurrency): CurrencyPrice {
  return { amountMajor: amountMinor / 100, label: formatMajorAmount(amountMinor, currency) };
}

/**
 * Resolves each tier's display price from the live CP payload, falling back
 * to this module's static PRICING_TIERS `price` (USD dollars) per tier
 * whenever the live fetch failed entirely, or a specific tier was missing
 * from the live response. Safe to call with `live: null` (the build-time
 * fetch failure path) -- every tier then resolves to its static USD
 * fallback with no INR price, which is exactly the CP's own offline
 * behavior.
 */
export function resolveTierPrices(
  live: LivePricingResponse | null,
): Record<PricingTier["id"], TierDisplayPrice> {
  const byId = new Map<string, LiveTier>((live?.tiers ?? []).map((t) => [t.id, t]));
  const out = {} as Record<PricingTier["id"], TierDisplayPrice>;

  for (const tier of PRICING_TIERS) {
    const raw = byId.get(tier.id);
    const fallbackMinor = Math.round(tier.price * 100);
    const usdMinor =
      tier.id === "free" ? (raw?.amount ?? fallbackMinor) : (raw?.usd?.amount ?? fallbackMinor);
    const inr = tier.id !== "free" && raw?.inr ? buildCurrencyPrice(raw.inr.amount, "INR") : null;

    out[tier.id] = { usd: buildCurrencyPrice(usdMinor, "USD"), inr };
  }

  return out;
}

/**
 * Appends `&currency=<currency>` to a paid tier's signup CTA href so the
 * checkout defaults to whichever currency is selected on the page. The free
 * tier's href never carries `?plan=`, so it is returned unchanged -- the
 * free lane stays a bare signup link regardless of the currency toggle.
 */
export function ctaHrefWithCurrency(cta: Cta, currency: BillingCurrency): string {
  if (!cta.href.includes("?")) return cta.href;
  return `${cta.href}&currency=${currency}`;
}
