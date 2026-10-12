import { z } from "zod";

// Pure logic for the "Chosen by the AI" outline of the page-creation approval
// card. Every string in an outline is the AI's and every image fact is the
// site's; this module only carries them to the caller as plain data for text
// nodes. It never builds markup, and never builds an href out of one of them.
//
// THE GRAMMAR IS THE CONTROL PLANE'S, AND IT IS STRICT ON PURPOSE.
// apps/api/internal/mcp/page_create_input.go decides what a stored request can
// hold (parse, don't strip), and apps/agent/tests/fixtures/ability-run/
// page-create-layout-cases.json pins that decision for both sides. This file
// mirrors it so that an input the card cannot show in full is never shown in
// part: every object is strict, so a key the card has no line for makes the
// whole request "not showable" instead of being dropped from the screen while
// the owner approves. A stripping parse here would hide a field the AI sent.
//
// Text rules (blank, controls, markup, character references, per-field
// lengths) are the agent's alone. This side checks that a text value is a
// string and nothing more. Links are not text: their rules are the control
// plane's, and parseLink holds them.

/** Limits of the outline grammar (page_create_input.go, the pageCreate* constants). */
export const OUTLINE_LIMITS = {
  topLevelNodes: 200,
  nodes: 400,
  items: 50,
  columnsMin: 2,
  columnsMax: 4,
  children: 50,
  images: 20,
  buttons: 12,
  buttonsPerBlock: 3,
  tables: 10,
  tableRows: 50,
  tableColumns: 6,
  quoteParagraphs: 10,
  columnWidthMin: 10,
  columnWidthMax: 90,
  attachmentIdMax: 2_147_483_647,
  urlChars: 2048,
} as const;

// --- Links ------------------------------------------------------------------

const URL_CHARSET = /^[A-Za-z0-9\-._~:/?#!$&()*+,;=%@]+$/;
const LINK_AUTHORITY = /^((?:[A-Za-z0-9-]+\.)+[A-Za-z0-9-]+)(?::([0-9]{1,5}))?$/;
const HEX_DIGIT = /^[0-9a-fA-F]$/;
// An "&" that starts a character reference: a name, a decimal number or a hex
// number, then ";". A link holds none.
const CHARACTER_REFERENCE = /&(?:[A-Za-z0-9]+|#[0-9]+|#[xX][0-9A-Fa-f]+);/;

/** A button link the grammar accepts, reduced to what the card needs. */
export type ParsedLink =
  | { readonly kind: "path" }
  | {
      readonly kind: "absolute";
      /** Lowercase host name, plus ":port" only when the port is not 443. */
      readonly host: string;
    };

/**
 * A button link the control plane accepts (pageLinkUsable): a path on the site
 * that starts with one "/" and holds no colon and no "&#" (a colon is written
 * %3A), or an https:// address with a lowercase scheme, a dotted DNS host, an
 * optional port 1 to 65535 and no user name or password. Neither form holds an
 * "&" that starts a character reference. Null for anything else, so the
 * request is not shown or approvable here.
 *
 * Hosts are ASCII by the character set, which is why the card needs no bidi
 * isolation for them: punycode ("xn--") hosts are shown exactly as written.
 */
export function parseLink(url: string): ParsedLink | null {
  if (url.length < 1 || url.length > OUTLINE_LIMITS.urlChars || !URL_CHARSET.test(url)) return null;
  for (let i = 0; i < url.length; i += 1) {
    if (url[i] === "%" && !(HEX_DIGIT.test(url[i + 1] ?? "") && HEX_DIGIT.test(url[i + 2] ?? ""))) {
      return null;
    }
  }
  if (CHARACTER_REFERENCE.test(url)) return null;
  if (url.startsWith("/")) {
    if (url.length > 1 && (url[1] === "/" || url[1] === "\\")) return null;
    return url.includes(":") || url.includes("&#") ? null : { kind: "path" };
  }
  if (!url.startsWith("https://")) return null;
  const rest = url.slice("https://".length);
  const cut = rest.search(/[/?#]/);
  const authority = cut === -1 ? rest : rest.slice(0, cut);
  if (authority.includes("@")) return null;
  const m = LINK_AUTHORITY.exec(authority);
  if (m === null) return null;
  const hostname = lowerAscii(m[1] ?? "");
  const portText = m[2];
  if (portText === undefined) return { kind: "absolute", host: hostname };
  const port = Number(portText);
  if (port < 1 || port > 65535) return null;
  return { kind: "absolute", host: port === 443 ? hostname : `${hostname}:${port}` };
}

/** Lowercases A to Z only, like the control plane's host key. */
function lowerAscii(s: string): string {
  return s.replace(/[A-Z]/g, (c) => String.fromCharCode(c.charCodeAt(0) + 32));
}

function withoutLeadingWww(host: string): string {
  return host.startsWith("www.") ? host.slice("www.".length) : host;
}

/**
 * Whole-host equality, case-insensitive, after dropping one leading "www." on
 * each side. Never a suffix or substring match: a sub-domain, a look-alike
 * that ends with the site's name and a site's name followed by another domain
 * are all other websites.
 */
export function isSameSiteHost(linkHost: string, siteHost: string): boolean {
  return withoutLeadingWww(lowerAscii(linkHost)) === withoutLeadingWww(lowerAscii(siteHost.trim()));
}

export type LinkTarget =
  | { readonly kind: "site" }
  | { readonly kind: "external"; readonly host: string };

/**
 * Where a button link leads, for the chip beside it. A path on the site, or an
 * address on the site's own host, is "site"; every other host is "external".
 * Null when the link is not one the grammar accepts.
 */
export function classifyLink(url: string, siteHost: string): LinkTarget | null {
  const parsed = parseLink(url);
  if (parsed === null) return null;
  if (parsed.kind === "path") return { kind: "site" };
  return isSameSiteHost(parsed.host, siteHost) ? { kind: "site" } : { kind: "external", host: parsed.host };
}

// --- The outline grammar ----------------------------------------------------

const text = z.string();

const headingNode = z.strictObject({
  type: z.literal("heading"),
  level: z.number().int().min(2).max(4),
  text,
});
const paragraphNode = z.strictObject({ type: z.literal("paragraph"), text });
const listNode = z.strictObject({
  type: z.literal("list"),
  ordered: z.boolean(),
  items: z.array(text).min(1).max(OUTLINE_LIMITS.items),
});
const imageNode = z.strictObject({
  type: z.literal("image"),
  attachment_id: z.number().int().min(1).max(OUTLINE_LIMITS.attachmentIdMax),
  alt: text,
  caption: text.optional(),
  align: z.enum(["none", "center", "wide", "full"]).optional(),
});
const buttonItem = z.strictObject({
  text,
  url: z.string().refine((u) => parseLink(u) !== null),
  style: z.enum(["fill", "outline"]).optional(),
});
const buttonsNode = z.strictObject({
  type: z.literal("buttons"),
  align: z.enum(["left", "center"]).optional(),
  buttons: z.array(buttonItem).min(1).max(OUTLINE_LIMITS.buttonsPerBlock),
});
const quoteNode = z.strictObject({
  type: z.literal("quote"),
  paragraphs: z.array(text).min(1).max(OUTLINE_LIMITS.quoteParagraphs),
  citation: text.optional(),
});
const separatorNode = z.strictObject({ type: z.literal("separator") });
const spacerNode = z.strictObject({ type: z.literal("spacer"), size: z.enum(["small", "medium", "large"]) });
const tableRow = z.array(text).min(1).max(OUTLINE_LIMITS.tableColumns);
const tableNode = z
  .strictObject({
    type: z.literal("table"),
    header: tableRow.optional(),
    rows: z.array(tableRow).min(1).max(OUTLINE_LIMITS.tableRows),
  })
  .refine((t) => {
    const widths = [...(t.header ? [t.header.length] : []), ...t.rows.map((r) => r.length)];
    return widths.every((w) => w === widths[0]);
  });

// Placement is the shape of the schema: a column holds leaves only, a group
// holds leaves or columns, and only the top level holds a group.
const leafOptions = [
  headingNode,
  paragraphNode,
  listNode,
  imageNode,
  buttonsNode,
  quoteNode,
  separatorNode,
  spacerNode,
  tableNode,
] as const;
const leafNode = z.discriminatedUnion("type", leafOptions);

const columnNode = z.strictObject({
  children: z.array(leafNode).min(1).max(OUTLINE_LIMITS.children),
});
/** Widths name every column, each from 10 to 90, and add up to 100. */
function columnWidthsValid(n: { widths?: number[] | undefined; columns: readonly unknown[] }): boolean {
  return (
    n.widths === undefined ||
    (n.widths.length === n.columns.length &&
      n.widths.every((w) => w >= OUTLINE_LIMITS.columnWidthMin && w <= OUTLINE_LIMITS.columnWidthMax) &&
      n.widths.reduce((a, b) => a + b, 0) === 100)
  );
}

const columnsNode = z
  .strictObject({
    type: z.literal("columns"),
    widths: z.array(z.number().int()).optional(),
    columns: z.array(columnNode).min(OUTLINE_LIMITS.columnsMin).max(OUTLINE_LIMITS.columnsMax),
  })
  .refine(columnWidthsValid);
const groupNode = z.strictObject({
  type: z.literal("group"),
  children: z.array(z.discriminatedUnion("type", [...leafOptions, columnsNode])).min(1).max(OUTLINE_LIMITS.children),
});
const outlineNode = z.discriminatedUnion("type", [...leafOptions, columnsNode, groupNode]);

/** The editor value of a page Elementor builds (ability_builder.go, pageEditorBuilderElementor). */
export const ELEMENTOR_EDITOR = "builder:elementor";

const pageInputSchema = z.strictObject({
  post_type: z.enum(["page", "post"]),
  editor: z.enum(["wordpress_blocks", "wordpress_classic", ELEMENTOR_EDITOR]),
  title: z.string().refine((t) => t.trim() !== ""),
  outline: z.array(outlineNode).min(1).max(OUTLINE_LIMITS.topLevelNodes),
  // Only with builder:elementor (page_create_input.go, validatePageCreateInput).
  elementor_format: z.enum(["site_default", "classic", "atomic"]).optional(),
});

export type LeafNode = z.infer<typeof leafNode>;
export type ColumnsNode = z.infer<typeof columnsNode>;
export type GroupNode = z.infer<typeof groupNode>;
export type OutlineNode = z.infer<typeof outlineNode>;
export type ImageNode = z.infer<typeof imageNode>;

/** Limits of one page-edit operation's outline (BuilderContract::MAX_OUTLINE_PER_OP). */
export const FRAGMENT_MAX_NODES = 50;

/**
 * The outline of one page-edit insert or replace, as data. It is the page
 * outline grammar (every object strict) with 1 to 50 top-level nodes; null
 * for anything the card cannot show in full, or that Elementor does not build
 * as asked, so the request is not shown as approvable.
 */
export function parseOutlineFragment(raw: unknown): readonly OutlineNode[] | null {
  const parsed = z.array(outlineNode).min(1).max(FRAGMENT_MAX_NODES).safeParse(raw);
  if (!parsed.success) return null;
  let refused = false;
  visitNodes(parsed.data, (n) => {
    if (elementorRefuses(n)) refused = true;
  });
  return refused ? null : parsed.data;
}

/** Calls `fn` for every node, containers first, in document order. */
export function visitNodes(nodes: readonly OutlineNode[], fn: (node: OutlineNode) => void): void {
  for (const node of nodes) {
    fn(node);
    if (node.type === "group") {
      visitNodes(node.children, fn);
    } else if (node.type === "columns") {
      for (const column of node.columns) visitNodes(column.children, fn);
    }
  }
}

/** The page-wide counters of the control plane's validation. */
function withinPageLimits(outline: readonly OutlineNode[]): boolean {
  let nodes = 0;
  let images = 0;
  let buttons = 0;
  let tables = 0;
  visitNodes(outline, (n) => {
    nodes += 1;
    if (n.type === "image") images += 1;
    else if (n.type === "table") tables += 1;
    else if (n.type === "buttons") {
      nodes += n.buttons.length;
      buttons += n.buttons.length;
    } else if (n.type === "columns") nodes += n.columns.length;
  });
  return (
    nodes <= OUTLINE_LIMITS.nodes &&
    images <= OUTLINE_LIMITS.images &&
    buttons <= OUTLINE_LIMITS.buttons &&
    tables <= OUTLINE_LIMITS.tables
  );
}

/** True for a node the classic editor cannot hold (spacing, sections, columns, buttons, captions). */
function needsBlockEditor(n: OutlineNode): boolean {
  return (
    n.type === "spacer" ||
    n.type === "group" ||
    n.type === "columns" ||
    n.type === "buttons" ||
    (n.type === "image" && n.caption !== undefined)
  );
}

// --- Image facts ------------------------------------------------------------

const mediaFactSchema = z.strictObject({
  id: z.number().int().min(1).max(OUTLINE_LIMITS.attachmentIdMax),
  filename: z.string().min(1),
  mime: z.enum(["image/jpeg", "image/png", "image/gif", "image/webp", "image/avif"]),
  width: z.number().int().min(0).max(100_000),
  height: z.number().int().min(0).max(100_000),
});

/** What the site said about one image when WPMgr checked the request (page_media). */
export type PageMediaFact = z.infer<typeof mediaFactSchema>;

/**
 * The request's page_media indexed by attachment id. Facts are trusted only as
 * a whole: anything that is not a list of well-formed facts with distinct ids
 * yields no facts at all, so every image the outline places is then missing a
 * fact and the request cannot be approved.
 */
export function indexPageMedia(raw: unknown): ReadonlyMap<number, PageMediaFact> {
  const parsed = z.array(mediaFactSchema).max(OUTLINE_LIMITS.images).safeParse(raw ?? []);
  if (!parsed.success) return new Map();
  const byId = new Map<number, PageMediaFact>();
  for (const fact of parsed.data) {
    if (byId.has(fact.id)) return new Map();
    byId.set(fact.id, fact);
  }
  return byId;
}

// --- The page builder -------------------------------------------------------

// The Elementor version a precheck may name (ability_builder_tree.go,
// elementorVersionPattern). It is the site's text and is shown as text.
export const ELEMENTOR_VERSION = /^[0-9]{1,4}\.[0-9]{1,4}(\.[0-9]{1,4})?([.-][0-9A-Za-z]{1,16}){0,2}$/;

const pageBuilderSchema = z.strictObject({
  builder: z.literal("elementor"),
  // "atomic" is the published elementor_format value for Elementor's Atomic
  // editor (ability_builder.go, pageElementorFormats).
  format: z.enum(["classic", "atomic"]),
  version: z.string().regex(ELEMENTOR_VERSION),
  layout: z.enum(["containers", "sections"]),
});

/** The page builder that builds the page, as the site's precheck named it (page_builder). */
export type PageBuilderFacts = z.infer<typeof pageBuilderSchema>;

/**
 * The request's page_builder, or null when it is missing, null, or anything
 * but a builder this card can describe in full.
 */
export function parsePageBuilder(raw: unknown): PageBuilderFacts | null {
  const parsed = pageBuilderSchema.safeParse(raw);
  return parsed.success ? parsed.data : null;
}

// A paragraph whose whole text is one web address, which Elementor would turn
// into an embedded player (ability_builder.go, elementorOwnAddress).
const ADDRESS_ONLY = /^[\t\n\v\f\r ]*https?:\/\/[^\t\n\v\f\r <>"]+[\t\n\v\f\r ]*$/i;

/**
 * True for a node Elementor does not build as the outline asks, which the
 * control plane refuses before any card exists (ability_builder.go,
 * elementorNodeProblem): an address-only paragraph, an image aligned wide or
 * full, an outline button, or a button link with "&".
 */
function elementorRefuses(n: OutlineNode): boolean {
  switch (n.type) {
    case "paragraph":
      return ADDRESS_ONLY.test(n.text);
    case "quote":
      return n.paragraphs.some((p) => ADDRESS_ONLY.test(p));
    case "image":
      return n.align !== undefined && n.align !== "none" && n.align !== "center";
    case "buttons":
      return n.buttons.some((b) => (b.style !== undefined && b.style !== "fill") || b.url.includes("&"));
    default:
      return false;
  }
}

// --- Reading the input text -------------------------------------------------

/**
 * True when `at`, the start of a number in the JSON text, is the value of one
 * of `keys`: the text before it is that key as a plain string, after a "{" or
 * a ",", then ":". A key spelled with escapes does not match.
 */
function isValueOfKey(json: string, at: number, keys: readonly string[]): boolean {
  let i = at - 1;
  while (i >= 0 && /\s/.test(json.charAt(i))) i -= 1;
  if (json.charAt(i) !== ":") return false;
  i -= 1;
  while (i >= 0 && /\s/.test(json.charAt(i))) i -= 1;
  return keys.some((key) => {
    const open = i - key.length - 1;
    if (open < 0 || json.slice(open, i + 1) !== `"${key}"`) return false;
    let before = open - 1;
    while (before >= 0 && /\s/.test(json.charAt(before))) before -= 1;
    return json.charAt(before) === "{" || json.charAt(before) === ",";
  });
}

/**
 * True when the JSON text holds a number written with a fraction or an
 * exponent. Every number in the grammar is a whole number, and the control
 * plane refuses 42.0 as it refuses 42.5, but JSON.parse reads both as 42.
 * `fractionalKeys` names the keys whose value is allowed to be a fraction.
 */
function hasFractionalNumber(json: string, fractionalKeys: readonly string[]): boolean {
  let inString = false;
  for (let i = 0; i < json.length; i += 1) {
    const ch = json.charAt(i);
    if (inString) {
      if (ch === "\\") i += 1;
      else if (ch === '"') inString = false;
    } else if (ch === '"') {
      inString = true;
    } else if (ch === "-" || (ch >= "0" && ch <= "9")) {
      let end = i + 1;
      while (end < json.length && /[0-9.eE+-]/.test(json.charAt(end))) end += 1;
      if (!/^-?(0|[1-9][0-9]*)$/.test(json.slice(i, end)) && !isValueOfKey(json, i, fractionalKeys)) return true;
      i = end - 1;
    }
  }
  return false;
}

/**
 * The input as data, or undefined for text that is not JSON the card can read
 * in full. A "__proto__" key anywhere is refused because the schema parser
 * skips that key by design, which would drop it from the screen. Numbers are
 * whole numbers, except the value of a key named in `fractionalKeys`.
 */
export function readInput(inputJson: string, fractionalKeys: readonly string[] = []): unknown {
  let hidden = false;
  let value: unknown;
  try {
    value = JSON.parse(inputJson, (key, v: unknown) => {
      if (key === "__proto__") hidden = true;
      return v;
    });
  } catch {
    return undefined;
  }
  return hidden || hasFractionalNumber(inputJson, fractionalKeys) ? undefined : value;
}

// --- The parsed preview -----------------------------------------------------

export interface PagePreview {
  readonly title: string;
  readonly outline: readonly OutlineNode[];
  /** The image facts, keyed by attachment id. Every image node has one. */
  readonly media: ReadonlyMap<number, PageMediaFact>;
  /** The page builder that builds the page; null for a WordPress editor. */
  readonly builder: PageBuilderFacts | null;
}

/**
 * Parses the exact AI input. Null when it is not a shape the card can show in
 * full: an unknown block or key, a placement or limit the grammar refuses, a
 * button link it refuses, a block the classic editor cannot hold, or an image
 * with no fact from the site. A page Elementor builds also needs the site's
 * builder facts (page_builder), an elementor_format they agree with, and no
 * node Elementor does not build; a page in a WordPress editor has no builder.
 * An input with outline_version 3 is a designed page: this reader refuses it
 * and parseDesignedPagePreview reads it, so neither shows the other in part.
 */
export function parsePagePreview(inputJson: string, pageMedia?: unknown, pageBuilder?: unknown): PagePreview | null {
  const raw = readInput(inputJson);
  if (raw === undefined) return null;
  const parsed = pageInputSchema.safeParse(raw);
  if (!parsed.success) return null;
  const { editor, title, outline, elementor_format: format } = parsed.data;
  if (!withinPageLimits(outline)) return null;
  let builder: PageBuilderFacts | null = null;
  if (editor === ELEMENTOR_EDITOR) {
    builder = parsePageBuilder(pageBuilder);
    if (builder === null) return null;
    if (format !== undefined && format !== "site_default" && format !== builder.format) return null;
  } else if (format !== undefined || pageBuilder != null) {
    return null;
  }
  let blockOnly = false;
  let builderRefuses = false;
  let imageWithoutFact = false;
  const media = indexPageMedia(pageMedia);
  visitNodes(outline, (n) => {
    if (needsBlockEditor(n)) blockOnly = true;
    if (builder !== null && elementorRefuses(n)) builderRefuses = true;
    if (n.type === "image" && !media.has(n.attachment_id)) imageWithoutFact = true;
  });
  if (editor === "wordpress_classic" && blockOnly) return null;
  if (builderRefuses || imageWithoutFact) return null;
  return { title, outline, media, builder };
}

// --- Designed pages (outline version 3) --------------------------------------

// A designed page is an input with outline_version 3: a design (six colours,
// two fonts, a type scale, a radius and a density), an optional page frame and
// an outline made of sections. A section holds the version 2 leaves, columns
// and groups, and patterns (hero, features, testimonials, pricing, faq, cta,
// logos, stats). It is built with Elementor's classic format alone.
//
// The grammar is the control plane's, and apps/agent/tests/fixtures/ability-run/
// page-create-v3-cases.json pins it for the control plane, the agent and this
// file. It is held as strictly as version 2: every object is closed, so a key
// the card has no line for makes the whole request not showable instead of
// being dropped from the screen while the owner approves.
//
// The card draws colours and font names, so only closed shapes get through: a
// colour is "#" and six hex digits, a font family is 1 to 60 letters, digits
// and spaces. Nothing here builds markup, and no value here is ever made into
// an href. Text values are the AI's words, carried as data. The text rules
// (blank, controls, markup, lengths) stay the agent's alone.

/** The limits of a designed page (OutlineV3Schema in the agent, the control plane's twin). */
export const DESIGNED_LIMITS = {
  /** Bytes of the input text: the least the agent reads. */
  inputBytes: 65_536,
  sections: 20,
  sectionChildren: 50,
  heroChildren: 6,
  ctaChildren: 4,
  planFeatures: 8,
  statValueMax: 999_999_999,
} as const;

/** Items per item pattern: [minimum, maximum]. */
export const PATTERN_ITEM_CAPS = {
  features: [3, 8],
  testimonials: [1, 6],
  plans: [1, 4],
  faq: [1, 12],
  logos: [3, 10],
  stats: [2, 4],
} as const;

/** The closed list of icon names a features item may show. */
export const PATTERN_ICONS = [
  "arrow-right", "award", "bell", "bolt", "book", "briefcase", "calendar", "chart", "check", "clock",
  "cloud", "code", "cog", "comment", "compass", "credit-card", "cube", "database", "envelope", "eye",
  "flag", "gift", "globe", "heart", "home", "image", "key", "laptop", "layers", "leaf",
  "lightbulb", "lock", "map-pin", "phone", "rocket", "search", "shield", "star", "tag", "users",
] as const;

/** Ids a theme already owns: never a section anchor. */
export const RESERVED_ANCHORS = [
  "content", "main", "primary", "page", "masthead", "colophon", "site-header", "site-footer", "wpadminbar",
] as const;

/** The six colour roles of a design, in the order the card shows them. */
export const DESIGN_COLOUR_ROLES = ["ground", "surface", "ink", "muted", "accent", "on_accent"] as const;

/** How a page sits in the theme (page_frame). */
export const PAGE_FRAMES = ["theme", "full_width", "canvas"] as const;
export type PageFrame = (typeof PAGE_FRAMES)[number];

const ANCHOR = /^[a-z][a-z0-9-]{1,39}$/;
const ANCHOR_LINK = /^#([a-z][a-z0-9-]{1,39})$/;
const RESERVED_ANCHOR_SET: ReadonlySet<string> = new Set<string>(RESERVED_ANCHORS);

/** The anchor a "#anchor" link names, or null for any other link or a malformed anchor. */
export function anchorOfLink(url: string): string | null {
  return ANCHOR_LINK.exec(url)?.[1] ?? null;
}

// --- The design

const HEX_COLOUR = /^#[0-9A-Fa-f]{6}$/;
const PURE_BLACK = "#000000";
const FONT_FAMILY = /^[A-Za-z0-9 ]{1,60}$/;
/** The least contrast of text on its background (WCAG AA for body text). */
const MIN_CONTRAST = 4.5;
const CONTRAST_PAIRS = [
  ["ink", "ground"],
  ["ink", "surface"],
  ["on_accent", "accent"],
] as const;

/** The relative luminance of a "#RRGGBB" colour. */
function luminance(hex: string): number {
  const channel = (from: number): number => {
    const c = Number.parseInt(hex.slice(from, from + 2), 16) / 255;
    return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * channel(1) + 0.7152 * channel(3) + 0.0722 * channel(5);
}

/** The contrast ratio of two "#RRGGBB" colours, from 1 to 21, the same whichever comes first. */
export function contrastRatio(a: string, b: string): number {
  const la = luminance(a);
  const lb = luminance(b);
  const [light, dark] = la < lb ? [lb, la] : [la, lb];
  return (light + 0.05) / (dark + 0.05);
}

// A colour is read in either letter case and kept in upper case; pure black is
// refused because a page built from it reads as a template.
const designColour = z
  .string()
  .regex(HEX_COLOUR)
  .refine((c) => c.toUpperCase() !== PURE_BLACK)
  .transform((c) => c.toUpperCase());

const designColours = z
  .strictObject({
    ground: designColour,
    surface: designColour,
    ink: designColour,
    muted: designColour,
    accent: designColour,
    on_accent: designColour,
  })
  .refine((c) => CONTRAST_PAIRS.every(([text, on]) => contrastRatio(c[text], c[on]) >= MIN_CONTRAST));

// "system" is a family name like any other here: it means "set no family".
const fontFamily = z.string().regex(FONT_FAMILY);

const designSchema = z.strictObject({
  colors: designColours,
  fonts: z.strictObject({ heading: fontFamily, body: fontFamily }),
  scale: z.strictObject({
    base_px: z.number().int().min(15).max(20),
    ratio: z.number().min(1.125).max(1.333),
  }),
  radius: z.enum(["none", "sm", "md", "lg"]),
  density: z.enum(["compact", "normal", "airy"]),
});

/** A design that passed every check. Its colours are upper case. */
export type PageDesign = z.infer<typeof designSchema>;

// --- The nodes of a section

const attachmentId = z.number().int().min(1).max(OUTLINE_LIMITS.attachmentIdMax);

// A heading may be level 1 here (the rule that it stands once, in the hero, is
// judged over the page), and a button may be styled as text.
const designedHeadingNode = z.strictObject({
  type: z.literal("heading"),
  level: z.number().int().min(1).max(4),
  text,
});
const designedButtonItem = z.strictObject({
  text,
  // Judged over the page: a "#anchor" link has to name a section of it.
  url: z.string(),
  style: z.enum(["fill", "outline", "text"]).optional(),
});
const designedButtonsNode = z.strictObject({
  type: z.literal("buttons"),
  align: z.enum(["left", "center"]).optional(),
  buttons: z.array(designedButtonItem).min(1).max(OUTLINE_LIMITS.buttonsPerBlock),
});

const designedLeafOptions = [
  designedHeadingNode,
  paragraphNode,
  listNode,
  imageNode,
  designedButtonsNode,
  quoteNode,
  separatorNode,
  spacerNode,
  tableNode,
] as const;
const designedLeafNode = z.discriminatedUnion("type", designedLeafOptions);

const designedColumnsNode = z
  .strictObject({
    type: z.literal("columns"),
    widths: z.array(z.number().int()).optional(),
    columns: z
      .array(z.strictObject({ children: z.array(designedLeafNode).min(1).max(OUTLINE_LIMITS.children) }))
      .min(OUTLINE_LIMITS.columnsMin)
      .max(OUTLINE_LIMITS.columnsMax),
  })
  .refine(columnWidthsValid);
const designedGroupNode = z.strictObject({
  type: z.literal("group"),
  children: z
    .array(z.discriminatedUnion("type", [...designedLeafOptions, designedColumnsNode]))
    .min(1)
    .max(OUTLINE_LIMITS.children),
});

// --- The patterns

const patternHead = <const P extends string, const V extends readonly [string, ...string[]]>(pattern: P, variants: V) => ({
  type: z.literal("pattern"),
  pattern: z.literal(pattern),
  variant: z.enum(variants),
});

const heroPattern = z.strictObject({
  ...patternHead("hero", ["background", "split", "centered"]),
  children: z
    .array(z.discriminatedUnion("type", [designedHeadingNode, paragraphNode, designedButtonsNode, imageNode]))
    .min(1)
    .max(DESIGNED_LIMITS.heroChildren),
});
const ctaPattern = z.strictObject({
  ...patternHead("cta", ["band"]),
  children: z
    .array(z.discriminatedUnion("type", [designedHeadingNode, paragraphNode, designedButtonsNode]))
    .min(1)
    .max(DESIGNED_LIMITS.ctaChildren),
});
const featuresPattern = z.strictObject({
  ...patternHead("features", ["split", "bento", "grid"]),
  features: z
    .array(z.strictObject({ icon: z.enum(PATTERN_ICONS), title: text, text }))
    .min(PATTERN_ITEM_CAPS.features[0])
    .max(PATTERN_ITEM_CAPS.features[1]),
});
const testimonialsPattern = z.strictObject({
  ...patternHead("testimonials", ["featured"]),
  testimonials: z
    .array(z.strictObject({ quote: text, name: text, role: text.optional(), attachment_id: attachmentId.optional() }))
    .min(PATTERN_ITEM_CAPS.testimonials[0])
    .max(PATTERN_ITEM_CAPS.testimonials[1]),
});
const pricingPattern = z.strictObject({
  ...patternHead("pricing", ["three-plans"]),
  plans: z
    .array(
      z.strictObject({
        name: text,
        price: text,
        period: text.optional(),
        features: z.array(text).min(1).max(DESIGNED_LIMITS.planFeatures),
        // Judged over the page, like a button link.
        cta: z.strictObject({ text, url: z.string() }),
        highlighted: z.boolean().optional(),
      }),
    )
    .min(PATTERN_ITEM_CAPS.plans[0])
    .max(PATTERN_ITEM_CAPS.plans[1]),
});
const faqPattern = z.strictObject({
  ...patternHead("faq", ["accordion"]),
  faq: z
    .array(z.strictObject({ question: text, answer: text }))
    .min(PATTERN_ITEM_CAPS.faq[0])
    .max(PATTERN_ITEM_CAPS.faq[1]),
});
const logosPattern = z.strictObject({
  ...patternHead("logos", ["row"]),
  logos: z
    .array(z.strictObject({ attachment_id: attachmentId, alt: text }))
    .min(PATTERN_ITEM_CAPS.logos[0])
    .max(PATTERN_ITEM_CAPS.logos[1]),
});
const statsPattern = z.strictObject({
  ...patternHead("stats", ["counters"]),
  stats: z
    .array(
      z.strictObject({
        value: z.number().int().min(0).max(DESIGNED_LIMITS.statValueMax),
        suffix: text.optional(),
        label: text,
      }),
    )
    .min(PATTERN_ITEM_CAPS.stats[0])
    .max(PATTERN_ITEM_CAPS.stats[1]),
});
const patternNode = z.discriminatedUnion("pattern", [
  heroPattern,
  ctaPattern,
  featuresPattern,
  testimonialsPattern,
  pricingPattern,
  faqPattern,
  logosPattern,
  statsPattern,
]);

// --- The page

const sectionNode = z.strictObject({
  type: z.literal("section"),
  anchor: z.string().regex(ANCHOR),
  width: z.enum(["boxed", "full"]).optional(),
  tone: z.enum(["ground", "surface", "inverse", "accent"]).optional(),
  background: z
    .strictObject({
      attachment_id: attachmentId,
      overlay: z.enum(["none", "soft", "strong"]).optional(),
      focal: z.enum(["center", "top", "bottom"]).optional(),
    })
    .optional(),
  spacing: z.enum(["md", "lg", "xl"]).optional(),
  align: z.enum(["start", "center"]).optional(),
  min_height: z.enum(["auto", "screen"]).optional(),
  children: z
    .array(z.discriminatedUnion("type", [...designedLeafOptions, designedColumnsNode, designedGroupNode, patternNode]))
    .min(1)
    .max(DESIGNED_LIMITS.sectionChildren),
});

const designedInputSchema = z.strictObject({
  post_type: z.enum(["page", "post"]),
  editor: z.literal(ELEMENTOR_EDITOR),
  // A designed page names its format: the default is resolved by the site.
  elementor_format: z.literal("classic"),
  outline_version: z.literal(3),
  title: z.string().refine((t) => t.trim() !== ""),
  design: designSchema,
  page_frame: z.enum(PAGE_FRAMES).optional(),
  outline: z.array(sectionNode).min(1).max(DESIGNED_LIMITS.sections),
});

export type DesignedLeaf = z.infer<typeof designedLeafNode>;
export type PatternNode = z.infer<typeof patternNode>;
export type SectionNode = z.infer<typeof sectionNode>;

/** What the page holds beyond its shape, found in one walk. */
interface DesignedFacts {
  anchors: string[];
  links: string[];
  /** Every attachment the page names: image blocks, section backgrounds, logos and avatars. */
  mediaIds: number[];
  nodes: number;
  images: number;
  buttons: number;
  tables: number;
  heroes: number;
  heroMisplaced: boolean;
  headingOneOutsideHero: boolean;
  headingOnesInHero: number;
  elementorRefuses: boolean;
}

function inspectSections(sections: readonly SectionNode[]): DesignedFacts {
  const f: DesignedFacts = {
    anchors: [],
    links: [],
    mediaIds: [],
    nodes: 0,
    images: 0,
    buttons: 0,
    tables: 0,
    heroes: 0,
    heroMisplaced: false,
    headingOneOutsideHero: false,
    headingOnesInHero: 0,
    elementorRefuses: false,
  };
  // An attachment counts toward the images of the page each time it is named.
  const media = (id: number): void => {
    f.images += 1;
    f.mediaIds.push(id);
  };
  const leaf = (n: DesignedLeaf, inHero: boolean): void => {
    f.nodes += 1;
    switch (n.type) {
      case "heading":
        if (n.level === 1) {
          if (inHero) f.headingOnesInHero += 1;
          else f.headingOneOutsideHero = true;
        }
        break;
      case "paragraph":
        if (ADDRESS_ONLY.test(n.text)) f.elementorRefuses = true;
        break;
      case "quote":
        if (n.paragraphs.some((p) => ADDRESS_ONLY.test(p))) f.elementorRefuses = true;
        break;
      case "image":
        media(n.attachment_id);
        if (n.align !== undefined && n.align !== "none" && n.align !== "center") f.elementorRefuses = true;
        break;
      case "buttons":
        f.nodes += n.buttons.length;
        f.buttons += n.buttons.length;
        for (const b of n.buttons) {
          f.links.push(b.url);
          if (b.url.includes("&")) f.elementorRefuses = true;
        }
        break;
      case "table":
        f.tables += 1;
        break;
      default:
        break;
    }
  };
  const columns = (n: z.infer<typeof designedColumnsNode>): void => {
    f.nodes += 1 + n.columns.length;
    for (const column of n.columns) for (const c of column.children) leaf(c, false);
  };
  sections.forEach((section, si) => {
    f.nodes += 1;
    f.anchors.push(section.anchor);
    if (section.background !== undefined) media(section.background.attachment_id);
    section.children.forEach((child, ci) => {
      switch (child.type) {
        case "pattern":
          f.nodes += 1;
          if (child.pattern === "hero") {
            f.heroes += 1;
            // The hero is the first child of the first section, and only there.
            if (si !== 0 || ci !== 0) f.heroMisplaced = true;
            for (const c of child.children) leaf(c, true);
          } else if (child.pattern === "cta") {
            for (const c of child.children) leaf(c, false);
          } else if (child.pattern === "pricing") {
            for (const plan of child.plans) f.links.push(plan.cta.url);
          } else if (child.pattern === "logos") {
            for (const logo of child.logos) media(logo.attachment_id);
          } else if (child.pattern === "testimonials") {
            for (const t of child.testimonials) if (t.attachment_id !== undefined) media(t.attachment_id);
          }
          break;
        case "group":
          f.nodes += 1;
          for (const c of child.children) {
            if (c.type === "columns") columns(c);
            else leaf(c, false);
          }
          break;
        case "columns":
          columns(child);
          break;
        default:
          leaf(child, false);
      }
    });
  });
  return f;
}

/** True when a link is one the grammar accepts: a "#anchor" of the page, a site path, or an https address. */
function designedLinkResolves(url: string, anchors: readonly string[]): boolean {
  if (url.startsWith("#")) {
    const anchor = anchorOfLink(url);
    return anchor !== null && anchors.includes(anchor);
  }
  return parseLink(url) !== null;
}

/**
 * The rules that need the whole page: one anchor for one section and none of
 * a theme's; every "#anchor" link names a section; the hero is the first child
 * of the first section and there is one; the page's one level 1 heading is in
 * it; the page-wide counters of version 2; and what Elementor does not build
 * as asked.
 */
function designedPageHolds(f: DesignedFacts): boolean {
  return (
    new Set(f.anchors).size === f.anchors.length &&
    f.anchors.every((a) => !RESERVED_ANCHOR_SET.has(a)) &&
    f.links.every((url) => designedLinkResolves(url, f.anchors)) &&
    f.heroes <= 1 &&
    !f.heroMisplaced &&
    !f.headingOneOutsideHero &&
    f.headingOnesInHero <= 1 &&
    f.nodes <= OUTLINE_LIMITS.nodes &&
    f.images <= OUTLINE_LIMITS.images &&
    f.buttons <= OUTLINE_LIMITS.buttons &&
    f.tables <= OUTLINE_LIMITS.tables &&
    !f.elementorRefuses
  );
}

/** The bytes of a text in UTF-8, as the input limit counts them. */
function utf8Length(value: string): number {
  let bytes = 0;
  for (let i = 0; i < value.length; i += 1) {
    const code = value.charCodeAt(i);
    if (code < 0x80) bytes += 1;
    else if (code < 0x800) bytes += 2;
    else if (code >= 0xd800 && code <= 0xdbff && (value.charCodeAt(i + 1) & 0xfc00) === 0xdc00) {
      bytes += 4;
      i += 1;
    } else bytes += 3;
  }
  return bytes;
}

/** A designed page the card can show in full. */
export interface DesignedPagePreview {
  readonly title: string;
  readonly design: PageDesign;
  /** The page_frame sent, else full width when the page opens with a hero, else the theme's. */
  readonly pageFrame: PageFrame;
  readonly sections: readonly SectionNode[];
  /** The image facts, keyed by attachment id. Every attachment the page names has one. */
  readonly media: ReadonlyMap<number, PageMediaFact>;
  /** The site's facts: Elementor, classic format, containers. */
  readonly builder: PageBuilderFacts;
}

/**
 * Parses the exact AI input of a designed page (outline_version 3). Null when
 * it is not a shape the card can show in full: an unknown key or value, a
 * design that is not fully set or whose text and background colours are too
 * close, a rule over the page the control plane refuses, a section background
 * or image block with no fact from the site, or a site that cannot build it
 * (it needs Elementor in the classic format, with containers). A version 2
 * input is read by parsePagePreview, which refuses this one.
 */
export function parseDesignedPagePreview(
  inputJson: string,
  pageMedia?: unknown,
  pageBuilder?: unknown,
): DesignedPagePreview | null {
  if (utf8Length(inputJson) > DESIGNED_LIMITS.inputBytes) return null;
  // The type ratio is the one number of the grammar that is not a whole number.
  const raw = readInput(inputJson, ["ratio"]);
  if (raw === undefined) return null;
  const parsed = designedInputSchema.safeParse(raw);
  if (!parsed.success) return null;
  const builder = parsePageBuilder(pageBuilder);
  if (builder === null || builder.format !== "classic" || builder.layout !== "containers") return null;
  const { title, design, outline: sections, page_frame: frame } = parsed.data;
  const facts = inspectSections(sections);
  if (!designedPageHolds(facts)) return null;
  const media = indexPageMedia(pageMedia);
  if (facts.mediaIds.some((id) => !media.has(id))) return null;
  const pageFrame = frame ?? (facts.heroes > 0 ? "full_width" : "theme");
  return { title, design, pageFrame, sections, media, builder };
}

// --- Words for the card -----------------------------------------------------

function plural(n: number, one: string, many: string): string {
  return `${n} ${n === 1 ? one : many}`;
}

/** True when the outline holds a block beyond headings, paragraphs and lists. */
export function usesLayout(outline: readonly OutlineNode[]): boolean {
  let found = false;
  visitNodes(outline, (n) => {
    if (n.type !== "heading" && n.type !== "paragraph" && n.type !== "list") found = true;
  });
  return found;
}

/**
 * The "Layout" row: counts of what the page holds beyond headings, paragraphs
 * and lists, such as "1 section · 2 column blocks · 3 images · 2 buttons".
 * Null when the outline is only those three. A page with nothing but
 * separators and spacing lists those, so the row is never empty.
 */
export function layoutSummary(outline: readonly OutlineNode[]): string | null {
  if (!usesLayout(outline)) return null;
  const c = { group: 0, columns: 0, image: 0, button: 0, table: 0, quote: 0, separator: 0, spacer: 0 };
  visitNodes(outline, (n) => {
    if (n.type === "group") c.group += 1;
    else if (n.type === "columns") c.columns += 1;
    else if (n.type === "image") c.image += 1;
    else if (n.type === "buttons") c.button += n.buttons.length;
    else if (n.type === "table") c.table += 1;
    else if (n.type === "quote") c.quote += 1;
    else if (n.type === "separator") c.separator += 1;
    else if (n.type === "spacer") c.spacer += 1;
  });
  const parts: string[] = [];
  if (c.group > 0) parts.push(plural(c.group, "section", "sections"));
  if (c.columns > 0) parts.push(plural(c.columns, "column block", "column blocks"));
  if (c.image > 0) parts.push(plural(c.image, "image", "images"));
  if (c.button > 0) parts.push(plural(c.button, "button", "buttons"));
  if (c.table > 0) parts.push(plural(c.table, "table", "tables"));
  if (c.quote > 0) parts.push(plural(c.quote, "quote", "quotes"));
  if (parts.length === 0) {
    if (c.separator > 0) parts.push(plural(c.separator, "separator", "separators"));
    if (c.spacer > 0) parts.push(plural(c.spacer, "space", "spaces"));
  }
  return parts.join(" · ");
}

export function headingLabel(level: number): string {
  return `Heading ${level}`;
}

/** The pixel heights the agent writes for each size (class-page-create-builder.php SPACER_PX). */
const SPACER_PX = { small: 24, medium: 48, large: 96 } as const;

export function spacerLabel(size: keyof typeof SPACER_PX): string {
  const word = size === "small" ? "Small" : size === "medium" ? "Medium" : "Large";
  return `Space · ${word} (${SPACER_PX[size]} px)`;
}

export function columnsLabel(count: number, widths: readonly number[] | undefined): string {
  const how = widths === undefined ? "equal widths" : widths.map((w) => `${w}%`).join(" / ");
  return `Columns · ${count} · ${how}`;
}

export function tableLabel(columns: number, rows: number, hasHeader: boolean): string {
  const base = `Table · ${plural(columns, "column", "columns")} × ${plural(rows, "row", "rows")}`;
  return hasHeader ? `${base} · header row` : base;
}

export function buttonsLabel(align: "left" | "center" | undefined): string {
  return align === "center" ? "Buttons · centred" : "Buttons";
}

export function buttonLabel(style: "fill" | "outline" | undefined): string {
  return style === "outline" ? "Button · Outline" : "Button · Filled";
}

/** "1200 × 800", or null when the site did not report a size. */
export function imageSizeLabel(width: number, height: number): string | null {
  return width > 0 && height > 0 ? `${width} × ${height}` : null;
}

/**
 * Our words before an image's alt text. On a page Elementor builds the alt is
 * the one saved with the image in the media library, so the line says so; an
 * empty one is not called decorative there, only absent from the library.
 */
export function imageAltLabel(alt: string, fromLibrary: boolean): string {
  if (alt === "") return fromLibrary ? "No alt text in the media library" : "No alt text (decorative)";
  return fromLibrary ? "Alt text (from the media library):" : "Alt text:";
}

export function imageAlignLabel(align: ImageNode["align"]): string | null {
  switch (align) {
    case "center":
      return "Centred";
    case "wide":
      return "Wide";
    case "full":
      return "Full width";
    default:
      return null;
  }
}

/** A quoted piece of the AI's text: the quote marks are ours. */
export function quoted(value: string): string {
  return `"${value}"`;
}
