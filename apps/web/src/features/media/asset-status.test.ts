import { describe, it, expect } from "vitest";

import { assetStatusPresentation } from "./asset-status";
import type { AssetStatus } from "./types";

// Per-state "snapshot" of the AssetStatusChip's presentation. The repo's vitest
// runs in a node env (no DOM — see use-updates.test.ts), so we snapshot the
// pure presentation table the chip renders from rather than the rendered DOM.
// This pins the tone/label/pulse/variant/interactive contract for all 8 states.

const ALL_STATUSES: AssetStatus[] = [
  "pending",
  "optimizing",
  "optimized",
  "failed",
  "restoring",
  "restored",
  "excluded",
  "originals_deleted",
];

describe("assetStatusPresentation", () => {
  it("maps every one of the 8 statuses to a stable presentation", () => {
    const table = Object.fromEntries(
      ALL_STATUSES.map((s) => [s, assetStatusPresentation(s)]),
    );
    expect(table).toMatchInlineSnapshot(`
      {
        "excluded": {
          "interactive": false,
          "label": "Excluded",
          "pulse": false,
          "tone": "muted",
          "variant": "ring",
        },
        "failed": {
          "interactive": true,
          "label": "Failed",
          "pulse": false,
          "tone": "destructive",
          "variant": "fill",
        },
        "optimized": {
          "interactive": false,
          "label": "Optimized",
          "pulse": false,
          "tone": "success",
          "variant": "fill",
        },
        "optimizing": {
          "interactive": false,
          "label": "Optimizing",
          "pulse": true,
          "tone": "info",
          "variant": "fill",
        },
        "originals_deleted": {
          "interactive": false,
          "label": "Originals deleted",
          "pulse": false,
          "tone": "warning",
          "variant": "fill",
        },
        "pending": {
          "interactive": false,
          "label": "Pending",
          "pulse": false,
          "tone": "info",
          "variant": "fill",
        },
        "restored": {
          "interactive": false,
          "label": "Restored",
          "pulse": false,
          "tone": "muted",
          "variant": "fill",
        },
        "restoring": {
          "interactive": false,
          "label": "Restoring",
          "pulse": true,
          "tone": "info",
          "variant": "fill",
        },
      }
    `);
  });

  it("only excluded is ring-only; only failed is interactive", () => {
    for (const s of ALL_STATUSES) {
      const p = assetStatusPresentation(s);
      expect(p.variant === "ring").toBe(s === "excluded");
      expect(p.interactive).toBe(s === "failed");
    }
  });

  it("only optimizing and restoring pulse (live in-flight states)", () => {
    for (const s of ALL_STATUSES) {
      const p = assetStatusPresentation(s);
      expect(p.pulse).toBe(s === "optimizing" || s === "restoring");
    }
  });

  it("appends a clamped percentage to the optimizing label", () => {
    expect(assetStatusPresentation("optimizing", 42).label).toBe(
      "Optimizing • 42%",
    );
    expect(assetStatusPresentation("optimizing", 0).label).toBe(
      "Optimizing • 0%",
    );
    expect(assetStatusPresentation("optimizing", 250).label).toBe(
      "Optimizing • 100%",
    );
    expect(assetStatusPresentation("optimizing", -5).label).toBe(
      "Optimizing • 0%",
    );
  });

  it("ignores progress for non-optimizing statuses", () => {
    expect(assetStatusPresentation("optimized", 42).label).toBe("Optimized");
  });
});
