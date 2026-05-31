import type { StatusTone } from "@/components/status/status-dot";

import type { AssetStatus } from "./types";

// Pure status → presentation mapping for the Media Optimizer's 8 asset
// statuses. Extracted from the chip component so it is unit-testable without a
// DOM (the repo's vitest runs in a node env — see use-updates.test.ts). The
// AssetStatusChip component is a thin presentational wrapper over this table.
//
// Tone mapping per the Phase 5 prompt (DESIGN tokens):
//   pending           → info-subtle
//   optimizing        → pulse info, "Optimizing • {n}%"
//   optimized         → success
//   failed            → destructive (click → reason)
//   restoring         → pulse info
//   restored          → muted
//   excluded          → ring-only (muted, no fill — "not optimizable")
//   originals_deleted → warning-subtle

/** Visual variant — drives the chip's fill vs ring-only treatment. */
export type AssetChipVariant = "fill" | "ring";

export interface AssetStatusPresentation {
  /** Semantic status-dot tone. */
  tone: StatusTone;
  /** Human label shown next to the dot. */
  label: string;
  /** Perpetual ping pulse — true for in-flight states. */
  pulse: boolean;
  /** Ring-only (no filled chip background) — used for `excluded`. */
  variant: AssetChipVariant;
  /** Whether the chip is interactive (failed → click reveals the reason). */
  interactive: boolean;
}

const PRESENTATION: Record<AssetStatus, AssetStatusPresentation> = {
  pending: {
    tone: "info",
    label: "Pending",
    pulse: false,
    variant: "fill",
    interactive: false,
  },
  optimizing: {
    tone: "info",
    label: "Optimizing",
    pulse: true,
    variant: "fill",
    interactive: false,
  },
  optimized: {
    tone: "success",
    label: "Optimized",
    pulse: false,
    variant: "fill",
    interactive: false,
  },
  failed: {
    tone: "destructive",
    label: "Failed",
    pulse: false,
    variant: "fill",
    interactive: true,
  },
  restoring: {
    tone: "info",
    label: "Restoring",
    pulse: true,
    variant: "fill",
    interactive: false,
  },
  restored: {
    tone: "muted",
    label: "Restored",
    pulse: false,
    variant: "fill",
    interactive: false,
  },
  excluded: {
    tone: "muted",
    label: "Excluded",
    pulse: false,
    variant: "ring",
    interactive: false,
  },
  originals_deleted: {
    tone: "warning",
    label: "Originals deleted",
    pulse: false,
    variant: "fill",
    interactive: false,
  },
};

/**
 * Resolve the presentation for an asset status. For `optimizing`, an optional
 * `progress` (0–100) appends "• {n}%" to the label so the chip reads
 * "Optimizing • 42%" while a job runs.
 */
export function assetStatusPresentation(
  status: AssetStatus,
  progress?: number,
): AssetStatusPresentation {
  const base = PRESENTATION[status];
  if (status === "optimizing" && typeof progress === "number") {
    const pct = Math.max(0, Math.min(100, Math.round(progress)));
    return { ...base, label: `Optimizing • ${pct}%` };
  }
  return base;
}
