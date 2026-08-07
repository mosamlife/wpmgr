// Shared content types for the marketing site.
// All copy lives in typed modules under lib/content/. Pages consume these
// directly as Server Component props. Phase 2 feature pages extend FeaturePageData.

export type Cta = {
  label: string;
  href: string;
  variant?: "primary" | "secondary" | "ghost";
  icon?: string;
};

export type Chip = {
  icon: string;
  value: string;
  label: string;
};

export type Step = {
  n: string;
  icon: string;
  title: string;
  desc: string;
};

export type FaqItem = {
  q: string;
  a: string;
};

// Feature grid types (mirrors content.ts for compatibility)
export type FeatureVisual = "cache-trend" | "rum-distribution" | "media-compare";

export type ClusterFeature = {
  icon: string;
  title: string;
  summary: string;
  bullets: string[];
  link?: { href: `#${string}` };
  visual?: FeatureVisual;
};

export type FeatureCluster = {
  id: `platform-${string}`;
  icon: string;
  name: string;
  tagline: string;
  features: ClusterFeature[];
};

// Phase 2 extension: per-feature page data shape
export type FeaturePageData = {
  slug: string;
  title: string;
  metaTitle: string;
  metaDescription: string;
  hero: {
    eyebrow: string;
    heading: string;
    subhead: string;
    primaryCta: Cta;
    secondaryCta?: Cta;
  };
  problem: {
    heading: string;
    body: string;
  };
  steps: Step[];
  subFeatures: Array<{
    icon: string;
    title: string;
    desc: string;
  }>;
  faq: FaqItem[];
  siblingLinks: Array<{ label: string; href: string }>;
  solutionLinks: Array<{ label: string; href: string }>;
};

// Phase 3 extension: per-solution page data shape
export type SolutionStat = {
  icon: string;
  value: string;
  label: string;
};

export type SolutionFeatureCard = {
  featureSlug: string;
  icon: string;
  title: string;
  summary: string;
  href: string;
};

export type SolutionPageData = {
  slug: string;
  /** Short display title, used in hub cards and breadcrumb */
  title: string;
  /** H1: the primary keyword phrase for this solution */
  heading: string;
  metaTitle: string;
  metaDescription: string;
  hero: {
    /** Problem-framed subheading that precedes the H1 */
    eyebrow: string;
    subhead: string;
    primaryCta: Cta;
    secondaryCta?: Cta;
  };
  /** Outcome narrative block (2 to 4 sentences) */
  outcomes: {
    heading: string;
    body: string;
  };
  /** The 3 to 5 proving feature cards that link down to feature pages */
  provingFeatures: SolutionFeatureCard[];
  /** Stats strip (3 items, tabular figures) */
  stats: SolutionStat[];
  faq: FaqItem[];
  /** Optional layout hint so each solution page feels distinct */
  layoutVariant?: "default" | "split" | "compact";
};

// ---------------------------------------------------------------------------
// Comparison pages (/compare/[slug]).
//
// These are the only pages permitted to name competitor products.
//
// TWO RULES, AND THEY ARE NOT THE SAME RULE.
//
//   WHAT A CELL SAYS must be true and sourced. Every claim about a named
//   company carries the URL it came from and the date it was checked, and the
//   page renders both. A comparison page that misstates a rival's price is the
//   one thing that gets screenshotted, and it would cost us the only asset
//   this page has.
//
//   WHICH ROWS APPEAR is ours to choose. Row selection is positioning, not
//   accuracy. We pick the axes the page argues on; we do not shade what sits
//   in the cells once picked.
//
// GROUP ORDER IS LOAD BEARING. The first group is parity: rows where all three
// products deliver. It costs us nothing, it answers "the new one must be
// missing things" before a reader forms the thought, and it makes the groups
// that follow believable. Do not reorder it to lead with a win.
// ---------------------------------------------------------------------------

/** One checkable statement about a named product, with its provenance. */
export type SourcedClaim = {
  /** Stable anchor id, used by matrix footnotes and the /sources register. */
  id: string;
  topic: string;
  claim: string;
  /** The page it was fetched from. Vendor site, docs, repo or wordpress.org. */
  sourceUrl: string;
  /** ISO date this was last checked. */
  verifiedOn: string;
};

/**
 * Cell tone. Semantic, never per product: a tone says what the CELL means, so
 * tinting our own column wholesale is not available and the parity group stays
 * credible.
 */
export type MatrixTone = "included" | "paid" | "partial" | "absent" | "neutral";

export type MatrixCell = {
  /** Free text, never a tick. A tick cannot say "paid add-on". */
  value: string;
  tone: MatrixTone;
  /** SourcedClaim ids backing this cell. A row is a claim we are making. */
  cites?: string[];
};

export type MatrixRow = {
  label: string;
  /** Keyed by product key, plus "wpmgr". Every product needs an entry. */
  cells: Record<string, MatrixCell>;
};

export type MatrixGroup = {
  label: string;
  rows: MatrixRow[];
};

export type ComparedProduct = {
  /** Stable key used across cells and lanes. */
  key: string;
  name: string;
  url: string;
  claims: SourcedClaim[];
};

/** One product's cost curve, computed live from published prices. */
export type CostModel = {
  productKey: string;
  label: string;
  /** Per site per period, in USD. 0 for free. */
  perSite: number;
  /** Flat fee covering `bundleCovers` sites, when the vendor offers one. */
  bundle?: number;
  bundleCovers?: number;
  /** A single flat fee for unlimited sites. */
  flat?: number;
  period: "month" | "year";
  note: string;
  cites?: string[];
};

/** One lane of the data-locality diagram. */
export type LocalityLane = {
  productKey: string;
  /** Node labels, left to right: where the data goes. */
  path: string[];
  note: string;
  cites?: string[];
};

export type ComparisonPageData = {
  slug: string;
  title: string;
  metaTitle: string;
  metaDescription: string;
  /** The query this page answers, recorded so intent stays honest. */
  targetQuery: string;
  hero: {
    heading: string;
    subhead: string;
    chips: string[];
  };
  products: ComparedProduct[];
  matrix: MatrixGroup[];
  /** The consolidation visual: what one tool replaces. */
  replaces: {
    heading: string;
    subhead: string;
    items: Array<{ icon: string; label: string }>;
  };
  cost: {
    heading: string;
    subhead: string;
    models: CostModel[];
  };
  locality: {
    heading: string;
    subhead: string;
    lanes: LocalityLane[];
  };
  faq: FaqItem[];
};
