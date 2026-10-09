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
const columnsNode = z
  .strictObject({
    type: z.literal("columns"),
    widths: z.array(z.number().int()).optional(),
    columns: z.array(columnNode).min(OUTLINE_LIMITS.columnsMin).max(OUTLINE_LIMITS.columnsMax),
  })
  .refine(
    (n) =>
      n.widths === undefined ||
      (n.widths.length === n.columns.length &&
        n.widths.every((w) => w >= OUTLINE_LIMITS.columnWidthMin && w <= OUTLINE_LIMITS.columnWidthMax) &&
        n.widths.reduce((a, b) => a + b, 0) === 100),
  );
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
const ELEMENTOR_VERSION = /^[0-9]{1,4}\.[0-9]{1,4}(\.[0-9]{1,4})?([.-][0-9A-Za-z]{1,16}){0,2}$/;

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
const ADDRESS_ONLY = /^[\t\n\x0B\f\r ]*https?:\/\/[^\t\n\x0B\f\r <>"]+[\t\n\x0B\f\r ]*$/i;

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
 * True when the JSON text holds a number written with a fraction or an
 * exponent. Every number in the grammar is a whole number, and the control
 * plane refuses 42.0 as it refuses 42.5, but JSON.parse reads both as 42.
 */
function hasFractionalNumber(json: string): boolean {
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
      if (!/^-?(0|[1-9][0-9]*)$/.test(json.slice(i, end))) return true;
      i = end - 1;
    }
  }
  return false;
}

/**
 * The input as data, or undefined for text that is not JSON the card can read
 * in full. A "__proto__" key anywhere is refused because the schema parser
 * skips that key by design, which would drop it from the screen.
 */
function readInput(inputJson: string): unknown {
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
  return hidden || hasFractionalNumber(inputJson) ? undefined : value;
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
