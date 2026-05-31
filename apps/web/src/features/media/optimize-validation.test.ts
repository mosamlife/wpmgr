import { describe, it, expect } from "vitest";

import {
  validateOptimize,
  optimizeExplainer,
  type OptimizeFormState,
} from "./optimize-validation";

// OptimizeDialog validation — pure, DOM-free coverage of the rules the dialog's
// confirm button reflects (mirrors the server checks in service.StartOptimize).

function base(overrides: Partial<OptimizeFormState> = {}): OptimizeFormState {
  return {
    targetFormat: "avif",
    targetQuality: "lossy",
    selectedCount: 3,
    allPending: false,
    ...overrides,
  };
}

describe("validateOptimize", () => {
  it("accepts a valid selection-based request", () => {
    expect(validateOptimize(base())).toEqual({ ok: true, error: null });
  });

  it("accepts all-pending with no explicit selection", () => {
    expect(
      validateOptimize(base({ selectedCount: 0, allPending: true })),
    ).toEqual({ ok: true, error: null });
  });

  it("rejects an empty selection that is not all-pending", () => {
    const r = validateOptimize(base({ selectedCount: 0, allPending: false }));
    expect(r.ok).toBe(false);
    expect(r.error).toMatch(/select at least one/i);
  });

  it("rejects an invalid target format", () => {
    const r = validateOptimize(
      base({ targetFormat: "gif" as unknown as OptimizeFormState["targetFormat"] }),
    );
    expect(r.ok).toBe(false);
    expect(r.error).toMatch(/format/i);
  });

  it("rejects an invalid quality", () => {
    const r = validateOptimize(
      base({
        targetQuality: "ultra" as unknown as OptimizeFormState["targetQuality"],
      }),
    );
    expect(r.ok).toBe(false);
    expect(r.error).toMatch(/quality/i);
  });

  it("accepts every valid format × quality combination", () => {
    const formats = ["avif", "webp", "original"] as const;
    const qualities = ["lossy", "lossless"] as const;
    for (const f of formats) {
      for (const q of qualities) {
        expect(
          validateOptimize(base({ targetFormat: f, targetQuality: q })).ok,
        ).toBe(true);
      }
    }
  });
});

describe("optimizeExplainer", () => {
  it("describes an in-place re-encode for the 'original' target", () => {
    expect(optimizeExplainer("original", "lossless")).toMatch(/in place/i);
    expect(optimizeExplainer("original", "lossless")).toMatch(/lossless/);
  });

  it("names the target format for avif/webp", () => {
    expect(optimizeExplainer("avif", "lossy")).toMatch(/AVIF/);
    expect(optimizeExplainer("webp", "lossy")).toMatch(/WebP/);
  });
});
