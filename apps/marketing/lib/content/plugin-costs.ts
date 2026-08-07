// /pricing/plugin-stack: what a premium WordPress plugin stack costs per year.
//
// EVERY FIGURE CAME FROM THE VENDOR'S OWN PRICING PAGE and carries the date it
// was checked. Four passes produced these, and the checking pass mattered: it
// caught a fabricated renewal claim about one vendor and a figure nearly nine
// times too large on another, both of which would have shipped.
//
// THREE RULES THIS FILE EXISTS TO ENFORCE.
//
// 1. NO BUNDLES. Every product here sells the category it is listed under and
//    little else. WP-Optimize and Perfmatters are deliberately absent even
//    though both are widely used: WP-Optimize sells caching, image
//    compression, minification AND database cleaning as one licence, and
//    Perfmatters is a performance suite that does not name database cleanup on
//    its pricing page at all. Counting either would inflate the total by
//    charging a reader twice for one purchase, which is exactly the accusation
//    that would destroy this page.
//
// 2. RECURRING PRICE, NOT FIRST-YEAR PRICE. Where a vendor advertises an
//    introductory discount, the number here is what the reader pays in year
//    two. Duplicator is the clearest case, its own footnote reading "Special
//    introductory pricing, all renewals are at full price", and it is one
//    reason Duplicator is not the backup product used below.
//
// 3. RE-CHECK IN A REAL BROWSER. Several vendors in this market render prices
//    with JavaScript, and a plain fetch returns a DIFFERENT number: one showed
//    a stale "60% OFF" banner that does not exist on the rendered page, and
//    another served a static fallback figure its own API later overwrites. If
//    you update this file, use a browser.

export type CostTier = {
  /** Sites this tier covers. Infinity for unlimited. */
  upTo: number;
  /** USD per year. Monthly vendors are converted and the note says so. */
  perYear: number;
  label: string;
};

export type CostProduct = {
  name: string;
  url: string;
  tiers: CostTier[];
  note: string;
  verifiedOn: string;
};

export type CostCategory = {
  key: string;
  label: string;
  icon: string;
  /** What WPMgr gives you instead, for the right-hand column. */
  wpmgr: string;
  products: CostProduct[];
};

export const PLUGIN_COST_CATEGORIES: CostCategory[] = [
  {
    key: "backups",
    label: "Backups and restore",
    icon: "DatabaseBackup",
    wpmgr: "Incremental, client-side encrypted, to storage you own",
    products: [
      {
        name: "UpdraftPlus Premium",
        url: "https://teamupdraft.com/updraftplus/pricing/",
        tiers: [
          { upTo: 2, perYear: 70, label: "Personal, 2 sites" },
          { upTo: 10, perYear: 95, label: "Business, 10 sites" },
          { upTo: 35, perYear: 145, label: "Agency, 35 sites" },
          { upTo: Infinity, perYear: 195, label: "Enterprise, unlimited" },
        ],
        note: "Annual licence by site tier. Each of these tiers includes 1 GB of the vendor's own storage; backups to your own remote storage are separate.",
        verifiedOn: "2026-08-07",
      },
    ],
  },
  {
    key: "security",
    label: "Security and malware scanning",
    icon: "ShieldCheck",
    wpmgr: "Hardening, file integrity and vulnerability matching, built in",
    products: [
      {
        name: "Wordfence Premium",
        url: "https://www.wordfence.com/products/pricing/",
        tiers: [
          { upTo: 1, perYear: 149, label: "1 licence" },
          { upTo: 4, perYear: 134.1, label: "2 to 4 licences" },
          { upTo: 14, perYear: 126.65, label: "5 to 14 licences" },
          { upTo: Infinity, perYear: 119.2, label: "15 or more licences" },
        ],
        note: "Priced per site, with published volume brackets. The figure shown is the per-site price at your bracket, multiplied by your site count.",
        verifiedOn: "2026-08-07",
      },
    ],
  },
  {
    key: "performance",
    label: "Page caching and performance",
    icon: "Zap",
    wpmgr: "Page cache, object cache and Core Web Vitals from real visitors",
    products: [
      {
        name: "WP Rocket",
        url: "https://wp-rocket.me/pricing/",
        tiers: [
          { upTo: 1, perYear: 59, label: "Single, 1 site" },
          { upTo: 3, perYear: 119, label: "Plus, 3 sites" },
          { upTo: 50, perYear: 299, label: "Multi, 50 sites" },
          { upTo: 100, perYear: 399, label: "Multi, 100 sites" },
          { upTo: 500, perYear: 599, label: "Multi, 500 sites" },
        ],
        note: "Annual licence by site tier, renewing automatically.",
        verifiedOn: "2026-08-07",
      },
      {
        name: "FlyingPress",
        url: "https://flyingpress.com/pricing/",
        tiers: [
          { upTo: 1, perYear: 59, label: "Starter, 1 site" },
          { upTo: 3, perYear: 109, label: "Pro, 3 sites" },
          { upTo: 25, perYear: 229, label: "Business, 25 sites" },
          { upTo: Infinity, perYear: 279, label: "Unlimited" },
        ],
        note: "Annual licence by site tier. No introductory discount is shown on the pricing page.",
        verifiedOn: "2026-08-07",
      },
    ],
  },
  {
    key: "images",
    label: "Image optimization",
    icon: "ImageDown",
    wpmgr: "AVIF and WebP conversion with originals kept as a fallback",
    products: [
      {
        name: "ShortPixel Unlimited",
        url: "https://shortpixel.com/pricing",
        tiers: [{ upTo: Infinity, perYear: 99.9, label: "Unlimited, billed yearly" }],
        note: "Priced by image volume rather than by site, so the cost does not rise with fleet size. Billed yearly is the page default.",
        verifiedOn: "2026-08-07",
      },
    ],
  },
  {
    key: "database",
    label: "Database cleaning",
    icon: "Database",
    wpmgr: "Orphan classification with a preview before anything is deleted",
    products: [
      {
        name: "Advanced Database Cleaner Pro",
        url: "https://sigmaplugin.com/downloads/wordpress-advanced-database-cleaner",
        tiers: [
          { upTo: 1, perYear: 39, label: "Starter, 1 site" },
          { upTo: 5, perYear: 59, label: "Standard, 5 sites" },
          { upTo: Infinity, perYear: 119, label: "Agency, unlimited" },
        ],
        note: "Chosen over the better known alternatives on purpose: it sells database cleaning alone, so it cannot double count against the caching or image lines above.",
        verifiedOn: "2026-08-07",
      },
    ],
  },
  {
    key: "uptime",
    label: "Uptime monitoring",
    icon: "Activity",
    wpmgr: "Built in, with TLS expiry checks",
    products: [
      {
        name: "UptimeRobot",
        url: "https://uptimerobot.com/pricing/",
        tiers: [
          { upTo: 10, perYear: 108, label: "Solo, 10 monitors" },
          { upTo: 50, perYear: 228, label: "Solo, 50 monitors" },
          { upTo: 100, perYear: 348, label: "Team, 100 monitors" },
          { upTo: 200, perYear: 648, label: "Scale, 200 monitors" },
          { upTo: 500, perYear: 1488, label: "Scale, 500 monitors" },
        ],
        note: "Priced per monitor and billed monthly; the annual figure is the monthly rate multiplied by twelve. A free tier covering 50 monitors exists at a longer check interval.",
        verifiedOn: "2026-08-07",
      },
    ],
  },
  {
    key: "reports",
    label: "Client reporting",
    icon: "FileText",
    wpmgr: "White label reports and a client portal, built in",
    products: [
      {
        name: "WP Umbrella",
        url: "https://wp-umbrella.com/pricing/",
        tiers: [{ upTo: Infinity, perYear: 26.28, label: "Pay as you go" }],
        note: "Priced per site per month, so this line scales linearly with the fleet. The annual figure is the monthly rate multiplied by twelve.",
        verifiedOn: "2026-08-07",
      },
    ],
  },
];

/** Categories whose price is per site rather than per tier. */
export const PER_SITE_KEYS = ["security", "reports"];

/**
 * The cheapest published tier that covers `sites`, or null when the fleet is
 * larger than anything the vendor lists.
 *
 * Returning null rather than clamping to the top tier is deliberate. Several
 * vendors stop publishing prices above a few hundred sites and quote privately
 * from there, and silently reusing the 500-site price for a 900-site fleet
 * would invent a number the vendor never offered. The UI says "priced on
 * request" instead, which is both true and the more damaging fact anyway.
 */
export function resolveTier(product: CostProduct, sites: number): CostTier | null {
  for (const tier of product.tiers) if (sites <= tier.upTo) return tier;
  return null;
}

/** Annual cost of one product for `sites` sites, or null when unpublished. */
export function annualCost(
  category: CostCategory,
  product: CostProduct,
  sites: number,
): number | null {
  const tier = resolveTier(product, sites);
  if (!tier) return null;
  // Per-site vendors publish a RATE and a volume bracket, so the bracket
  // selects the rate and the fleet size multiplies it. Tiered vendors publish
  // a single figure that already covers the whole bracket.
  return PER_SITE_KEYS.includes(category.key) ? tier.perYear * sites : tier.perYear;
}

/** Site counts offered as presets. 500 is the ceiling because it is the
 *  largest fleet every product above still publishes a price for. */
export const SITE_PRESETS = [1, 5, 10, 25, 50, 100, 250, 500];

/** WPMgr hosted tiers, mirrored from lib/content/pricing.ts, in $/month. */
export const WPMGR_TIERS = [
  { name: "Free", sites: 3, perMonth: 0 },
  { name: "Starter", sites: 10, perMonth: 15 },
  { name: "Agency", sites: 50, perMonth: 59 },
  { name: "Scale", sites: 200, perMonth: 169 },
];

export function resolveWpmgrTier(sites: number) {
  return WPMGR_TIERS.find((t) => sites <= t.sites) ?? null;
}
