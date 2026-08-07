// Comparison page registry.
//
// These pages are the ONLY place on the marketing site that names competitor
// products. See lib/content/types.ts for the shape and the reasoning, and
// scripts/check-copy.mjs for the disparagement rule that is scoped to this
// directory.

import type { ComparisonPageData } from "@/lib/content/types";
import { MANAGEWP_VS_MAINWP } from "./managewp-vs-mainwp";

export const COMPARE_REGISTRY: Record<string, ComparisonPageData> = {
  [MANAGEWP_VS_MAINWP.slug]: MANAGEWP_VS_MAINWP,
};

export const COMPARE_SLUGS = Object.keys(COMPARE_REGISTRY);

export function getComparison(slug: string): ComparisonPageData | null {
  return COMPARE_REGISTRY[slug] ?? null;
}
