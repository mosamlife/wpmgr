import type { TargetFormat, TargetQuality } from "./types";

// Pure validation for OptimizeDialog. Extracted so it is unit-testable without a
// DOM (matching the repo's node-env vitest). Mirrors the server-side checks in
// service.StartOptimize: target_format ∈ {avif,webp,original},
// target_quality ∈ {lossy,lossless}, and at least one target (selection or
// all-pending) must be present.

export interface OptimizeFormState {
  targetFormat: TargetFormat;
  targetQuality: TargetQuality;
  /** Number of explicitly selected assets (0 when "all pending" is chosen). */
  selectedCount: number;
  /** Whether the operator chose "all pending" rather than a selection. */
  allPending: boolean;
}

export interface OptimizeValidation {
  ok: boolean;
  /** Field-keyed error messages (what/why) for the first failing rule. */
  error: string | null;
}

const FORMATS: ReadonlySet<TargetFormat> = new Set([
  "avif",
  "webp",
  "original",
]);
const QUALITIES: ReadonlySet<TargetQuality> = new Set(["lossy", "lossless"]);

export function validateOptimize(state: OptimizeFormState): OptimizeValidation {
  if (!FORMATS.has(state.targetFormat)) {
    return { ok: false, error: "Choose a target format: AVIF, WebP, or Original." };
  }
  if (!QUALITIES.has(state.targetQuality)) {
    return { ok: false, error: "Choose a quality: Lossy or Lossless." };
  }
  if (!state.allPending && state.selectedCount < 1) {
    return {
      ok: false,
      error: "Select at least one asset, or choose Optimize all pending.",
    };
  }
  return { ok: true, error: null };
}

/** Human one-liner for the "what happens" explainer, given the target. */
export function optimizeExplainer(
  targetFormat: TargetFormat,
  targetQuality: TargetQuality,
): string {
  if (targetFormat === "original") {
    return `We re-encode each image in place at ${targetQuality} quality, keeping its current format. Originals are archived on the site so you can restore.`;
  }
  const fmt = targetFormat === "avif" ? "AVIF" : "WebP";
  return `We encode each image and its registered sizes to ${fmt} at ${targetQuality} quality. The site serves the new variants; originals are archived so you can restore until you delete them.`;
}
