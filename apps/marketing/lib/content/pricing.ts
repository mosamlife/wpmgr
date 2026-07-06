// Pricing page content module. Tier data, FAQ, and supporting copy for
// /pricing. House rules enforced by scripts/check-copy.mjs: no em dashes,
// no en dashes, no competitor plugin names.
import type { Cta, FaqItem } from "./types";
import { SITE_CONFIG } from "@/lib/site";

export type PricingTier = {
  id: "free" | "starter" | "agency" | "scale";
  name: string;
  price: number;
  audience: string;
  mostPopular?: boolean;
  features: string[];
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
      href: SITE_CONFIG.signup,
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
      href: SITE_CONFIG.signup,
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
      href: SITE_CONFIG.signup,
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
      href: SITE_CONFIG.signup,
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
    a: "All paid plans are billed through Paddle, our payment processor and merchant of record. Paddle accepts major credit and debit cards and a range of local payment methods depending on your country, and it handles invoicing and tax automatically.",
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
  { label: "Get started for free", href: SITE_CONFIG.signup, variant: "primary", icon: "ArrowRight" },
  { label: "Star on GitHub", href: SITE_CONFIG.github, variant: "secondary", icon: "Github" },
];
