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
// These are the only pages on the site permitted to name competitor products,
// and the type is shaped to enforce the rule that makes that safe: EVERY
// factual claim about another product carries the URL it came from and the
// date it was checked. A comparison page is the one page whose whole value is
// being trustworthy, so an unsourced claim costs more here than anywhere else.
//
// `strengths` is required and must be non-empty for each competitor. A
// comparison in which the author wins on every axis is not believed by anyone,
// and the credibility spent admitting what a rival does better is what makes
// the rest of the page land.
// ---------------------------------------------------------------------------

/** One checkable statement about a named product, with its provenance. */
export type SourcedClaim = {
  /** pricing | hosting | install-count | features | licensing | limits */
  topic: string;
  /** One neutral factual sentence. */
  claim: string;
  /** The product owner's own page. Never a review site or a listicle. */
  sourceUrl: string;
  /** ISO date this was last checked against sourceUrl. */
  verifiedOn: string;
};

export type ComparedProduct = {
  name: string;
  /** Short neutral description in our own words. */
  summary: string;
  claims: SourcedClaim[];
  /** What this product genuinely does better than WPMgr. Must be non-empty. */
  strengths: string[];
  /** Their wordpress.org listing or product site. */
  url: string;
};

/** One row of the at-a-glance table. */
export type ComparisonRow = {
  label: string;
  /** Keyed by product name, plus "WPMgr". Free text, not a tick or a cross: a
   *  boolean cannot express "paid add-on" or "incremental only on annual". */
  values: Record<string, string>;
};

export type ComparisonPageData = {
  slug: string;
  title: string;
  metaTitle: string;
  metaDescription: string;
  /** The query this page is written to answer, recorded so intent stays honest. */
  targetQuery: string;
  hero: {
    heading: string;
    subhead: string;
  };
  /** Stated up front, because a comparison written by one of the products is
   *  only trustworthy if it says so before the reader works it out. */
  disclosure: string;
  products: ComparedProduct[];
  table: ComparisonRow[];
  /** Who each option actually suits, including cases where it is not us. */
  verdicts: Array<{ heading: string; body: string }>;
  faq: FaqItem[];
};
