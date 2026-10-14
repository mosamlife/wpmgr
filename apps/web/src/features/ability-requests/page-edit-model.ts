import { z } from "zod";
import type { AbilityRequest } from "@wpmgr/api";

import { settingNotSentLine } from "@/features/ai-trust/ai-trust-copy";

import {
  approvedNotStartedText,
  automaticStatus,
  clockTime,
  notSentText,
  type AbilityStatus,
  type AbilityStatusKind,
} from "./ability-card-model";
import {
  ELEMENTOR_VERSION, PATTERN_ITEM_CAPS, designSchema, parseDesignedOutlineFragment,
  parseOutlineFragment, patternVariantSchema, readInput, type DesignedEditNode, type OutlineNode, type PageDesign,
} from "./outline-model";

import { editTemplateSchema, showsOnWords, templatePublished, templateRulesSchema, templateStatus, type EditTemplate } from "./template-card-model";

type EditNode = OutlineNode | DesignedEditNode;

// Pure logic for the approval card of "AI changes a draft in Elementor"
// (wpmgr/page-edit, BF-E). Two sources are read together and must agree:
//
//   - input_json: the exact input the AI chose. Its words (new text, the
//     outlines it inserts) are shown as "Chosen by the AI".
//   - page_edit: the card the control plane checked against the site's own
//     precheck (apps/api/internal/mcp/page_edit_precheck.go,
//     PageEditCardFacts). Everything under a `from_the_site` member came from
//     the site, and every kind and label WPMgr shows is WPMgr's own words.
//
// THE PARSE IS STRICT ON PURPOSE, like outline-model.ts: an input or a card
// the screen has no line for makes the whole request "not showable" and
// keeps Approve off, instead of being shown in part. Every string here is
// carried to the caller as plain data for a text node; nothing builds markup
// or an href out of one. Links use the site's address and an integer id only.

export const PAGE_EDIT_ABILITY = "wpmgr/page-edit";

/** A wpmgr/page-edit request. */
export function isPageEdit(r: AbilityRequest): boolean {
  return r.ability_name === PAGE_EDIT_ABILITY;
}

/** "Change a draft in Elementor · Shop". The site name is the site's own label, rendered as text. */
export function pageEditCardTitle(siteLabel: string): string {
  return `Change a draft in Elementor · ${siteLabel}`;
}

// --- The AI's input ---------------------------------------------------------

const REF_PATTERN = /^[A-Za-z0-9_-]{1,32}$/;
const refSchema = z.string().regex(REF_PATTERN);
const FIELDS = ["text", "url", "alt", "caption"] as const;
function isPatternField(field: string): boolean {
  if (field !== field.trim()) return false;
  const match = /^(features|testimonials|plans|faq|stats)\.(0|[1-9][0-9]?)\.(.+)$/.exec(field);
  if (match === null) return false;
  const array = match[1] as "features" | "testimonials" | "plans" | "faq" | "stats";
  if (Number(match[2]) >= PATTERN_ITEM_CAPS[array][1]) return false;
  const tail = match[3]!;
  switch (array) {
    case "features": return tail === "title" || tail === "text";
    case "testimonials": return tail === "quote" || tail === "name" || tail === "role";
    case "plans": return ["name", "price", "period", "cta.text", "cta.url"].includes(tail) || /^features\.[0-7]$/.test(tail);
    case "faq": return tail === "question" || tail === "answer";
    case "stats": return tail === "value" || tail === "suffix" || tail === "label";
  }
}
const fieldSchema = z.union([z.enum(FIELDS), z.string().refine(isPatternField)]);
const positionSchema = z.enum(["first", "last"]);
const widgetSchema = z.string().regex(/^[a-z][a-z0-9_-]{0,63}$/);
const parentSchema = z.union([z.literal("root"), refSchema]);
const nativeValueSchema = z.json();
export type NativeValue = z.infer<typeof nativeValueSchema>;
type StoredValue = string | StoredValue[] | { [key: string]: StoredValue };
const storedValueSchema: z.ZodType<StoredValue> = z.lazy(() => z.union([
  z.string(), z.array(storedValueSchema),
  z.record(z.string(), storedValueSchema).refine((v) => Object.keys(v).length > 0
    && !Object.keys(v).every((key, i) => key === String(i))),
]));
const settingsSchema = z.union([
  z.record(z.string(), z.union([storedValueSchema, z.null()])), z.tuple([]),
]);
const nativeAddFields = { op: z.literal("add_native"), widget: widgetSchema, settings: settingsSchema };
const nativeDuplicateFields = { op: z.literal("duplicate"), ref: refSchema };

export type EditField = z.infer<typeof fieldSchema>;
export type HowAnchor = "after" | "before" | "into";

const outlineJson = z.array(z.unknown());
const operationSchema = z.union([
  z.strictObject({ op: z.literal("set_shows_on"), rules: templateRulesSchema }),
  z.strictObject({ op: z.literal("set_text"), ref: refSchema, field: fieldSchema, text: z.string() }),
  z.strictObject({ op: z.literal("insert"), after: refSchema, outline: outlineJson }),
  z.strictObject({ op: z.literal("insert"), before: refSchema, outline: outlineJson }),
  z.strictObject({ op: z.literal("insert"), into: refSchema, position: positionSchema.optional(), outline: outlineJson }),
  z.strictObject({ op: z.literal("replace"), ref: refSchema, outline: outlineJson }),
  z.strictObject({ op: z.literal("remove"), ref: refSchema }),
  z.strictObject({ op: z.literal("move"), ref: refSchema, after: refSchema }),
  z.strictObject({ op: z.literal("move"), ref: refSchema, before: refSchema }),
  z.strictObject({ op: z.literal("move"), ref: refSchema, into: refSchema, position: positionSchema.optional() }),
  z.strictObject({ op: z.literal("set_native"), ref: refSchema, settings: settingsSchema }),
  z.strictObject({ ...nativeAddFields, after: refSchema }),
  z.strictObject({ ...nativeAddFields, before: refSchema }),
  z.strictObject({ ...nativeAddFields, into: refSchema, position: positionSchema.optional() }),
  z.strictObject({ ...nativeDuplicateFields, after: refSchema }),
  z.strictObject({ ...nativeDuplicateFields, before: refSchema }),
  z.strictObject({ ...nativeDuplicateFields, into: refSchema, position: positionSchema.optional() }),
]);
const inputSchema = z.strictObject({
  post_id: z.number().int().min(1),
  base_fingerprint: z.string().regex(/^[0-9a-f]{64}$/),
  operations: z.array(operationSchema).min(1).max(25),
  design: designSchema.optional(),
});

/** One operation of the AI's input in one shape: the target, the place and the new nodes. */
export type EditOp =
  | { readonly op: "set_text"; readonly ref: string; readonly field: EditField; readonly text: string }
  | {
      readonly op: "insert";
      readonly how: HowAnchor;
      readonly anchor: string;
      readonly position: "first" | "last" | null;
      readonly outline: readonly EditNode[];
    }
  | { readonly op: "replace"; readonly ref: string; readonly outline: readonly EditNode[] }
  | { readonly op: "remove"; readonly ref: string }
  | { readonly op: "move" | "duplicate"; readonly ref: string; readonly how: HowAnchor; readonly anchor: string; readonly position?: "first" | "last" }
  | { readonly op: "set_native"; readonly ref: string; readonly settings: Record<string, StoredValue | null> }
  | { readonly op: "add_native"; readonly widget: string; readonly settings: Record<string, StoredValue | null>;
      readonly how: HowAnchor; readonly anchor: string; readonly position?: "first" | "last" };

interface EditInput {
  readonly postId: number;
  readonly ops: readonly EditOp[];
  readonly design: PageDesign | null;
  readonly showsOn: readonly string[] | null;
}

function normalise(o: z.infer<typeof operationSchema>, designed: boolean): EditOp | null {
  const fragment = designed ? parseDesignedOutlineFragment : parseOutlineFragment;
  switch (o.op) {
    case "set_shows_on": return null;
    case "set_text":
      return { op: "set_text", ref: o.ref, field: o.field, text: o.text };
    case "insert": {
      const outline = fragment(o.outline);
      if (outline === null) return null;
      if ("after" in o) return { op: "insert", how: "after", anchor: o.after, position: null, outline };
      if ("before" in o) return { op: "insert", how: "before", anchor: o.before, position: null, outline };
      return { op: "insert", how: "into", anchor: o.into, position: o.position ?? null, outline };
    }
    case "replace": {
      const outline = fragment(o.outline);
      return outline === null ? null : { op: "replace", ref: o.ref, outline };
    }
    case "remove":
      return { op: "remove", ref: o.ref };
    case "move":
    case "duplicate":
    case "add_native": {
      const placed = "after" in o ? { how: "after" as const, anchor: o.after }
        : "before" in o ? { how: "before" as const, anchor: o.before }
        : { how: "into" as const, anchor: o.into, position: o.position };
      if (o.op === "add_native") return { op: o.op, widget: o.widget, settings: Array.isArray(o.settings) ? {} : o.settings, ...placed };
      return { op: o.op, ref: o.ref, ...placed };
    }
    case "set_native":
      return { op: o.op, ref: o.ref, settings: Array.isArray(o.settings) ? {} : o.settings };
  }
}

function parseInput(inputJson: string): EditInput | null {
  const raw = readInput(inputJson);
  if (raw === undefined) return null;
  const parsed = inputSchema.safeParse(raw);
  if (!parsed.success) return null;
  const ops: EditOp[] = [];
  let showsOn: readonly string[] | null = null;
  for (const o of parsed.data.operations) {
    if (o.op === "set_shows_on") {
      if (showsOn !== null) return null;
      showsOn = o.rules;
      continue;
    }
    const op = normalise(o, parsed.data.design !== undefined);
    if (op === null) return null;
    ops.push(op);
  }
  return { postId: parsed.data.post_id, ops, design: parsed.data.design ?? null, showsOn };
}

// --- The checked card -------------------------------------------------------

/** The node kinds the agent's projection names (BuilderContract::KINDS). */
const KINDS = [
  "heading",
  "paragraph",
  "list",
  "image",
  "buttons",
  "quote",
  "separator",
  "spacer",
  "table",
  "group",
  "columns",
  "section",
  "pattern",
  "column",
  "locked",
  "native",
] as const;
export type NodeKind = (typeof KINDS)[number];

const kindSchema = z.enum(KINDS.filter((kind) => kind !== "native"));
const levelSchema = z.number().int().min(1).max(6);
const siteTextSchema = z.record(fieldSchema, z.string());
type SiteText = z.infer<typeof siteTextSchema>;

const anchorSchema = z.strictObject({
  ref: refSchema,
  how: z.enum(["after", "before", "into"]),
  position: positionSchema.optional(),
  kind: kindSchema,
  level: levelSchema.optional(),
  label: z.string().min(1).optional(),
});
const nativeKeySchema = z.strictObject({
  key: z.string().min(1),
  from_the_site: z.strictObject({ before: nativeValueSchema }).optional(),
  after: nativeValueSchema.optional(),
  removed: z.literal(true).optional(),
  links: z.array(z.strictObject({ text: z.string().min(1), external_domain: z.string().min(1).optional() })).min(1).optional(),
}).refine((k) => k.removed === true
  ? k.after === undefined && k.from_the_site !== undefined && k.links === undefined
  : k.after !== undefined && k.after !== null);
const nativePlaceSchema = z.strictObject({ parent: parentSchema, index: z.number().int().min(0) });
const removedNodeSchema = z.strictObject({
  ref: refSchema, kind: z.enum(["widget", "container", "section", "column"]), widget: widgetSchema.optional(),
}).refine((n) => (n.kind === "widget") === (n.widget !== undefined));
const nativeDetailsSchema = z.strictObject({
  widget: widgetSchema.optional(), keys: z.array(nativeKeySchema).min(1).optional(),
  removed_count: z.number().int().min(1).optional(), removed_outline: z.array(removedNodeSchema).min(1).optional(),
  from: nativePlaceSchema.optional(), to: nativePlaceSchema.optional(),
});
const nativeChangeSchema = z.union([
  z.strictObject({ op: z.literal("set_native"), ref: refSchema,
    native: z.strictObject({ widget: widgetSchema, keys: z.array(nativeKeySchema).min(1).optional() }) }),
  z.strictObject({ op: z.literal("add_native"), new_refs: z.array(refSchema).length(1),
    native: z.strictObject({ widget: widgetSchema, keys: z.array(nativeKeySchema).min(1).optional() }) }),
  z.strictObject({ op: z.literal("duplicate"), ref: refSchema, new_refs: z.array(refSchema).min(1),
    native: z.strictObject({ widget: widgetSchema }) }),
  z.strictObject({ op: z.literal("remove"), ref: refSchema,
    native: nativeDetailsSchema.pick({ widget: true, removed_count: true, removed_outline: true })
      .required({ removed_count: true, removed_outline: true })
      .refine((n) => n.removed_count === n.removed_outline.length) }),
  z.strictObject({ op: z.literal("move"), ref: refSchema,
    native: nativeDetailsSchema.pick({ widget: true, from: true, to: true }).required({ from: true, to: true }) }),
]);
const changeSchema = z.strictObject({
  op: z.enum(["set_text", "insert", "replace", "remove", "move", "set_native", "add_native", "duplicate"]),
  ref: refSchema.optional(),
  kind: kindSchema.optional(),
  level: levelSchema.optional(),
  field: fieldSchema.optional(),
  after: z.string().optional(),
  new_refs: z.array(refSchema).optional(),
  anchor: anchorSchema.optional(),
  from_the_site: z.strictObject({ before: siteTextSchema }).optional(),
  native: nativeDetailsSchema.optional(),
}).refine((c) => c.native !== undefined ? nativeChangeSchema.safeParse(c).success
  : !["set_native", "add_native", "duplicate"].includes(c.op));
const nativeOutlineNodeSchema = z.strictObject({
  ref: refSchema, parent: parentSchema, kind: z.literal("native"), widget: widgetSchema, summary: z.string(),
});
const outlineNodeSchema = z.strictObject({
  ref: refSchema,
  parent: z.string().regex(/^(root|[A-Za-z0-9_-]{1,32})$/),
  kind: z.enum(KINDS),
  level: levelSchema.optional(),
  editable: z.array(fieldSchema).optional(),
  from_the_site: siteTextSchema.optional(),
  label: z.string().min(1).optional(),
  variant: patternVariantSchema.optional(),
  widget: widgetSchema.optional(),
  summary: z.string().optional(),
}).refine((n) => n.kind === "native" ? nativeOutlineNodeSchema.safeParse(n).success
  : n.widget === undefined && n.summary === undefined);
const afterOutlineSchema = z.strictObject({
  node_count: z.number().int().min(0),
  truncated: z.boolean(),
  nodes: z.array(outlineNodeSchema),
});
const factsSchema = z
  .strictObject({
    kind: z.literal("builder_edit"),
    post: z.strictObject({
      id: z.number().int().min(1),
      from_the_site: z.strictObject({ title: z.string() }),
    }),
    builder: z.strictObject({
      id: z.literal("elementor"),
      version: z.string().regex(ELEMENTOR_VERSION),
      format: z.enum(["classic", "atomic"]),
    }),
    template: editTemplateSchema.optional(),
    changes: z.array(z.union([changeSchema, z.strictObject({ op: z.literal("set_shows_on"), label: z.literal("Where it shows") })])).max(25),
    after_outline: afterOutlineSchema.optional(),
    after_outline_omitted: z.boolean().optional(),
    checked_at: z.string().refine((s) => !Number.isNaN(Date.parse(s))),
  })
  // The page after the change is either on the card or left off it, never both and never neither.
  .refine((f) => (f.after_outline !== undefined) !== (f.after_outline_omitted === true));

type Facts = z.infer<typeof factsSchema>;
type Change = z.infer<typeof changeSchema>;
type OutlineFact = z.infer<typeof outlineNodeSchema>;

// --- Words (ours) -----------------------------------------------------------

/** A piece of a line: our words, or text from the site (rendered in its own isolated run). */
export interface Seg {
  readonly site: boolean;
  readonly s: string;
}
const ours = (s: string): Seg => ({ site: false, s });
const fromSite = (s: string): Seg => ({ site: true, s });

const KIND_LABELS: Record<Exclude<NodeKind, "heading" | "locked" | "native">, string> = {
  paragraph: "Paragraph",
  list: "List",
  image: "Image",
  buttons: "Button",
  quote: "Quote",
  separator: "Separator",
  spacer: "Space",
  table: "Table",
  group: "Section",
  columns: "Columns",
  section: "Section",
  pattern: "Pattern",
  column: "Column",
};
/** The label of a node WPMgr does not edit when the card gives none. */
export const LOCKED_FALLBACK_LABEL = "Element WPMgr does not edit";

/**
 * WPMgr's name for a kind of node. Never the site's: a locked node takes the
 * label the card carries for it, which is WPMgr's own wording for the node.
 */
export function kindLabel(kind: NodeKind, level: number | undefined, label: string | undefined): string {
  if (kind === "native") return "Elementor widget";
  if (kind === "locked") return label !== undefined ? label : LOCKED_FALLBACK_LABEL;
  if (kind === "heading") return level === undefined ? "Heading" : `Heading ${level}`;
  return KIND_LABELS[kind];
}

const FIELD_VERBS: Record<(typeof FIELDS)[number], string> = {
  text: "Change text",
  url: "Change link",
  caption: "Change caption",
  alt: "Change alt text",
};

/** A pattern field by item and value, in the reader's document order. */
export function editFieldWords(field: EditField): string {
  if (!isPatternField(field)) return field;
  const [array, index, ...tail] = field.split(".");
  const item = { features: "Feature", testimonials: "Testimonial", plans: "Plan", faq: "FAQ", stats: "Stat" }[array!];
  const part = tail[0] === "features" ? `feature ${Number(tail[1]) + 1}`
    : tail[0] === "cta" ? (tail[1] === "url" ? "button link" : "button text") : tail.join(" ");
  return `${item} ${Number(index) + 1} · ${part}`;
}

function fieldVerb(field: EditField): string {
  if (isPatternField(field)) return field.endsWith(".url") ? "Change link" : "Change text";
  return FIELD_VERBS[field as (typeof FIELDS)[number]];
}

export const REPLACE_NOTE = "The replaced block's styling is not kept.";

// --- The view ---------------------------------------------------------------

export type EditOpName = EditOp["op"];

/** One change as the card lists it. */
export interface ChangeLine {
  readonly op: EditOpName | "set_shows_on";
  /** What the change does, in order: our words and the site's. */
  readonly title: readonly Seg[];
  /** set_text: the text before (the site's) and the text asked for (the AI's). */
  readonly was?: string;
  readonly now?: string;
  /** insert and replace: the nodes it puts on the page, as the AI chose them. */
  readonly outline?: readonly EditNode[];
  readonly note?: string;
  /** The line shows text that came from the site. */
  readonly fromSite: boolean;
  readonly native?: z.infer<typeof nativeDetailsSchema>;
  readonly newRefs?: readonly string[];
}

export type Mark = "changed" | "new" | "moved";

/** One node of the page after the change. */
export interface AfterNode {
  readonly ref: string;
  readonly kind: NodeKind;
  /** WPMgr's name for the kind. */
  readonly label: string;
  /** The node's text from the site, with WPMgr's name for its field. */
  readonly texts: ReadonlyArray<{ readonly field: EditField; readonly text: string }>;
  readonly mark: Mark | null;
  readonly children: readonly AfterNode[];
  readonly summary?: string;
  readonly widget?: string;
}

export interface AfterView {
  readonly nodes: readonly AfterNode[];
  readonly nodeCount: number;
  readonly truncated: boolean;
}

export interface PageEditView {
  readonly postId: number;
  /** The draft's title, from the site. */
  readonly title: string;
  readonly builderVersion: string;
  readonly lines: readonly ChangeLine[];
  /** Null when the page after the change was too large to keep on the card. */
  readonly after: AfterView | null;
  readonly checkedAt: string;
  /** True when a set_text is among the changes. */
  readonly hasTextChange: boolean;
  readonly design: PageDesign | null;
  readonly template: EditTemplate | null;
  readonly setsPlaces: boolean;
}

function siteTextOf(t: SiteText | undefined): SiteText {
  return t ?? {};
}

/** What names a node in a line: its text, else its caption. */
function detailSegs(t: SiteText): Seg[] {
  if (t.text !== undefined && t.text !== "") return [ours(' "'), fromSite(t.text), ours('"')];
  if (t.caption !== undefined && t.caption !== "") return [ours(' · caption "'), fromSite(t.caption), ours('"')];
  return [];
}

/** An anchor in a line: its text from the site when it has one, else WPMgr's name for it. */
function anchorSegs(a: z.infer<typeof anchorSchema>, text: SiteText | undefined): Seg[] {
  const t = siteTextOf(text);
  const named = t.text !== undefined && t.text !== "" ? t.text : t.caption !== undefined && t.caption !== "" ? t.caption : null;
  if (named !== null) return [ours('"'), fromSite(named), ours('"')];
  return [ours(kindLabel(a.kind, a.level, a.label))];
}

function sameAnchor(change: Change, op: EditOp): boolean {
  const a = change.anchor;
  if (op.op !== "insert" && op.op !== "move") return a === undefined;
  if (a === undefined || a.ref !== op.anchor || a.how !== op.how) return false;
  // An insert into a node always says where among its children; the default is the end.
  if (op.how === "into") return a.position === (op.position ?? "last");
  return a.position === undefined;
}

/** True when the card's change is the AI's operation, field for field. */
function agrees(change: Change, op: EditOp): boolean {
  if (change.op !== op.op) return false;
  if (change.native !== undefined) return nativeAgrees(change, op);
  switch (op.op) {
    case "set_text":
      return (
        change.ref === op.ref &&
        change.field === op.field &&
        change.after === op.text &&
        change.anchor === undefined &&
        change.new_refs === undefined &&
        change.kind !== undefined &&
        change.kind !== "locked" &&
        (!isPatternField(op.field) || change.kind === "pattern")
      );
    case "insert":
      return (
        change.ref === undefined &&
        change.after === undefined &&
        sameAnchor(change, op) &&
        (change.new_refs?.length ?? 0) > 0
      );
    case "replace":
      return (
        change.ref === op.ref &&
        change.after === undefined &&
        change.anchor === undefined &&
        (change.new_refs?.length ?? 0) > 0 &&
        change.kind !== undefined
      );
    case "remove":
      return (
        change.ref === op.ref &&
        change.after === undefined &&
        change.anchor === undefined &&
        change.new_refs === undefined &&
        change.kind !== undefined
      );
    case "move":
      return (
        change.ref === op.ref &&
        change.after === undefined &&
        change.new_refs === undefined &&
        sameAnchor(change, op) &&
        change.kind !== undefined
      );
    case "set_native":
    case "add_native":
    case "duplicate":
      return false;
  }
}

function sameValue(a: NativeValue, b: NativeValue): boolean {
  if (a === b) return true;
  if (a === null || b === null || typeof a !== "object" || typeof b !== "object") return false;
  if (Array.isArray(a) || Array.isArray(b)) return Array.isArray(a) && Array.isArray(b)
    && a.length === b.length && a.every((v, i) => sameValue(v, b[i]!));
  const keys = Object.keys(a);
  return keys.length === Object.keys(b).length && keys.every((k) => Object.hasOwn(b, k) && sameValue(a[k]!, b[k]!));
}

/** Links remain data; their domain is shown beside the complete address. */
function nativeLinks(value: NativeValue): Array<{ text: string; external_domain: string }> {
  if (typeof value === "string") {
    try {
      const u = new URL(value);
      return (u.protocol === "https:" || u.protocol === "http:") && u.hostname !== ""
        ? [{ text: value, external_domain: u.hostname }] : [];
    } catch { return []; }
  }
  if (value !== null && typeof value === "object") return Object.values(value).flatMap(nativeLinks);
  return [];
}

function nativeLinksAgree(key: z.infer<typeof nativeKeySchema>): boolean {
  const sortLinks = (links: Array<{ text: string; external_domain?: string }>) => links
    .map((link) => ({ text: link.text, external_domain: link.external_domain?.toLowerCase() ?? "" }))
    .sort((a, b) => a.text.localeCompare(b.text));
  return sameValue(sortLinks(key.links ?? []), sortLinks(key.after === undefined ? [] : nativeLinks(key.after)));
}

function nativeAgrees(change: Change, op: EditOp): boolean {
  const n = change.native!;
  if (op.op !== "add_native" && ("ref" in op ? change.ref !== op.ref : true)) return false;
  if (op.op === "set_native" || op.op === "add_native") {
    if (op.op === "add_native" && n.widget !== op.widget) return false;
    const keys = n.keys ?? [];
    if (new Set(keys.map((k) => k.key)).size !== keys.length) return false;
    if (op.op === "add_native" && keys.length !== Object.values(op.settings).filter((v) => v !== null).length) return false;
    return keys.every((k) => Object.hasOwn(op.settings, k.key) && (k.removed === true
      ? op.settings[k.key] === null
      : sameValue(k.after!, op.settings[k.key]!))
      && nativeLinksAgree(k));
  }
  if (op.op === "duplicate") return new Set(change.new_refs).size === change.new_refs!.length;
  if (op.op === "remove") return n.removed_outline?.[0]?.ref === op.ref
    && new Set(n.removed_outline.map((node) => node.ref)).size === n.removed_count
    && n.widget === n.removed_outline[0]?.widget;
  if (op.op === "move") {
    // Into placement names the destination parent and the first position explicitly.
    return op.how !== "into" || (n.to?.parent === op.anchor && (op.position !== "first" || n.to.index === 0));
  }
  return false;
}

export function nativeWidgetWords(widget: string): string {
  return widget.split("-").map((word) => word.charAt(0).toUpperCase() + word.slice(1)).join(" ");
}

function lineFor(change: Change, op: EditOp, textOf: (ref: string) => SiteText | undefined): ChangeLine {
  if (change.native !== undefined) {
    const native = change.native;
    const widget = native.widget === undefined ? "" : nativeWidgetWords(native.widget);
    const title = op.op === "set_native" ? `Change the ${widget} widget`
      : op.op === "add_native" ? `Add a ${widget} widget`
      : op.op === "duplicate" ? "Duplicate a widget"
      : op.op === "remove" ? (native.widget === undefined ? "Remove a section" : "Remove a widget")
      : "Move a widget";
    return { op: op.op, title: [ours(title)], native, newRefs: change.new_refs,
      fromSite: op.op !== "add_native" };
  }
  const before = siteTextOf(change.from_the_site?.before);
  const kind = change.kind;
  const named = kind === undefined ? "" : kindLabel(kind, change.level, undefined);
  switch (op.op) {
    case "set_text": {
      const was = before[op.field] ?? "";
      return {
        op: op.op,
        title: [ours(`${fieldVerb(op.field)} · ${named}${isPatternField(op.field) ? ` · ${editFieldWords(op.field)}` : ""}`)],
        was,
        now: op.text,
        fromSite: true,
      };
    }
    case "insert": {
      const a = change.anchor!;
      const where =
        op.how === "into" ? (op.position === "first" ? "Add at the start of " : "Add at the end of ") : `Add ${op.how} `;
      return {
        op: op.op,
        title: [ours(where), ...anchorSegs(a, textOf(a.ref))],
        outline: op.outline,
        fromSite: textOf(a.ref) !== undefined,
      };
    }
    case "replace": {
      const detail = detailSegs(before);
      return {
        op: op.op,
        title: [ours(`Replace · ${named}`), ...detail, ours(" with")],
        outline: op.outline,
        note: REPLACE_NOTE,
        fromSite: detail.length > 0,
      };
    }
    case "remove": {
      const detail = detailSegs(before);
      return { op: op.op, title: [ours(`Remove · ${named}`), ...detail], fromSite: detail.length > 0 };
    }
    case "move": {
      const a = change.anchor!;
      const detail = detailSegs(before);
      const anchorText = textOf(a.ref);
      return {
        op: op.op,
        title: [ours(`Move · ${named}`), ...detail, ours(` → ${op.how} `), ...anchorSegs(a, anchorText)],
        fromSite: detail.length > 0 || anchorText !== undefined,
      };
    }
    case "set_native":
    case "add_native":
    case "duplicate":
      throw new Error("Native operations require native card facts");
  }
}

const FIELD_ORDER: readonly EditField[] = ["text", "caption", "url", "alt"];

function textsOf(node: OutlineFact): Array<{ field: EditField; text: string }> {
  const t = siteTextOf(node.from_the_site);
  const out: Array<{ field: EditField; text: string }> = [];
  for (const field of [...FIELD_ORDER, ...Object.keys(t).filter(isPatternField)]) {
    const text = t[field];
    if (text !== undefined && text !== "") out.push({ field, text });
  }
  return out;
}

/** The flat, parent-first outline as a tree. Null when a node's parent is not before it. */
function afterView(facts: Facts, marks: ReadonlyMap<string, Mark>): AfterView | null {
  const outline = facts.after_outline;
  if (outline === undefined) return null;
  interface Draft {
    node: OutlineFact;
    children: Draft[];
  }
  const byRef = new Map<string, Draft>();
  const roots: Draft[] = [];
  for (const node of outline.nodes) {
    if (byRef.has(node.ref)) return null;
    const draft: Draft = { node, children: [] };
    if (node.parent === "root") {
      roots.push(draft);
    } else {
      const parent = byRef.get(node.parent);
      if (parent === undefined) return null;
      parent.children.push(draft);
    }
    byRef.set(node.ref, draft);
  }
  const build = (d: Draft): AfterNode => ({
    ref: d.node.ref,
    kind: d.node.kind,
    label: d.node.kind === "pattern" && d.node.variant !== undefined
      ? `Pattern · ${d.node.variant.replace(/-/g, " ")}` : d.node.kind === "native"
        ? `Elementor ${d.node.widget} widget` : kindLabel(d.node.kind, d.node.level, d.node.label),
    summary: d.node.summary,
    widget: d.node.widget,
    texts: textsOf(d.node),
    mark: marks.get(d.node.ref) ?? null,
    children: d.children.map(build),
  });
  return { nodes: roots.map(build), nodeCount: outline.node_count, truncated: outline.truncated };
}

/**
 * The request as the card shows it, or null when the card cannot show it in
 * full: no card, an input or card with a part this screen has no line for,
 * an operation the card does not agree with, or a card for another post.
 */
export function parsePageEdit(r: AbilityRequest): PageEditView | null {
  const input = parseInput(r.input_json);
  if (input === null) return null;
  const parsed = factsSchema.safeParse(r.page_edit);
  if (!parsed.success) return null;
  const facts = parsed.data;
  if (facts.post.id !== input.postId) return null;
  const template = facts.template;
  const placesChanged = template !== undefined
    && JSON.stringify(template.shows_on.before) !== JSON.stringify(template.shows_on.after);
  if (template === undefined && input.showsOn !== null) return null;
  if (template !== undefined && JSON.stringify(template.shows_on.after)
    !== JSON.stringify(input.showsOn ?? template.shows_on.before)) return null;
  const displayChanges = facts.changes.filter((c) => c.op === "set_shows_on");
  if (displayChanges.length !== (placesChanged ? 1 : 0)) return null;
  if (displayChanges.length && facts.changes.at(-1)?.op !== "set_shows_on") return null;
  const changes = facts.changes.filter((c): c is Change => c.op !== "set_shows_on");
  if (changes.length !== input.ops.length || (input.ops.length === 0 && input.showsOn === null)) return null;

  const after = facts.after_outline;
  const nodeText = new Map<string, SiteText>();
  for (const n of after?.nodes ?? []) nodeText.set(n.ref, siteTextOf(n.from_the_site));
  const textOf = (ref: string): SiteText | undefined => {
    const t = nodeText.get(ref);
    return t !== undefined && (t.text !== undefined || t.caption !== undefined) ? t : undefined;
  };

  const marks = new Map<string, Mark>();
  const lines: ChangeLine[] = [];
  for (let i = 0; i < input.ops.length; i += 1) {
    const op = input.ops[i]!;
    const change = changes[i]!;
    if (!agrees(change, op)) return null;
    if (op.op === "set_text" || op.op === "set_native") marks.set(op.ref, "changed");
    if (op.op === "move") marks.set(op.ref, "moved");
    for (const ref of change.new_refs ?? []) marks.set(ref, "new");
    lines.push(lineFor(change, op, textOf));
  }
  if (placesChanged && template) lines.push({
    op: "set_shows_on", fromSite: false,
    title: [ours("Where it shows: "), fromSite(showsOnWords(template.shows_on.before, template.rule_titles)),
      ours(" → "), fromSite(showsOnWords(template.shows_on.after, template.rule_titles))],
  });
  const tree = afterView(facts, marks);
  if (after !== undefined && tree === null) return null;
  return {
    postId: facts.post.id,
    title: facts.post.from_the_site.title,
    builderVersion: facts.builder.version,
    lines,
    after: tree,
    checkedAt: facts.checked_at,
    design: input.design,
    hasTextChange: input.ops.some((o) => o.op === "set_text"),
    template: template ?? null,
    setsPlaces: input.showsOn !== null,
  };
}

// --- Words for the card ------------------------------------------------------

export function changesCount(n: number): string {
  return `${n} ${n === 1 ? "change" : "changes"}`;
}

export const EDIT_TOO_LARGE_COPY = "The page is too large to show in full here. Every change above is shown.";
export const KEEPS_COPY = "Styling a person set on changed text is kept.";
export const CHECKS_COPY =
  "WPMgr saves a copy of the page first and checks afterwards that only this page changed and that Elementor saved exactly these changes. If not, it puts the page back.";
export const EFFECT_COPY = "Draft only. Nobody sees it until a person publishes it.";
export const UNDO_LATER_FIRST_COPY = "Undo the later change first.";
export const UNDO_EXPIRED_COPY = "Undo is no longer available (14 days).";
export const UNDO_NOT_AVAILABLE_COPY = "Undo is not available for this change.";
export const UNDO_RETENTION_DAYS = 14;

/**
 * "Until Oct 15 you can undo this change, newest first. ..." The date is
 * fourteen days after the request was made: the change is made after that, so
 * the undo is open at least until then.
 */
export function undoPromise(createdAt: string): string {
  const made = new Date(createdAt);
  const until = Number.isNaN(made.getTime())
    ? "the end of the undo window"
    : new Date(made.getTime() + UNDO_RETENTION_DAYS * 86_400_000).toLocaleDateString(undefined, {
        month: "short",
        day: "numeric",
      });
  return `Until ${until} you can undo this change, newest first. Later changes you approve, like a featured image, stay.`;
}

/** "09:52 UTC", or a plain word when the time is unreadable. */
export function utcTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "an unreadable time";
  const hh = String(d.getUTCHours()).padStart(2, "0");
  const mm = String(d.getUTCMinutes()).padStart(2, "0");
  return `${hh}:${mm} UTC`;
}

export function checkedLine(iso: string): string {
  return `Page unchanged since the AI read it (${utcTime(iso)})`;
}

// --- Status ------------------------------------------------------------------

export type EditTone = "neutral" | "amber" | "red";

export interface PageEditStatus extends AbilityStatus {
  readonly tone: EditTone;
  /** The page exists as the change left it: offer Open in Elementor and Preview. */
  readonly links: boolean;
}

export const EDIT_DONE_COPY = "Draft changed in Elementor.";
export const EDIT_UNDONE_COPY = "Change undone. The page is back as it was before this change.";
export const EDIT_UNDO_RUNNING_COPY = "WPMgr is putting the page back.";
export const EDIT_UNDO_CONFLICT_COPY =
  "WPMgr won't undo: the page changed after this change (by a person, or by a later change to undo first).";
export const EDIT_UNDO_PUBLISHED_COPY = "This page is published now. Change it in Elementor.";
export const EDIT_UNDO_FAILED_COPY =
  "WPMgr could not undo this change. Open the page in Elementor and check it; its revisions are in WordPress.";
/** An undo the site refused because the copy it kept of the page was not the one WPMgr recorded (undo_code snapshot_tampered). */
export const EDIT_UNDO_TAMPERED_COPY =
  "WPMgr's saved copy of this page was changed on the site, so WPMgr won't use it. Nothing changed.";
export const EDIT_STALE_COPY =
  "The page changed after the AI looked at it. Nothing changed. Ask the AI to read the page again and retry.";
/** A conflict refusal from an API that does not say which conflict it was. */
export const EDIT_CONFLICT_COPY =
  "The page changed after the AI looked at it, or someone has it open in Elementor. Nothing changed. Ask the AI to read the page again and retry.";
export const EDIT_EDITOR_OPEN_COPY = "Someone has this page open in Elementor. Nothing changed.";
export const EDIT_AUTOSAVE_COPY = "Someone has unsaved Elementor changes on this draft. Nothing changed.";
export const EDIT_PUT_BACK_COPY =
  "Elementor did not save the change exactly as approved, so WPMgr put the page back as it was.";
export const EDIT_NOT_SAVED_COPY = "Elementor did not save the change, so nothing changed.";
export const EDIT_UNVERIFIED_COPY =
  "Elementor did not save the change exactly as approved. Open the page in Elementor and check it.";
export const EDIT_NOT_PUT_BACK_COPY =
  "WPMgr could not put the page back exactly. Open it in Elementor and check it; its revisions are in WordPress.";
export const EDIT_NO_SNAPSHOT_COPY = "WPMgr could not save a copy of the page first, so it changed nothing.";
export const EDIT_SNAPSHOT_TOO_LARGE_COPY =
  "WPMgr could not save a copy of the page first: the page is too large to copy safely (over 4 MB). It changed nothing.";
export const EDIT_ADMIN_ONLY_COPY =
  "This draft has content only an administrator can save (custom HTML or code). WPMgr's limited account would remove it, so WPMgr won't change this page.";
export const EDIT_UNKNOWN_ELEMENTS_COPY =
  "This draft uses an Elementor widget that is not active on the site any more. Saving would lose it, so WPMgr won't change this page.";
export const EDIT_LAYOUT_CHANGED_COPY =
  "Elementor would have saved this layout differently from what you were shown, so WPMgr stopped. Nothing changed.";
export const EDIT_SANITISER_COPY =
  "This site changes page content when saving it, often because of a plugin, in a way WPMgr can't approve. Ask the AI to simplify the page.";
export const EDIT_NOT_ELIGIBLE_COPY =
  "This page is no longer a draft that WPMgr created for the AI, so WPMgr changed nothing.";
export const EDIT_REFUSED_COPY = "The site refused to change the page. Nothing was changed.";
export const EDIT_FAILED_COPY =
  "The change was not made because something went wrong on the site. Open the page in Elementor and check it.";

/** The agent's refusal codes that mean the edit was refused on the site before it touched the page. */
const FAILED_PLAIN: Record<string, string> = {
  preview_changed: EDIT_STALE_COPY,
  snapshot_failed: EDIT_NO_SNAPSHOT_COPY,
  snapshot_too_large: EDIT_SNAPSHOT_TOO_LARGE_COPY,
  page_has_admin_only_content: EDIT_ADMIN_ONLY_COPY,
  page_has_unknown_elements: EDIT_UNKNOWN_ELEMENTS_COPY,
  builder_would_change_layout: EDIT_LAYOUT_CHANGED_COPY,
  sanitiser_changed_new_content: EDIT_SANITISER_COPY,
  target_not_eligible: EDIT_NOT_ELIGIBLE_COPY,
  target_not_draft: EDIT_NOT_ELIGIBLE_COPY,
};

/** Codes where the save ran and the page was checked afterwards. */
const PUT_BACK_CODES = new Set(["verify_mismatch", "builder_save_refused", "builder_crashed"]);

/**
 * Which conflict refused a page edit (the API's outcome_detail, a closed set
 * and never the site's words): the page moved on since the AI read it, or
 * someone has it open, or has unsaved changes on it.
 */
const CONFLICT_COPY: Record<string, string> = {
  changed_since_read: EDIT_STALE_COPY,
  editor_open: EDIT_EDITOR_OPEN_COPY,
  autosave_pending: EDIT_AUTOSAVE_COPY,
};

function has(map: Record<string, string>, key: string): boolean {
  return Object.prototype.hasOwnProperty.call(map, key);
}

/**
 * What Elementor's save changed outside the page (the API's outside_change, a
 * closed set and never the site's words), in the contract's own terms. Null,
 * or a value this page has no label for, says nothing of the kind.
 */
const OUTSIDE_CHANGE_LABEL: Record<string, string> = {
  active_kit: "the site's active Elementor kit",
  other_posts: "another post or page, or its data",
  terms: "categories or tags",
  site_settings: "a site setting",
  users: "a user account, a role or the site's administrators",
};

/** The sentence for an edit that changed something outside the page, with the kind in brackets when the API names one. */
function outsideChangeCopy(putBack: boolean, kind: string | null | undefined): string {
  const label = kind != null && has(OUTSIDE_CHANGE_LABEL, kind) ? ` (${OUTSIDE_CHANGE_LABEL[kind]})` : "";
  const next = putBack ? "WPMgr put the page back." : "Open the page in Elementor and check it.";
  return `Something outside this page changed while Elementor saved it${label}. ${next} WPMgr cannot undo changes another plugin made.`;
}

/**
 * The person's wording for a failed edit. restored is the site's put-back
 * report: it decides first, because a page that was not put back exactly is
 * the one thing the person must act on whatever the code was. detail names
 * the conflict behind a `conflict` refusal; without it (a row from an API
 * that does not say) the wording covers every conflict. outside names what a
 * `side_effect_detected` refusal found changed outside the page; without it
 * the sentence says only that something did.
 */
export function failedEditStatus(
  code: string | null | undefined,
  restored: boolean | null | undefined,
  outcome?: string | null,
  detail?: string | null,
  outside?: string | null,
): PageEditStatus {
  const base = { kind: "failed" as AbilityStatusKind, links: true };
  if (code === "restore_mismatch" || restored === false) {
    return { ...base, text: EDIT_NOT_PUT_BACK_COPY, tone: "red" };
  }
  if (code === "conflict") {
    const text = detail != null && has(CONFLICT_COPY, detail) ? CONFLICT_COPY[detail]! : EDIT_CONFLICT_COPY;
    return { ...base, text, tone: "neutral", links: false };
  }
  if (code === "side_effect_detected") {
    return { ...base, text: outsideChangeCopy(restored === true, outside), tone: "red" };
  }
  if (code != null && PUT_BACK_CODES.has(code)) {
    if (restored === true) return { ...base, text: EDIT_PUT_BACK_COPY, tone: "amber" };
    return {
      ...base,
      text: code === "verify_mismatch" ? EDIT_UNVERIFIED_COPY : EDIT_NOT_SAVED_COPY,
      tone: "amber",
    };
  }
  if (code != null && has(FAILED_PLAIN, code)) {
    return { ...base, text: FAILED_PLAIN[code]!, tone: "neutral", links: false };
  }
  const reason = code == null ? null : requestRefusalReason(code);
  if (reason !== null) {
    return { ...base, text: `The change was not made: ${reason}. Nothing was changed.`, tone: "neutral", links: false };
  }
  if (outcome === "refused") return { ...base, text: EDIT_REFUSED_COPY, tone: "neutral", links: false };
  return { ...base, text: EDIT_FAILED_COPY, tone: "neutral" };
}

/**
 * The status line of a page edit in every state, for the card under its body.
 * A change a site's setting approved is decided by no person, so where WPMgr
 * cannot vouch for the result the line says so in the one sentence every card
 * uses (approval tiers, `automaticStatus`).
 */
export function pageEditStatus(r: AbilityRequest): PageEditStatus {
  const base = automaticStatus(r, basePageEditStatus(r));
  const template = editTemplateSchema.safeParse(r.page_edit?.template);
  if (!template.success) return base;
  const status = templateStatus(r, template.data, base);
  if (templatePublished(r)) return { ...status, tone: "amber", links: true };
  if (r.undo_state === "refused_conflict") return { ...status, tone: "red", links: true };
  return status;
}

/** The status as a person's own approval reads it; `pageEditStatus` adjusts it for an automatic one. */
function basePageEditStatus(r: AbilityRequest): PageEditStatus {
  const plain = (kind: AbilityStatusKind, text: string, links = false): PageEditStatus => ({
    kind,
    text,
    tone: "neutral",
    links,
  });
  switch (r.state) {
    case "pending":
      return plain("pending", "Waiting for your decision.");
    case "approved":
      return plain("approved", approvedNotStartedText(r));
    case "dispatched":
      return plain("running", "WPMgr is changing the draft.");
    case "outcome_unknown":
      return {
        kind: "unknown_outcome",
        text: "WPMgr could not confirm whether the change was made. Open the page in Elementor and check it.",
        tone: "amber",
        links: true,
      };
    case "done":
      switch (r.undo_state) {
        case "undone":
          return plain("undone", EDIT_UNDONE_COPY, true);
        case "in_progress":
          return plain("running", EDIT_UNDO_RUNNING_COPY, true);
        case "refused_conflict":
          return plain("undo_refused", EDIT_UNDO_CONFLICT_COPY, true);
        case "refused_published":
          return plain("undo_refused", EDIT_UNDO_PUBLISHED_COPY, true);
        case "failed":
          // The API's undo_code says why the undo failed. A copy the site
          // kept that was not the one WPMgr recorded is refused before
          // anything is written; any other failed undo asks for a check.
          return {
            kind: "undo_failed",
            text: r.undo_code === "snapshot_tampered" ? EDIT_UNDO_TAMPERED_COPY : EDIT_UNDO_FAILED_COPY,
            tone: "red",
            links: true,
          };
        default:
          return plain("done", EDIT_DONE_COPY, true);
      }
    case "failed":
      return failedEditStatus(r.outcome_code, r.restored, r.outcome, r.outcome_detail, r.outside_change);
    case "not_sent":
      return plain(
        "not_sent",
        settingNotSentLine(r.not_sent_reason) ??
          `Nothing was sent: ${notSentText(r.not_sent_reason)}. Nothing was changed.`,
      );
    case "declined":
      return plain("declined", `Declined at ${clockTime(r.decided_at)}. Nothing was changed.`);
    case "withdrawn":
      return plain("withdrawn", "Withdrawn because its connection was revoked. Nothing was changed.");
    case "expired":
      return plain("expired", `Closed unanswered at ${clockTime(r.expires_at)}. Nothing was changed.`);
    default:
      return plain("unrecognised", "This request is in a state this page does not know.");
  }
}

// --- Undo, per change --------------------------------------------------------

export type EditUndoView =
  /** [Undo this change]: the newest applied change of its page, inside the window. */
  | "offer"
  /** A later change of the page is still in effect. */
  | "later_first"
  /** The 14 days are over. */
  | "expired"
  /** An applied change that kept no copy to undo with. */
  | "none"
  /** Nothing to say: not an applied change, or its undo has a result. */
  | "silent";

/**
 * What the card says about undoing one change. windowOpen is the client
 * clock against undo_available_until; undo_offered is the server's word that
 * an undo would start now, which for a page edit is only true on the newest
 * applied change of its page that has not been undone.
 */
export function editUndoView(r: AbilityRequest, windowOpen: boolean): EditUndoView {
  if (r.page_edit?.template && templatePublished(r)) return "silent";
  if (r.state !== "done" || r.outcome !== "applied") return "silent";
  if (r.undo_state == null) return "none";
  if (r.undo_state !== "available") return "silent";
  if (!windowOpen) return "expired";
  return r.undo_offered ? "offer" : "later_first";
}

// --- The draft the AI's changes are on --------------------------------------

/** The post a page edit changes, from its card; null when the card is not one. */
export function editPostId(r: AbilityRequest): number | null {
  const parsed = z.object({ post: z.object({ id: z.number().int().min(1) }) }).safeParse(r.page_edit);
  return parsed.success ? parsed.data.post.id : null;
}

/**
 * How many applied page edits of the draft a page-create request made were
 * made since: the changes its undo also covers. Zero for a request that is
 * not a page creation or made no post.
 */
export function laterEditCount(create: AbilityRequest, all: readonly AbilityRequest[]): number {
  const post = create.created_post_id;
  if (create.ability_name !== "wpmgr/page-create" || post == null) return 0;
  const since = Date.parse(create.created_at);
  return all.filter(
    (r) =>
      isPageEdit(r) &&
      r.site_id === create.site_id &&
      r.state === "done" &&
      r.outcome === "applied" &&
      editPostId(r) === post &&
      Date.parse(r.created_at) >= since,
  ).length;
}

export function alsoCoversLine(n: number): string {
  return `Also covers the ${n} AI ${n === 1 ? "change" : "changes"} made since.`;
}

// --- Requests the AI made that never became a card ---------------------------

const REQUEST_REFUSAL_REASONS: Record<string, string> = {
  node_not_found: "it named a part of the page that is not there",
  node_not_editable: "it asked to change something WPMgr does not edit (a form, a slider, an add-on widget)",
  op_not_supported_by_builder: "Elementor's version of this request is not supported",
  page_too_large: "the change was too large",
  ops_invalid: "its changes did not fit together",
  bad_input: "the request did not follow the rules for changing a page",
};

/** The plain reason for a refusal that stops a request before any card exists; null for any other code. */
export function requestRefusalReason(code: string): string | null {
  return has(REQUEST_REFUSAL_REASONS, code) ? REQUEST_REFUSAL_REASONS[code]! : null;
}

// The words the site's draft check gives for a page the AI may not change or
// read (the agent's DraftEligibility reasons, the read's own and the edit
// target checks). They are tokens, never site text, and only a person sees the
// reason: the AI is told one fixed thing for all of them. A token with no
// entry here names no reason.
const INELIGIBLE_REASONS: Record<string, string> = {
  no_marker: "a person's draft",
  marker_not_a_request: "a person's draft",
  ledger_missing: "a person's draft",
  ledger_other_post: "a person's draft",
  ledger_not_completed: "a draft WPMgr has not finished creating",
  missing: "it no longer exists",
  post_missing: "it no longer exists",
  trashed: "it is in the trash",
  not_draft: "it is no longer a draft, for example because it was published",
  not_published: "it is private, scheduled or otherwise not published",
  password: "it is password-protected",
  not_builder_page: "it was not built with Elementor",
  unreadable: "WPMgr could not read it",
};

/** The person-only reason for a page the AI may not change or read; null for a word WPMgr has no reason for. */
export function ineligibleReason(detail: string | null | undefined): string | null {
  return detail != null && has(INELIGIBLE_REASONS, detail) ? INELIGIBLE_REASONS[detail]! : null;
}

// --- The grey rows in the record of what a connection asked -------------------
//
// A request WPMgr refuses before it becomes a card leaves no card, only an
// audit row (action mcp.tool.denied) that names the ability, the post, the
// code and, for a page that may not be touched, the site's reason word
// (apps/api/internal/mcp/page_structure.go withBuilderEditTarget). This is the
// sentence for that row. Every word is WPMgr's own and the only variable is
// the post's number, so nothing the AI or the site wrote reaches it.

export const PAGE_STRUCTURE_ABILITY = "wpmgr/page-structure";
/** The audit action of a refused tool call. */
export const TOOL_DENIED_ACTION = "mcp.tool.denied";

export type RefusalActivityKind = "refused" | "not_eligible";

export interface RefusalActivity {
  readonly kind: RefusalActivityKind;
  readonly text: string;
}

function postNumber(v: unknown): number | null {
  return typeof v === "number" && Number.isSafeInteger(v) && v >= 1 ? v : null;
}

function reasonSuffix(detail: unknown): string {
  const reason = typeof detail === "string" ? ineligibleReason(detail) : null;
  return reason === null ? "" : ` (${reason})`;
}

/**
 * The sentence for a refused page request, from the metadata of its audit row,
 * or null when the row is not one of these (another tool, another ability, a
 * code with no wording here). A page edit is refused for its input or for the
 * page it names; a page structure read, only for the page.
 */
export function refusalActivity(meta: Record<string, unknown> | null | undefined): RefusalActivity | null {
  if (meta == null) return null;
  const post = postNumber(meta.post_id);
  const code = typeof meta.code === "string" ? meta.code : null;
  if (meta.ability === PAGE_EDIT_ABILITY) {
    if (code === "target_not_eligible") {
      if (post === null) return null;
      return {
        kind: "not_eligible",
        text: `The AI asked to change "#${post}", which is not a draft WPMgr created for it${reasonSuffix(meta.detail)}. WPMgr refused.`,
      };
    }
    // A request that does not follow the input rules is refused before any
    // code is chosen: the row names only the argument.
    const reason = code !== null ? requestRefusalReason(code) : meta.argument === "input" ? requestRefusalReason("bad_input") : null;
    if (reason === null) return null;
    const subject = post === null ? "a page" : `"#${post}"`;
    return {
      kind: "refused",
      text: `The AI asked to change ${subject} and WPMgr refused the request before showing it to you: ${reason}.`,
    };
  }
  if (meta.ability === PAGE_STRUCTURE_ABILITY && code === "post_not_readable" && post !== null) {
    return {
      kind: "not_eligible",
      text: `The AI asked to read "#${post}", which WPMgr does not let it read${reasonSuffix(meta.detail)}. WPMgr refused.`,
    };
  }
  return null;
}
