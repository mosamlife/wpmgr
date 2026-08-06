// Build-time adapters: maps HUB_CLUSTERS + SOLUTION_HUB_CARDS into the menu
// shape and declares the panel / simple-link nav model.
// This is a plain TS module (no "use client") that tree-shakes into the client
// bundle only what is needed for the header.

import { HUB_CLUSTERS, type FeatureHubCluster } from "@/lib/content/features";
import { SOLUTION_HUB_CARDS, type SolutionHubCard } from "@/lib/content/solutions";
import { HEADER_NAV, SITE_CONFIG } from "@/lib/site";

// ---------------------------------------------------------------------------
// Shared row shape used in both panels
// ---------------------------------------------------------------------------

export type NavRow = {
  href: string;
  icon: string;
  title: string;
  summary: string;
};

// ---------------------------------------------------------------------------
// Features panel: 5 columns, one per cluster
// ---------------------------------------------------------------------------

export type FeaturesColumn = {
  id: string;
  icon: string;
  name: string;
  tagline: string;
  rows: NavRow[];
};

export const FEATURES_COLUMNS: FeaturesColumn[] = HUB_CLUSTERS.map(
  (cluster: FeatureHubCluster) => ({
    id: cluster.id,
    icon: cluster.icon,
    name: cluster.name,
    tagline: cluster.tagline,
    rows: cluster.features.map((f) => ({
      href: `/features/${f.slug}/`,
      icon: f.icon,
      title: f.title,
      summary: f.summary,
    })),
  }),
);

// ---------------------------------------------------------------------------
// Solutions panel: 2 columns partitioned by group
// ---------------------------------------------------------------------------

export type SolutionsColumn = {
  label: string;
  rows: NavRow[];
};

export const SOLUTIONS_COLUMNS: SolutionsColumn[] = [
  {
    label: "By audience",
    rows: SOLUTION_HUB_CARDS.filter((c: SolutionHubCard) => c.group === "audience").map((c) => ({
      href: `/solutions/${c.slug}/`,
      icon: c.icon,
      title: c.title,
      summary: c.summary,
    })),
  },
  {
    label: "By job to be done",
    rows: SOLUTION_HUB_CARDS.filter((c: SolutionHubCard) => c.group === "jtbd").map((c) => ({
      href: `/solutions/${c.slug}/`,
      icon: c.icon,
      title: c.title,
      summary: c.summary,
    })),
  },
];

// ---------------------------------------------------------------------------
// Top-level nav model
// ---------------------------------------------------------------------------

export type PanelId = "features" | "solutions";

export type NavItemSimple = {
  kind: "link";
  label: string;
  href: string;
  external?: boolean;
};

export type NavItemPanel = {
  kind: "panel";
  id: PanelId;
  label: string;
  href: string;
};

export type NavTopItem = NavItemSimple | NavItemPanel;

// Map HEADER_NAV into our typed model. Index 0=Features, 1=Solutions are
// panels; the rest are plain links. Literal indices are stable (checked at
// build time via the HEADER_NAV definition above) but TypeScript strict mode
// widens array index access to T|undefined; assert with non-null since these
// indices are known to exist.
export const NAV_ITEMS: NavTopItem[] = [
  { kind: "panel", id: "features", label: HEADER_NAV[0]!.label, href: HEADER_NAV[0]!.href },
  { kind: "panel", id: "solutions", label: HEADER_NAV[1]!.label, href: HEADER_NAV[1]!.href },
  { kind: "link", label: HEADER_NAV[2]!.label, href: HEADER_NAV[2]!.href },
  { kind: "link", label: HEADER_NAV[3]!.label, href: HEADER_NAV[3]!.href },
  { kind: "link", label: HEADER_NAV[4]!.label, href: HEADER_NAV[4]!.href },
];

// Callout cell in the features panel right rail (one only).
export const FEATURES_CALLOUT = {
  href: "/features/security",
  icon: "ShieldCheck",
  title: "Security suite",
  description:
    "Hardening, IP bans, file integrity, vulnerability scanning, and site-user 2FA in one place.",
  ctaLabel: "See the security suite",
};

// Docs URL for external CTA in panels.
export const DOCS_URL = SITE_CONFIG.docs;
