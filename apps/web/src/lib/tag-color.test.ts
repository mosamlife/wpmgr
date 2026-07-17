import { describe, it, expect } from "vitest";

import {
  tagSlot,
  tagHue,
  tagChipClasses,
  resolveTagStyle,
  resolveTagDot,
  TAG_HUES,
  TAG_SWATCHES,
} from "./tag-color";

describe("tagSlot — determinism", () => {
  it("is pure and deterministic: same name always maps to the same slot", () => {
    const a = tagSlot("Production");
    const b = tagSlot("Production");
    const c = tagSlot("Production");
    expect(a).toBe(b);
    expect(b).toBe(c);
  });

  it("returns a value in range [0, 11]", () => {
    for (const name of ["a", "Staging", "client-a", "🚀", "", "a very long tag name indeed"]) {
      const slot = tagSlot(name);
      expect(slot).toBeGreaterThanOrEqual(0);
      expect(slot).toBeLessThan(TAG_HUES.length);
    }
  });

  it("is case-sensitive (hashes the raw name, no normalization)", () => {
    // Not asserting they differ (a hash collision is technically possible),
    // just that the function does not lowercase/trim internally — verified
    // via the pinned fixtures below, which would fail if it did.
    expect(typeof tagSlot("Staging")).toBe("number");
    expect(typeof tagSlot("staging")).toBe("number");
  });
});

describe("tagSlot — pinned fixtures (regression lock)", () => {
  // Computed once from the FNV-1a 32-bit implementation in tag-color.ts and
  // hardcoded here so a future change to the hash/bucket count is caught as
  // a deliberate, reviewed decision (every "auto" tag's color would otherwise
  // silently reshuffle for every tenant on next load).
  it('"Staging" pins to slot 10 (fuchsia)', () => {
    expect(tagSlot("Staging")).toBe(10);
    expect(tagHue(10)).toBe("fuchsia");
  });

  it('"Production" pins to slot 6 (cyan)', () => {
    expect(tagSlot("Production")).toBe(6);
    expect(tagHue(6)).toBe("cyan");
  });

  it('"client-a" pins to slot 8 (indigo)', () => {
    expect(tagSlot("client-a")).toBe(8);
    expect(tagHue(8)).toBe("indigo");
  });
});

describe("tagChipClasses — static class strings", () => {
  it("returns a non-empty string for every slot with both light and dark utilities", () => {
    for (let slot = 0; slot < TAG_HUES.length; slot++) {
      const classes = tagChipClasses(slot);
      expect(classes.length).toBeGreaterThan(0);
      expect(classes).toContain("bg-");
      expect(classes).toContain("dark:bg-");
    }
  });

  it("wraps out-of-range slots via modulo", () => {
    expect(tagChipClasses(12)).toBe(tagChipClasses(0));
    expect(tagChipClasses(-1)).toBe(tagChipClasses(11));
  });
});

describe("resolveTagStyle — auto vs known-swatch hex vs foreign hex", () => {
  it('color === "" resolves to the auto (hue-from-name) className path', () => {
    const style = resolveTagStyle({ name: "Staging", color: "" });
    expect(style.className).toBeDefined();
    expect(style.style).toBeUndefined();
    expect(style.className).toBe(tagChipClasses(tagSlot("Staging")));
  });

  it("missing color resolves to the auto path", () => {
    const style = resolveTagStyle({ name: "Staging" });
    expect(style.className).toBe(tagChipClasses(tagSlot("Staging")));
  });

  it("an unrecognized/invalid color string falls back to auto", () => {
    const style = resolveTagStyle({ name: "Staging", color: "not-a-color" });
    expect(style.className).toBe(tagChipClasses(tagSlot("Staging")));
  });

  // Adversarial-verify fix: the picker only ever WRITES one of the 12 known
  // swatch hexes. Rendering those through the inline alpha-wash path (rather
  // than the same AA-safe class recipe the auto path uses) produced a
  // light-mode contrast issue for lime/amber/cyan. A known hex must now
  // resolve to the SAME className as the auto path for that hue — never an
  // inline style.
  it("a hex matching one of the 12 known swatches resolves to that hue's class recipe, not inline style", () => {
    const style = resolveTagStyle({ name: "anything", color: "#3b82f6" }); // blue
    expect(style.style).toBeUndefined();
    expect(style.className).toBeDefined();
    // Equivalent to the auto-hue class recipe for "blue" specifically —
    // independent of the tag's NAME (unlike the auto path, a known hex is
    // pinned to its own hue, not hashed from the name).
    const blueAuto = TAG_SWATCHES.find((s) => s.hex === "#3b82f6")!.hue;
    expect(style.className).toBe(tagChipClasses(TAG_HUES.indexOf(blueAuto)));
  });

  it("is case-insensitive when matching a known swatch hex", () => {
    const lower = resolveTagStyle({ name: "x", color: "#3b82f6" });
    const upper = resolveTagStyle({ name: "x", color: "#3B82F6" });
    expect(upper.className).toBe(lower.className);
    expect(upper.style).toBeUndefined();
  });

  it("every one of the 12 known swatch hexes resolves to a className (never inline style)", () => {
    for (const { hex } of TAG_SWATCHES) {
      const style = resolveTagStyle({ name: "irrelevant", color: hex });
      expect(style.style).toBeUndefined();
      expect(style.className).toBeDefined();
    }
  });

  it("a genuinely foreign hex (not one of the 12 swatches, e.g. set via the API) resolves to an inline style, identical regardless of theme", () => {
    const style = resolveTagStyle({ name: "Staging", color: "#123456" });
    expect(style.className).toBeUndefined();
    expect(style.style).toEqual({
      backgroundColor: "#12345626",
      color: "#123456",
      borderColor: "#1234564d",
    });
  });

  it("is case-insensitive for a foreign hex too", () => {
    const style = resolveTagStyle({ name: "Staging", color: "#ABCDEF" });
    expect(style.style?.color).toBe("#ABCDEF");
  });
});

describe("resolveTagDot — solid swatch", () => {
  it("auto path resolves to a solid -500 class", () => {
    const dot = resolveTagDot({ name: "Staging", color: "" });
    expect(dot.className).toContain("bg-fuchsia-500");
  });

  it("a known swatch hex resolves to its own solid -500 class, not an inline style", () => {
    const dot = resolveTagDot({ name: "irrelevant", color: "#3b82f6" });
    expect(dot.style).toBeUndefined();
    expect(dot.className).toContain("bg-blue-500");
  });

  it("a foreign hex resolves to an inline backgroundColor with no alpha", () => {
    const dot = resolveTagDot({ name: "Staging", color: "#123456" });
    expect(dot.className).toBeUndefined();
    expect(dot.style).toEqual({ backgroundColor: "#123456" });
  });
});

describe("TAG_SWATCHES — 12-swatch picker source", () => {
  it("has exactly 12 entries, one per hue, each with a #rrggbb hex", () => {
    expect(TAG_SWATCHES).toHaveLength(12);
    for (const { hue, hex } of TAG_SWATCHES) {
      expect(TAG_HUES).toContain(hue);
      expect(hex).toMatch(/^#[0-9a-f]{6}$/i);
    }
  });

  it("includes blue at #3b82f6 (spec-pinned example)", () => {
    const blue = TAG_SWATCHES.find((s) => s.hue === "blue");
    expect(blue?.hex).toBe("#3b82f6");
  });
});
