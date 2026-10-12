import { describe, expect, it } from "vitest";

import {
  backgroundFraming,
  cornersWords,
  fontWords,
  frameWords,
  patternWords,
  sectionFacts,
  sectionTone,
  spacingWords,
  swatchesOf,
  thumbSize,
  toneSwatch,
  typeRamp,
} from "./design-model";
import { DESIGN_COLOUR_ROLES, PAGE_FRAMES, SCALE_RATIOS, type PageDesign, type SectionNode } from "./outline-model";

// The card draws the sizes the agent writes. This table is the agent's own
// output: DesignTokens::typography() (apps/agent/includes/abilities/builders/
// class-design-tokens.php) for every base size the grammar admits and every
// ratio name, as [display, h2, h3, lead, body, eyebrow, button] in pixels.
const AGENT_SIZES: Array<[number, PageDesign["scale"]["ratio"], number[]]> = [
  [15, "major_second", [27, 21, 19, 17, 15, 12, 14]],
  [15, "minor_third", [37, 26, 22, 18, 15, 12, 14]],
  [15, "major_third", [46, 29, 23, 19, 15, 12, 14]],
  [15, "perfect_fourth", [63, 36, 27, 20, 15, 12, 14]],
  [16, "major_second", [29, 23, 20, 18, 16, 13, 15]],
  [16, "minor_third", [40, 28, 23, 19, 16, 13, 15]],
  [16, "major_third", [49, 31, 25, 20, 16, 13, 15]],
  [16, "perfect_fourth", [67, 38, 28, 21, 16, 13, 15]],
  [17, "major_second", [31, 24, 22, 19, 17, 14, 16]],
  [17, "minor_third", [42, 29, 24, 20, 17, 14, 16]],
  [17, "major_third", [52, 33, 27, 21, 17, 14, 16]],
  [17, "perfect_fourth", [72, 40, 30, 23, 17, 14, 16]],
  [18, "major_second", [32, 26, 23, 20, 18, 14, 17]],
  [18, "minor_third", [45, 31, 26, 22, 18, 14, 17]],
  [18, "major_third", [55, 35, 28, 23, 18, 14, 17]],
  [18, "perfect_fourth", [76, 43, 32, 24, 18, 14, 17]],
  [19, "major_second", [34, 27, 24, 21, 19, 15, 18]],
  [19, "minor_third", [47, 33, 27, 23, 19, 15, 18]],
  [19, "major_third", [58, 37, 30, 24, 19, 15, 18]],
  [19, "perfect_fourth", [80, 45, 34, 25, 19, 15, 18]],
  [20, "major_second", [36, 28, 25, 23, 20, 16, 19]],
  [20, "minor_third", [50, 35, 29, 24, 20, 16, 19]],
  [20, "major_third", [61, 39, 31, 25, 20, 16, 19]],
  [20, "perfect_fourth", [84, 47, 36, 27, 20, 16, 19]],
];

describe("the type ramp", () => {
  it("covers every base size and ratio the grammar admits", () => {
    expect(new Set(AGENT_SIZES.map(([, ratio]) => ratio))).toEqual(new Set(SCALE_RATIOS));
    expect(new Set(AGENT_SIZES.map(([base]) => base))).toEqual(new Set([15, 16, 17, 18, 19, 20]));
  });

  it.each(AGENT_SIZES)("writes the agent's sizes for a base of %i px and %s", (base_px, ratio, sizes) => {
    expect(typeRamp({ base_px, ratio }).map((s) => s.size)).toEqual(sizes);
  });

  it("names the seven styles in the agent's order, with its faces and weights", () => {
    expect(typeRamp({ base_px: 17, ratio: "major_third" }).map((s) => [s.style, s.label, s.face, s.weight])).toEqual([
      ["display", "Display", "heading", 700],
      ["h2", "Heading 2", "heading", 700],
      ["h3", "Heading 3", "heading", 600],
      ["lead", "Lead", "body", 400],
      ["body", "Body", "body", 400],
      ["eyebrow", "Eyebrow", "body", 600],
      ["button", "Button", "body", 600],
    ]);
  });
});

describe("the swatches", () => {
  const colors = {
    ground: "#FAF7F0",
    surface: "#EFE8DA",
    ink: "#1B1F2A",
    muted: "#555B66",
    accent: "#B4452B",
    on_accent: "#FFF9F0",
  };

  it("follow the card's order of roles, each with its own colour", () => {
    const swatches = swatchesOf(colors);
    expect(swatches.map((s) => s.role)).toEqual([...DESIGN_COLOUR_ROLES]);
    for (const s of swatches) {
      expect(s.hex).toBe(colors[s.role]);
      expect(s.chip).toBe(colors[s.role]);
    }
  });

  it("paint a chip only with a colour that is a closed shape", () => {
    const bad = swatchesOf({ ...colors, accent: "#B4452B;background:url(//evil.example)", ink: "red", muted: "#555b66" });
    expect(bad.map((s) => s.chip)).toEqual([colors.ground, colors.surface, null, null, null, colors.on_accent]);
  });
});

describe("the words of a design", () => {
  it("say what each page frame is, one distinct name for each", () => {
    const names = PAGE_FRAMES.map((f) => frameWords(f).name);
    expect(new Set(names).size).toBe(PAGE_FRAMES.length);
    expect(frameWords("theme")).toEqual({
      name: "In the theme",
      line: "The theme's own page layout, with its header and footer.",
    });
    expect(frameWords("canvas").line).toBe("No theme header, footer or page title.");
  });

  it("give the corners and the spacing in the agent's own numbers", () => {
    expect([cornersWords("none"), cornersWords("sm"), cornersWords("md"), cornersWords("lg")]).toEqual([
      "Square (0 px)",
      "Soft (6 px)",
      "Rounded (12 px)",
      "Very rounded (20 px)",
    ]);
    expect([spacingWords("compact"), spacingWords("normal"), spacingWords("airy")]).toEqual([
      "Compact (75% of normal)",
      "Normal",
      "Airy (125% of normal)",
    ]);
  });

  it("show the design font system as the site's own font, and any other name as it is", () => {
    expect(fontWords("system")).toBe("The site's own font");
    expect(fontWords("Source Sans 3")).toBe("Source Sans 3");
    expect(fontWords("System")).toBe("System");
  });

  it("use no em or en dash", () => {
    const words = [
      ...PAGE_FRAMES.flatMap((f) => Object.values(frameWords(f))),
      ...swatchesOf({
        ground: "#FAF7F0",
        surface: "#EFE8DA",
        ink: "#1B1F2A",
        muted: "#555B66",
        accent: "#B4452B",
        on_accent: "#FFF9F0",
      }).flatMap((s) => [s.label, s.hint]),
      ...typeRamp({ base_px: 17, ratio: "major_third" }).map((s) => s.label),
      cornersWords("none"),
      cornersWords("sm"),
      cornersWords("md"),
      cornersWords("lg"),
      spacingWords("compact"),
      spacingWords("normal"),
      spacingWords("airy"),
      fontWords("system"),
    ];
    // An en dash (U+2013) or an em dash (U+2014), named by code so this file holds neither.
    const dashes = new RegExp(`[${String.fromCharCode(0x2013)}${String.fromCharCode(0x2014)}]`);
    for (const w of words) expect(w).not.toMatch(dashes);
  });
});

// --- The sections of a designed page

// The tones, the defaults and the overlay strengths are the agent's own:
// ElementorSectionCompiler (DEFAULT_TONE, DEFAULT_WIDTH, DEFAULT_SPACING,
// DEFAULT_OVERLAY, DEFAULT_FOCAL, DEFAULT_MIN_HEIGHT, BACKGROUND_TONE,
// SCREEN_HEIGHT_VH) and DesignTokens::tone() and overlay().

const COLOURS: PageDesign["colors"] = {
  ground: "#FAF7F0",
  surface: "#EFE8DA",
  ink: "#1B1F2A",
  muted: "#555B66",
  accent: "#B4452B",
  on_accent: "#FFF9F0",
};

type SectionLook = Partial<Pick<SectionNode, "tone" | "background" | "width" | "spacing" | "align" | "min_height">>;

describe("the tone a section is written in", () => {
  it.each<[string, SectionLook, string]>([
    ["names none", {}, "ground"],
    ["is ground", { tone: "ground" }, "ground"],
    ["is surface", { tone: "surface" }, "surface"],
    ["is inverse", { tone: "inverse" }, "inverse"],
    ["is accent", { tone: "accent" }, "accent"],
    ["names none, with a background", { background: { attachment_id: 5 } }, "inverse"],
    ["is surface, with a background", { tone: "surface", background: { attachment_id: 5 } }, "inverse"],
    ["is accent, with a background", { tone: "accent", background: { attachment_id: 5 } }, "inverse"],
  ])("a section whose tone %s is written in %s", (_name, section, expected) => {
    expect(sectionTone(section)).toBe(expected);
  });
});

describe("the colour a tone paints", () => {
  it.each([
    ["ground", "Ground", "ground", "#FAF7F0"],
    ["surface", "Surface", "surface", "#EFE8DA"],
    ["inverse", "Inverse", "ink", "#1B1F2A"],
    ["accent", "Accent", "accent", "#B4452B"],
  ] as const)("the %s tone is the design's %s colour", (tone, label, role, hex) => {
    expect(toneSwatch(COLOURS, tone)).toEqual({ tone, label, role, hex, chip: hex });
  });

  it("hands on no chip for a colour that is not a closed shape", () => {
    const odd = { ...COLOURS, accent: "#B4452B;background:url(//evil.example)" };
    expect(toneSwatch(odd, "accent").chip).toBeNull();
  });
});

describe("a section's settings in words", () => {
  it.each<[string, SectionLook, string[]]>([
    ["names none: the agent's defaults", {}, ["Boxed", "Large spacing", "Text at the start", "Height follows content"]],
    [
      "names every option",
      { width: "full", spacing: "xl", align: "center", min_height: "screen" },
      ["Full width", "Extra large spacing", "Text centred", "Height 80% of the screen"],
    ],
    ["names the medium spacing", { spacing: "md" }, ["Boxed", "Medium spacing", "Text at the start", "Height follows content"]],
    ["names the large spacing", { spacing: "lg" }, ["Boxed", "Large spacing", "Text at the start", "Height follows content"]],
  ])("a section that %s", (_name, section, expected) => {
    expect(sectionFacts(section)).toEqual(expected);
  });
});

describe("a background's framing", () => {
  it.each([
    [{}, { overlay: "strong", opacity: 0.65, overlayWords: "Strong overlay", focal: "center", focalTop: 50, focalWords: "Focal point: centre" }],
    [{ overlay: "soft", focal: "top" }, { overlay: "soft", opacity: 0.45, overlayWords: "Soft overlay", focal: "top", focalTop: 12, focalWords: "Focal point: top" }],
    [{ overlay: "none", focal: "bottom" }, { overlay: "none", opacity: null, overlayWords: "No overlay", focal: "bottom", focalTop: 88, focalWords: "Focal point: bottom" }],
  ] as const)("%j is drawn as %j", (named, expected) => {
    expect(backgroundFraming({ attachment_id: 5, ...named })).toEqual(expected);
  });
});

describe("a pattern by name and variant", () => {
  it.each([
    ["hero", "background", "Hero · background"],
    ["hero", "split", "Hero · split"],
    ["hero", "centered", "Hero · centered"],
    ["cta", "band", "Call to action · band"],
    ["features", "split", "Features · split"],
    ["features", "bento", "Features · bento"],
    ["features", "grid", "Features · grid"],
    ["testimonials", "featured", "Testimonials · featured"],
    ["pricing", "three-plans", "Pricing · three plans"],
    ["faq", "accordion", "FAQ · accordion"],
    ["logos", "row", "Logos · row"],
    ["stats", "counters", "Stats · counters"],
  ] as const)("%s %s reads %s", (pattern, variant, words) => {
    expect(patternWords({ pattern, variant })).toBe(words);
  });
});

describe("the miniature of a background", () => {
  it.each([
    [1600, 900, 114],
    [1000, 1000, 64],
    [400, 1200, 48],
    [4000, 500, 160],
    [0, 0, 114],
    [1600, 0, 114],
    [0, 900, 114],
  ])("an image of %i by %i is %i px wide at 64 px high", (width, height, wide) => {
    expect(thumbSize(width, height)).toEqual({ width: wide, height: 64 });
  });
});

describe("the words for sections", () => {
  it("hold no en dash and no em dash", () => {
    const words = [
      ...(["ground", "surface", "inverse", "accent"] as const).map((t) => toneSwatch(COLOURS, t).label),
      ...sectionFacts({}),
      ...sectionFacts({ width: "full", spacing: "xl", align: "center", min_height: "screen" }),
      ...sectionFacts({ spacing: "md" }),
      backgroundFraming({ attachment_id: 5 }).overlayWords,
      backgroundFraming({ attachment_id: 5, overlay: "soft" }).overlayWords,
      backgroundFraming({ attachment_id: 5, overlay: "none" }).overlayWords,
      backgroundFraming({ attachment_id: 5, focal: "top" }).focalWords,
      patternWords({ pattern: "pricing", variant: "three-plans" }),
      patternWords({ pattern: "cta", variant: "band" }),
      patternWords({ pattern: "faq", variant: "accordion" }),
    ];
    const dashes = new RegExp(`[${String.fromCharCode(0x2013)}${String.fromCharCode(0x2014)}]`);
    for (const w of words) expect(w).not.toMatch(dashes);
  });
});
