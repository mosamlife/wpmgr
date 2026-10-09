import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import type { AbilityRequest } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";
import { elementorFacts, mediaFact, pageCreateRow, pageInput, type PageBuilderRow } from "@/test/ability-request-rows";

import { AiEditingSection } from "./ai-editing-section";

// The approval card for a page Elementor builds (the create card), on the
// site's Content tab: rendered through the real router, the real QueryClient
// and the real hooks; only the @wpmgr/api wire boundary is faked. page_builder
// is what apps/api/internal/abilityrequest/handler.go returns
// (RequestDTO.PageBuilder, read by mcp/page_create_input.go
// readPageCardBuilder), and the outline is the agent's own Elementor golden
// inputs (apps/agent/tests/fixtures/ability-run/elementor-classic-*.json).

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

// --- fixtures ---------------------------------------------------------------

const GOLDEN = join(process.cwd(), "..", "agent", "tests", "fixtures", "ability-run", "elementor-classic-containers.json");

/** Two of the agent's golden Elementor outlines as one page: every block type, nested. */
function goldenOutline(): unknown[] {
  const file = JSON.parse(readFileSync(GOLDEN, "utf8")) as { cases: Array<{ name: string; input: unknown[] }> };
  const pick = (name: string) => {
    const found = file.cases.find((c) => c.name === name);
    if (!found) throw new Error(`the ${name} case is missing from the agent's Elementor goldens`);
    return found.input;
  };
  return [...pick("group-with-columns-and-widths"), ...pick("mixed-top-level")];
}

const elementorInput = (over: { post_type?: "page" | "post"; elementor_format?: "site_default" | "classic" } = {}) =>
  pageInput({ editor: "builder:elementor", outline: goldenOutline(), ...over });

/** A pending Elementor request as the queue returns it. */
function elementorRow(page_builder: PageBuilderRow | null, over: Partial<AbilityRequest> = {}): AbilityRequest {
  return pageCreateRow({ input_json: elementorInput(), media: [mediaFact(7, { filename: "team.jpg" })], page_builder, ...over });
}

const decided = { decided_at: "2026-10-01T09:58:00Z" };

async function openCard(): Promise<HTMLElement> {
  return screen.findByRole("article", { name: /Create a draft/ });
}

/** The rows of a definition list as [label, text] pairs, each line of a value joined by one space. */
function rows(dl: HTMLElement): Array<[string, string]> {
  return Array.from(dl.querySelectorAll("dt")).map((dt) => {
    const dd = dt.nextElementSibling as HTMLElement;
    const parts = Array.from(dd.querySelectorAll("p"));
    const text = parts.length > 0 ? parts.map((p) => p.textContent ?? "").join(" ") : (dd.textContent ?? "");
    return [dt.textContent ?? "", text];
  });
}

/** The value beside a label in the card's first list. */
function rowText(card: HTMLElement, label: string): string {
  const dt = within(card).getAllByText(label, { selector: "dt" })[0];
  return dt?.nextElementSibling?.textContent ?? "";
}

const ELEMENTOR_ROWS: Array<[string, string]> = [
  [
    "Elementor",
    "Built from Elementor's own containers and widgets, styled by this site's Elementor settings. WPMgr does not change Elementor's site-wide styles, templates, header or footer.",
  ],
  [
    "Checks",
    "WPMgr checks afterwards that Elementor saved exactly this layout and that nothing outside the page changed. If not, it moves the draft to the trash.",
  ],
  ["Also happens", "Elementor rebuilds this page's style file the first time it is viewed."],
  ["Effect", "Draft only. Nobody sees it until a person publishes it."],
  ["Undo", "Undo moves the draft to the trash."],
];

const NOT_SHOWABLE = "WPMgr cannot show this request in full, so it cannot be approved here. Decline it and ask the AI again.";
const NOTHING_PUBLISHED = "Nothing is published. Undo moves the draft to the trash.";

// --- the create card ----------------------------------------------------------

describe("a page Elementor builds with classic widgets in containers", () => {
  it("shows the title, the editor the site reported, the layout, who asked and the whole outline", async () => {
    renderTab([elementorRow(elementorFacts())]);
    const card = await openCard();
    expect(within(card).getByRole("heading", { level: 3 })).toHaveTextContent("Create a draft page in Elementor · Shop One");
    const editor = within(card).getByTestId("editor-line");
    expect(within(editor).getByText("Elementor 3.35.9 · classic widgets, containers")).toBeInTheDocument();
    expect(within(editor).getByText("From the site")).toBeInTheDocument();
    expect(within(card).getByTestId("layout-summary")).toHaveTextContent(
      "1 section · 2 column blocks · 1 image · 2 buttons · 2 tables · 1 quote",
    );
    expect(rowText(card, "Asked by")).toBe("Claude");
    const outline = within(card).getByTestId("ability-outline");
    for (const text of ["Plans", "Prices include VAT.", "Welcome", "team.jpg", "Columns · 3 · equal widths"]) {
      expect(outline).toHaveTextContent(text);
    }
  });

  it("states what Elementor builds, what WPMgr checks, what else happens, the effect and the undo", async () => {
    renderTab([elementorRow(elementorFacts())]);
    const card = await openCard();
    expect(rows(within(card).getByTestId("builder-rows"))).toEqual(ELEMENTOR_ROWS);
    expect(card).not.toHaveTextContent(NOTHING_PUBLISHED);
    expect(card).not.toHaveTextContent(NOT_SHOWABLE);
  });

  it("can be approved, with Decline focused, and posts the row's own presented_digest", async () => {
    approveMock.mockReturnValue(ok(elementorRow(elementorFacts(), { state: "approved", ...decided })));
    renderTab([elementorRow(elementorFacts(), { presented_digest: "e".repeat(64) })]);
    const card = await openCard();
    expect(within(card).getByRole("button", { name: "Decline" })).toHaveFocus();
    const approve = within(card).getByRole("button", { name: "Approve" });
    expect(approve).toBeEnabled();
    fireEvent.click(approve);
    await waitFor(() =>
      expect(approveMock).toHaveBeenCalledWith({
        path: { siteId: "site-1", requestId: "pc-1" },
        body: { presented_digest: "e".repeat(64) },
      }),
    );
  });

  it.each(["site_default", "classic"] as const)("is shown when the AI asked for elementor_format %s", async (format) => {
    renderTab([elementorRow(elementorFacts(), { input_json: elementorInput({ elementor_format: format }) })]);
    const card = await openCard();
    expect(within(card).getByRole("button", { name: "Approve" })).toBeEnabled();
  });
});

describe("a page Elementor builds with classic widgets in sections", () => {
  it("says sections in the editor line and in what Elementor builds", async () => {
    renderTab([elementorRow(elementorFacts({ layout: "sections", version: "3.20.4" }))]);
    const card = await openCard();
    expect(within(card).getByTestId("editor-line")).toHaveTextContent("Elementor 3.20.4 · classic widgets, sections");
    const shown = rows(within(card).getByTestId("builder-rows"));
    expect(shown[0]).toEqual([
      "Elementor",
      "Built from Elementor's own sections, columns and widgets, styled by this site's Elementor settings. WPMgr does not change Elementor's site-wide styles, templates, header or footer.",
    ]);
    expect(shown.slice(1)).toEqual(ELEMENTOR_ROWS.slice(1));
    expect(within(card).getByRole("button", { name: "Approve" })).toBeEnabled();
  });
});

describe("a page Elementor builds in its Atomic editor", () => {
  it("names the Atomic editor and keeps every other row", async () => {
    // Built straight, bypassing the fixture builder on purpose: the control
    // plane's reader stores format classic only today, so this is the wire
    // once Atomic pages ship (elementor_format "atomic", ability_builder.go).
    const row = { ...elementorRow(elementorFacts()), page_builder: elementorFacts({ format: "atomic", version: "4.3.4" }) };
    renderTab([row]);
    const card = await openCard();
    expect(within(card).getByTestId("editor-line")).toHaveTextContent("Elementor 4.3.4 · Atomic editor");
    expect(within(card).getByTestId("editor-line")).not.toHaveTextContent("classic");
    expect(rows(within(card).getByTestId("builder-rows"))).toEqual(ELEMENTOR_ROWS);
    expect(within(card).getByRole("button", { name: "Approve" })).toBeEnabled();
  });
});

// --- the failure state ----------------------------------------------------------

describe("an Elementor request the card cannot show in full", () => {
  function expectNotApprovable(card: HTMLElement): void {
    expect(card).toHaveTextContent(NOT_SHOWABLE);
    expect(card).toHaveTextContent(NOTHING_PUBLISHED);
    expect(within(card).queryByTestId("ability-outline")).toBeNull();
    expect(within(card).queryByTestId("layout-summary")).toBeNull();
    expect(within(card).queryByTestId("builder-rows")).toBeNull();
    expect(within(card).getByRole("button", { name: "Approve" })).toBeDisabled();
    expect(within(card).getByRole("button", { name: "Decline" })).toBeEnabled();
  }

  it("page_builder null (the stored facts did not read back) cannot be approved, and names Elementor", async () => {
    renderTab([elementorRow(null)]);
    const card = await openCard();
    expectNotApprovable(card);
    expect(within(card).getByTestId("editor-line")).toHaveTextContent(/^Elementor$/);
    expect(card).not.toHaveTextContent("Editor not recorded");
  });

  // Built straight, bypassing the fixture builder on purpose: none of these is
  // a value the control plane returns today; each is the wire of an API older
  // or newer than this dashboard.
  it.each<[string, unknown]>([
    ["no page_builder member (an older API)", undefined],
    ["an unknown format", { ...elementorFacts(), format: "flexbox" }],
    ["an unknown layout", { ...elementorFacts(), layout: "grid" }],
    ["another builder", { ...elementorFacts(), builder: "bricks" }],
    ["a version with markup", { ...elementorFacts(), version: "3.35.9<b>" }],
    ["an extra member", { ...elementorFacts(), kit: "1" }],
    ["a missing member", { builder: "elementor", format: "classic", layout: "containers" }],
  ])("%s cannot be approved", async (_name, page_builder) => {
    const row = { ...elementorRow(elementorFacts()), page_builder } as AbilityRequest;
    renderTab([row]);
    expectNotApprovable(await openCard());
  });

  it("an outline Elementor does not build as asked cannot be approved", async () => {
    // The control plane refuses an outline button for Elementor before any
    // card exists (ability_builder.go, elementorNodeProblem); the card never
    // shows one as approvable.
    const input_json = pageInput({
      editor: "builder:elementor",
      outline: [{ type: "buttons", buttons: [{ text: "Go", url: "/go", style: "outline" }] }],
    });
    renderTab([pageCreateRow({ input_json, page_builder: elementorFacts() })]);
    expectNotApprovable(await openCard());
  });
});

// --- a block-editor page is unchanged ----------------------------------------------

describe("a block-editor page", () => {
  const blocksInput = pageInput({ outline: [{ type: "heading", level: 2, text: "Hello" }, { type: "separator" }] });

  it("keeps its title, editor, effect line and Approve", async () => {
    renderTab([pageCreateRow({ input_json: blocksInput })]);
    const card = await openCard();
    expect(within(card).getByRole("heading", { level: 3 })).toHaveTextContent("Create a draft page · Shop One");
    expect(within(card).getByTestId("editor-line")).toHaveTextContent(/^WordPress block editor$/);
    expect(card).not.toHaveTextContent("Elementor");
    expect(card).toHaveTextContent(NOTHING_PUBLISHED);
    expect(within(card).queryByTestId("builder-rows")).toBeNull();
    expect(within(card).getByTestId("layout-summary")).toHaveTextContent("1 separator");
    expect(within(card).getByRole("button", { name: "Approve" })).toBeEnabled();
  });

  it("a block-editor request that carries builder facts cannot be approved", async () => {
    // Built straight: the control plane never pairs a WordPress editor with a
    // page builder, so a row that does is not shown in part.
    const row = { ...pageCreateRow({ input_json: blocksInput }), page_builder: elementorFacts() };
    renderTab([row]);
    const card = await openCard();
    expect(card).toHaveTextContent(NOT_SHOWABLE);
    expect(within(card).getByRole("button", { name: "Approve" })).toBeDisabled();
  });

  it("once created, links to the WordPress editor as before", async () => {
    renderTab([
      pageCreateRow({ input_json: blocksInput, state: "done", outcome: "created", created_post_id: 7, ...decided }),
    ]);
    const card = await openCard();
    expect(card).toHaveTextContent("Draft created.");
    expect(card).not.toHaveTextContent("Draft created in Elementor.");
    const links = within(card).getAllByRole("link");
    expect(links.map((a) => [a.textContent, a.getAttribute("href")])).toEqual([
      ["Edit the draft in WordPress", "https://example.com/wp-admin/post.php?post=7&action=edit"],
    ]);
  });
});

// --- the done state ------------------------------------------------------------------

describe("a draft Elementor created", () => {
  const done = (over: Partial<AbilityRequest> = {}) =>
    elementorRow(elementorFacts(), { state: "done", outcome: "created", created_post_id: 418, ...decided, ...over });

  /** The status links, by name, with their addresses. */
  function linksOf(card: HTMLElement): Array<[string | null, string | null]> {
    return within(card)
      .queryAllByRole("link")
      .filter((a) => !(a.textContent ?? "").startsWith("View image in WordPress"))
      .map((a) => [a.textContent, a.getAttribute("href")]);
  }

  it("says so and links to Open in Elementor and Preview, in a new tab", async () => {
    renderTab([done()]);
    const card = await openCard();
    expect(card).toHaveTextContent("Draft created in Elementor.");
    expect(linksOf(card)).toEqual([
      ["Open in Elementor", "https://example.com/wp-admin/post.php?post=418&action=elementor"],
      ["Preview", "https://example.com/?page_id=418&preview=true"],
    ]);
    for (const name of ["Open in Elementor", "Preview"]) {
      const a = within(card).getByRole("link", { name });
      expect(a.getAttribute("target")).toBe("_blank");
      expect(a.getAttribute("rel")).toBe("noopener noreferrer");
    }
    expect(within(card).queryByRole("link", { name: "Edit the draft in WordPress" })).toBeNull();
  });

  it("previews a post by its post id", async () => {
    renderTab([done({ input_json: elementorInput({ post_type: "post" }), post_type: "post" })]);
    const card = await openCard();
    expect(card).toHaveTextContent("Create a draft post in Elementor · Shop One");
    expect(within(card).getByRole("link", { name: "Preview" }).getAttribute("href")).toBe(
      "https://example.com/?p=418&preview=true",
    );
  });

  it("follows a WordPress that lives in a sub-directory", async () => {
    renderTab([done()], "https://example.com/blog");
    const card = await openCard();
    expect(linksOf(card)).toEqual([
      ["Open in Elementor", "https://example.com/blog/wp-admin/post.php?post=418&action=elementor"],
      ["Preview", "https://example.com/blog/?page_id=418&preview=true"],
    ]);
  });

  it("is built from the site's address and an integer only, never from the AI's words", async () => {
    const hostile = 'x"><a href="https://evil.example">pwn</a>';
    const input_json = pageInput({ editor: "builder:elementor", title: hostile, outline: [{ type: "paragraph", text: hostile }] });
    renderTab([done({ input_json, page_media: null })]);
    const card = await openCard();
    expect(card).toHaveTextContent(hostile);
    for (const [, href] of linksOf(card)) {
      expect(href).toMatch(/^https:\/\/example\.com\/(wp-admin\/post\.php\?post=418&action=elementor|\?page_id=418&preview=true)$/);
    }
    expect(card.querySelectorAll('a[href*="evil"]')).toHaveLength(0);
  });

  it.each([
    ["no address", undefined],
    ["a javascript address", "javascript:alert(1)"],
    ["an address that is not a URL", "not a url"],
  ])("offers no link with %s", async (_name, siteUrl) => {
    renderTab([done()], siteUrl);
    const card = await openCard();
    expect(card).toHaveTextContent("Draft created in Elementor.");
    expect(linksOf(card)).toEqual([]);
  });
});

// --- the AI's and the site's words are only text ---------------------------------

describe("the AI's words on an Elementor card", () => {
  it("are inert text, never markup", async () => {
    const hostile = '<img src=x onerror="alert(1)"><script>alert(2)</script>';
    const input_json = pageInput({
      editor: "builder:elementor",
      title: hostile,
      outline: [{ type: "heading", level: 2, text: hostile }, { type: "paragraph", text: hostile }],
    });
    renderTab([pageCreateRow({ input_json, page_builder: elementorFacts() })]);
    const card = await openCard();
    expect(within(card).getByTestId("ability-outline")).toHaveTextContent(hostile);
    expect(card.querySelector("img")).toBeNull();
    expect(card.querySelector("script")).toBeNull();
    expect(within(card).getByRole("button", { name: "Approve" })).toBeEnabled();
  });
});
