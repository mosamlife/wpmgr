import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import type { AbilityRequest } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";
import { autoApproved, elementorFacts, pageCreateRow, pageInput, templateCreateRow, templateEditRow } from "@/test/ability-request-rows";
import { AiEditingSection } from "./ai-editing-section";

// Wire fields: abilityrequest/handler.go, PageTemplateDTO, pageEditCardFactsFor;
// display changes: mcp/page_edit_precheck.go, PageEditCardChange.
const { editing, list, undo, approve } = vi.hoisted(() => ({
  editing: vi.fn(), list: vi.fn(), undo: vi.fn(), approve: vi.fn(),
}));
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));
vi.mock("@wpmgr/api", async (original) => ({
  ...await original<typeof import("@wpmgr/api")>(),
  getSiteContentEditing: editing, listSiteAbilityRequests: list,
  undoAbilityRequest: undo, approveAbilityRequest: approve,
}));
const ok = (data: unknown) => Promise.resolve({ data, response: { status: 200 } });
const applied = {
  state: "done", outcome: "applied", decided_at: "2026-10-01T09:58:00Z",
  undo_state: "available", undo_offered: true,
  undo_available_until: new Date(Date.now() + 86_400_000).toISOString(),
} satisfies Partial<AbilityRequest>;
const created = { ...applied, outcome: "created", created_post_id: 57 };
async function renderCard(row: AbilityRequest) {
  editing.mockReturnValue(ok({ site_id: "site-1", enabled: true }));
  list.mockReturnValue(ok({ requests: [row], limit: 50, offset: 0 }));
  renderWithProviders(<AiEditingSection siteId="site-1" siteUrl="https://example.com" canOperate />,
    { withRouter: true, initialPath: "/sites/site-1/content" });
  return screen.findByRole("article");
}
function rowValue(card: HTMLElement, label: string) {
  return within(card).getByText(label, { selector: "dt" }).nextElementSibling!;
}
beforeEach(() => vi.clearAllMocks());

describe("template cards W1 to W4", () => {
  it.each(["header", "footer"] as const)("W1 shows a new %s draft and sends its undo", async (type) => {
    const row = templateCreateRow({ type, also_live: [{ id: 12, title: "Main header", shows_on: ["include/general"], shows_on_problem: null }] },
      { ...created, ...autoApproved() });
    undo.mockReturnValue(ok({ ...row, undo_state: "in_progress", undo_offered: false }));
    const card = await renderCard(row);
    expect(within(card).getByRole("heading")).toHaveTextContent(`New ${type} draft in Elementor`);
    expect(rowValue(card, type === "header" ? "Header" : "Footer")).toHaveTextContent('#57 “Brewcraft header” · Draft · Elementor 4.3.4 with Pro');
    expect(rowValue(card, "Shows on")).toHaveTextContent('Entire site, except page “Contact” (#42). Only after a person publishes it.');
    expect(rowValue(card, "Shows on")).toHaveTextContent("Chosen by the AI");
    expect(rowValue(card, "Also live")).toHaveTextContent('“Main header” (#12) shows on the entire site.');
    expect(card).toHaveTextContent("When you publish this one, set that one to draft so the two don't compete.");
    expect(rowValue(card, "Asked by")).toHaveTextContent("Claude");
    expect(card).toHaveTextContent("Draft created in Elementor.");
    expect(rowValue(card, "Checks")).toHaveTextContent("Widgets and settings checked against this site's Elementor and Pro.");
    expect(rowValue(card, "Effect")).toHaveTextContent("Draft only. Visitors see it only after a person publishes it in Elementor.");
    expect(rowValue(card, "Undo")).toHaveTextContent("Undo moves the draft to the trash.");
    expect(within(card).getByRole("link", { name: "Open in Elementor" })).toHaveAttribute("href", "https://example.com/wp-admin/post.php?post=57&action=elementor");
    expect(within(card).queryByRole("link", { name: "Preview" })).toBeNull();
    fireEvent.click(within(card).getByRole("button", { name: "Undo this change" }));
    await waitFor(() => expect(undo).toHaveBeenCalledWith({ path: { siteId: "site-1", requestId: "pc-1" }, body: {} }));
  });
  it("W2 lists where it shows with content edits and restores both", async () => {
    const row = templateEditRow({}, applied);
    undo.mockReturnValue(ok({ ...row, undo_state: "in_progress", undo_offered: false }));
    const card = await renderCard(row);
    expect(within(card).getByRole("heading")).toHaveTextContent("Change a header draft in Elementor");
    expect(rowValue(card, "Header")).toHaveTextContent('#418 “Brewcraft header” · Draft · Elementor 4.3.4 with Pro');
    expect(card).toHaveTextContent('Where it shows: Entire site → All pages, except page “Contact” (#42)');
    expect(card).toHaveTextContent("Draft changed in Elementor.");
    expect(rowValue(card, "Undo")).toHaveTextContent("Undo puts back the content and where it shows.");
    fireEvent.click(within(card).getByRole("button", { name: "Undo this change" }));
    const dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent("Undo puts back the content and where it shows.");
    fireEvent.click(within(dialog).getByRole("button", { name: /^Undo$/ }));
    await waitFor(() => expect(undo).toHaveBeenCalledWith({ path: { siteId: "site-1", requestId: "pe-1" }, body: {} }));
  });
  it.each(["header", "footer"] as const)("W3 closes undo for a published %s on create and edit", async (type) => {
    const row = templateEditRow({ type, refusal_message: `A person published this ${type}, scheduled it or made it private, so it cannot be undone. Change it in WordPress.` },
      { ...applied, undo_state: "refused_published", undo_offered: false });
    const card = await renderCard(row);
    expect(card).toHaveTextContent(`A person published this ${type} in Elementor. WPMgr no longer changes it, and undo is closed.`);
    expect(rowValue(card, type === "header" ? "Header" : "Footer")).toHaveTextContent("Published");
    expect(within(card).queryByRole("button", { name: "Undo this change" })).toBeNull();
  });
  it("W3 also closes a creation's undo", async () => {
    const card = await renderCard(templateCreateRow({}, { ...created, undo_state: "refused_published", undo_offered: false }));
    expect(card).toHaveTextContent("A person published this header in Elementor. WPMgr no longer changes it, and undo is closed.");
    expect(within(card).queryByRole("button")).toBeNull();
  });
  it.each(["header", "footer"] as const)("W4 describes a %s edited in Elementor", async (type) => {
    const card = await renderCard(templateEditRow({ type }, { ...applied, undo_state: "refused_conflict", undo_offered: false }));
    expect(card).toHaveTextContent(`This ${type} changed in Elementor after the AI's change. Undo would overwrite that work, so nothing was undone.`);
    expect(within(card).queryByRole("button", { name: "Undo this change" })).toBeNull();
    expect(within(card).getByRole("link", { name: "Open in Elementor" })).toBeInTheDocument();
  });
});

describe("display rules in plain words", () => {
  it.each([
    ["include/general", "Entire site"], ["include/singular/front_page", "Front page"],
    ["include/singular/page", "All pages"], ["include/singular/page/42", 'Page “Contact” (#42)'],
    ["include/singular/post", "All posts"], ["include/singular/post/42", 'Post “Contact” (#42)'],
    ["include/singular/not_found404", "404 page"], ["include/archive", "All archives"],
    ["include/singular/page/99", "page #99"], ["include/singular/post/99", "post #99"],
    ["include/archive/category/5", "another place (include/archive/category/5)"],
  ])("shows %s as %s", async (rule, words) => {
    const card = await renderCard(templateCreateRow({ shows_on: [rule] }, created));
    expect(rowValue(card, "Shows on")).toHaveTextContent(`${words}. Only after a person publishes it.`);
  });
  it("joins multiple includes and excludes without hiding unknown places", async () => {
    const card = await renderCard(templateCreateRow({ shows_on: ["include/singular/page", "include/singular/post", "exclude/singular/page/1", "exclude/singular/post/2", "exclude/archive/category/5"],
      rule_titles: [{ id: 1, title: "A" }, { id: 2, title: "B" }] }, created));
    expect(rowValue(card, "Shows on")).toHaveTextContent('All pages and all posts, except page “A” (#1) and post “B” (#2) and another place (exclude/archive/category/5).');
  });
  it("shows an empty list", async () => {
    const card = await renderCard(templateCreateRow({ shows_on: [] }, created));
    expect(rowValue(card, "Shows on")).toHaveTextContent("Nowhere yet. A person chooses where it shows in Elementor.");
  });
  it("shows site titles as text", async () => {
    const title = '<img src=x onerror="alert(1)"><script>alert(2)</script>';
    const card = await renderCard(templateCreateRow({ rule_titles: [{ id: 42, title }],
      also_live: [{ id: 12, title, shows_on: ["include/general"], shows_on_problem: null }] }, created));
    expect(rowValue(card, "Shows on")).toHaveTextContent(`except page “${title}” (#42)`);
    expect(rowValue(card, "Also live")).toHaveTextContent(`“${title}” (#12)`);
    expect(card.querySelector("img, script")).toBeNull();
  });
  it.each([null, []])("omits Also live for %s", async (also_live) => {
    const card = await renderCard(templateCreateRow({ also_live }, created));
    expect(within(card).queryByText("Also live", { selector: "dt" })).toBeNull();
  });
  it("warns once about several live templates and reports unreadable rules", async () => {
    const card = await renderCard(templateCreateRow({ also_live: [
      { id: 12, title: "Main", shows_on: ["include/general"], shows_on_problem: null },
      { id: 13, title: "Other", shows_on: null, shows_on_problem: "unreadable" },
    ] }, created));
    expect(rowValue(card, "Also live")).toHaveTextContent('“Main” (#12) shows on the entire site.');
    expect(rowValue(card, "Also live")).toHaveTextContent('“Other” (#13)');
    expect(rowValue(card, "Also live")).toHaveTextContent("WPMgr could not read where it shows.");
    expect(within(card).getAllByText("When you publish this one, set those to draft so the two don't compete.")).toHaveLength(1);
  });
});

describe("template edit and page boundaries", () => {
  it.each(["create", "edit"])("approves a complete pending %s template through the request hook", async (kind) => {
    const row = kind === "create" ? templateCreateRow() : templateEditRow();
    approve.mockReturnValue(ok({ ...row, state: "approved", decided_at: "2026-10-01T09:58:00Z" }));
    const card = await renderCard(row);
    const button = within(card).getByRole("button", { name: "Approve" });
    expect(button).toBeEnabled();
    expect(within(card).getByRole("button", { name: "Decline" })).toHaveFocus();
    fireEvent.click(button);
    await waitFor(() => expect(approve).toHaveBeenCalledWith({
      path: { siteId: "site-1", requestId: row.id }, body: { presented_digest: row.presented_digest },
    }));
  });
  it.each(["type", "rules", "missing facts", "unknown input key"])("cannot approve a creation with %s that do not match its full preview", async (problem) => {
    const row = templateCreateRow();
    const input = JSON.parse(row.input_json) as Record<string, unknown>;
    if (problem === "type") input.template = "footer";
    if (problem === "rules") input.shows_on = [];
    if (problem === "missing facts") row.page_template = undefined;
    if (problem === "unknown input key") input.extra = "hidden";
    row.input_json = JSON.stringify(input);
    const card = await renderCard(row);
    expect(within(card).getByRole("button", { name: "Approve" })).toBeDisabled();
    expect(within(card).getByRole("button", { name: "Decline" })).toBeEnabled();
  });
  it("cannot approve an edit whose display list disagrees with the AI's input", async () => {
    const row = templateEditRow();
    row.page_edit!.template!.shows_on.after = [];
    const card = await renderCard(row);
    expect(within(card).getByRole("button", { name: "Approve" })).toBeDisabled();
  });
  it("renders an existing place outside the AI's list on an edit without changing it", async () => {
    const card = await renderCard(templateEditRow({
      shows_on: { before: ["include/archive/category/5"], after: ["include/archive/category/5"] },
    }, {}, false));
    expect(within(card).getByRole("button", { name: "Approve" })).toBeEnabled();
    expect(card).not.toHaveTextContent("Where it shows:");
  });
  it("shows unknown before rules when changing places and renders the target title as text", async () => {
    const row = templateEditRow({ shows_on: { before: ["include/archive/category/5"], after: [] } }, applied);
    const title = '<img src=x onerror="alert(1)"><script>alert(2)</script>';
    row.page_edit!.post.from_the_site.title = title;
    const card = await renderCard(row);
    expect(rowValue(card, "Header")).toHaveTextContent(title);
    expect(card).toHaveTextContent("Where it shows: another place (include/archive/category/5) → Nowhere yet. A person chooses where it shows in Elementor.");
    expect(card.querySelector("img, script")).toBeNull();
  });
  it("uses W3 for a published outcome refusal before undo was attempted", async () => {
    const card = await renderCard(templateEditRow({}, {
      state: "failed", outcome: "refused", outcome_code: "refused_published", decided_at: "2026-10-01T09:58:00Z",
    }));
    expect(card).toHaveTextContent("A person published this header in Elementor. WPMgr no longer changes it, and undo is closed.");
    expect(within(card).queryByRole("button", { name: "Undo this change" })).toBeNull();
  });
  it("respects the creation undo window", async () => {
    const card = await renderCard(templateCreateRow({}, {
      ...created, undo_offered: true, undo_available_until: new Date(Date.now() - 86_400_000).toISOString(),
    }));
    expect(within(card).queryByRole("button", { name: "Undo this change" })).toBeNull();
    expect(card).toHaveTextContent("Undo is no longer available. Its period has ended.");
  });
  it("can approve a template with an omitted empty display list", async () => {
    const row = templateCreateRow({ shows_on: [] });
    const input = JSON.parse(row.input_json) as Record<string, unknown>;
    delete input.shows_on;
    row.input_json = JSON.stringify(input);
    const card = await renderCard(row);
    expect(within(card).getByRole("button", { name: "Approve" })).toBeEnabled();
    expect(rowValue(card, "Shows on")).toHaveTextContent("Nowhere yet. A person chooses where it shows in Elementor.");
  });
  it("supports a places-only edit", async () => {
    const row = templateEditRow({}, applied);
    row.input_json = JSON.stringify({ ...JSON.parse(row.input_json), operations: [{ op: "set_shows_on", rules: row.page_edit!.template!.shows_on.after }] });
    row.page_edit!.changes = [{ op: "set_shows_on", label: "Where it shows" }];
    const card = await renderCard(row);
    expect(rowValue(card, "Changes")).toHaveTextContent("1 change");
    expect(card).toHaveTextContent("Where it shows: Entire site → All pages");
  });
  it("an omitted places operation keeps today's undo wording", async () => {
    const card = await renderCard(templateEditRow({}, {}, false));
    expect(card).not.toHaveTextContent("Where it shows:");
    expect(rowValue(card, "Undo")).toHaveTextContent("you can undo this change, newest first");
    expect(card).not.toHaveTextContent("Undo puts back the content and where it shows.");
  });
  it("does not invent a display change for a set to the same places", async () => {
    const card = await renderCard(templateEditRow({ shows_on: { before: ["include/general"], after: ["include/general"] } }, applied));
    expect(card).not.toHaveTextContent("Where it shows:");
    expect(rowValue(card, "Undo")).toHaveTextContent("Undo puts back the content and where it shows.");
  });
  it.each(["create", "edit"])("shows the %s refusal_message as the outcome", async (kind) => {
    const refusal_message = "Elementor Pro stored different places than asked. Nothing was kept.";
    const failed = { state: "failed", outcome: "verify_mismatch", outcome_code: "verify_mismatch", restored: true,
      decided_at: "2026-10-01T09:58:00Z" } satisfies Partial<AbilityRequest>;
    const card = await renderCard(kind === "create" ? templateCreateRow({ refusal_message }, failed) : templateEditRow({ refusal_message }, failed));
    expect(card).toHaveTextContent(refusal_message);
  });
  it.each(["page", "post"] as const)("keeps the %s creation wording", async (post_type) => {
    const card = await renderCard(pageCreateRow({ post_type, input_json: pageInput({ post_type, editor: "builder:elementor", outline: [{ type: "heading", level: 2, text: "Hello" }] }),
      page_builder: elementorFacts(), ...created }));
    expect(within(card).getByRole("heading")).toHaveTextContent(`Create a draft ${post_type} in Elementor · Shop One`);
    expect(card).toHaveTextContent("Draft only. Nobody sees it until a person publishes it.");
    expect(within(card).getByRole("button", { name: /^Undo$/ })).toBeInTheDocument();
    expect(within(card).queryByText("Shows on", { selector: "dt" })).toBeNull();
  });
});
