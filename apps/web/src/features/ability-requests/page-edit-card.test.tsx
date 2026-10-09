import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import type { AbilityRequest } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";
import {
  defaultEditOps,
  editFacts,
  editInput,
  pageEditRow,
  type EditOpInput,
  type PageEditRow,
} from "@/test/ability-request-rows";

import { AiEditingSection } from "./ai-editing-section";

// The approval card for an AI page edit (wpmgr/page-edit), on the site's
// Content tab: rendered through the real router, the real QueryClient and the
// real hooks; only the @wpmgr/api wire boundary is faked. The rows are held to
// the states the database allows (test/ability-request-rows.ts), the card is
// the shape the control plane returns (apps/api/internal/mcp/
// page_edit_precheck.go, PageEditCardFacts: the DTO page_edit in
// abilityrequest/handler.go) and the AI's input is the published schema's
// (apps/agent/tests/fixtures/ability-run/page-edit-schema.json).

const { editingMock, listSiteMock, approveMock } = vi.hoisted(() => ({
  editingMock: vi.fn(),
  listSiteMock: vi.fn(),
  approveMock: vi.fn(),
}));

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));

vi.mock("@wpmgr/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wpmgr/api")>();
  return {
    ...actual,
    getSiteContentEditing: editingMock,
    listSiteAbilityRequests: listSiteMock,
    approveAbilityRequest: approveMock,
  };
});

function ok(data: unknown) {
  return Promise.resolve({ data, error: undefined, response: { status: 200 } });
}

function renderTab(rows: AbilityRequest[], siteUrl: string | null | undefined = "https://example.com") {
  editingMock.mockReturnValue(ok({ site_id: "site-1", enabled: true }));
  listSiteMock.mockReturnValue(ok({ requests: rows, limit: 50, offset: 0 }));
  return renderWithProviders(<AiEditingSection siteId="site-1" siteUrl={siteUrl} canOperate />, {
    withRouter: true,
    initialPath: "/sites/site-1/content",
  });
}

beforeEach(() => {
  editingMock.mockReset();
  listSiteMock.mockReset();
  approveMock.mockReset();
});

async function openCard(): Promise<HTMLElement> {
  return screen.findByRole("article", { name: /Change a draft in Elementor/ });
}

const NOT_SHOWABLE =
  "WPMgr cannot show this request in full, so it cannot be approved here. Decline it and ask the AI again.";

const flat = (el: Element | null | undefined) => (el?.textContent ?? "").replace(/\s+/g, " ").trim();

/** The rows of a definition list as [label, text] pairs. */
function rows(dl: HTMLElement): Array<[string, string]> {
  return Array.from(dl.querySelectorAll("dt")).map((dt) => [flat(dt), flat(dt.nextElementSibling)]);
}

/** The title of each change, in order. */
function titles(card: HTMLElement): string[] {
  return within(card)
    .getAllByTestId("change-title")
    .map((t) => flat(t));
}

/** The AI's input with its second operation (the insert) replaced, and the card to match. */
function withOps(ops: EditOpInput[], facts: PageEditRow, over: Partial<AbilityRequest> = {}): AbilityRequest {
  return pageEditRow({ ops, page_edit: facts, ...over });
}

// --- the pending card ---------------------------------------------------------

describe("a pending page edit", () => {
  it("names the page, the site, who asked and how many changes, each under its tag", async () => {
    renderTab([pageEditRow()]);
    const card = await openCard();
    expect(within(card).getByRole("heading", { level: 3 })).toHaveTextContent("Change a draft in Elementor · Shop One");
    expect(card).toHaveTextContent(/From the site\s*example\.com/);
    const page = within(card).getByTestId("page-line");
    expect(flat(page.firstElementChild)).toBe('#418 "Spring sale" · Draft · Elementor 3.35.9');
    expect(within(page).getByText("From the site")).toBeInTheDocument();
    const changes = within(card).getByText("Changes", { selector: "dt" }).nextElementSibling as HTMLElement;
    expect(flat(changes.firstElementChild)).toBe("4 changes");
    expect(within(changes).getByText("Chosen by the AI")).toBeInTheDocument();
    expect(flat(within(card).getByText("Asked by", { selector: "dt" }).nextElementSibling)).toBe("Claude");
  });

  it("says 1 change for one change", async () => {
    const ops = [defaultEditOps()[0]!];
    const facts = editFacts({ changes: [editFacts().changes[0]!] });
    renderTab([withOps(ops, facts)]);
    const card = await openCard();
    const changes = within(card).getByText("Changes", { selector: "dt" }).nextElementSibling as HTMLElement;
    expect(flat(changes.firstElementChild)).toBe("1 change");
  });

  it("lists every change in the order asked, in our words", async () => {
    renderTab([pageEditRow()]);
    const card = await openCard();
    expect(titles(card)).toEqual([
      "Change text · Heading 2",
      'Add after "Free delivery"',
      'Replace · Image · caption "Team photo" with',
      'Move · Paragraph "FAQ: returns in 30 days" → after "Our products"',
    ]);
  });

  it("shows what a text change replaces and what it asks for", async () => {
    renderTab([pageEditRow()]);
    const card = await openCard();
    expect(flat(within(card).getByTestId("was"))).toBe("Spring sale");
    expect(flat(within(card).getByTestId("now"))).toBe("Summer sale");
  });

  it.each<[string, "text" | "url" | "caption", string]>([
    ["text", "text", "Change text · Paragraph"],
    ["a link", "url", "Change link · Button"],
    ["a caption", "caption", "Change caption · Image"],
  ])("names the field of a change to %s", async (_n, field, title) => {
    const kind = field === "text" ? "paragraph" : field === "url" ? "buttons" : "image";
    const ops: EditOpInput[] = [{ op: "set_text", ref: "n1", field, text: "New value" }];
    const facts = editFacts({
      changes: [{ op: "set_text", ref: "n1", kind, field, after: "New value", from_the_site: { before: { [field]: "Old value" } } }],
      after_outline: { node_count: 1, truncated: false, nodes: [{ ref: "n1", parent: "root", kind, editable: [field], from_the_site: { [field]: "New value" } }] },
    });
    renderTab([withOps(ops, facts)]);
    const card = await openCard();
    expect(titles(card)).toEqual([title]);
    expect(flat(within(card).getByTestId("was"))).toBe("Old value");
    expect(flat(within(card).getByTestId("now"))).toBe("New value");
  });

  it("shows the nodes an insert and a replace put on the page, every value, with the replace's styling note", async () => {
    renderTab([pageEditRow()]);
    const card = await openCard();
    const outlines = within(card).getAllByTestId("change-outline");
    expect(outlines).toHaveLength(2);
    expect(flat(outlines[0])).toBe("Paragraph Open every day.");
    expect(flat(outlines[1])).toBe("Heading 3 Our team");
    const lines = within(card).getAllByTestId("change-line");
    expect(within(lines[2]!).getByText("The replaced block's styling is not kept.")).toBeInTheDocument();
    expect(within(lines[1]!).queryByText("The replaced block's styling is not kept.")).toBeNull();
    // Each outline is a region a keyboard can scroll.
    expect(outlines[0]).toHaveAttribute("tabindex", "0");
  });

  it("lists the page after the change and marks what changed, what is new and what moved", async () => {
    renderTab([pageEditRow()]);
    const card = await openCard();
    const after = within(card).getByTestId("page-after");
    expect(after).toHaveAttribute("role", "region");
    const markOf = (ref: string) => after.querySelector(`[data-ref="${ref}"]`)?.querySelector("[data-mark]")?.getAttribute("data-mark") ?? null;
    expect(markOf("h_sale")).toBe("changed");
    expect(markOf("p_open")).toBe("new");
    expect(markOf("h_team")).toBe("new");
    expect(markOf("p_faq")).toBe("moved");
    expect(markOf("p_free")).toBeNull();
    expect(markOf("h_prod")).toBeNull();
    for (const text of ["Summer sale", "Free delivery", "Open every day.", "Our team", "Our products", "FAQ: returns in 30 days"]) {
      expect(after).toHaveTextContent(text);
    }
    expect(after).toHaveTextContent("Heading 2");
    expect(after).toHaveTextContent("Heading 3");
    // A mark sits on its own node, not on a neighbour.
    const sale = after.querySelector('[data-ref="h_sale"]');
    expect(sale?.querySelectorAll("[data-mark]")).toHaveLength(1);
  });

  it("shows an element WPMgr does not edit under WPMgr's own label", async () => {
    renderTab([pageEditRow()]);
    const card = await openCard();
    const form = within(card).getByTestId("page-after").querySelector('[data-ref="f_form"]');
    expect(flat(form)).toBe("Elementor element WPMgr does not edit (WPMgr does not edit this)");
  });

  it("states what is kept, what WPMgr checks, the effect, the undo and when it was checked", async () => {
    renderTab([pageEditRow()]);
    const card = await openCard();
    const until = new Date("2026-10-15T09:55:00Z").toLocaleDateString(undefined, { month: "short", day: "numeric" });
    expect(rows(within(card).getByTestId("edit-rows"))).toEqual([
      ["Keeps", "Styling a person set on changed text is kept."],
      [
        "Checks",
        "WPMgr saves a copy of the page first and checks afterwards that only this page changed and that Elementor saved exactly these changes. If not, it puts the page back.",
      ],
      ["Effect", "Draft only. Nobody sees it until a person publishes it."],
      [
        "Undo",
        `Until ${until} you can undo this change, newest first. Later changes you approve, like a featured image, stay.`,
      ],
      ["Checked", "Page unchanged since the AI read it (09:52 UTC)"],
    ]);
  });

  it("says nothing about kept styling when no text is changed", async () => {
    const ops: EditOpInput[] = [{ op: "remove", ref: "p_free" }];
    const facts = editFacts({
      changes: [{ op: "remove", ref: "p_free", kind: "paragraph", from_the_site: { before: { text: "Free delivery" } } }],
      after_outline: { node_count: 1, truncated: false, nodes: [{ ref: "h_prod", parent: "root", kind: "heading", level: 2, editable: ["text"], from_the_site: { text: "Our products" } }] },
    });
    renderTab([withOps(ops, facts)]);
    const card = await openCard();
    const shown = rows(within(card).getByTestId("edit-rows")).map(([label]) => label);
    expect(shown).toEqual(["Checks", "Effect", "Undo", "Checked"]);
    expect(card).not.toHaveTextContent("Styling a person set");
  });

  it("can be approved, with Decline focused, and posts the row's own presented_digest", async () => {
    approveMock.mockReturnValue(ok(pageEditRow({ state: "approved", decided_at: "2026-10-01T09:58:00Z" })));
    renderTab([pageEditRow({ presented_digest: "d".repeat(64) })]);
    const card = await openCard();
    expect(within(card).getByRole("button", { name: "Decline" })).toHaveFocus();
    const approve = within(card).getByRole("button", { name: "Approve" });
    expect(approve).toBeEnabled();
    fireEvent.click(approve);
    await waitFor(() =>
      expect(approveMock).toHaveBeenCalledWith({
        path: { siteId: "site-1", requestId: "pe-1" },
        body: { presented_digest: "d".repeat(64) },
      }),
    );
  });

  it("shows no Undo and no links while it waits", async () => {
    renderTab([pageEditRow()]);
    const card = await openCard();
    expect(within(card).queryByRole("button", { name: /Undo/ })).toBeNull();
    expect(within(card).queryAllByRole("link")).toHaveLength(0);
  });
});

// --- a request the card cannot show in full -------------------------------------

describe("a page edit the card cannot show in full", () => {
  function expectNotApprovable(card: HTMLElement): void {
    expect(card).toHaveTextContent(NOT_SHOWABLE);
    expect(within(card).queryByTestId("page-edit-preview")).toBeNull();
    expect(within(card).queryByTestId("change-line")).toBeNull();
    expect(within(card).queryByTestId("edit-rows")).toBeNull();
    expect(within(card).getByRole("button", { name: "Approve" })).toBeDisabled();
    expect(within(card).getByRole("button", { name: "Decline" })).toBeEnabled();
  }

  const base = (): PageEditRow => editFacts();
  const withChange = (i: number, change: Record<string, unknown>): PageEditRow => {
    const facts = base();
    return { ...facts, changes: facts.changes.map((c, j) => (j === i ? ({ ...c, ...change }) : c)) };
  };

  // Built straight, bypassing the fixture builder on purpose: none of these is a
  // value the control plane returns today; each is the wire of an API older or
  // newer than this dashboard, or a card that does not match the AI's input.
  it.each<[string, () => AbilityRequest]>([
    ["no page_edit (the card did not read back)", () => ({ ...pageEditRow(), page_edit: null })],
    ["no page_edit member (an older API)", () => ({ ...pageEditRow(), page_edit: undefined })],
    ["an unknown operation on the card", () => pageEditRow({ page_edit: withChange(0, { op: "rotate" }) })],
    [
      "an unknown operation in the AI's input",
      () => pageEditRow({ ops: [{ op: "rotate", ref: "h_sale" }], page_edit: { ...base(), changes: [base().changes[0]!] } }),
    ],
    [
      "a node kind the screen has no name for",
      () => pageEditRow({ page_edit: { ...base(), after_outline: { ...base().after_outline!, nodes: [{ ref: "z1", parent: "root", kind: "carousel" }] } } }),
    ],
    ["a change kind the screen has no name for", () => pageEditRow({ page_edit: withChange(0, { kind: "carousel" }) })],
    ["a text change that asks for other text than the AI's", () => pageEditRow({ page_edit: withChange(0, { after: "Winter sale" }) })],
    ["a change to another field than the AI's", () => pageEditRow({ page_edit: withChange(0, { field: "caption" }) })],
    ["an insert after another node than the AI's", () => pageEditRow({ page_edit: withChange(1, { anchor: { ref: "h_prod", how: "after", kind: "paragraph" } }) })],
    ["an insert before when the AI asked after", () => pageEditRow({ page_edit: withChange(1, { anchor: { ref: "p_free", how: "before", kind: "paragraph" } }) })],
    ["a replace of another node than the AI's", () => pageEditRow({ page_edit: withChange(2, { ref: "img_other" }) })],
    ["a move to another place than the AI's", () => pageEditRow({ page_edit: withChange(3, { anchor: { ref: "h_prod", how: "before", kind: "heading", level: 2 } }) })],
    ["an insert that makes no node", () => pageEditRow({ page_edit: withChange(1, { new_refs: [] }) })],
    ["a card for another post", () => pageEditRow({ page_edit: { ...base(), post: { id: 419, from_the_site: { title: "Other" } } } })],
    ["fewer changes than operations", () => pageEditRow({ page_edit: { ...base(), changes: base().changes.slice(0, 3) } })],
    ["a change the AI did not ask for", () => pageEditRow({ ops: defaultEditOps().slice(0, 3) })],
    ["an extra member on a change", () => pageEditRow({ page_edit: withChange(0, { colour: "red" }) })],
    ["an extra member on the card", () => pageEditRow({ page_edit: { ...base(), note: "x" } as PageEditRow })],
    ["another builder", () => pageEditRow({ page_edit: { ...base(), builder: { id: "bricks", version: "1.9.0", format: "classic" } } })],
    ["a version with markup", () => pageEditRow({ page_edit: { ...base(), builder: { id: "elementor", version: "3.35.9<b>", format: "classic" } } })],
    ["a card of another kind", () => pageEditRow({ page_edit: { ...base(), kind: "rest_write" } as unknown as PageEditRow })],
    ["a page after that is neither shown nor left out", () => pageEditRow({ page_edit: { ...base(), after_outline: undefined } })],
    ["a page after that is both shown and left out", () => pageEditRow({ page_edit: { ...base(), after_outline_omitted: true } })],
    [
      "a node whose parent is not before it",
      () => pageEditRow({ page_edit: { ...base(), after_outline: { ...base().after_outline!, nodes: [{ ref: "a1", parent: "b1", kind: "paragraph" }] } } }),
    ],
    [
      "a node twice",
      () =>
        pageEditRow({
          page_edit: {
            ...base(),
            after_outline: { ...base().after_outline!, nodes: [{ ref: "a1", parent: "root", kind: "paragraph" }, { ref: "a1", parent: "root", kind: "paragraph" }] },
          },
        }),
    ],
    ["an unreadable checked time", () => pageEditRow({ page_edit: { ...base(), checked_at: "yesterday-ish" } })],
    ["input that is not JSON", () => ({ ...pageEditRow(), input_json: "not json" })],
    ["input with a member the schema does not have", () => ({ ...pageEditRow(), input_json: editInput().replace('"operations"', '"extra":1,"operations"') })],
    ["input naming a ref the schema refuses", () => pageEditRow({ ops: [{ op: "remove", ref: "bad ref!" }] })],
    ["input with a number written 418.0", () => ({ ...pageEditRow(), input_json: editInput().replace('"post_id":418', '"post_id":418.0') })],
    [
      "an outline Elementor does not build as asked",
      () =>
        pageEditRow({
          ops: [{ op: "insert", after: "p_free", outline: [{ type: "buttons", buttons: [{ text: "Go", url: "/go", style: "outline" }] }] }],
          page_edit: { ...base(), changes: [base().changes[1]!] },
        }),
    ],
    [
      "an outline with a block the grammar does not have",
      () =>
        pageEditRow({
          ops: [{ op: "insert", after: "p_free", outline: [{ type: "video", url: "https://example.com/v.mp4" }] }],
          page_edit: { ...base(), changes: [base().changes[1]!] },
        }),
    ],
  ])("%s cannot be approved", async (_name, make) => {
    renderTab([make()]);
    expectNotApprovable(await openCard());
  });

  it("keeps the title, the site and who asked, and still names the editor", async () => {
    renderTab([{ ...pageEditRow(), page_edit: null }]);
    const card = await openCard();
    expect(within(card).getByRole("heading", { level: 3 })).toHaveTextContent("Change a draft in Elementor · Shop One");
    expect(card).toHaveTextContent("Claude");
  });
});

// --- the AI's and the site's words are only text ---------------------------------

describe("the AI's and the site's words on a page edit card", () => {
  const hostile = '<img src=x onerror="alert(1)"><script>alert(2)</script><b>bold</b>';

  function hostileRow(): AbilityRequest {
    const ops: EditOpInput[] = [
      { op: "set_text", ref: "h_sale", field: "text", text: "Now <b>bold</b> & <i>it</i>" },
      { op: "insert", after: "p_free", outline: [{ type: "heading", level: 2, text: hostile }, { type: "paragraph", text: hostile }] },
    ];
    const facts = editFacts({
      post: { id: 418, from_the_site: { title: hostile } },
      changes: [
        { op: "set_text", ref: "h_sale", kind: "heading", level: 2, field: "text", after: "Now <b>bold</b> & <i>it</i>", from_the_site: { before: { text: hostile } } },
        { op: "insert", new_refs: ["p_new"], anchor: { ref: "p_free", how: "after", kind: "paragraph" } },
      ],
      after_outline: {
        node_count: 3,
        truncated: false,
        nodes: [
          { ref: "h_sale", parent: "root", kind: "heading", level: 2, editable: ["text"], from_the_site: { text: "Now <b>bold</b> & <i>it</i>" } },
          { ref: "p_free", parent: "root", kind: "paragraph", editable: ["text"], from_the_site: { text: hostile } },
          { ref: "p_new", parent: "root", kind: "paragraph", editable: ["text"], from_the_site: { text: hostile } },
        ],
      },
    });
    return pageEditRow({ ops, page_edit: facts });
  }

  it("are inert text, never markup, in every place they appear", async () => {
    renderTab([hostileRow()]);
    const card = await openCard();
    expect(flat(within(card).getByTestId("was"))).toBe(hostile);
    expect(flat(within(card).getByTestId("now"))).toBe("Now <b>bold</b> & <i>it</i>");
    expect(flat(within(card).getByTestId("page-line"))).toContain(hostile);
    expect(flat(within(card).getAllByTestId("change-title")[1])).toContain(hostile);
    expect(flat(within(card).getAllByTestId("change-outline")[0])).toContain(hostile);
    expect(flat(within(card).getByTestId("page-after"))).toContain(hostile);
    expect(card.querySelector("img")).toBeNull();
    expect(card.querySelector("script")).toBeNull();
    expect(card.querySelector("b")).toBeNull();
    expect(card.querySelector("i")).toBeNull();
    expect(within(card).getByRole("button", { name: "Approve" })).toBeEnabled();
  });

  it("is isolated from the words around it, so odd text direction cannot reorder them", async () => {
    renderTab([hostileRow()]);
    const card = await openCard();
    const was = within(card).getByTestId("was");
    expect(was.tagName).toBe("BDI");
    expect(within(card).getByTestId("now").tagName).toBe("BDI");
    // The anchor text in the title is the site's, in its own isolated run.
    const anchorRun = within(within(card).getAllByTestId("change-title")[1]!).getByText(hostile);
    expect(anchorRun.tagName).toBe("BDI");
  });
});

// --- what the page after the change shows when it cannot show it all ----------------

describe("a page after the change that is too large", () => {
  it("says so in place of the page, and every change above is still shown", async () => {
    const facts = editFacts();
    const { after_outline: _dropped, ...rest } = facts;
    void _dropped;
    renderTab([pageEditRow({ page_edit: { ...rest, after_outline_omitted: true } })]);
    const card = await openCard();
    expect(within(card).queryByTestId("page-after")).toBeNull();
    expect(within(card).getByTestId("after-omitted")).toHaveTextContent(
      "The page is too large to show in full here. Every change above is shown.",
    );
    // The anchor has no text on the card now, so the line names it by WPMgr's word.
    expect(titles(card)[1]).toBe("Add after Paragraph");
    expect(within(card).getAllByTestId("change-outline")).toHaveLength(2);
    expect(within(card).getByRole("button", { name: "Approve" })).toBeEnabled();
  });

  it("says how much of the page is listed when the list was cut", async () => {
    const facts = editFacts();
    renderTab([pageEditRow({ page_edit: { ...facts, after_outline: { ...facts.after_outline!, node_count: 700, truncated: true } } })]);
    const card = await openCard();
    expect(within(card).getByTestId("after-truncated")).toHaveTextContent("Only the first 9 of 700 parts of the page are listed.");
  });

  it("does not claim a cut list when the list is whole", async () => {
    renderTab([pageEditRow()]);
    const card = await openCard();
    expect(within(card).queryByTestId("after-truncated")).toBeNull();
  });
});

// --- images, removals and moves ------------------------------------------------------

describe("an image in what an insert puts on the page", () => {
  const imageOps = (): EditOpInput[] => [
    {
      op: "insert",
      after: "p_free",
      outline: [{ type: "image", attachment_id: 42, alt: "Library alt", caption: "Our van", align: "center" }],
    },
  ];
  const imageFacts = (): PageEditRow =>
    editFacts({
      changes: [{ op: "insert", new_refs: ["i_van"], anchor: { ref: "p_free", how: "after", kind: "paragraph" } }],
      after_outline: {
        node_count: 2,
        truncated: false,
        nodes: [
          { ref: "p_free", parent: "root", kind: "paragraph", editable: ["text"], from_the_site: { text: "Free delivery" } },
          { ref: "i_van", parent: "root", kind: "image", editable: ["caption"], from_the_site: { caption: "Our van" } },
        ],
      },
    });

  it("is named by its media library number, never by a file name, and links to its edit screen", async () => {
    renderTab([pageEditRow({ ops: imageOps(), page_edit: imageFacts() })]);
    const card = await openCard();
    const outline = within(card).getByTestId("change-outline");
    expect(outline).toHaveTextContent("Image · media library item 42");
    expect(outline).toHaveTextContent('Alt text (from the media library): "Library alt"');
    expect(outline).toHaveTextContent('Caption: "Our van"');
    expect(outline).toHaveTextContent("Centred");
    const link = within(outline).getByRole("link", { name: "View image in WordPress (media library item 42)" });
    expect(link.getAttribute("href")).toBe("https://example.com/wp-admin/post.php?post=42&action=edit");
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.getAttribute("rel")).toBe("noopener noreferrer");
    expect(card.querySelector("img")).toBeNull();
    expect(within(card).getByRole("button", { name: "Approve" })).toBeEnabled();
  });

  it("offers no link when the site's address is not a plain web address", async () => {
    renderTab([pageEditRow({ ops: imageOps(), page_edit: imageFacts() })], "javascript:alert(1)");
    const card = await openCard();
    expect(within(card).getByTestId("change-outline")).toHaveTextContent("Image · media library item 42");
    expect(within(card).queryAllByRole("link")).toHaveLength(0);
  });
});

describe("a removed node and a moved node", () => {
  it("names a removed node with no text by WPMgr's word for its kind, with no site tag", async () => {
    const ops: EditOpInput[] = [{ op: "remove", ref: "c_row" }];
    const facts = editFacts({
      changes: [{ op: "remove", ref: "c_row", kind: "columns" }],
      after_outline: { node_count: 1, truncated: false, nodes: [{ ref: "h_prod", parent: "root", kind: "heading", level: 2, editable: ["text"], from_the_site: { text: "Our products" } }] },
    });
    renderTab([withOps(ops, facts)]);
    const card = await openCard();
    expect(titles(card)).toEqual(["Remove · Columns"]);
    const line = within(card).getByTestId("change-line");
    expect(within(line).queryByText("From the site")).toBeNull();
  });

  it("names a removed node with text by its kind and its text, tagged as the site's", async () => {
    const ops: EditOpInput[] = [{ op: "remove", ref: "p_free" }];
    const facts = editFacts({
      changes: [{ op: "remove", ref: "p_free", kind: "paragraph", from_the_site: { before: { text: "Free delivery" } } }],
      after_outline: { node_count: 1, truncated: false, nodes: [{ ref: "h_prod", parent: "root", kind: "heading", level: 2, editable: ["text"], from_the_site: { text: "Our products" } }] },
    });
    renderTab([withOps(ops, facts)]);
    const card = await openCard();
    expect(titles(card)).toEqual(['Remove · Paragraph "Free delivery"']);
    expect(within(within(card).getByTestId("change-line")).getByText("From the site")).toBeInTheDocument();
  });

  it("names the place of an insert into a node, first or last", async () => {
    const ops: EditOpInput[] = [
      { op: "insert", into: "col1", position: "first", outline: [{ type: "paragraph", text: "A" }] },
      { op: "insert", into: "col2", outline: [{ type: "paragraph", text: "B" }] },
    ];
    const facts = editFacts({
      changes: [
        { op: "insert", new_refs: ["a1"], anchor: { ref: "col1", how: "into", position: "first", kind: "column" } },
        { op: "insert", new_refs: ["b1"], anchor: { ref: "col2", how: "into", position: "last", kind: "column" } },
      ],
      after_outline: {
        node_count: 4,
        truncated: false,
        nodes: [
          { ref: "col1", parent: "root", kind: "column", editable: [] },
          { ref: "a1", parent: "col1", kind: "paragraph", editable: ["text"], from_the_site: { text: "A" } },
          { ref: "col2", parent: "root", kind: "column", editable: [] },
          { ref: "b1", parent: "col2", kind: "paragraph", editable: ["text"], from_the_site: { text: "B" } },
        ],
      },
    });
    renderTab([withOps(ops, facts)]);
    const card = await openCard();
    expect(titles(card)).toEqual(["Add at the start of Column", "Add at the end of Column"]);
  });

  it("names an anchor WPMgr does not edit by the label the card carries for it", async () => {
    const ops: EditOpInput[] = [{ op: "insert", before: "f_form", outline: [{ type: "paragraph", text: "Above the form" }] }];
    const facts = editFacts({
      changes: [{ op: "insert", new_refs: ["a1"], anchor: { ref: "f_form", how: "before", kind: "locked", label: "Elementor shortcode" } }],
      after_outline: {
        node_count: 2,
        truncated: false,
        nodes: [
          { ref: "a1", parent: "root", kind: "paragraph", editable: ["text"], from_the_site: { text: "Above the form" } },
          { ref: "f_form", parent: "root", kind: "locked", label: "Elementor shortcode" },
        ],
      },
    });
    renderTab([withOps(ops, facts)]);
    const card = await openCard();
    expect(titles(card)).toEqual(["Add before Elementor shortcode"]);
  });
});
