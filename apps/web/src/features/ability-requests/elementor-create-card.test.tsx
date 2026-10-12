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

  it("keeps Approve off until the sections are shown, so nothing is approved unseen", async () => {
    renderTab([designedRow(designedInput("v3-full-landing-page"))]);
    const card = await openCard();
    expect(within(card).getByTestId("design-summary")).toBeInTheDocument();
    expect(card).toHaveTextContent(NOT_SHOWABLE);
    expect(within(card).getByRole("button", { name: "Approve" })).toBeDisabled();
    expect(within(card).getByRole("button", { name: "Decline" })).toBeEnabled();
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
      expect(within(card).queryByRole("list", { name: "Design colours" })).toBeNull();
      expect(within(card).queryByRole("list", { name: "Type ramp" })).toBeNull();
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
    expect(card.querySelector("img")).toBeNull();
    expect(card.innerHTML).not.toContain("evil.example");
    expect(within(card).getByRole("button", { name: "Approve" })).toBeDisabled();
  });
});
