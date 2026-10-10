import type { AbilityRequest } from "@wpmgr/api";

// Fixture rows for the AI page-create approval card, held to the states the
// database allows. A test that builds a row the table would refuse proves
// nothing about a screen a person can reach, so every row built here is checked
// against the CHECK constraints it could trip:
//
//   m156 (assistant_ability_requests): the ten states, decided_at on exactly the
//     decided states and inside the window, the outcome each state may carry,
//     not_sent_reason iff outcome not_sent, ledger refs (created_post_id,
//     restored, trashed) only on done, failed and outcome_unknown rows,
//     grant_via in (token, browser_sign_in), site_host printable ASCII.
//   m158: undo_state on done rows, or on failed and outcome_unknown rows that
//     name a created post.
//   m161: route_id, route_sha256 and card_facts belong to wpmgr/rest-write rows
//     only (a page-create row carries none of them).
//   The control plane (apps/api/internal/mcp/ability_write.go): card copy
//     version 2 when the outline holds a block beyond heading, paragraph and
//     list, else 1; the editor column is the input's editor; and page_media is
//     the request's distinct image ids in document order
//     (apps/api/internal/abilityrequest/handler.go pageMediaFor), or null.
//   A wpmgr/page-edit row (mcp/page_edit_precheck.go createPageEditRequest,
//     m169): editor builder:elementor, no post_type, snapshot builder_document,
//     effect copy draft, no created post and nothing trashed (the post it
//     changes is the card's, never one the write created), card_facts of kind
//     builder_edit on the wire as page_edit, an applied outcome on done, and
//     page_edit null on every other ability.
//   page_builder (handler.go pageBuilderFor) is null except on a page-create
//     row whose editor is builder:elementor, and holds what the control
//     plane's reader returns (mcp/page_create_input.go readPageCardBuilder):
//     builder elementor, format classic, layout containers or sections, a
//     version of the checked shape. It is null on an Elementor row whose
//     stored facts do not read back.
//   m174 (assistant_ability_requests_policy_approval_shape_check, ..._ask_reason_check,
//     ..._not_sent_reason_check; abilityrequest/approval_dto.go approvalOf and
//     askReasonOf): an approval is on the approved states only, a person's
//     approval names no setting, a setting's approval names the mode and the
//     class WPMgr decided and belongs to a request that never waited (no
//     ask_reason), an ask_reason is one of the contract's ten, and a request
//     closed because a setting or a class changed carries setting_changed or
//     class_changed.

const STATES = [
  "pending",
  "approved",
  "declined",
  "expired",
  "withdrawn",
  "dispatched",
  "done",
  "failed",
  "not_sent",
  "outcome_unknown",
] as const;
const DECIDED = new Set(["approved", "declined", "dispatched", "done", "failed", "not_sent", "outcome_unknown"]);
const SENT = new Set(["done", "failed", "outcome_unknown"]);
const NOT_SENT_REASONS = new Set([
  "grant_inactive",
  "assistant_paused",
  "organisation_deleted",
  "capability_not_held",
  "site_absent",
  "forbidden_by_context",
  "agent_outdated",
  "dispatch_deadline_passed",
  "transport_pre_send",
  "entry_changed",
  "entry_disabled",
  "route_changed",
  "route_disabled",
  "setting_changed",
  "class_changed",
]);
const UNDO_STATES = new Set(["available", "in_progress", "undone", "refused_conflict", "refused_published", "failed"]);
/** The states in which a request has been approved, by a person or by a setting (approval_dto.go approvedStates). */
const APPROVED = new Set(["approved", "dispatched", "done", "failed", "not_sent", "outcome_unknown"]);
/** The AIAskReason enum of the contract: the reasons the wire can carry. */
const ASK_REASONS = new Set([
  "kind_always_asks",
  "unknown_target_state",
  "site_mode_ask",
  "kind_not_in_mode",
  "setter_lacks_permission",
  "over_change_budget",
  "over_site_cap",
  "connection_never_auto",
  "connection_setter_invalid",
  "not_checked",
]);

/** The v1 block types: anything else makes the request card copy version 2. */
const V1_TYPES = new Set(["heading", "paragraph", "list"]);

export type PageMediaRow = NonNullable<AbilityRequest["page_media"]>[number];

/** Image ids of an input in document order, distinct, depth first. */
export function imageIdsOf(inputJson: string): number[] {
  const ids: number[] = [];
  const walk = (node: unknown): void => {
    if (Array.isArray(node)) {
      node.forEach(walk);
    } else if (node !== null && typeof node === "object") {
      const o = node as Record<string, unknown>;
      if (o.type === "image" && typeof o.attachment_id === "number" && !ids.includes(o.attachment_id)) {
        ids.push(o.attachment_id);
      }
      for (const v of Object.values(o)) walk(v);
    }
  };
  walk(JSON.parse(inputJson));
  return ids;
}

function usesLayoutBlock(inputJson: string): boolean {
  let layout = false;
  const walk = (node: unknown): void => {
    if (Array.isArray(node)) {
      node.forEach(walk);
    } else if (node !== null && typeof node === "object") {
      const o = node as Record<string, unknown>;
      if (typeof o.type === "string" && !V1_TYPES.has(o.type)) layout = true;
      for (const v of Object.values(o)) walk(v);
    }
  };
  const outline = (JSON.parse(inputJson) as { outline?: unknown }).outline;
  walk(outline);
  return layout;
}

/** Throws when a row is a state the database (or the control plane) would not store. */
export function assertDbShape(r: AbilityRequest): void {
  const fail = (why: string): never => {
    throw new Error(`fixture row ${r.id} is not a state the database allows: ${why}`);
  };
  if (!(STATES as readonly string[]).includes(r.state)) fail(`state ${r.state}`);
  if (DECIDED.has(r.state) !== (r.decided_at != null)) fail("decided_at must be set on exactly the decided states");
  if (r.decided_at != null && DECIDED.has(r.state) && r.state !== "declined") {
    if (!(Date.parse(r.decided_at) < Date.parse(r.expires_at))) fail("an approval must fall inside the window");
  }
  if (!(Date.parse(r.expires_at) > Date.parse(r.created_at))) fail("expires_at must be after created_at");

  const outcomeOk =
    r.state === "done"
      ? r.outcome === "created" || r.outcome === "applied"
      : r.state === "failed"
        ? r.outcome === "refused" || r.outcome === "verify_mismatch" || r.outcome === "failed"
        : r.state === "not_sent"
          ? r.outcome === "not_sent"
          : r.state === "outcome_unknown"
            ? r.outcome == null || r.outcome === "outcome_unknown"
            : r.outcome == null;
  if (!outcomeOk) fail(`outcome ${String(r.outcome)} on a ${r.state} row`);
  if (r.outcome_code != null && r.outcome == null) fail("outcome_code needs an outcome");
  if (r.outcome_code != null && !/^[a-z][a-z0-9_]{0,63}$/.test(r.outcome_code)) fail("outcome_code shape");
  // outcome_detail (abilityrequest/outcome_detail.go): only a page edit refused
  // as a conflict carries one, and only from the closed set of three.
  if (r.outcome_detail != null) {
    if (r.ability_name !== "wpmgr/page-edit" || r.outcome_code !== "conflict") {
      fail("outcome_detail belongs to a wpmgr/page-edit refused as a conflict");
    }
    if (!["changed_since_read", "editor_open", "autosave_pending"].includes(r.outcome_detail)) {
      fail("outcome_detail value");
    }
  }
  // outside_change (abilityrequest/outcome_detail.go outsideChangeFor): only a
  // page edit refused with side_effect_detected carries one, and only from the
  // closed set of five.
  if (r.outside_change != null) {
    if (r.ability_name !== "wpmgr/page-edit" || r.outcome_code !== "side_effect_detected") {
      fail("outside_change belongs to a wpmgr/page-edit refused with side_effect_detected");
    }
    if (!["active_kit", "other_posts", "terms", "site_settings", "users"].includes(r.outside_change)) {
      fail("outside_change value");
    }
  }
  // undo_code (m169 assistant_ability_requests_undo_code_check and
  // ..._undo_code_only_when_failed_check; abilityrequest/undo.go undoCodeOf):
  // only an undo that failed carries one, and only from the closed set of two.
  if (r.undo_code != null) {
    if (r.undo_state !== "failed") fail("undo_code belongs to an undo that failed");
    if (!["snapshot_tampered", "restore_mismatch"].includes(r.undo_code)) fail("undo_code value");
  }
  if ((r.outcome === "not_sent") !== (r.not_sent_reason != null)) fail("not_sent_reason belongs to not_sent rows only");
  if (r.not_sent_reason != null && !NOT_SENT_REASONS.has(r.not_sent_reason)) fail("not_sent_reason value");

  if (r.created_post_id != null && !(Number.isInteger(r.created_post_id) && r.created_post_id > 0)) {
    fail("created_post_id must be a positive integer");
  }
  if (r.outcome === "created" && r.created_post_id == null) fail("a created outcome names its post");
  if ((r.created_post_id != null || r.restored != null || r.trashed != null) && !SENT.has(r.state)) {
    fail("ledger refs belong to done, failed and outcome_unknown rows");
  }

  if (r.undo_state != null) {
    if (!UNDO_STATES.has(r.undo_state)) fail("undo_state value");
    const recovery = (r.state === "failed" || r.state === "outcome_unknown") && r.created_post_id != null;
    if (r.state !== "done" && !recovery) fail("an undo needs a done row, or a failed row that names a created post");
  }
  if ((r.undo_state == null) !== (r.undo_available_until == null)) fail("undo_available_until goes with undo_state");

  if (r.ability_name !== "wpmgr/rest-write" && (r.route_id != null || r.route_sha256 != null || r.card_facts != null)) {
    fail("route_id, route_sha256 and card_facts belong to wpmgr/rest-write rows");
  }
  if (!/^[!-~]{1,255}$/.test(r.site_host)) fail("site_host must be printable ASCII with no spaces");
  if (!["token", "browser_sign_in"].includes(r.grant_via)) fail("grant_via");
  if (r.presented_digest !== undefined && !/^[0-9a-f]{64}$/.test(r.presented_digest)) fail("presented_digest shape");

  if (r.approval != null) {
    if (!APPROVED.has(r.state)) fail("an approval belongs to the approved states");
    if (r.approval.source === "person" && r.approval.setting != null) fail("a person's approval names no setting");
    if (r.approval.source === "policy") {
      if (r.ask_reason != null) fail("a request a setting approved never waited, so it has no ask_reason");
      if (r.change_class == null) fail("a setting's approval names the class WPMgr decided");
    }
  }
  if (r.ask_reason != null && !ASK_REASONS.has(r.ask_reason)) fail("ask_reason value");

  if (r.ability_name === "wpmgr/page-edit") {
    if (r.editor !== ELEMENTOR) fail("a page edit is an Elementor edit");
    if (r.post_type != null) fail("a page edit records no post type");
    if (r.snapshot !== "builder_document") fail("a page edit snapshots the builder document");
    if (r.effect_copy !== "draft") fail("a page edit changes a draft");
    if (r.created_post_id != null || r.trashed != null) fail("a page edit created and trashed no post");
    if (r.state === "done" && r.outcome !== "applied") fail("a done page edit is applied");
    if (r.state === "outcome_unknown" && r.undo_state != null) fail("an unknown page edit has no undo");
    if (r.page_media != null || r.page_builder != null) fail("page_media and page_builder are for page-create rows");
  } else if (r.page_edit != null) {
    fail("page_edit is for wpmgr/page-edit rows only");
  }

  if (r.ability_name === "wpmgr/page-create") {
    const wantVersion = usesLayoutBlock(r.input_json) ? 2 : 1;
    if (r.card_copy_version !== wantVersion) fail(`card_copy_version must be ${wantVersion} for this outline`);
    if (r.page_media != null) {
      const ids = imageIdsOf(r.input_json);
      const got = r.page_media.map((m) => m.id);
      if (JSON.stringify(ids) !== JSON.stringify(got)) fail("page_media must list the outline's distinct image ids in order");
    }
    const editor = inputEditor(r.input_json);
    if (r.editor !== editor) fail("the editor column is the input's editor");
    if (r.page_builder != null) {
      if (editor !== ELEMENTOR) fail("page_builder belongs to a page Elementor builds");
      const b = r.page_builder;
      if (
        Object.keys(b).length !== 4 ||
        b.builder !== "elementor" ||
        b.format !== "classic" ||
        (b.layout !== "containers" && b.layout !== "sections") ||
        !/^[0-9]{1,4}\.[0-9]{1,4}(\.[0-9]{1,4})?([.-][0-9A-Za-z]{1,16}){0,2}$/.test(b.version)
      ) {
        fail("page_builder must be a value the control plane's reader returns");
      }
    }
  } else if (r.page_media != null || r.page_builder != null) {
    fail("page_media and page_builder are for wpmgr/page-create rows only");
  }
}

const ELEMENTOR = "builder:elementor";

/** The editor an input names, as the control plane records it. */
function inputEditor(inputJson: string): string | null {
  const editor = (JSON.parse(inputJson) as { editor?: unknown }).editor;
  return typeof editor === "string" ? editor : null;
}

export interface PageInput {
  title?: string;
  post_type?: "page" | "post";
  editor?: "wordpress_blocks" | "wordpress_classic" | "builder:elementor";
  elementor_format?: "site_default" | "classic" | "atomic";
  outline: unknown[];
}

export function pageInput(input: PageInput): string {
  return JSON.stringify({
    post_type: input.post_type ?? "page",
    editor: input.editor ?? "wordpress_blocks",
    title: input.title ?? "Spring menu",
    outline: input.outline,
    ...(input.elementor_format === undefined ? {} : { elementor_format: input.elementor_format }),
  });
}

export type PageBuilderRow = NonNullable<AbilityRequest["page_builder"]>;

/** The page_builder of a page Elementor builds, as the control plane returns it. */
export function elementorFacts(over: Partial<PageBuilderRow> = {}): PageBuilderRow {
  return { builder: "elementor", format: "classic", version: "3.35.9", layout: "containers", ...over };
}

/** What the site reported for an attachment (the page_media fact shape). */
export function mediaFact(id: number, over: Partial<PageMediaRow> = {}): PageMediaRow {
  return { id, filename: `photo-${id}.jpg`, mime: "image/jpeg", width: 1200, height: 800, ...over };
}

export const SITE_HOST = "example.com";

/**
 * A wpmgr/page-create row as the queue returns it. The default is a pending
 * text-only request; pass `input_json` (from pageInput) for a layout. The card
 * copy version follows the outline and page_media follows `media` (null when
 * the outline places no image), as the control plane writes them.
 */
export function pageCreateRow(
  over: Partial<AbilityRequest> & { media?: PageMediaRow[] | null } = {},
): AbilityRequest {
  const { media, ...rest } = over;
  const input_json = rest.input_json ?? pageInput({ outline: [{ type: "paragraph", text: "Big savings." }] });
  const layout = usesLayoutBlock(input_json);
  const row: AbilityRequest = {
    id: "pc-1",
    site_id: "site-1",
    ability_name: "wpmgr/page-create",
    input_json,
    title_excerpt: "Spring menu",
    editor: inputEditor(input_json),
    post_type: "page",
    effect_copy: "draft",
    snapshot: "created_post_trash",
    site_label: "Shop One",
    site_host: SITE_HOST,
    grant_label: "Claude",
    grant_via: "token",
    setup_client: "claude-code",
    card_copy_version: layout ? 2 : 1,
    presented_digest: "c".repeat(64),
    state: "pending",
    created_at: "2026-10-01T09:55:00Z",
    expires_at: "2026-10-01T11:00:00Z",
    undo_offered: false,
    resolve_gave_up: false,
    page_media: media === undefined ? null : media,
    page_builder: null,
    ...rest,
  };
  assertDbShape(row);
  return row;
}

// --- wpmgr/page-edit rows -----------------------------------------------------

export type PageEditRow = NonNullable<AbilityRequest["page_edit"]>;

/** A page-edit operation of the AI's input, in the schema's shapes. */
export type EditOpInput = Record<string, unknown>;

const FINGERPRINT = "8".repeat(64);

/**
 * The AI's input of a page edit (apps/agent/tests/fixtures/ability-run/
 * page-edit-schema.json). The default is four changes on page 418: the text
 * of a heading, a paragraph added after one, an image replaced by a heading,
 * and a paragraph moved after a heading.
 */
export function defaultEditOps(): EditOpInput[] {
  return [
    { op: "set_text", ref: "h_sale", field: "text", text: "Summer sale" },
    { op: "insert", after: "p_free", outline: [{ type: "paragraph", text: "Open every day." }] },
    { op: "replace", ref: "img_team", outline: [{ type: "heading", level: 3, text: "Our team" }] },
    { op: "move", ref: "p_faq", after: "h_prod" },
  ];
}

export function editInput(ops: EditOpInput[] = defaultEditOps(), over: { post_id?: number } = {}): string {
  return JSON.stringify({ post_id: over.post_id ?? 418, base_fingerprint: FINGERPRINT, operations: ops });
}

/**
 * The checked card of the default four changes, as the control plane returns
 * it (mcp/page_edit_precheck.go, PageEditCardFacts): the page after them is a
 * flat list, parents first, the way the agent's projection writes it.
 */
export function editFacts(over: Partial<PageEditRow> = {}): PageEditRow {
  const facts: PageEditRow = {
    kind: "builder_edit",
    post: { id: 418, from_the_site: { title: "Spring sale" } },
    builder: { id: "elementor", version: "3.35.9", format: "classic" },
    changes: [
      {
        op: "set_text",
        ref: "h_sale",
        kind: "heading",
        level: 2,
        field: "text",
        after: "Summer sale",
        from_the_site: { before: { text: "Spring sale" } },
      },
      { op: "insert", new_refs: ["p_open"], anchor: { ref: "p_free", how: "after", kind: "paragraph" } },
      {
        op: "replace",
        ref: "img_team",
        kind: "image",
        new_refs: ["h_team"],
        from_the_site: { before: { caption: "Team photo" } },
      },
      {
        op: "move",
        ref: "p_faq",
        kind: "paragraph",
        from_the_site: { before: { text: "FAQ: returns in 30 days" } },
        anchor: { ref: "h_prod", how: "after", kind: "heading", level: 2 },
      },
    ],
    after_outline: {
      node_count: 9,
      truncated: false,
      nodes: [
        { ref: "g_main", parent: "root", kind: "group", editable: [], from_the_site: {} },
        { ref: "h_sale", parent: "g_main", kind: "heading", level: 2, editable: ["text"], from_the_site: { text: "Summer sale" } },
        { ref: "p_free", parent: "g_main", kind: "paragraph", editable: ["text"], from_the_site: { text: "Free delivery" } },
        { ref: "p_open", parent: "g_main", kind: "paragraph", editable: ["text"], from_the_site: { text: "Open every day." } },
        { ref: "h_team", parent: "g_main", kind: "heading", level: 3, editable: ["text"], from_the_site: { text: "Our team" } },
        { ref: "f_form", parent: "g_main", kind: "locked", label: "Elementor element WPMgr does not edit" },
        { ref: "g_prod", parent: "root", kind: "group", editable: [], from_the_site: {} },
        { ref: "h_prod", parent: "g_prod", kind: "heading", level: 2, editable: ["text"], from_the_site: { text: "Our products" } },
        { ref: "p_faq", parent: "g_prod", kind: "paragraph", editable: ["text"], from_the_site: { text: "FAQ: returns in 30 days" } },
      ],
    },
    checked_at: "2026-10-01T09:52:00Z",
    ...over,
  };
  return facts;
}

/**
 * A wpmgr/page-edit row as the queue returns it: pending by default, on page
 * 418. Pass `ops` to change the AI's input (the card is then yours to match),
 * and `page_edit` to change the checked card. Built from the states the
 * database allows (assertDbShape).
 */
export function pageEditRow(
  over: Partial<AbilityRequest> & { ops?: EditOpInput[]; post_id?: number } = {},
): AbilityRequest {
  const { ops, post_id, ...rest } = over;
  const row: AbilityRequest = {
    id: "pe-1",
    site_id: "site-1",
    ability_name: "wpmgr/page-edit",
    input_json: editInput(ops, { post_id }),
    title_excerpt: "Spring sale",
    editor: ELEMENTOR,
    post_type: null,
    effect_copy: "draft",
    snapshot: "builder_document",
    site_label: "Shop One",
    site_host: SITE_HOST,
    grant_label: "Claude",
    grant_via: "token",
    setup_client: "claude-code",
    card_copy_version: 1,
    presented_digest: "e".repeat(64),
    state: "pending",
    created_at: "2026-10-01T09:55:00Z",
    expires_at: "2026-10-01T11:00:00Z",
    undo_offered: false,
    resolve_gave_up: false,
    page_media: null,
    page_builder: null,
    page_edit: editFacts(),
    ...rest,
  };
  assertDbShape(row);
  return row;
}

// --- how a request was approved, and why one waits (approval tiers) -----------

type ApprovalFields = Pick<AbilityRequest, "approval" | "change_class" | "change_kind_name">;

/** WPMgr's name for the draft class as copy uses it (aipolicy Class.KindName). */
const AI_DRAFT_KIND = "changes to the AI's own drafts";

/**
 * What a request carries once a site's setting approved it with no person
 * deciding (approval_dto.go approvalOf): the setting it relied on, copied onto
 * the row, and the class WPMgr decided. `over` changes the copied setting.
 */
export function autoApproved(
  over: Partial<NonNullable<NonNullable<AbilityRequest["approval"]>["setting"]>> = {},
): ApprovalFields {
  return {
    approval: {
      source: "policy",
      setting: {
        mode: "ai_drafts",
        source: "person",
        set_by_user_id: "user-1",
        set_by_name: "Priya",
        set_by_account_deleted: false,
        set_at: "2026-10-09T12:00:00Z",
        ...over,
      },
    },
    change_class: "ai_draft",
    change_kind_name: AI_DRAFT_KIND,
  };
}

/** What a request carries once a person approved it: no setting, and the class WPMgr decided. */
export function personApproved(): ApprovalFields {
  return { approval: { source: "person", setting: null }, change_class: "ai_draft", change_kind_name: AI_DRAFT_KIND };
}

/** What a request that waits for a person carries: why it waits, and the class WPMgr decided. */
export function waitsBecause(
  reason: NonNullable<AbilityRequest["ask_reason"]>,
): Pick<AbilityRequest, "ask_reason" | "change_class" | "change_kind_name"> {
  return { ask_reason: reason, change_class: "ai_draft", change_kind_name: AI_DRAFT_KIND };
}
