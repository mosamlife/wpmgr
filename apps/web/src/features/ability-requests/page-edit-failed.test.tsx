import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, within } from "@testing-library/react";
import type { AbilityRequest } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";
import { pageEditRow } from "@/test/ability-request-rows";

import { AiEditingSection } from "./ai-editing-section";

// What the card of an AI page edit says when the edit did not go through, and
// what it says about an undo that did not either (section 5.4 of the BF-E
// design, the owner's acceptance copy). Rendered through the real router, the
// real QueryClient and the real hooks; only the @wpmgr/api wire boundary is
// faked. Every sentence below is written out here in full on purpose: a test
// that imported the model's constant would pass whatever the constant said.
//
// The rows are what the API returns. A failed edit's outcome_code, outcome and
// restored are the agent's refusal as the worker records it
// (apps/api/internal/abilityrequest/worker.go refusedOutcome, withRestoreReport);
// outcome_detail is the closed conflict word from abilityrequest/
// outcome_detail.go (changed_since_read, editor_open, autosave_pending); an
// undo that did not go through is undo_state on the done row
// (refused_conflict, refused_published, failed), set by undo.go.

const { editingMock, listSiteMock } = vi.hoisted(() => ({
  editingMock: vi.fn(),
  listSiteMock: vi.fn(),
}));

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));

vi.mock("@wpmgr/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wpmgr/api")>();
  return { ...actual, getSiteContentEditing: editingMock, listSiteAbilityRequests: listSiteMock };
});

function ok(data: unknown) {
  return Promise.resolve({ data, error: undefined, response: { status: 200 } });
}

function renderTab(rows: AbilityRequest[]) {
  editingMock.mockReturnValue(ok({ site_id: "site-1", enabled: true }));
  listSiteMock.mockReturnValue(ok({ requests: rows, limit: 50, offset: 0 }));
  return renderWithProviders(<AiEditingSection siteId="site-1" siteUrl="https://example.com" canOperate />, {
    withRouter: true,
    initialPath: "/sites/site-1/content",
  });
}

beforeEach(() => {
  editingMock.mockReset();
  listSiteMock.mockReset();
});

const flat = (el: Element | null | undefined) => (el?.textContent ?? "").replace(/\s+/g, " ").trim();

const DECIDED = "2026-10-01T09:58:00Z";

/** A page edit that failed with the agent's refusal `code`. */
function failed(code: string | undefined, over: Partial<AbilityRequest> = {}): AbilityRequest {
  return pageEditRow({ state: "failed", outcome: "refused", outcome_code: code, decided_at: DECIDED, ...over });
}

/** An applied page edit whose undo has this result. */
function undoResult(undoState: string): AbilityRequest {
  return pageEditRow({
    state: "done",
    outcome: "applied",
    decided_at: DECIDED,
    undo_state: undoState,
    undo_available_until: new Date(Date.now() + 10 * 86_400_000).toISOString(),
    undo_offered: false,
  });
}

async function statusOf(row: AbilityRequest): Promise<{ card: HTMLElement; line: HTMLElement }> {
  renderTab([row]);
  const card = await screen.findByRole("article", { name: /Change a draft in Elementor/ });
  return { card, line: within(card).getByTestId("status-line") };
}

// --- the edit was refused or stopped -------------------------------------------

describe("a page edit that was refused before the page was touched", () => {
  const STALE = "The page changed after the AI looked at it. Nothing changed. Ask the AI to read the page again and retry.";

  it.each<[string, AbilityRequest, string]>([
    ["the page changed after the AI read it", failed("conflict", { outcome_detail: "changed_since_read" }), STALE],
    ["the page changed between the card and the dispatch", failed("preview_changed"), STALE],
    [
      "someone has the page open in Elementor",
      failed("conflict", { outcome_detail: "editor_open" }),
      "Someone has this page open in Elementor. Nothing changed.",
    ],
    [
      "someone has unsaved Elementor changes on the draft",
      failed("conflict", { outcome_detail: "autosave_pending" }),
      "Someone has unsaved Elementor changes on this draft. Nothing changed.",
    ],
    [
      "a conflict from an API that does not say which",
      failed("conflict"),
      "The page changed after the AI looked at it, or someone has it open in Elementor. Nothing changed. Ask the AI to read the page again and retry.",
    ],
    [
      "the site could not save a copy of the page first",
      failed("snapshot_failed"),
      "WPMgr could not save a copy of the page first, so it changed nothing.",
    ],
    [
      "the page is too large to copy",
      failed("snapshot_too_large"),
      "WPMgr could not save a copy of the page first: the page is too large to copy safely (over 4 MB). It changed nothing.",
    ],
  ])("says, for %s, that nothing changed", async (_name, row, text) => {
    const { card, line } = await statusOf(row);
    expect(flat(line)).toBe(text);
    expect(line).toHaveAttribute("data-tone", "neutral");
    expect(line).not.toHaveAttribute("role");
    // Nothing was changed, so there is no page to open or preview.
    expect(within(card).queryAllByRole("link")).toHaveLength(0);
    expect(within(card).queryByRole("button", { name: /Undo/ })).toBeNull();
  });

  it.each<[string, AbilityRequest, string]>([
    [
      "an admin-only widget",
      failed("page_has_admin_only_content"),
      "This draft has content only an administrator can save (custom HTML or code). WPMgr's limited account would remove it, so WPMgr won't change this page.",
    ],
    [
      "a widget that is not active",
      failed("page_has_unknown_elements"),
      "This draft uses an Elementor widget that is not active on the site any more. Saving would lose it, so WPMgr won't change this page.",
    ],
    [
      "a layout Elementor would save differently",
      failed("builder_would_change_layout"),
      "Elementor would have saved this layout differently from what you were shown, so WPMgr stopped. Nothing changed.",
    ],
    [
      "a site that changes content on save",
      failed("sanitiser_changed_new_content"),
      "This site changes page content when saving it, often because of a plugin, in a way WPMgr can't approve. Ask the AI to simplify the page.",
    ],
    [
      "a page that is no longer an eligible draft",
      failed("target_not_eligible"),
      "This page is no longer a draft that WPMgr created for the AI, so WPMgr changed nothing.",
    ],
    [
      "a request the site refused for a part of the page that is gone",
      failed("node_not_found"),
      "The change was not made: it named a part of the page that is not there. Nothing was changed.",
    ],
    ["a refusal with a code this page has no words for", failed("something_new"), "The site refused to change the page. Nothing was changed."],
  ])("says, for %s, why it stopped", async (_name, row, text) => {
    const { line } = await statusOf(row);
    expect(flat(line)).toBe(text);
  });

  it("says something went wrong, and where to look, for a failure with no code", async () => {
    const { line } = await statusOf(failed(undefined, { outcome: "failed" }));
    expect(flat(line)).toBe(
      "The change was not made because something went wrong on the site. Open the page in Elementor and check it.",
    );
  });
});

// --- the edit was made and the page was put back --------------------------------

describe("a page edit that Elementor did not save as approved", () => {
  it.each<[string, AbilityRequest]>([
    ["the page read back differently", failed("verify_mismatch", { outcome: "verify_mismatch", restored: true })],
    ["Elementor refused the save", failed("builder_save_refused", { restored: true })],
    ["Elementor crashed while saving", failed("builder_crashed", { restored: true })],
  ])("says WPMgr put the page back, in amber, when %s", async (_name, row) => {
    const { card, line } = await statusOf(row);
    expect(flat(line)).toBe(
      "Elementor did not save the change exactly as approved, so WPMgr put the page back as it was.",
    );
    expect(line).toHaveAttribute("data-tone", "amber");
    expect(line).not.toHaveAttribute("role", "alert");
    // The page exists, so the card still links to it.
    expect(within(card).getByRole("link", { name: "Open in Elementor" })).toBeInTheDocument();
  });

  it("does not claim the page was put back when the site did not say so", async () => {
    const { line } = await statusOf(failed("verify_mismatch", { outcome: "verify_mismatch" }));
    expect(flat(line)).toBe(
      "Elementor did not save the change exactly as approved. Open the page in Elementor and check it.",
    );
    expect(line).toHaveAttribute("data-tone", "amber");
  });

  it("says Elementor did not save the change when nothing was saved and nothing needed putting back", async () => {
    const { line } = await statusOf(failed("builder_crashed"));
    expect(flat(line)).toBe("Elementor did not save the change, so nothing changed.");
  });
});

describe("a page edit that changed something outside the page", () => {
  it("says WPMgr put the page back, in red, as an alert", async () => {
    const { card, line } = await statusOf(failed("side_effect_detected", { restored: true }));
    // The design asks for a short label of what changed in this sentence; the
    // API does not carry one yet, so the sentence stands without it.
    expect(flat(line)).toBe(
      "Something outside this page changed while Elementor saved it. WPMgr put the page back. WPMgr cannot undo changes another plugin made.",
    );
    expect(line).toHaveAttribute("data-tone", "red");
    expect(line).toHaveAttribute("role", "alert");
    expect(within(card).getByRole("link", { name: "Open in Elementor" })).toBeInTheDocument();
  });

  it("asks the person to check the page when it was not put back", async () => {
    const { line } = await statusOf(failed("side_effect_detected"));
    expect(flat(line)).toBe(
      "Something outside this page changed while Elementor saved it. Open the page in Elementor and check it. WPMgr cannot undo changes another plugin made.",
    );
    expect(line).toHaveAttribute("data-tone", "red");
  });
});

describe("a page edit WPMgr could not put back exactly", () => {
  const NOT_PUT_BACK =
    "WPMgr could not put the page back exactly. Open it in Elementor and check it; its revisions are in WordPress.";

  it.each<[string, AbilityRequest]>([
    ["the site says the put-back did not match", failed("restore_mismatch", { restored: false })],
    ["the site says the put-back did not match, and sent no report", failed("restore_mismatch")],
    ["the page read back differently and the put-back failed", failed("verify_mismatch", { outcome: "verify_mismatch", restored: false })],
    ["Elementor crashed and the put-back failed", failed("builder_crashed", { restored: false })],
    ["a change outside the page was found and the put-back failed", failed("side_effect_detected", { restored: false })],
  ])("needs attention, in red, as an alert, when %s", async (_name, row) => {
    const { card, line } = await statusOf(row);
    expect(flat(line)).toBe(NOT_PUT_BACK);
    expect(line).toHaveAttribute("data-tone", "red");
    expect(line).toHaveAttribute("role", "alert");
    expect(within(card).getByRole("link", { name: "Open in Elementor" })).toBeInTheDocument();
  });
});

// --- the undo of an applied edit did not go through ------------------------------

describe("an undo of an applied page edit that did not go through", () => {
  it("says the page changed after the change, when the site refused the undo for that", async () => {
    const { card, line } = await statusOf(undoResult("refused_conflict"));
    expect(flat(line)).toBe(
      "WPMgr won't undo: the page changed after this change (by a person, or by a later change to undo first).",
    );
    expect(line).toHaveAttribute("data-tone", "neutral");
    // A refusal that stands is not offered again.
    expect(within(card).queryByRole("button", { name: /Undo/ })).toBeNull();
  });

  it("says the page is published now, when the site refused the undo for that", async () => {
    const { card, line } = await statusOf(undoResult("refused_published"));
    expect(flat(line)).toBe("This page is published now. Change it in Elementor.");
    expect(within(card).queryByRole("button", { name: /Undo/ })).toBeNull();
  });

  it("needs attention, in red, as an alert, when the undo failed", async () => {
    const { card, line } = await statusOf(undoResult("failed"));
    expect(flat(line)).toBe(
      "WPMgr could not undo this change. Open the page in Elementor and check it; its revisions are in WordPress.",
    );
    expect(line).toHaveAttribute("data-tone", "red");
    expect(line).toHaveAttribute("role", "alert");
    expect(within(card).queryByRole("button", { name: /Undo/ })).toBeNull();
  });

  it("says WPMgr is putting the page back while the undo runs", async () => {
    const { line } = await statusOf(undoResult("in_progress"));
    expect(flat(line)).toBe("WPMgr is putting the page back.");
  });

  it("says the change is undone and the page is back as it was", async () => {
    const { card, line } = await statusOf(undoResult("undone"));
    expect(flat(line)).toBe("Change undone. The page is back as it was before this change.");
    expect(within(card).queryByRole("button", { name: /Undo/ })).toBeNull();
  });
});
