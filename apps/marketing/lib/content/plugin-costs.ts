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
  /**
   * Overrides the tier-times-PER_SITE_KEYS multiplication below, for a vendor
   * whose published price is neither a flat per-tier fee nor a flat per-site
   * rate: ManageWP's report add-ons are a per-site rate up to 25 sites and a
   * stepped per-100-site bundle above it. When present this IS the annual
   * cost; `tiers` is still used to resolve the bracket label shown next to
   * the product name.
   */
  computeAnnual?: (sites: number) => number;
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
      // FlyingPress was here as an alternate and is deliberately removed, not
      // replaced. Its own feature page sells caching PLUS AVIF/WebP image
      // optimisation PLUS database optimisation under one licence, so
      // selecting it alongside the separate image and database rows below
      // double-counted two categories. There is no partial-credit figure a
      // vendor has published for "just the caching part," and inventing one
      // would fabricate a number this file's own rules forbid. Performance is
      // WP Rocket only until a genuinely single-purpose alternate turns up.
    ],
  },
  {
    key: "images",
    label: "Image optimization",
    icon: "ImageDown",
    wpmgr: "AVIF and WebP conversion with originals kept as a fallback",
    products: [
      {
        name: "Imagify Infinite",
        url: "https://imagify.io/pricing",
        tiers: [{ upTo: Infinity, perYear: 119.88, label: "Infinite, billed yearly" }],
        note: "$9.99/month billed yearly, unlimited websites and unlimited images. This line does NOT grow with fleet size: a 1-site reader and a 200-site reader pay the same figure, which is unfavourable to the plugin-stack total but is what the vendor publishes. ShortPixel, the previous product here, is not used because its pricing page ships empty price containers filled in at runtime by a third-party billing widget, and the figure could not be independently confirmed.",
        verifiedOn: "2026-08-17",
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
        // WP Umbrella was here at "full price" and is deliberately removed,
        // not adjusted. Its own pricing page sells ONE plan, EUR 1.99/site/
        // month, that already includes backups, uptime monitoring,
        // performance, database optimisation, safe updates with rollback,
        // vulnerability monitoring AND white-label reporting. Charging a
        // reader for that plan on this row while also charging them for a
        // separate backup, uptime and database row above double, triple and
        // quadruple counts one purchase. Its stored figure was also a USD
        // conversion of a EUR price at an unrecorded FX rate, so it drifted
        // with the euro and could not be reproduced.
        //
        // ManageWP's two report add-ons sell client reporting alone, priced
        // natively in USD, so neither problem applies.
        name: "ManageWP Advanced Client Reports + White Label",
        url: "https://managewp.com/pricing",
        tiers: [
          { upTo: 25, perYear: 24, label: "Both add-ons, $1/site/month each, per site" },
          {
            upTo: Infinity,
            perYear: 600,
            label: "Both add-ons, bundled at $50/month per 100-site bundle",
          },
        ],
        // Each add-on is $1/site/month up to 25 sites. Past 25 sites, ManageWP
        // sells the add-on in bundles that each cover up to 100 sites for
        // $25/month, and bundles stack, so both add-ons together are
        // $50/month per bundle of up to 100 sites. That is a step function,
        // not a flat per-site rate, so it is computed directly here instead
        // of through the tier-times-sites multiplication every other per-site
        // row uses; the tiers above exist only to show the right bracket
        // label next to the product name.
        computeAnnual: (sites: number) =>
          sites <= 25 ? sites * 2 * 12 : Math.ceil(sites / 100) * 50 * 12,
        note: "Two add-ons on top of a ManageWP plan, each $1/site/month up to 25 sites. Above 25 sites, ManageWP sells each add-on in bundles covering up to 100 sites for $25/month, and bundles stack, so the two add-ons together cost $50/month per bundle of up to 100 sites.",
        verifiedOn: "2026-08-17",
      },
    ],
  },
];

/**
 * Categories whose price is per site rather than per tier.
 *
 * "reports" is deliberately NOT here even though ManageWP's add-ons are
 * per-site up to 25 sites: past that they are a stepped per-100-site bundle,
 * not a flat rate, so a uniform tier-times-sites multiplication would be
 * wrong at fleet sizes above 25. Its product defines `computeAnnual` instead;
 * see `annualCost` below.
 */
export const PER_SITE_KEYS = ["security"];

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
  if (product.computeAnnual) return product.computeAnnual(sites);
  const tier = resolveTier(product, sites);
  if (!tier) return null;
  // Per-site vendors publish a RATE and a volume bracket, so the bracket
  // selects the rate and the fleet size multiplies it. Tiered vendors publish
  // a single figure that already covers the whole bracket.
  return PER_SITE_KEYS.includes(category.key) ? tier.perYear * sites : tier.perYear;
}

/** Site counts offered as presets. 200 is the ceiling because it is WPMgr's
 *  own largest published plan (see PRICING_TIERS in lib/content/pricing.ts):
 *  past it, the right-hand column has no number to compare against, only
 *  "on request", and a five-figure plugin-stack total opposite no number at
 *  all is a strawman rather than a comparison. Nobody buys per-site security
 *  and reporting licences at that scale either, so the honest range for this
 *  page stops where WPMgr's own price list does. */
export const SITE_PRESETS = [1, 5, 10, 25, 50, 100, 200];

/**
 * A WPMgr hosted tier, resolved by the page from PRICING_TIERS plus whatever
 * the control plane quotes live at build time.
 *
 * This is a PARAMETER rather than a constant on purpose. An earlier version
 * hardcoded the ladder here, which duplicated /pricing and would have let the
 * two pages disagree the first time a price changed. A calculator whose whole
 * pitch is "every figure is sourced" cannot be the page quoting a stale price
 * for our own product.
 */
export type WpmgrTier = { name: string; sites: number; perMonth: number };

/** The cheapest hosted tier covering `sites`, or null above the largest one. */
export function resolveWpmgrTier(tiers: WpmgrTier[], sites: number): WpmgrTier | null {
  return tiers.find((t) => sites <= t.sites) ?? null;
}
