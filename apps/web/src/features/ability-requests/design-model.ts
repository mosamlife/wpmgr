import {
  DESIGN_COLOUR_ROLES,
  type PageDesign,
  type PageFrame,
  type PatternNode,
  type SectionNode,
} from "./outline-model";

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

// --- The sections of a designed page

// What the card says about a section. The tones, the defaults and the overlay
// strengths are the agent's own (ElementorSectionCompiler: DEFAULT_TONE,
// DEFAULT_WIDTH, DEFAULT_SPACING, DEFAULT_OVERLAY, DEFAULT_FOCAL,
// DEFAULT_MIN_HEIGHT, BACKGROUND_TONE and SCREEN_HEIGHT_VH; DesignTokens::tone()
// and overlay()): a section that names no tone is ground, a section with a
// background image is written in the inverse tone, and a background that names
// no overlay has a strong one. Nothing here builds markup.

export type SectionTone = "ground" | "surface" | "inverse" | "accent";

/** The tone a section is written in: inverse whenever it has a background image, whatever tone it names. */
export function sectionTone(section: Pick<SectionNode, "tone" | "background">): SectionTone {
  if (section.background !== undefined) return "inverse";
  return section.tone ?? "ground";
}

/** Which colour of the design each tone paints a section's background with (DesignTokens::tone()). */
const TONES: Record<SectionTone, { label: string; role: ColourRole }> = {
  ground: { label: "Ground", role: "ground" },
  surface: { label: "Surface", role: "surface" },
  inverse: { label: "Inverse", role: "ink" },
  accent: { label: "Accent", role: "accent" },
};

export interface ToneSwatch {
  readonly tone: SectionTone;
  readonly label: string;
  /** The colour of the design the tone paints the section with. */
  readonly role: ColourRole;
  /** The colour as written. */
  readonly hex: string;
  /** The colour to paint the chip with: the colour itself when it is a closed shape, else null. */
  readonly chip: string | null;
}

/** The swatch of a tone, painted with the design's own colour for it. */
export function toneSwatch(colors: PageDesign["colors"], tone: SectionTone): ToneSwatch {
  const { label, role } = TONES[tone];
  const hex = colors[role];
  return { tone, label, role, hex, chip: CHIP_COLOUR.test(hex) ? hex : null };
}

const WIDTHS: Record<NonNullable<SectionNode["width"]>, string> = { boxed: "Boxed", full: "Full width" };
const SPACINGS: Record<NonNullable<SectionNode["spacing"]>, string> = {
  md: "Medium spacing",
  lg: "Large spacing",
  xl: "Extra large spacing",
};
const ALIGNS: Record<NonNullable<SectionNode["align"]>, string> = { start: "Text at the start", center: "Text centred" };
/** The height of a section of screen height, in percent of the screen (ElementorSectionCompiler::SCREEN_HEIGHT_VH). */
const SCREEN_HEIGHT_PERCENT = 80;
const HEIGHTS: Record<NonNullable<SectionNode["min_height"]>, string> = {
  auto: "Height follows content",
  screen: `Height ${SCREEN_HEIGHT_PERCENT}% of the screen`,
};

/** A section's width, spacing, text alignment and height in words, the agent's default for each one the AI did not name. */
export function sectionFacts(
  section: Pick<SectionNode, "width" | "spacing" | "align" | "min_height">,
): readonly string[] {
  return [
    WIDTHS[section.width ?? "boxed"],
    SPACINGS[section.spacing ?? "lg"],
    ALIGNS[section.align ?? "start"],
    HEIGHTS[section.min_height ?? "auto"],
  ];
}

type Background = NonNullable<SectionNode["background"]>;

/** The overlay on a background: the design's ink at this opacity (DesignTokens::OVERLAY_OPACITY); none writes no overlay. */
const OVERLAYS: Record<NonNullable<Background["overlay"]>, { words: string; opacity: number | null }> = {
  none: { words: "No overlay", opacity: null },
  soft: { words: "Soft overlay", opacity: 0.45 },
  strong: { words: "Strong overlay", opacity: 0.65 },
};

/** Where the focal marker sits on the miniature, in percent from its top, and the word for the point. */
const FOCALS: Record<NonNullable<Background["focal"]>, { words: string; top: number }> = {
  center: { words: "centre", top: 50 },
  top: { words: "top", top: 12 },
  bottom: { words: "bottom", top: 88 },
};

export interface BackgroundFraming {
  readonly overlay: NonNullable<Background["overlay"]>;
  /** The overlay's opacity, or null when there is none. */
  readonly opacity: number | null;
  readonly overlayWords: string;
  readonly focal: NonNullable<Background["focal"]>;
  /** Percent from the top of the miniature. */
  readonly focalTop: number;
  readonly focalWords: string;
}

/** How a section's background image is framed: the overlay and the focal point, the agent's default for each one not named. */
export function backgroundFraming(background: Background): BackgroundFraming {
  const overlay = background.overlay ?? "strong";
  const focal = background.focal ?? "center";
  return {
    overlay,
    opacity: OVERLAYS[overlay].opacity,
    overlayWords: OVERLAYS[overlay].words,
    focal,
    focalTop: FOCALS[focal].top,
    focalWords: `Focal point: ${FOCALS[focal].words}`,
  };
}

const PATTERN_NAMES: Record<PatternNode["pattern"], string> = {
  hero: "Hero",
  cta: "Call to action",
  features: "Features",
  testimonials: "Testimonials",
  pricing: "Pricing",
  faq: "FAQ",
  logos: "Logos",
  stats: "Stats",
};

/** A pattern by our name for it and its variant: "Pricing · three plans". Both come from closed lists. */
export function patternWords(pattern: Pick<PatternNode, "pattern" | "variant">): string {
  return `${PATTERN_NAMES[pattern.pattern]} · ${pattern.variant.replace(/-/g, " ")}`;
}

/** The miniature of a background is this high, in pixels, and as wide as the image's proportions allow. */
const THUMB_HEIGHT = 64;
const THUMB_RATIO_MIN = 0.75;
const THUMB_RATIO_MAX = 2.5;
/** The proportions of an image whose size the site did not report. */
const THUMB_RATIO_UNKNOWN = 16 / 9;

/** The size of the miniature of a background, in whole pixels: the image's proportions held between tall and wide limits. */
export function thumbSize(width: number, height: number): { width: number; height: number } {
  const ratio = width > 0 && height > 0 ? width / height : THUMB_RATIO_UNKNOWN;
  const held = Math.min(THUMB_RATIO_MAX, Math.max(THUMB_RATIO_MIN, ratio));
  return { width: Math.round(THUMB_HEIGHT * held), height: THUMB_HEIGHT };
}
