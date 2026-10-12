import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import type { AbilityRequest } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";
import {
  elementorFacts,
  imageIdsOf,
  mediaFact,
  pageCreateRow,
  pageInput,
  type PageBuilderRow,
} from "@/test/ability-request-rows";

import { AiEditingSection } from "./ai-editing-section";
import { PATTERN_ITEM_CAPS } from "./outline-model";

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

// Two images as the agent's goldens place them (ids 5 and 7 in
// elementor-classic-containers.json). The agent's precheck refuses an alt that
// differs from the image's alt in the media library (the mapper's image(),
// code image_alt_from_library; the control plane carries the refusal to the AI
// in mcp/ability_write.go, precheckRefusalHints), so no card exists for one:
// on a card that can be approved, the outline's alt is the library's.
const IMAGES = [
  { type: "image", attachment_id: 5, alt: "Library alt text", caption: "Our van & crew", align: "center" },
  { type: "image", attachment_id: 7, alt: "", align: "none" },
];
const IMAGE_MEDIA = [mediaFact(5, { filename: "van.png", mime: "image/png" }), mediaFact(7, { filename: "team.jpg" })];

/** The alt line of each image row of the outline, in document order. */
function altLines(card: HTMLElement): string[] {
  const outline = within(card).getByTestId("ability-outline");
  return Array.from(outline.querySelectorAll("p"), (p) => p.textContent ?? "").filter((t) => /^(No )?alt text/i.test(t));
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

// --- an image's alt text ------------------------------------------------------------

describe("the images on a page Elementor builds", () => {
  const imagesRow = (outline: unknown[] = IMAGES, media = IMAGE_MEDIA) =>
    pageCreateRow({ input_json: pageInput({ editor: "builder:elementor", outline }), media, page_builder: elementorFacts() });

  it("say the alt text is the media library's, and show an empty one as none", async () => {
    renderTab([imagesRow()]);
    const card = await openCard();
    expect(altLines(card)).toEqual([
      'Alt text (from the media library): "Library alt text"',
      "No alt text in the media library",
    ]);
    // The block editor's wording is not shown beside it.
    expect(card).not.toHaveTextContent("Alt text:");
    expect(card).not.toHaveTextContent("decorative");
  });

  it("keep every other line of an image row", async () => {
    renderTab([imagesRow()]);
    const card = await openCard();
    const outline = within(card).getByTestId("ability-outline");
    for (const text of ["van.png · 1200 × 800", "team.jpg", 'Caption: "Our van & crew"', "Centred"]) {
      expect(outline).toHaveTextContent(text);
    }
    expect(within(card).getAllByRole("link", { name: /^View image in WordPress/ })).toHaveLength(2);
    expect(within(card).getByRole("button", { name: "Approve" })).toBeEnabled();
  });

  it("show the alt text as text, never as markup", async () => {
    const hostile = '<img src=x onerror="alert(1)"><script>alert(2)</script>';
    renderTab([imagesRow([{ type: "image", attachment_id: 5, alt: hostile }], [mediaFact(5)])]);
    const card = await openCard();
    expect(altLines(card)).toEqual([`Alt text (from the media library): "${hostile}"`]);
    expect(card.querySelector("img")).toBeNull();
    expect(card.querySelector("script")).toBeNull();
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

  // Built straight, bypassing the fixture builder on purpose: it holds the
  // editor column to the input's editor (assertDbShape). The card's title,
  // editor line and links follow the column, and its outline and Elementor rows
  // follow the input, so a column and an input that disagree about Elementor
  // are never shown in part, in either direction.
  it.each<[string, AbilityRequest]>([
    [
      "the column names a WordPress editor and the input names Elementor",
      { ...elementorRow(elementorFacts()), editor: "wordpress_blocks" },
    ],
    [
      "the column names Elementor and the input names a WordPress editor",
      {
        ...pageCreateRow({ input_json: pageInput({ outline: [{ type: "paragraph", text: "Hello" }] }) }),
        editor: "builder:elementor",
      },
    ],
  ])("when %s it cannot be approved", async (_name, row) => {
    renderTab([row]);
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

  it("keeps the wording of an image's alt text, with no mention of the media library", async () => {
    renderTab([pageCreateRow({ input_json: pageInput({ outline: IMAGES }), media: IMAGE_MEDIA })]);
    const card = await openCard();
    expect(altLines(card)).toEqual(['Alt text: "Library alt text"', "No alt text (decorative)"]);
    expect(card).not.toHaveTextContent("media library");
    const outline = within(card).getByTestId("ability-outline");
    for (const text of ["van.png · 1200 × 800", "team.jpg", 'Caption: "Our van & crew"', "Centred"]) {
      expect(outline).toHaveTextContent(text);
    }
    expect(within(card).getAllByRole("link", { name: /^View image in WordPress/ })).toHaveLength(2);
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
    ["no address", null],
    ["an empty address", ""],
    ["a javascript address", "javascript:alert(1)"],
    ["an address that is not a URL", "not a url"],
  ])("offers no link with %s", async (_name, siteUrl) => {
    renderTab([done()], siteUrl);
    const card = await openCard();
    expect(card).toHaveTextContent("Draft created in Elementor.");
    expect(linksOf(card)).toEqual([]);
    // Nor the words of a link that has nowhere to go.
    expect(card).not.toHaveTextContent("Open in Elementor");
    expect(card).not.toHaveTextContent("Preview");
  });

  // The card reads a draft in the trash from either signal on a done row:
  // `trashed`, or an undo_state of undone (ability-card-model.ts, undoStatus).
  // The site address and the post id are both good, so the trash is the only
  // reason there is nothing to open.
  it.each<[string, Partial<AbilityRequest>]>([
    ["trashed", { trashed: true }],
    ["undone", { undo_state: "undone", undo_available_until: "2026-10-23T09:58:00Z" }],
  ])("offers no link to a draft that is %s", async (_name, over) => {
    renderTab([done(over)]);
    const card = await openCard();
    expect(card).toHaveTextContent("Moved to the trash.");
    expect(card).not.toHaveTextContent("Draft created in Elementor.");
    expect(linksOf(card)).toEqual([]);
    expect(card).not.toHaveTextContent("Open in Elementor");
    expect(card).not.toHaveTextContent("Preview");
    expect(card).not.toHaveTextContent("Edit the draft in WordPress");
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

// --- a designed page (outline version 3) ----------------------------------------
//
// The design the AI chose is drawn from the exact input: six swatches, the type
// ramp at the sizes the agent writes, the fonts, corners and spacing, and the
// page frame the site resolved (the control plane adds it to the input as its
// last member, mcp/page_create_input.go pageCreateInputWithFrame). The inputs
// are the shared designed-page cases that the control plane, the agent and this
// card all read.

const V3_CASES = join(process.cwd(), "..", "agent", "tests", "fixtures", "ability-run", "page-create-v3-cases.json");

type Doc = Record<string, unknown>;

/** The input text of one shared designed-page case, after `change` has edited its page. */
function designedInput(name: string, change?: (doc: Doc) => void): string {
  const file = JSON.parse(readFileSync(V3_CASES, "utf8")) as { cases: Array<{ name: string; input: string }> };
  const found = file.cases.find((c) => c.name === name);
  if (!found) throw new Error(`the shared designed-page cases have no ${name}`);
  if (change === undefined) return found.input;
  const doc = JSON.parse(found.input) as Doc;
  change(doc);
  return JSON.stringify(doc);
}

const designOf = (doc: Doc) => doc.design as Doc & { colors: Doc; fonts: Doc; scale: Doc };

/** A pending designed-page request as the queue returns it: every attachment the page names has a fact. */
function designedRow(input_json: string, over: Partial<AbilityRequest> = {}): AbilityRequest {
  const ids = imageIdsOf(input_json);
  return pageCreateRow({
    input_json,
    page_builder: elementorFacts(),
    media: ids.length > 0 ? ids.map((id) => mediaFact(id)) : null,
    ...over,
  });
}

/** Six different colours, so a swatch drawn from the wrong role cannot match by accident. */
const SIX_COLOURS = {
  ground: "#FAF7F0",
  surface: "#EFE8DA",
  ink: "#1B1F2A",
  muted: "#555B66",
  accent: "#B4452B",
  on_accent: "#FFF9F0",
};

describe("a designed page", () => {
  /** The rows of the design's list as [label, text] pairs. */
  const designRows = (card: HTMLElement) => rows(within(card).getByTestId("design-rows"));

  it("shows the six design colours as swatches, each named by its role and spelled out", async () => {
    renderTab([designedRow(designedInput("v3-minimal-page", (d) => (designOf(d).colors = { ...SIX_COLOURS })))]);
    const card = await openCard();
    const items = within(within(card).getByRole("list", { name: "Design colours" })).getAllByRole("listitem");
    expect(items.map((li) => [within(li).getByTestId("swatch-role").textContent, within(li).getByTestId("swatch-hex").textContent])).toEqual([
      ["Ground", "#FAF7F0"],
      ["Surface", "#EFE8DA"],
      ["Ink", "#1B1F2A"],
      ["Muted", "#555B66"],
      ["Accent", "#B4452B"],
      ["On accent", "#FFF9F0"],
    ]);
    // The chip is drawn in the colour its own label spells out.
    for (const [i, hex] of Object.values(SIX_COLOURS).entries()) {
      expect(within(items[i]!).getByTestId("swatch-chip")).toHaveStyle({ backgroundColor: hex });
    }
  });

  it("draws the type ramp in pixels, at the sizes the agent writes for a base of 17 and a major third", async () => {
    renderTab([designedRow(designedInput("v3-minimal-page"))]);
    const card = await openCard();
    const ramp = within(card).getByRole("list", { name: "Type ramp" });
    const steps = within(ramp)
      .getAllByRole("listitem")
      .map((li) => [within(li).getByTestId("ramp-name").textContent, within(li).getByTestId("ramp-size").textContent]);
    expect(steps).toEqual([
      ["Display", "52 px"],
      ["Heading 2", "33 px"],
      ["Heading 3", "27 px"],
      ["Lead", "21 px"],
      ["Body", "17 px"],
      ["Eyebrow", "14 px"],
      ["Button", "16 px"],
    ]);
    // Each sample is drawn at the size beside it, in the weight the agent writes.
    expect(within(ramp).getByTestId("ramp-sample-display")).toHaveStyle({ fontSize: "52px", fontWeight: "700" });
    expect(within(ramp).getByTestId("ramp-sample-h3")).toHaveStyle({ fontSize: "27px", fontWeight: "600" });
    expect(within(ramp).getByTestId("ramp-sample-body")).toHaveStyle({ fontSize: "17px", fontWeight: "400" });
  });

  it("follows the design's own base and ratio: 20 px and a perfect fourth", async () => {
    const input = designedInput("v3-minimal-page", (d) => (designOf(d).scale = { base_px: 20, ratio: "perfect_fourth" }));
    renderTab([designedRow(input)]);
    const card = await openCard();
    const sizes = within(within(card).getByRole("list", { name: "Type ramp" }))
      .getAllByRole("listitem")
      .map((li) => within(li).getByTestId("ramp-size").textContent);
    expect(sizes).toEqual(["84 px", "47 px", "36 px", "27 px", "20 px", "16 px", "19 px"]);
  });

  it.each([
    ["theme", "In the theme", "The theme's own page layout, with its header and footer."],
    ["full_width", "Full width", "The theme's header and footer, with the content spanning the full width."],
    ["canvas", "Content only", "No theme header, footer or page title."],
  ])("names the page frame %s that the site resolved", async (frame, name, line) => {
    renderTab([designedRow(designedInput("v3-minimal-page", (d) => (d.page_frame = frame)))]);
    const card = await openCard();
    expect(designRows(card)).toContainEqual(["Page frame", `${name} ${line}`]);
  });

  it.each<[string, ((d: Doc) => void) | undefined, Array<[string, string]>]>([
    [
      "the site's own font for system, rounded corners and normal spacing",
      undefined,
      [
        ["Heading font", "Manrope"],
        ["Body font", "The site's own font"],
        ["Corners", "Rounded (12 px)"],
        ["Spacing", "Normal"],
      ],
    ],
    [
      "two named fonts, very rounded corners and airy spacing",
      (d) => {
        const design = designOf(d);
        design.fonts = { heading: "Source Serif 4", body: "Inter" };
        design.radius = "lg";
        design.density = "airy";
      },
      [
        ["Heading font", "Source Serif 4"],
        ["Body font", "Inter"],
        ["Corners", "Very rounded (20 px)"],
        ["Spacing", "Airy (125% of normal)"],
      ],
    ],
    [
      "square corners and compact spacing",
      (d) => {
        designOf(d).radius = "none";
        designOf(d).density = "compact";
      },
      [
        ["Corners", "Square (0 px)"],
        ["Spacing", "Compact (75% of normal)"],
      ],
    ],
  ])("shows the fonts, the corners and the spacing: %s", async (_name, change, expected) => {
    renderTab([designedRow(designedInput("v3-minimal-page", change))]);
    const card = await openCard();
    for (const row of expected) expect(designRows(card)).toContainEqual(row);
  });

  it("shows the whole design of a full landing page the same way, with its frame", async () => {
    renderTab([designedRow(designedInput("v3-full-landing-page"))]);
    const card = await openCard();
    expect(within(within(card).getByRole("list", { name: "Design colours" })).getAllByRole("listitem")).toHaveLength(6);
    expect(within(within(card).getByRole("list", { name: "Type ramp" })).getAllByRole("listitem")).toHaveLength(7);
    expect(designRows(card)).toContainEqual(["Heading font", "Manrope"]);
    expect(designRows(card)).toContainEqual(["Body font", "Source Sans 3"]);
    expect(designRows(card)[0]?.[0]).toBe("Page frame");
    expect(designRows(card)[0]?.[1]).toMatch(/^Full width /);
  });

  // Approve keys on what the card shows in full: a designed page is shown once
  // its design and its sections are, and the card says nothing of "cannot show".
  it("can be approved once its design and its sections are shown, and posts the row's own presented_digest", async () => {
    approveMock.mockReturnValue(ok(designedRow(designedInput("v3-full-landing-page"), { state: "approved", ...decided })));
    renderTab([designedRow(designedInput("v3-full-landing-page"), { presented_digest: "e".repeat(64) })]);
    const card = await openCard();
    expect(within(card).getByTestId("design-summary")).toBeInTheDocument();
    expect(within(card).getByTestId("design-sections")).toBeInTheDocument();
    expect(card).not.toHaveTextContent(NOT_SHOWABLE);
    expect(card).not.toHaveTextContent(NOTHING_PUBLISHED);
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

  it("keeps Approve off for a shown designed page that carries no presented_digest", async () => {
    renderTab([designedRow(designedInput("v3-full-landing-page"), { presented_digest: undefined })]);
    const card = await openCard();
    expect(within(card).getByTestId("design-sections")).toBeInTheDocument();
    expect(within(card).getByRole("button", { name: "Approve" })).toBeDisabled();
    expect(within(card).getByRole("button", { name: "Decline" })).toBeEnabled();
  });

  it("states what Elementor builds from the design, not from the site's Elementor settings", async () => {
    renderTab([designedRow(designedInput("v3-full-landing-page"))]);
    const card = await openCard();
    expect(rows(within(card).getByTestId("builder-rows"))).toEqual([
      [
        "Elementor",
        "Built from Elementor's own containers and widgets, styled by the design chosen above. WPMgr does not change Elementor's site-wide styles, templates, header or footer.",
      ],
      ...ELEMENTOR_ROWS.slice(1),
    ]);
  });

  it("draws no design on a version 2 Elementor card or on a block-editor card", async () => {
    renderTab([
      elementorRow(elementorFacts(), { id: "pc-v2" }),
      pageCreateRow({ id: "pc-blocks", input_json: pageInput({ outline: [{ type: "paragraph", text: "Hello." }] }) }),
    ]);
    const cards = await screen.findAllByRole("article", { name: /Create a draft/ });
    expect(cards).toHaveLength(2);
    for (const card of cards) {
      expect(within(card).queryByTestId("design-summary")).toBeNull();
      expect(within(card).queryByTestId("design-sections")).toBeNull();
      expect(within(card).queryByRole("list", { name: "Design colours" })).toBeNull();
      expect(within(card).queryByRole("list", { name: "Type ramp" })).toBeNull();
      expect(within(card).queryByRole("list", { name: "Page sections" })).toBeNull();
    }
  });

  // The card draws colours and numbers from closed shapes only; anything else
  // makes the whole request not showable, and nothing of the design is drawn.
  it.each<[string, (d: Doc) => void]>([
    ["a font name that is markup", (d) => (designOf(d).fonts = { heading: 'x"><img src=x onerror=alert(1)>', body: "system" })],
    ["a colour that carries style rules", (d) => (designOf(d).colors = { ...SIX_COLOURS, accent: "#B4452B;background:url(//evil.example)" })],
    ["a ratio that is a number", (d) => (designOf(d).scale = { base_px: 17, ratio: 1.25 })],
    ["a page frame that is not one of the three", (d) => (d.page_frame = "wide")],
  ])("draws none of a design with %s", async (_name, change) => {
    renderTab([designedRow(designedInput("v3-minimal-page", change))]);
    const card = await openCard();
    expect(card).toHaveTextContent(NOT_SHOWABLE);
    expect(within(card).queryByTestId("design-summary")).toBeNull();
    expect(within(card).queryByTestId("design-sections")).toBeNull();
    expect(card.querySelector("img")).toBeNull();
    expect(card.innerHTML).not.toContain("evil.example");
    expect(within(card).getByRole("button", { name: "Approve" })).toBeDisabled();
  });
});

// --- the sections of a designed page ---------------------------------------------
//
// Every section is listed in page order with the swatch of the tone it is
// written in, its #anchor, its settings in words, a miniature of its background
// image and the patterns it holds, by name and variant. The tones and the
// defaults are the agent's own (ElementorSectionCompiler: DEFAULT_TONE,
// DEFAULT_WIDTH, DEFAULT_SPACING, DEFAULT_OVERLAY, DEFAULT_FOCAL,
// DEFAULT_MIN_HEIGHT and BACKGROUND_TONE; DesignTokens::tone and overlay).

type MediaOver = Parameters<typeof mediaFact>[1];
type SectionDoc = Record<string, unknown> & { children: Array<Record<string, unknown>> };

const sectionDocs = (doc: Doc) => doc.outline as SectionDoc[];

/** The full landing page in six different colours, its hero background (attachment 42) as the site described it. */
function landingRow(change?: (doc: Doc) => void, hero: MediaOver = {}): AbilityRequest {
  const input = designedInput("v3-full-landing-page", (d) => {
    designOf(d).colors = { ...SIX_COLOURS };
    change?.(d);
  });
  const media = imageIdsOf(input).map((id) =>
    mediaFact(id, id === 42 ? { filename: "hero.jpg", width: 1600, height: 900, ...hero } : {}),
  );
  return designedRow(input, { page_media: media, presented_digest: "e".repeat(64) });
}

describe("the sections of a designed page", () => {
  const sectionsOf = (card: HTMLElement) => within(card).getAllByTestId("design-section");
  const textOf = (el: HTMLElement, id: string) => within(el).getByTestId(id).textContent;

  it("lists every section in page order, each with its #anchor and the tone it is written in", async () => {
    renderTab([landingRow()]);
    const card = await openCard();
    expect(within(card).getByRole("list", { name: "Page sections" })).toBeInTheDocument();
    expect(sectionsOf(card).map((li) => [textOf(li, "section-anchor"), textOf(li, "section-tone")])).toEqual([
      ["#top", "Inverse"],
      ["#clients", "Surface"],
      ["#features", "Ground"],
      ["#numbers", "Surface"],
      ["#stories", "Ground"],
      ["#pricing", "Surface"],
      ["#faq", "Ground"],
      ["#signup", "Accent"],
    ]);
  });

  it("paints each tone swatch with the design's colour for that tone, inverse being ink", async () => {
    renderTab([landingRow()]);
    const card = await openCard();
    const { ground, surface, ink, accent } = SIX_COLOURS;
    const expected = [ink, surface, ground, surface, ground, surface, ground, accent];
    sectionsOf(card).forEach((li, i) => {
      expect(within(li).getByTestId("section-tone-swatch")).toHaveStyle({ backgroundColor: expected[i]! });
    });
  });

  it("writes a section with a background image in the inverse tone, whatever tone it names", async () => {
    renderTab([landingRow((d) => (sectionDocs(d)[0]!.tone = "surface"))]);
    const card = await openCard();
    const top = sectionsOf(card)[0]!;
    expect(textOf(top, "section-tone")).toBe("Inverse");
    expect(within(top).getByTestId("section-tone-swatch")).toHaveStyle({ backgroundColor: SIX_COLOURS.ink });
  });

  it("states each section's settings in words, with the values the agent writes when the AI names none", async () => {
    renderTab([landingRow()]);
    const card = await openCard();
    const facts = sectionsOf(card).map((li) => textOf(li, "section-facts"));
    expect(facts).toEqual([
      "Full width · Extra large spacing · Text centred · Height 80% of the screen",
      "Boxed · Medium spacing · Text at the start · Height follows content",
      "Boxed · Large spacing · Text at the start · Height follows content",
      "Boxed · Large spacing · Text at the start · Height follows content",
      "Boxed · Large spacing · Text at the start · Height follows content",
      "Boxed · Extra large spacing · Text at the start · Height follows content",
      "Boxed · Large spacing · Text at the start · Height follows content",
      "Boxed · Large spacing · Text centred · Height follows content",
    ]);
  });

  it("shows a section's background as a miniature in the image's proportions, with the file's facts", async () => {
    renderTab([landingRow()]);
    const card = await openCard();
    const thumb = within(sectionsOf(card)[0]!).getByTestId("background-thumb");
    expect(textOf(thumb, "thumb-file")).toBe("hero.jpg");
    expect(textOf(thumb, "thumb-size")).toBe("1600 × 900");
    expect(textOf(thumb, "thumb-framing")).toBe("Strong overlay · Focal point: centre");
    expect(within(thumb).getByTestId("thumb-tile")).toHaveStyle({ width: "114px", height: "64px" });
    // The overlay is the design's ink at the strength the agent writes, and the marker sits at the focal point.
    expect(within(thumb).getByTestId("thumb-overlay")).toHaveStyle({ backgroundColor: SIX_COLOURS.ink, opacity: "0.65" });
    expect(within(thumb).getByTestId("thumb-focal")).toHaveStyle({ top: "50%" });
    const link = within(thumb).getByRole("link", { name: /View image in WordPress/ });
    expect(link).toHaveAttribute("href", "https://example.com/wp-admin/post.php?post=42&action=edit");
    expect(link).toHaveAttribute("target", "_blank");
    expect(link).toHaveAttribute("rel", "noopener noreferrer");
    // A section with no background has none, and the page has one background.
    expect(within(card).getAllByTestId("background-thumb")).toHaveLength(1);
    expect(within(sectionsOf(card)[1]!).queryByTestId("background-thumb")).toBeNull();
  });

  it.each<[string, string, string | null, string, string]>([
    ["none", "top", null, "12%", "No overlay · Focal point: top"],
    ["soft", "bottom", "0.45", "88%", "Soft overlay · Focal point: bottom"],
    ["strong", "center", "0.65", "50%", "Strong overlay · Focal point: centre"],
  ])("draws the overlay %s and the focal point %s as the agent writes them", async (overlay, focal, opacity, top, words) => {
    renderTab([landingRow((d) => (sectionDocs(d)[0]!.background = { attachment_id: 42, overlay, focal }))]);
    const card = await openCard();
    const thumb = within(sectionsOf(card)[0]!).getByTestId("background-thumb");
    if (opacity === null) expect(within(thumb).queryByTestId("thumb-overlay")).toBeNull();
    else expect(within(thumb).getByTestId("thumb-overlay")).toHaveStyle({ opacity });
    expect(within(thumb).getByTestId("thumb-focal")).toHaveStyle({ top });
    expect(textOf(thumb, "thumb-framing")).toBe(words);
  });

  it("states a strong overlay and a central focal point when the background names neither", async () => {
    renderTab([landingRow((d) => (sectionDocs(d)[0]!.background = { attachment_id: 42 }))]);
    const card = await openCard();
    const thumb = within(sectionsOf(card)[0]!).getByTestId("background-thumb");
    expect(textOf(thumb, "thumb-framing")).toBe("Strong overlay · Focal point: centre");
  });

  it("draws no picture of a site file: the miniature is made of numbers and closed values", async () => {
    renderTab([landingRow()]);
    const card = await openCard();
    expect(card.querySelector("img, picture, canvas, video")).toBeNull();
    expect(card.querySelector("[style*='url(']")).toBeNull();
  });

  it("shows the site's file name as text in a bidi isolate, never as markup", async () => {
    const hostile = "<img src=x onerror=alert(1)>.jpg";
    renderTab([landingRow(undefined, { filename: hostile })]);
    const card = await openCard();
    const file = within(card).getByTestId("thumb-file");
    expect(file.textContent).toBe(hostile);
    expect(file.querySelector("bdi")?.textContent).toBe(hostile);
    expect(card.querySelector("img")).toBeNull();
  });

  it("names each pattern and its variant, one to a section, in page order", async () => {
    renderTab([landingRow()]);
    const card = await openCard();
    expect(within(card).getAllByTestId("section-pattern").map((el) => el.textContent)).toEqual([
      "Hero · background",
      "Logos · row",
      "Features · split",
      "Stats · counters",
      "Testimonials · featured",
      "Pricing · three plans",
      "FAQ · accordion",
      "Call to action · band",
    ]);
    expect(sectionsOf(card).map((li) => within(li).getAllByTestId("section-pattern").length)).toEqual([1, 1, 1, 1, 1, 1, 1, 1]);
  });

  it("shows what a section holds beside its patterns, the AI's words in a bidi isolate", async () => {
    const hostile = "<b>What we do</b> & <img src=x onerror=alert(1)>";
    renderTab([landingRow((d) => (sectionDocs(d)[2]!.children[0]!.text = hostile))]);
    const card = await openCard();
    const features = sectionsOf(card)[2]!;
    expect(within(features).getByText("Heading 2")).toBeInTheDocument();
    expect(within(features).getByText(hostile).closest("bdi")).not.toBeNull();
    expect(card.querySelector("img")).toBeNull();
  });

  it("labels the three button styles, the text style included", async () => {
    renderTab([designedRow(designedInput("buttons-in-three-styles"))]);
    const card = await openCard();
    expect(within(card).getAllByText(/^Button · /).map((el) => el.textContent)).toEqual([
      "Button · Filled",
      "Button · Outline",
      "Button · Text",
    ]);
  });

  it("marks a button link to a section of the page, shown as text and never as a link", async () => {
    renderTab([designedRow(designedInput("link-to-a-later-anchor"))]);
    const card = await openCard();
    expect(card).toHaveTextContent("Links to #pricing");
    expect(card.querySelector("[data-chip='section']")).toHaveTextContent("A section of this page");
    expect(card.querySelector("a[href='#pricing']")).toBeNull();
  });
});

// --- the bodies of the patterns of a designed page --------------------------------
//
// A pattern is shown by its name and variant and by what it holds, so the person
// approving reads every word the page will carry: a hero's and a call to action's
// heading, text and buttons (a hero's picture by its file name), each feature's
// icon in words with its title and text, each counter's value, suffix and label,
// each logo's file name, each testimonial's quote, name and role, each plan's
// name, price, period, features and button, and each question with its answer.
// The AI's words are in a bidi isolate and are never markup; the small labels
// are ours.

type Items = Array<Record<string, unknown>>;

/** A pending designed page built from one shared case, each image as the site described it. */
function patternRow(name: string, change?: (doc: Doc) => void, files: Record<number, MediaOver> = {}): AbilityRequest {
  const input = designedInput(name, change);
  const media = imageIdsOf(input).map((id) => mediaFact(id, files[id] ?? {}));
  return designedRow(input, { page_media: media });
}

describe("the bodies of the patterns of a designed page", () => {
  const sectionOf = (card: HTMLElement, anchor: string): HTMLElement => {
    const found = within(card)
      .getAllByTestId("design-section")
      .find((li) => within(li).getByTestId("section-anchor").textContent === `#${anchor}`);
    if (found === undefined) throw new Error(`the card shows no section #${anchor}`);
    return found;
  };
  const bodyOf = (card: HTMLElement, anchor: string) => within(sectionOf(card, anchor)).getByTestId("pattern-body");
  const textOf = (parent: HTMLElement, id: string) => within(parent).getByTestId(id).textContent;
  const textsOf = (parent: HTMLElement, id: string) => within(parent).getAllByTestId(id).map((el) => el.textContent);
  /** True when every element that holds exactly `text` sits in a bidi isolate (and there is one). */
  const fenced = (parent: HTMLElement, text: string) => {
    const found = within(parent).getAllByText(text);
    return found.length > 0 && found.every((el) => el.closest("bdi") !== null);
  };
  const patternOf = (doc: Doc, name: string) => {
    const found = sectionDocs(doc)
      .flatMap((s) => s.children)
      .find((c) => c.pattern === name);
    if (found === undefined) throw new Error(`the page has no ${name} pattern`);
    return found;
  };
  const itemsOf = (doc: Doc, name: string, key: string) => patternOf(doc, name)[key] as Items;

  it("shows a pattern's body beside its name, in the section that holds it", async () => {
    renderTab([patternRow("v3-full-landing-page")]);
    const card = await openCard();
    for (const li of within(card).getAllByTestId("design-section")) {
      const line = within(li).getByTestId("section-pattern");
      const body = within(li).getByTestId("pattern-body");
      expect(body.compareDocumentPosition(line) & Node.DOCUMENT_POSITION_PRECEDING).toBeTruthy();
    }
    expect(within(card).getAllByTestId("pattern-body").map((el) => el.getAttribute("data-pattern"))).toEqual([
      "hero",
      "logos",
      "features",
      "stats",
      "testimonials",
      "pricing",
      "faq",
      "cta",
    ]);
  });

  it("shows a hero's heading, text, buttons and the file name of its picture, in that order", async () => {
    renderTab([patternRow("patterns-at-their-smallest-counts", undefined, { 42: { filename: "team-at-work.png", mime: "image/png" } })]);
    const card = await openCard();
    const hero = bodyOf(card, "top");
    expect(hero.textContent).toMatch(
      /Heading 1.*Websites that earn their keep.*Paragraph.*Lead\..*Button · Filled.*"Start".*Links to \/start.*Image.*team-at-work\.png/s,
    );
    expect(fenced(hero, "Websites that earn their keep")).toBe(true);
    expect(fenced(hero, "Lead.")).toBe(true);
    expect(fenced(hero, '"Start"')).toBe(true);
    expect(fenced(hero, "team-at-work.png")).toBe(true);
    expect(fenced(hero, '"The team at work"')).toBe(true);
  });

  it("shows a call to action's heading, text and buttons, each button in its own style", async () => {
    renderTab([patternRow("v3-full-landing-page")]);
    const card = await openCard();
    const cta = bodyOf(card, "signup");
    expect(within(cta).getByText("Heading 2")).toBeInTheDocument();
    expect(fenced(cta, "Ready when you are")).toBe(true);
    expect(fenced(cta, "Start a trial today and keep everything if you stay.")).toBe(true);
    expect(within(cta).getAllByText(/^Button · /).map((el) => el.textContent)).toEqual(["Button · Filled", "Button · Text"]);
    expect(fenced(cta, '"Start free"')).toBe(true);
    expect(fenced(cta, '"Book a call"')).toBe(true);
    expect(cta).toHaveTextContent("Links to /signup");
    expect(cta).toHaveTextContent("Links to https://example.com/call");
  });

  it("shows each feature's icon in words, its title and its text", async () => {
    renderTab([
      patternRow("v3-full-landing-page", (d) => {
        itemsOf(d, "features", "features")[0]!.icon = "credit-card";
        itemsOf(d, "features", "features")[1]!.icon = "map-pin";
      }),
    ]);
    const card = await openCard();
    const features = bodyOf(card, "features");
    expect(within(features).getByRole("list", { name: "Features" })).toBeInTheDocument();
    expect(textsOf(features, "feature-icon")).toEqual(["credit card", "map pin", "chart", "users", "rocket", "globe"]);
    const titles = ["Fast setup", "Safe by default", "Clear numbers", "Built for teams", "Quick launches", "Many languages"];
    const texts = [
      "Go live in an afternoon, not a quarter.",
      "Every change is reviewed before it goes out.",
      "See what each page earns, week by week.",
      "Share access without sharing passwords.",
      "Ship a new page the day the idea lands.",
      "Reach customers in the language they read.",
    ];
    expect(textsOf(features, "feature-title")).toEqual(titles);
    expect(textsOf(features, "feature-text")).toEqual(texts);
    for (const t of [...titles, ...texts]) expect(fenced(features, t)).toBe(true);
  });

  it("shows each counter's value, suffix and label, and a value of zero with no suffix", async () => {
    renderTab([
      patternRow("v3-full-landing-page", (d) => {
        itemsOf(d, "stats", "stats")[2] = { value: 0, label: "open issues" };
      }),
    ]);
    const card = await openCard();
    const stats = bodyOf(card, "numbers");
    expect(within(stats).getByRole("list", { name: "Stats" })).toBeInTheDocument();
    expect(textsOf(stats, "stat-value")).toEqual(["1200+", "98%", "0"]);
    expect(textsOf(stats, "stat-label")).toEqual(["sites launched", "would recommend us", "open issues"]);
    const suffixes = within(stats).getAllByTestId("stat-value").map((el) => el.querySelector("bdi")?.textContent ?? null);
    expect(suffixes).toEqual(["+", "%", null]);
    expect(fenced(stats, "sites launched")).toBe(true);
  });

  it("shows each logo by the file name the site reported, with the alt text saved with it", async () => {
    const names = ["acme.png", "birchwood.png", "cobalt.png", "dunmore.png", "everly.png", "fernhill.png"];
    const files = Object.fromEntries(names.map((filename, i) => [101 + i, { filename, mime: "image/png" } as const]));
    renderTab([patternRow("v3-full-landing-page", undefined, files)]);
    const card = await openCard();
    const logos = bodyOf(card, "clients");
    expect(within(logos).getByRole("list", { name: "Logos" })).toBeInTheDocument();
    expect(textsOf(logos, "logo-file")).toEqual(names);
    expect(within(logos).getAllByText("Alt text (from the media library):")).toHaveLength(names.length);
    for (const n of names) expect(fenced(logos, n)).toBe(true);
    expect(fenced(logos, '"Acme logo"')).toBe(true);
    expect(fenced(logos, '"Fernhill logo"')).toBe(true);
  });

  it("shows each testimonial's quote, name, role and picture", async () => {
    renderTab([
      patternRow("v3-full-landing-page", undefined, { 201: { filename: "dana.jpg" }, 202: { filename: "marcus.jpg" }, 203: { filename: "priya.jpg" } }),
    ]);
    const card = await openCard();
    const body = bodyOf(card, "stories");
    expect(within(body).getByRole("list", { name: "Testimonials" })).toBeInTheDocument();
    expect(textsOf(body, "testimonial-quote")).toEqual([
      '"The new site paid for itself in a month."',
      '"Updates that used to take a week now take an hour."',
      '"Our bookings doubled after the relaunch."',
    ]);
    expect(textsOf(body, "testimonial-name")).toEqual(["Dana Whitfield", "Marcus Lee", "Priya Nair"]);
    expect(textsOf(body, "testimonial-role")).toEqual(["Owner, Whitfield Bakery", "Operations lead", "Director, Nair Dental"]);
    expect(textsOf(body, "testimonial-picture")).toEqual(["dana.jpg", "marcus.jpg", "priya.jpg"]);
    expect(fenced(body, '"Our bookings doubled after the relaunch."')).toBe(true);
    expect(fenced(body, "Director, Nair Dental")).toBe(true);
  });

  it("leaves out the role and the picture a testimonial does not have", async () => {
    renderTab([
      patternRow("v3-full-landing-page", (d) => {
        const second = itemsOf(d, "testimonials", "testimonials")[1]!;
        delete second.role;
        delete second.attachment_id;
      }),
    ]);
    const card = await openCard();
    const items = within(bodyOf(card, "stories")).getAllByTestId("testimonial-item");
    expect(items).toHaveLength(3);
    expect(items[1]).toHaveTextContent("Marcus Lee");
    expect(items[1]).not.toHaveTextContent("undefined");
    expect(within(items[1]!).queryByTestId("testimonial-role")).toBeNull();
    expect(within(items[1]!).queryByTestId("testimonial-picture")).toBeNull();
    expect(textOf(items[0]!, "testimonial-role")).toBe("Owner, Whitfield Bakery");
    expect(textOf(items[2]!, "testimonial-picture")).toBe("photo-203.jpg");
  });

  it("shows each plan's name, price, period, features and button, the highlighted plan's button filled", async () => {
    renderTab([patternRow("v3-full-landing-page")]);
    const card = await openCard();
    const pricing = bodyOf(card, "pricing");
    expect(within(pricing).getByRole("list", { name: "Plans" })).toBeInTheDocument();
    const plans = within(pricing).getAllByTestId("plan-item");
    expect(plans.map((li) => textOf(li, "plan-name"))).toEqual(["Starter", "Team", "Business"]);
    expect(plans.map((li) => textOf(li, "plan-price"))).toEqual(["19", "49", "99"]);
    expect(plans.map((li) => textOf(li, "plan-period"))).toEqual(["month", "month", "month"]);
    expect(plans.map((li) => textsOf(li, "plan-feature"))).toEqual([
      ["1 site", "Email support"],
      ["5 sites", "Priority support", "Weekly reports"],
      ["25 sites", "Phone support", "Daily reports"],
    ]);
    expect(plans.map((li) => within(li).queryByTestId("plan-highlighted") !== null)).toEqual([false, true, false]);
    expect(plans.map((li) => within(li).getByText(/^Button · /).textContent)).toEqual([
      "Button · Outline",
      "Button · Filled",
      "Button · Outline",
    ]);
    expect(plans.map((li) => within(li).getByText(/^"Choose /).textContent)).toEqual([
      '"Choose Starter"',
      '"Choose Team"',
      '"Choose Business"',
    ]);
    for (const li of plans) {
      expect(li).toHaveTextContent("Links to #signup");
      expect(li.querySelector("[data-chip='section']")).toHaveTextContent("A section of this page");
    }
    expect(fenced(pricing, "Priority support")).toBe(true);
    expect(fenced(pricing, "Business")).toBe(true);
  });

  it("leaves out the period a plan does not have", async () => {
    renderTab([
      patternRow("v3-full-landing-page", (d) => {
        delete itemsOf(d, "pricing", "plans")[1]!.period;
      }),
    ]);
    const card = await openCard();
    const plans = within(bodyOf(card, "pricing")).getAllByTestId("plan-item");
    expect(within(plans[1]!).queryByTestId("plan-period")).toBeNull();
    expect(plans[1]).not.toHaveTextContent("undefined");
    expect(textOf(plans[0]!, "plan-period")).toBe("month");
    expect(textOf(plans[2]!, "plan-period")).toBe("month");
  });

  it("shows each question with its answer", async () => {
    renderTab([patternRow("v3-full-landing-page")]);
    const card = await openCard();
    const faq = bodyOf(card, "faq");
    expect(within(faq).getByRole("list", { name: "Questions" })).toBeInTheDocument();
    const questions = [
      "How long does a launch take?",
      "Can I edit the page myself?",
      "Do you move my old content?",
      "What does support cover?",
      "Can I cancel?",
      "Is there a contract?",
    ];
    const answers = [
      "Most pages are live within a week of approval.",
      "Yes. Every page opens in the visual editor.",
      "We copy the text and images you choose.",
      "Fixes, updates and questions, by email or phone.",
      "At any time, from the billing page.",
      "No. Plans renew monthly.",
    ];
    expect(textsOf(faq, "faq-question")).toEqual(questions);
    expect(textsOf(faq, "faq-answer")).toEqual(answers);
    for (const t of [...questions, ...answers]) expect(fenced(faq, t)).toBe(true);
  });

  it.each([
    ["features", "features", "feature-item"],
    ["testimonials", "testimonials", "testimonial-item"],
    ["pricing", "plans", "plan-item"],
    ["faq", "faq", "faq-item"],
    ["logos", "logos", "logo-item"],
    ["stats", "stats", "stat-item"],
  ] as const)("shows every item of the %s pattern at its largest count", async (name, key, id) => {
    const doc = JSON.parse(designedInput("patterns-at-their-largest-counts")) as Doc;
    const largest = PATTERN_ITEM_CAPS[key][1];
    expect(itemsOf(doc, name, key)).toHaveLength(largest);
    renderTab([patternRow("patterns-at-their-largest-counts")]);
    const card = await openCard();
    expect(within(card).getAllByTestId(id)).toHaveLength(largest);
  });

  it("keeps every word of the AI in a pattern in a bidi isolate, never as markup", async () => {
    const asWritten: string[] = [];
    const betweenOurQuotes: string[] = [];
    let n = 0;
    /** A fresh hostile text, noted as the card is to show it. */
    const fresh = (quotedByUs = false): string => {
      n += 1;
      const text = `<img src=x onerror=alert(${n})>`;
      (quotedByUs ? betweenOurQuotes : asWritten).push(quotedByUs ? `"${text}"` : text);
      return text;
    };
    renderTab([
      patternRow("v3-full-landing-page", (d) => {
        const hero = patternOf(d, "hero").children as Items;
        hero[0]!.text = fresh();
        hero[1]!.text = fresh();
        (hero[2]!.buttons as Items)[0]!.text = fresh(true);
        for (const l of itemsOf(d, "logos", "logos")) l.alt = fresh(true);
        for (const f of itemsOf(d, "features", "features")) {
          f.title = fresh();
          f.text = fresh();
        }
        for (const s of itemsOf(d, "stats", "stats")) {
          s.suffix = fresh();
          s.label = fresh();
        }
        for (const t of itemsOf(d, "testimonials", "testimonials")) {
          t.quote = fresh(true);
          t.name = fresh();
          t.role = fresh();
        }
        for (const p of itemsOf(d, "pricing", "plans")) {
          p.name = fresh();
          p.price = fresh();
          p.period = fresh();
          const features = p.features as string[];
          features.forEach((_, j) => (features[j] = fresh()));
          (p.cta as Record<string, unknown>).text = fresh(true);
        }
        for (const q of itemsOf(d, "faq", "faq")) {
          q.question = fresh();
          q.answer = fresh();
        }
        const cta = patternOf(d, "cta").children as Items;
        cta[0]!.text = fresh();
        cta[1]!.text = fresh();
        (cta[2]!.buttons as Items)[0]!.text = fresh(true);
      }),
    ]);
    const card = await openCard();
    expect(asWritten.length).toBeGreaterThan(0);
    expect(betweenOurQuotes.length).toBeGreaterThan(0);
    for (const text of [...asWritten, ...betweenOurQuotes]) {
      expect(fenced(card, text), text).toBe(true);
    }
    expect(card.querySelector("img")).toBeNull();
  });
});

// --- "Open in Elementor" on the card of a designed page --------------------------
//
// Once the draft exists the card links to Elementor's editor for the post and
// to its preview, built from the site's own address and the post id alone. Both
// are offered next to the design the page was built from, and neither before
// the draft exists nor after it is gone.

describe("the draft a designed page created", () => {
  const done = (over: Partial<AbilityRequest> = {}) =>
    designedRow(designedInput("v3-minimal-page"), { state: "done", outcome: "created", created_post_id: 418, ...decided, ...over });

  /** The status links, by name, with their addresses. */
  function linksOf(card: HTMLElement): Array<[string | null, string | null]> {
    return within(card)
      .queryAllByRole("link")
      .map((a) => [a.textContent, a.getAttribute("href")]);
  }

  it("links to Open in Elementor and to Preview, in a new tab, beside the design", async () => {
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
    expect(within(card).getByTestId("design-summary")).toBeInTheDocument();
  });

  it("follows the post's own id and a WordPress that lives in a sub-directory", async () => {
    renderTab([done({ created_post_id: 9007 })], "https://example.com/blog");
    const card = await openCard();
    expect(linksOf(card)[0]).toEqual([
      "Open in Elementor",
      "https://example.com/blog/wp-admin/post.php?post=9007&action=elementor",
    ]);
  });

  it("offers no link before the draft exists", async () => {
    renderTab([designedRow(designedInput("v3-minimal-page"))]);
    const card = await openCard();
    expect(linksOf(card)).toEqual([]);
    expect(card).not.toHaveTextContent("Open in Elementor");
  });

  it.each([
    ["no site address", null],
    ["an address that is not a plain web address", "javascript:alert(1)"],
  ])("offers no link with %s", async (_name, siteUrl) => {
    renderTab([done()], siteUrl);
    const card = await openCard();
    expect(card).toHaveTextContent("Draft created in Elementor.");
    expect(linksOf(card)).toEqual([]);
  });

  it("offers no link to a draft that was moved to the trash", async () => {
    renderTab([done({ trashed: true })]);
    const card = await openCard();
    expect(card).toHaveTextContent("Moved to the trash.");
    expect(linksOf(card)).toEqual([]);
  });
});

