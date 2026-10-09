import { z } from "zod";
import type { AbilityRequest } from "@wpmgr/api";

import {
  clockTime,
  notSentText,
  type AbilityStatus,
  type AbilityStatusKind,
} from "./ability-card-model";
import { ELEMENTOR_VERSION, parseOutlineFragment, readInput, type OutlineNode } from "./outline-model";

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

// --- The AI's input ---------------------------------------------------------

const REF_PATTERN = /^[A-Za-z0-9_-]{1,32}$/;
const refSchema = z.string().regex(REF_PATTERN);
const FIELDS = ["text", "url", "alt", "caption"] as const;
const fieldSchema = z.enum(FIELDS);
const positionSchema = z.enum(["first", "last"]);

export type EditField = (typeof FIELDS)[number];
export type HowAnchor = "after" | "before" | "into";

const outlineJson = z.array(z.unknown());
const operationSchema = z.union([
  z.strictObject({ op: z.literal("set_text"), ref: refSchema, field: fieldSchema, text: z.string() }),
  z.strictObject({ op: z.literal("insert"), after: refSchema, outline: outlineJson }),
  z.strictObject({ op: z.literal("insert"), before: refSchema, outline: outlineJson }),
  z.strictObject({ op: z.literal("insert"), into: refSchema, position: positionSchema.optional(), outline: outlineJson }),
  z.strictObject({ op: z.literal("replace"), ref: refSchema, outline: outlineJson }),
  z.strictObject({ op: z.literal("remove"), ref: refSchema }),
  z.strictObject({ op: z.literal("move"), ref: refSchema, after: refSchema }),
  z.strictObject({ op: z.literal("move"), ref: refSchema, before: refSchema }),
]);
const inputSchema = z.strictObject({
  post_id: z.number().int().min(1),
  base_fingerprint: z.string().regex(/^[0-9a-f]{64}$/),
  operations: z.array(operationSchema).min(1).max(25),
});

/** One operation of the AI's input in one shape: the target, the place and the new nodes. */
export type EditOp =
  | { readonly op: "set_text"; readonly ref: string; readonly field: EditField; readonly text: string }
  | {
      readonly op: "insert";
      readonly how: HowAnchor;
      readonly anchor: string;
      readonly position: "first" | "last" | null;
      readonly outline: readonly OutlineNode[];
    }
  | { readonly op: "replace"; readonly ref: string; readonly outline: readonly OutlineNode[] }
  | { readonly op: "remove"; readonly ref: string }
  | { readonly op: "move"; readonly ref: string; readonly how: "after" | "before"; readonly anchor: string };

interface EditInput {
  readonly postId: number;
  readonly ops: readonly EditOp[];
}

function normalise(o: z.infer<typeof operationSchema>): EditOp | null {
  switch (o.op) {
    case "set_text":
      return { op: "set_text", ref: o.ref, field: o.field, text: o.text };
    case "insert": {
      const outline = parseOutlineFragment(o.outline);
      if (outline === null) return null;
      if ("after" in o) return { op: "insert", how: "after", anchor: o.after, position: null, outline };
      if ("before" in o) return { op: "insert", how: "before", anchor: o.before, position: null, outline };
      return { op: "insert", how: "into", anchor: o.into, position: o.position ?? null, outline };
    }
    case "replace": {
      const outline = parseOutlineFragment(o.outline);
      return outline === null ? null : { op: "replace", ref: o.ref, outline };
    }
    case "remove":
      return { op: "remove", ref: o.ref };
    case "move":
      return "after" in o
        ? { op: "move", ref: o.ref, how: "after", anchor: o.after }
        : { op: "move", ref: o.ref, how: "before", anchor: o.before };
  }
}

function parseInput(inputJson: string): EditInput | null {
  const raw = readInput(inputJson);
  if (raw === undefined) return null;
  const parsed = inputSchema.safeParse(raw);
  if (!parsed.success) return null;
  const ops: EditOp[] = [];
  for (const o of parsed.data.operations) {
    const op = normalise(o);
    if (op === null) return null;
    ops.push(op);
  }
  return { postId: parsed.data.post_id, ops };
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
  "column",
  "locked",
] as const;
export type NodeKind = (typeof KINDS)[number];

const kindSchema = z.enum(KINDS);
const levelSchema = z.number().int().min(1).max(6);
const siteTextSchema = z.strictObject({
  text: z.string().optional(),
  url: z.string().optional(),
  alt: z.string().optional(),
  caption: z.string().optional(),
});
type SiteText = z.infer<typeof siteTextSchema>;

const anchorSchema = z.strictObject({
  ref: refSchema,
  how: z.enum(["after", "before", "into"]),
  position: positionSchema.optional(),
  kind: kindSchema,
  level: levelSchema.optional(),
  label: z.string().min(1).optional(),
});
const changeSchema = z.strictObject({
  op: z.enum(["set_text", "insert", "replace", "remove", "move"]),
  ref: refSchema.optional(),
  kind: kindSchema.optional(),
  level: levelSchema.optional(),
  field: fieldSchema.optional(),
  after: z.string().optional(),
  new_refs: z.array(refSchema).optional(),
  anchor: anchorSchema.optional(),
  from_the_site: z.strictObject({ before: siteTextSchema }).optional(),
});
const outlineNodeSchema = z.strictObject({
  ref: refSchema,
  parent: z.string().regex(/^(root|[A-Za-z0-9_-]{1,32})$/),
  kind: kindSchema,
  level: levelSchema.optional(),
  editable: z.array(fieldSchema).optional(),
  from_the_site: siteTextSchema.optional(),
  label: z.string().min(1).optional(),
});
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
    changes: z.array(changeSchema).min(1).max(25),
    after_outline: afterOutlineSchema.optional(),
    after_outline_omitted: z.boolean().optional(),
    checked_at: z.string().refine((s) => !Number.isNaN(Date.parse(s))),
  })
  // The page after the change is either on the card or left off it, never both and never neither.
  .refine((f) => (f.after_outline !== undefined) !== (f.after_outline_omitted === true));

type Facts = z.infer<typeof factsSchema>;
type Change = Facts["changes"][number];
type OutlineFact = z.infer<typeof outlineNodeSchema>;

// --- Words (ours) -----------------------------------------------------------

/** A piece of a line: our words, or text from the site (rendered in its own isolated run). */
export interface Seg {
  readonly site: boolean;
  readonly s: string;
}
const ours = (s: string): Seg => ({ site: false, s });
const fromSite = (s: string): Seg => ({ site: true, s });

const KIND_LABELS: Record<Exclude<NodeKind, "heading" | "locked">, string> = {
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
  column: "Column",
};
/** The label of a node WPMgr does not edit when the card gives none. */
export const LOCKED_FALLBACK_LABEL = "Element WPMgr does not edit";

/**
 * WPMgr's name for a kind of node. Never the site's: a locked node takes the
 * label the card carries for it, which is WPMgr's own wording for the node.
 */
export function kindLabel(kind: NodeKind, level: number | undefined, label: string | undefined): string {
  if (kind === "locked") return label !== undefined ? label : LOCKED_FALLBACK_LABEL;
  if (kind === "heading") return level === undefined ? "Heading" : `Heading ${level}`;
  return KIND_LABELS[kind];
}

const FIELD_VERBS: Record<EditField, string> = {
  text: "Change text",
  url: "Change link",
  caption: "Change caption",
  alt: "Change alt text",
};

export const REPLACE_NOTE = "The replaced block's styling is not kept.";

// --- The view ---------------------------------------------------------------

export type EditOpName = EditOp["op"];

/** One change as the card lists it. */
export interface ChangeLine {
  readonly op: EditOpName;
  /** What the change does, in order: our words and the site's. */
  readonly title: readonly Seg[];
  /** set_text: the text before (the site's) and the text asked for (the AI's). */
  readonly was?: string;
  readonly now?: string;
  /** insert and replace: the nodes it puts on the page, as the AI chose them. */
  readonly outline?: readonly OutlineNode[];
  readonly note?: string;
  /** The line shows text that came from the site. */
  readonly fromSite: boolean;
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
  if (op.op === "insert" && op.how === "into") return a.position === (op.position ?? "last");
  return a.position === undefined;
}

/** True when the card's change is the AI's operation, field for field. */
function agrees(change: Change, op: EditOp): boolean {
  if (change.op !== op.op) return false;
  switch (op.op) {
    case "set_text":
      return (
        change.ref === op.ref &&
        change.field === op.field &&
        change.after === op.text &&
        change.anchor === undefined &&
        change.new_refs === undefined &&
        change.kind !== undefined &&
        change.kind !== "locked"
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
  }
}

function lineFor(change: Change, op: EditOp, textOf: (ref: string) => SiteText | undefined): ChangeLine {
  const before = siteTextOf(change.from_the_site?.before);
  const kind = change.kind;
  const named = kind === undefined ? "" : kindLabel(kind, change.level, undefined);
  switch (op.op) {
    case "set_text": {
      const was = before[op.field] ?? "";
      return {
        op: op.op,
        title: [ours(`${FIELD_VERBS[op.field]} · ${named}`)],
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
  }
}

const FIELD_ORDER: readonly EditField[] = ["text", "caption", "url", "alt"];

function textsOf(node: OutlineFact): Array<{ field: EditField; text: string }> {
  const t = siteTextOf(node.from_the_site);
  const out: Array<{ field: EditField; text: string }> = [];
  for (const field of FIELD_ORDER) {
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
    label: kindLabel(d.node.kind, d.node.level, d.node.label),
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
  if (facts.changes.length !== input.ops.length) return null;

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
    const change = facts.changes[i]!;
    if (!agrees(change, op)) return null;
    if (op.op === "set_text") marks.set(op.ref, "changed");
    if (op.op === "move") marks.set(op.ref, "moved");
    for (const ref of change.new_refs ?? []) marks.set(ref, "new");
    lines.push(lineFor(change, op, textOf));
  }
  const tree = afterView(facts, marks);
  if (after !== undefined && tree === null) return null;
  return {
    postId: facts.post.id,
    title: facts.post.from_the_site.title,
    builderVersion: facts.builder.version,
    lines,
    after: tree,
    checkedAt: facts.checked_at,
    hasTextChange: input.ops.some((o) => o.op === "set_text"),
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

/** The status line of a page edit in every state, for the card under its body. */
export function pageEditStatus(r: AbilityRequest): PageEditStatus {
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
      return plain(
        "approved",
        `Approved at ${clockTime(r.decided_at)}. Not started yet. WPMgr sends it to the site shortly.`,
      );
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
      return plain("not_sent", `Nothing was sent: ${notSentText(r.not_sent_reason)}. Nothing was changed.`);
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
