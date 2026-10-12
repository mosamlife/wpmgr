import { DESIGN_COLOUR_ROLES, type PageDesign, type PageFrame } from "./outline-model";

// What the approval card says about the design of a designed page (outline
// version 3). The words are ours and the numbers are the page's: the sizes are
// the ones the agent writes, so the card draws what the page will carry.
// DesignTokens (apps/agent/includes/abilities/builders/class-design-tokens.php)
// writes each text style in whole pixels, the base size times the ratio to a
// power or a percent of the base size, a half rounded up; design-model.test.ts
// holds the agent's own output for every base size and ratio. Nothing here
// builds markup, and a value that is not a closed shape is never handed on.

export type ColourRole = (typeof DESIGN_COLOUR_ROLES)[number];

const ROLE_WORDS: Record<ColourRole, { label: string; hint: string }> = {
  ground: { label: "Ground", hint: "Background" },
  surface: { label: "Surface", hint: "Alternate background" },
  ink: { label: "Ink", hint: "Headings" },
  muted: { label: "Muted", hint: "Body text" },
  accent: { label: "Accent", hint: "Highlights" },
  on_accent: { label: "On accent", hint: "Text on accent" },
};

/** The colours of a design are "#" and six upper case hex digits once read; anything else is never drawn. */
const CHIP_COLOUR = /^#[0-9A-F]{6}$/;

export interface Swatch {
  readonly role: ColourRole;
  readonly label: string;
  /** What the colour is used for. */
  readonly hint: string;
  /** The colour as written. */
  readonly hex: string;
  /** The colour to paint the chip with: the colour itself when it is a closed shape, else null. */
  readonly chip: string | null;
}

/** The six swatches of a design, in the order DESIGN_COLOUR_ROLES gives. */
export function swatchesOf(colors: PageDesign["colors"]): readonly Swatch[] {
  return DESIGN_COLOUR_ROLES.map((role) => {
    const hex = colors[role];
    return { role, ...ROLE_WORDS[role], hex, chip: CHIP_COLOUR.test(hex) ? hex : null };
  });
}

// --- The type ramp

export type RampStyle = "display" | "h2" | "h3" | "lead" | "body" | "eyebrow" | "button";

export interface RampStep {
  readonly style: RampStyle;
  readonly label: string;
  /** Which of the design's two fonts the style is set in. */
  readonly face: "heading" | "body";
  readonly weight: number;
  /** Whole pixels. */
  readonly size: number;
}

/** What each text style is: the agent's own table (TYPOGRAPHY), in the order it lists them. */
interface RampRule {
  readonly style: RampStyle;
  readonly label: string;
  readonly face: "heading" | "body";
  readonly weight: number;
  /** Steps up the scale from the base size. */
  readonly steps: number;
  /** Percent of the base size, for a style that is a share of it instead of a step. */
  readonly percent?: number;
}

const RAMP: readonly RampRule[] = [
  { style: "display", label: "Display", face: "heading", weight: 700, steps: 5 },
  { style: "h2", label: "Heading 2", face: "heading", weight: 700, steps: 3 },
  { style: "h3", label: "Heading 3", face: "heading", weight: 600, steps: 2 },
  { style: "lead", label: "Lead", face: "body", weight: 400, steps: 1 },
  { style: "body", label: "Body", face: "body", weight: 400, steps: 0 },
  { style: "eyebrow", label: "Eyebrow", face: "body", weight: 600, steps: 0, percent: 80 },
  { style: "button", label: "Button", face: "body", weight: 600, steps: 0, percent: 95 },
];

/** The ratio each name stands for (the agent's DesignTokens::RATIOS). */
const RATIO_VALUES: Record<PageDesign["scale"]["ratio"], number> = {
  major_second: 1.125,
  minor_third: 1.2,
  major_third: 1.25,
  perfect_fourth: 1.333,
};

/**
 * The base size times the ratio, `steps` times, in whole pixels with a half
 * rounded up; repeated multiplication, as the agent does it. A percent style
 * is that share of the base size, a half rounded up.
 */
function sizeOf(basePx: number, ratio: number, rule: RampRule): number {
  if (rule.percent !== undefined) return Math.floor((basePx * rule.percent + 50) / 100);
  let size = basePx;
  for (let i = 0; i < rule.steps; i += 1) size *= ratio;
  return Math.floor(size + 0.5);
}

/** The seven text styles of a design's scale, largest first, as the agent writes them. */
export function typeRamp(scale: PageDesign["scale"]): readonly RampStep[] {
  const ratio = RATIO_VALUES[scale.ratio];
  return RAMP.map((rule) => ({
    style: rule.style,
    label: rule.label,
    face: rule.face,
    weight: rule.weight,
    size: sizeOf(scale.base_px, ratio, rule),
  }));
}

// --- Fonts, corners, spacing and the frame

/** The design font `system` sets no family: the page keeps the site's own. */
const SYSTEM_FONT = "system";

export function fontWords(family: string): string {
  return family === SYSTEM_FONT ? "The site's own font" : family;
}

const CORNERS: Record<PageDesign["radius"], string> = {
  none: "Square (0 px)",
  sm: "Soft (6 px)",
  md: "Rounded (12 px)",
  lg: "Very rounded (20 px)",
};

export function cornersWords(radius: PageDesign["radius"]): string {
  return CORNERS[radius];
}

const SPACING: Record<PageDesign["density"], string> = {
  compact: "Compact (75% of normal)",
  normal: "Normal",
  airy: "Airy (125% of normal)",
};

export function spacingWords(density: PageDesign["density"]): string {
  return SPACING[density];
}

const FRAMES: Record<PageFrame, { name: string; line: string }> = {
  theme: { name: "In the theme", line: "The theme's own page layout, with its header and footer." },
  full_width: {
    name: "Full width",
    line: "The theme's header and footer, with the content spanning the full width.",
  },
  canvas: { name: "Content only", line: "No theme header, footer or page title." },
};

/** What a page frame means for the page, in words. */
export function frameWords(frame: PageFrame): { name: string; line: string } {
  return FRAMES[frame];
}
