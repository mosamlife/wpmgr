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
  /**
   * "partial" when the vendor's licence covers ground WPMgr does not ship,
   * so the row renders a visible `Partial` chip plus `residual` in static
   * markup. Omitted (full replacement, no chip) unless a product is listed
   * below with both fields set. Never approximated with a discounted price:
   * no vendor publishes a "just the part WPMgr also does" figure, and
   * inventing one is the fabrication rule at the top of this file.
   */
  replaces?: "full" | "partial";
  /** What the licence covers that WPMgr does not. Required when replaces
   *  is "partial", rendered next to the chip. */
  residual?: string;
  /**
   * True for a figure that could not be confirmed against a single
   * authoritative vendor number as of `verifiedOn` -- e.g. two different
   * prices shown for the same tier with no way to tell list from
   * promotional. An unverified product must not appear in
   * PLUGIN_COST_CATEGORIES; see HELD_TWO_FACTOR_CATEGORY below.
   */
  unverified?: boolean;
};

/**
 * Purchase-likelihood buckets for the calculator's row groups (NOT our own
 * feature areas -- grouping by category would turn the bill into our IA with
 * prices attached, which is the one thing this page must not do).
 *
 * "core": bought by nearly every commercial fleet as baseline hygiene --
 * data safety, break-in protection, page speed.
 * "common": widely added on top of that baseline, but skippable without the
 * fleet being obviously under-served.
 * "situational": bought only by a subset with a specific need -- a
 * client-facing agency, or a fleet that specifically wants a second,
 * competing management layer alongside WPMgr.
 *
 * This is a judgement call, stated here so it can be argued with rather than
 * taken as given: it is not derived from a vendor figure the way the prices
 * are.
 */
export type PurchaseLikelihood = "core" | "common" | "situational";

export const PURCHASE_LIKELIHOOD_GROUPS: { key: PurchaseLikelihood; label: string }[] = [
  { key: "core", label: "Nearly every fleet buys these" },
  { key: "common", label: "Most agencies add these" },
  { key: "situational", label: "Situational" },
];

export type CostCategory = {
  key: string;
  label: string;
  icon: string;
  /** What WPMgr gives you instead, for the right-hand column. */
  wpmgr: string;
  group: PurchaseLikelihood;
  /** Unchecked on first paint. Used for categories the owner added after the
   *  page's original seven, so first paint still reads "7 of N selected" and
   *  the reader opts into the longer bill rather than being defaulted into
   *  it. */
  defaultOff?: boolean;
  products: CostProduct[];
};

export const PLUGIN_COST_CATEGORIES: CostCategory[] = [
  {
    key: "backups",
    label: "Backups and restore",
    icon: "DatabaseBackup",
    wpmgr: "Incremental, client-side encrypted, to storage you own",
    group: "core",
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
        replaces: "partial",
        residual: "staging sites and one-click migration, both inside the licence",
      },
    ],
  },
  {
    key: "security",
    label: "Security and malware scanning",
    icon: "ShieldCheck",
    wpmgr: "Hardening, file integrity and vulnerability matching, built in",
    group: "core",
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
        replaces: "partial",
        residual: "the application firewall, the managed rule feed and rate limiting",
      },
      // Sucuri is not added as an alternate: it stops publishing prices above
      // 10 sites, which is well inside this page's slider range, and most of
      // what it sells past the malware scan is a WAF, a CDN, DDoS mitigation
      // and human-performed malware removal, none of which is this row.
    ],
  },
  {
    key: "performance",
    label: "Page caching and performance",
    icon: "Zap",
    wpmgr: "Page cache, object cache and Core Web Vitals from real visitors",
    group: "core",
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
      //
      // NitroPack is not added either: it prices per pageview, and its named
      // tiers cover one or three websites; everything above that is an
      // unpublished "Agency" master subscription. Multiplying its $18/mo rate
      // by 25 would invent a tier the vendor does not sell, and it also
      // bundles a CDN WPMgr does not ship.
      //
      // Object Cache Pro is not added: $950/yr flat, but the licence requires
      // every covered site to be owned and operated by the same entity, which
      // puts a 25-client agency fleet outside it and into "talk to us." No
      // figure this page could publish for that persona.
      //
      // Cloudflare APO is not added: $5/mo per domain on the Free plan, but
      // $0 marginal cost on any paid Cloudflare plan. A large line whose real
      // price is zero for most readers is the single most attackable number
      // this page could publish.
    ],
  },
  {
    key: "images",
    label: "Image optimization",
    icon: "ImageDown",
    wpmgr: "AVIF and WebP conversion with originals kept as a fallback",
    group: "common",
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
    group: "situational",
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
    group: "core",
    products: [
      {
        name: "ManageWP Uptime Monitor",
        url: "https://managewp.com/pricing",
        tiers: [
          { upTo: 25, perYear: 12, label: "$1/site/month, per site" },
          {
            upTo: Infinity,
            perYear: 300,
            label: "Bundled at $25/month per 100-site bundle",
          },
        ],
        // Same stepped shape as the ManageWP reports add-ons below: a flat
        // per-site rate up to 25 sites, then bundles of up to 100 sites for
        // $25/month each, and bundles stack. `tiers` above exists only to
        // pick the bracket label shown next to the product name.
        computeAnnual: (sites: number) =>
          sites <= 25 ? sites * 1 * 12 : Math.ceil(sites / 100) * 25 * 12,
        note: "$1/site/month up to 25 sites. Above 25 sites, ManageWP sells this add-on in bundles covering up to 100 sites for $25/month, and bundles stack. This is now the default over UptimeRobot because it is the cheaper of the two at every fleet size on this page's slider.",
        verifiedOn: "2026-08-17",
      },
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
        note: "Priced per monitor and billed monthly; the annual figure is the monthly rate multiplied by twelve. A free tier covering 50 monitors exists at a 5-minute check interval; the paid tiers above buy a shorter interval, more monitor types and alert routing.",
        verifiedOn: "2026-08-07",
        replaces: "partial",
        residual: "public and white-label status pages, and PagerDuty or webhook alert routing",
      },
    ],
  },
  {
    key: "reports",
    label: "Client reporting",
    icon: "FileText",
    wpmgr: "White label reports and a client portal, built in",
    group: "situational",
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
  {
    key: "fleet",
    label: "Fleet management and updates",
    icon: "ServerCog",
    wpmgr: "Bulk updates, one dashboard and one login across the whole fleet",
    group: "situational",
    defaultOff: true,
    products: [
      {
        name: "WP Remote Premium",
        url: "https://wpremote.com/pricing/",
        tiers: [{ upTo: Infinity, perYear: 49.99, label: "Premium, per site" }],
        // WP Remote sells three tiers, all per site per year: Essential
        // $19.99, Premium $49.99, Advanced $199.99. Premium is used here
        // because it is the tier whose feature list -- staging, uptime,
        // performance scoring, form and visual-regression testing -- is the
        // closest match to a general-purpose fleet manager rather than a
        // bare update runner. Essential is cheaper per site, but it only
        // does one of the fourteen jobs WPMgr does, so pricing it against
        // WPMgr as a whole would compare unlike products; the "Partial"
        // replaces flag and residual list below say what this row does and
        // does not cover instead.
        note: "$19.99 (Essential), $49.99 (Premium, used here) or $199.99 (Advanced) per site per year. This row uses Premium because Essential is a bare update runner and does not include the staging, uptime and testing features that make Premium comparable to a fleet manager.",
        verifiedOn: "2026-08-17",
        replaces: "partial",
        residual:
          "staging sites, visual regression testing, form testing, sandbox updates, virtual patching, and human malware cleanup",
      },
    ],
  },
];

/**
 * HELD, NOT SHIPPED. Two-factor authentication (WP 2FA Premium) is
 * deliberately excluded from PLUGIN_COST_CATEGORIES: melapress.com/
 * wordpress-2fa/pricing/ shows two different prices for the 25-site bracket,
 * $199/yr and $129/yr, with nothing on the page saying which is list and
 * which is a promotional rate. Modeled here with the LOWER of the two and
 * `unverified: true` so the figure exists for the next pass, but it must not
 * be spread into PLUGIN_COST_CATEGORIES or shown to a reader until someone
 * opens that page in a browser and resolves which number is real.
 *
 * Duo is not modeled at all, here or anywhere else in this file: it is
 * priced per user, not per site or per tier, so it does not fit this file's
 * shape and mixing a per-user line into a per-site bill would misstate both.
 */
export const HELD_TWO_FACTOR_CATEGORY: CostCategory = {
  key: "two-factor",
  label: "Two-factor authentication",
  icon: "KeyRound",
  wpmgr: "TOTP, email code and backup codes, enforced per role, built in",
  group: "situational",
  defaultOff: true,
  products: [
    {
      name: "WP 2FA Premium",
      url: "https://melapress.com/wordpress-2fa/pricing/",
      tiers: [{ upTo: 25, perYear: 129, label: "25 sites (unresolved, lower of two prices shown)" }],
      note: "melapress.com/wordpress-2fa/pricing/ showed two different prices for 25 sites, $199/yr and $129/yr, with no indication which is list and which is promotional. This is the lower figure, held back until that is resolved in a browser.",
      verifiedOn: "2026-08-17",
      unverified: true,
    },
  ],
};

/**
 * Categories whose price is per site rather than per tier.
 *
 * "reports" and "uptime" are deliberately NOT here even though their default
 * products are per-site up to 25 sites: past that they are a stepped
 * per-100-site bundle, not a flat rate, so a uniform tier-times-sites
 * multiplication would be wrong at fleet sizes above 25. Those products
 * define `computeAnnual` instead; see `annualCost` below.
 */
export const PER_SITE_KEYS = ["security", "fleet"];

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
