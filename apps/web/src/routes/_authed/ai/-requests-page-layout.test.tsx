import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, fireEvent, waitFor, within } from "@testing-library/react";
import type { AbilityRequest } from "@wpmgr/api";

import { createTestQueryClient, renderWithProviders } from "@/test/render";
import { authKeys } from "@/features/auth/use-auth";
import {
  assertDbShape,
  mediaFact,
  pageCreateRow,
  pageInput,
  type PageMediaRow,
} from "@/test/ability-request-rows";

import { Route } from "./requests";

// The page-layout approval card, rendered through the real route, the real
// TanStack Router and the real QueryClient; only the @wpmgr/api wire boundary
// is faked. Rows come from the fixture builder in @/test/ability-request-rows,
// which refuses any row the database would not store. The realistic layout is
// the agent's own "blocks-every-node" case (the bytes its builder, the control
// plane and this card are all tested against), with the image facts the site
// reported for it. page_media is null on the wire for a request with no image
// (apps/api/internal/abilityrequest/handler.go, RequestDTO.PageMedia).

const { listMock, abilityListMock, approveAbilityMock } = vi.hoisted(() => ({
  listMock: vi.fn(),
  abilityListMock: vi.fn(),
  approveAbilityMock: vi.fn(),
}));

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));

vi.mock("@wpmgr/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wpmgr/api")>();
  return {
    ...actual,
    listAssistantRequests: listMock,
    listAbilityRequests: abilityListMock,
    approveAbilityRequest: approveAbilityMock,
  };
});

const RequestsPage = Route.options.component!;
const TENANT = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";

function ok(data: unknown) {
  return Promise.resolve({ data, error: undefined, response: { status: 200 } });
}

function renderPage(rows: AbilityRequest[]) {
  listMock.mockReturnValue(ok({ requests: [], pending_count: 0, limit: 50, offset: 0 }));
  abilityListMock.mockReturnValue(
    ok({ requests: rows, pending_count: rows.filter((r) => r.state === "pending").length, limit: 50, offset: 0 }),
  );
  const queryClient = createTestQueryClient();
  queryClient.setQueryData(authKeys.me, {
    user: { id: "user-1", email: "priya@example.test", name: "Priya" },
    memberships: [{ user_id: "user-1", tenant_id: TENANT, role: "admin" }],
    active_tenant_id: TENANT,
    scope: "org",
  });
  return renderWithProviders(<RequestsPage />, { withRouter: true, initialPath: "/ai/requests", queryClient });
}

beforeEach(() => {
  listMock.mockReset();
  abilityListMock.mockReset();
  approveAbilityMock.mockReset();
});

// --- fixtures ---------------------------------------------------------------

const LAYOUT_FIXTURE = join(process.cwd(), "..", "agent", "tests", "fixtures", "ability-run", "page-create-layout.json");

/** The agent's every-node layout, and what the site reported for its images. */
function everyNode(): { input_json: string; media: PageMediaRow[] } {
  const file = JSON.parse(readFileSync(LAYOUT_FIXTURE, "utf8")) as {
    cases: Array<{ name: string; input: string; preview: { media?: PageMediaRow[] } }>;
  };
  const found = file.cases.find((c) => c.name === "blocks-every-node");
  if (!found?.preview.media) throw new Error("the blocks-every-node case is missing from the agent fixture");
  const input = { ...(JSON.parse(found.input) as object), title: "Spring menu" };
  const media = found.preview.media.map(({ id, filename, mime, width, height }) => ({ id, filename, mime, width, height }));
  return { input_json: JSON.stringify(input), media };
}

async function openCard(): Promise<HTMLElement> {
  return screen.findByRole("article", { name: /Create a draft/ });
}

/** The outline as the lines a person reads: every paragraph, list item and cell, in order. */
function lines(outline: HTMLElement): string[] {
  return Array.from(outline.querySelectorAll("p, li, th, td")).map((el) =>
    (el.textContent ?? "").replace(/\s+/g, " ").trim(),
  );
}

const para = { type: "paragraph", text: "Big savings." };

// --- the whole layout --------------------------------------------------------

describe("the layout outline", () => {
  it("shows every block of a real layout as a line, in order", async () => {
    const { input_json, media } = everyNode();
    renderPage([pageCreateRow({ input_json, media })]);
    const card = await openCard();
    const outline = within(card).getByTestId("ability-outline");
    expect(lines(outline)).toEqual([
      "Spring menu",
      "Heading 2 Welcome to Café Ünï 🚀",
      `Paragraph We serve breakfast & lunch [1]. "Fresh" daily, it's true.`,
      "Section",
      "Heading 3 Our team",
      "Columns · 2 · 60% / 40%",
      "Column 1",
      "Image · team-photo.jpg · 1200 × 800",
      `Alt text: "Bob's "dog" & cat"`,
      `Caption: "Rex, our mascot [2]"`,
      "Paragraph Left",
      "Column 2",
      "Heading 4 Right",
      "Buttons · centred",
      `Button · Filled "Book a call"`,
      "Links to https://calendly.example/acme/30min Another website: calendly.example",
      `Button · Outline "Contact & map"`,
      "Links to /contact?x=1&y=2#map This site",
      "Columns · 3 · equal widths",
      "Column 1",
      "Image · café-menu.png · 640 × 480",
      "No alt text (decorative)",
      "Centred",
      "Column 2",
      "List",
      "One",
      "Two",
      "Column 3",
      "Quote",
      "To be & not to be.",
      "Second [3].",
      "Citation: Jane & John",
      "Image · team-photo.jpg · 1200 × 800",
      `Alt text: "Again"`,
      "Wide",
      "Image · banner.webp",
      `Alt text: "Full width"`,
      "Full width",
      "Separator",
      "Space · Medium (48 px)",
      "Table · 2 columns × 2 rows · header row",
      "Plan",
      "Price",
      "Basic",
      "$10",
      "Pro [1]",
      "(empty)",
      "Numbered list",
      "First",
      "Second",
    ]);
  });

  it("summarises what the page holds in a Layout row", async () => {
    const { input_json, media } = everyNode();
    renderPage([pageCreateRow({ input_json, media })]);
    const card = await openCard();
    expect(within(card).getByText("Layout")).toBeInTheDocument();
    expect(within(card).getByTestId("layout-summary")).toHaveTextContent(
      "1 section · 2 column blocks · 4 images · 2 buttons · 1 table · 1 quote",
    );
  });

  it("shows the file name and size the site reported, and no picture", async () => {
    const { input_json, media } = everyNode();
    renderPage([pageCreateRow({ input_json, media })]);
    const card = await openCard();
    const outline = within(card).getByTestId("ability-outline");
    expect(outline).toHaveTextContent("team-photo.jpg · 1200 × 800");
    expect(outline).toHaveTextContent("café-menu.png · 640 × 480");
    // A file the site gave no size for shows its name and no size at all.
    expect(lines(outline)).toContain("Image · banner.webp");
    expect(outline.textContent).not.toMatch(/banner\.webp ·/);
    // Nothing is loaded from the site: no image, no address at all.
    expect(document.querySelectorAll("img, picture, video, iframe, svg [href], [src], [srcset]").length).toBe(0);
  });

  it("scrolls inside a bounded box that is never collapsed, and keyboard users can reach it", async () => {
    const { input_json, media } = everyNode();
    renderPage([pageCreateRow({ input_json, media })]);
    const outline = within(await openCard()).getByTestId("ability-outline");
    expect(outline.className).toContain("max-h-96");
    expect(outline.className).toContain("overflow-y-auto");
    expect(outline.getAttribute("tabindex")).toBe("0");
    expect(outline.getAttribute("role")).toBe("region");
    // It is named by the "Chosen by the AI" label above it.
    expect(within(await openCard()).getByRole("region", { name: "Chosen by the AI" })).toBe(outline);
  });

  it("indents each container one step and names it for assistive technology", async () => {
    const { input_json, media } = everyNode();
    renderPage([pageCreateRow({ input_json, media })]);
    const outline = within(await openCard()).getByTestId("ability-outline");
    const section = within(outline).getByRole("group", { name: "Section" });
    const columns = within(section).getByRole("group", { name: "Columns · 2 · 60% / 40%" });
    const column = within(columns).getByRole("group", { name: "Column 2" });
    expect(within(column).getByRole("group", { name: "Buttons · centred" })).toBeInTheDocument();
    expect(within(column).getByText(/Heading 4/)).toBeInTheDocument();
  });

  it("shows all fifty rows of a table", async () => {
    const rows = Array.from({ length: 50 }, (_, i) => [`row-${i + 1}`, `value-${i + 1}`]);
    const input_json = pageInput({ outline: [{ type: "table", header: ["Name", "Value"], rows }] });
    renderPage([pageCreateRow({ input_json })]);
    const outline = within(await openCard()).getByTestId("ability-outline");
    expect(within(outline).getAllByRole("row")).toHaveLength(51);
    expect(within(outline).getByText("row-1")).toBeInTheDocument();
    expect(within(outline).getByText("row-50")).toBeInTheDocument();
    expect(within(outline).getByText("value-50")).toBeInTheDocument();
    expect(lines(outline)).toContain("Table · 2 columns × 50 rows · header row");
  });

  it("shows a table with no header as rows only", async () => {
    const input_json = pageInput({ outline: [{ type: "table", rows: [["a"], ["b"]] }] });
    renderPage([pageCreateRow({ input_json })]);
    const outline = within(await openCard()).getByTestId("ability-outline");
    expect(lines(outline)).toEqual(["Spring menu", "Table · 1 column × 2 rows", "a", "b"]);
    expect(within(outline).queryAllByRole("columnheader")).toHaveLength(0);
  });

  it("marks the header cells as headers", async () => {
    const input_json = pageInput({ outline: [{ type: "table", header: ["Plan", "Price"], rows: [["Basic", "$10"]] }] });
    renderPage([pageCreateRow({ input_json })]);
    const outline = within(await openCard()).getByTestId("ability-outline");
    expect(within(outline).getAllByRole("columnheader").map((h) => h.textContent)).toEqual(["Plan", "Price"]);
  });

  it("shows an unordered list as bullets and a numbered one as numbers", async () => {
    const input_json = pageInput({
      outline: [
        { type: "list", ordered: false, items: ["a", "b"] },
        { type: "list", ordered: true, items: ["c"] },
      ],
    });
    renderPage([pageCreateRow({ input_json })]);
    const outline = within(await openCard()).getByTestId("ability-outline");
    expect(outline.querySelectorAll("ul > li")).toHaveLength(2);
    expect(outline.querySelectorAll("ol > li")).toHaveLength(1);
  });

  it("a heading is bold and its level is stated", async () => {
    const input_json = pageInput({ outline: [{ type: "heading", level: 3, text: "Visit us" }, para] });
    renderPage([pageCreateRow({ input_json })]);
    const outline = within(await openCard()).getByTestId("ability-outline");
    expect(within(outline).getByText("Heading 3")).toBeInTheDocument();
    expect(within(outline).getByText("Visit us").className).toContain("font-semibold");
    expect(within(outline).getByText("Big savings.").className).not.toContain("font-semibold");
  });

  it("names the three spacer sizes in pixels", async () => {
    const input_json = pageInput({
      outline: ["small", "medium", "large"].map((size) => ({ type: "spacer", size })),
    });
    renderPage([pageCreateRow({ input_json })]);
    const outline = within(await openCard()).getByTestId("ability-outline");
    expect(lines(outline).slice(1)).toEqual([
      "Space · Small (24 px)",
      "Space · Medium (48 px)",
      "Space · Large (96 px)",
    ]);
  });
});

// --- a plain page is unchanged -----------------------------------------------

describe("a request with only headings, paragraphs and lists", () => {
  it("has no Layout row and shows its lines as before", async () => {
    const input_json = pageInput({
      outline: [
        { type: "heading", level: 2, text: "Sale" },
        para,
        { type: "list", ordered: false, items: ["One"] },
      ],
    });
    renderPage([pageCreateRow({ input_json })]);
    const card = await openCard();
    expect(within(card).queryByText("Layout")).toBeNull();
    expect(within(card).queryByTestId("layout-summary")).toBeNull();
    expect(lines(within(card).getByTestId("ability-outline"))).toEqual([
      "Spring menu",
      "Heading 2 Sale",
      "Paragraph Big savings.",
      "List",
      "One",
    ]);
    expect(within(card).getByRole("button", { name: "Approve" })).toBeEnabled();
    expect(card).toHaveTextContent("Nothing is published. Undo moves the draft to the trash.");
  });

  it("still shows Editor, Asked by, Set up for and Timing", async () => {
    renderPage([pageCreateRow()]);
    const card = await openCard();
    for (const term of ["Editor", "Asked by", "Set up for", "Timing"]) {
      expect(within(card).getByText(term)).toBeInTheDocument();
    }
    expect(card).toHaveTextContent("WordPress block editor");
  });
});

// --- links to other websites ---------------------------------------------------

describe("button links", () => {
  async function chipsFor(urls: string[]): Promise<Record<string, string>> {
    const input_json = pageInput({
      outline: [{ type: "buttons", buttons: urls.slice(0, 3).map((url) => ({ text: "Go", url })) }],
    });
    renderPage([pageCreateRow({ input_json })]);
    const outline = within(await openCard()).getByTestId("ability-outline");
    const out: Record<string, string> = {};
    for (const line of lines(outline)) {
      const m = /^Links to (\S+) (This site|Another website: \S+)$/.exec(line);
      if (m) out[m[1]!] = m[2]!;
    }
    return out;
  }

  it("flags another website with an amber chip naming its host", async () => {
    const input_json = pageInput({ outline: [{ type: "buttons", buttons: [{ text: "Book", url: "https://book.example.net/x" }] }] });
    renderPage([pageCreateRow({ input_json })]);
    const outline = within(await openCard()).getByTestId("ability-outline");
    const chip = within(outline).getByText("Another website: book.example.net");
    expect(chip.className).toContain("warning");
    expect(chip.className).not.toContain("destructive");
  });

  it("a path on the site, and the site's own host, are this site", async () => {
    expect(await chipsFor(["/pricing", "https://example.com/pricing", "https://www.example.com/"])).toEqual({
      "/pricing": "This site",
      "https://example.com/pricing": "This site",
      "https://www.example.com/": "This site",
    });
  });

  it("a look-alike of the site's name is another website", async () => {
    expect(
      await chipsFor(["https://evil-example.com/", "https://example.com.evil.io/", "https://shop.example.com/"]),
    ).toEqual({
      "https://evil-example.com/": "Another website: evil-example.com",
      "https://example.com.evil.io/": "Another website: example.com.evil.io",
      "https://shop.example.com/": "Another website: shop.example.com",
    });
  });

  it("an unusual port is another website, and the host is shown with it", async () => {
    expect(await chipsFor(["https://example.com:8443/"])).toEqual({
      "https://example.com:8443/": "Another website: example.com:8443",
    });
  });

  it("the address is plain text, never a link", async () => {
    const input_json = pageInput({
      outline: [{ type: "buttons", buttons: [{ text: "Book", url: "https://book.example.net/x" }, { text: "Home", url: "/" }] }],
    });
    renderPage([pageCreateRow({ input_json })]);
    const card = await openCard();
    expect(within(card).queryAllByRole("link")).toHaveLength(0);
    expect(card.querySelectorAll("a[href]")).toHaveLength(0);
  });

  it("states the style of each button and where the group sits", async () => {
    const input_json = pageInput({
      outline: [
        {
          type: "buttons",
          align: "center",
          buttons: [{ text: "A", url: "/a" }, { text: "B", url: "/b", style: "outline" }, { text: "C", url: "/c", style: "fill" }],
        },
        { type: "buttons", align: "left", buttons: [{ text: "D", url: "/d" }] },
      ],
    });
    renderPage([pageCreateRow({ input_json })]);
    const outline = within(await openCard()).getByTestId("ability-outline");
    expect(lines(outline).filter((l) => /^(Buttons|Button )/.test(l))).toEqual([
      "Buttons · centred",
      'Button · Filled "A"',
      'Button · Outline "B"',
      'Button · Filled "C"',
      "Buttons",
      'Button · Filled "D"',
    ]);
  });
});

// --- text only ------------------------------------------------------------------

describe("the AI's words and the site's file names are only ever text", () => {
  const HOSTILE = '<img src=x onerror="window.__pwn=1"><script>window.__pwn=2</script>';
  // Every place the AI's or the site's text can land on the card.
  const SLOTS = [
    "title",
    "heading",
    "paragraph",
    "list item",
    "second list item",
    "alt text",
    "caption",
    "quote paragraph",
    "citation",
    "button text",
    "table header",
    "table cell",
    "the site's file name",
  ];

  function hostileRow(): AbilityRequest {
    const input_json = pageInput({
      title: HOSTILE,
      outline: [
        { type: "heading", level: 2, text: HOSTILE },
        { type: "paragraph", text: HOSTILE },
        { type: "list", ordered: false, items: [HOSTILE, HOSTILE] },
        { type: "image", attachment_id: 5, alt: HOSTILE, caption: HOSTILE },
        { type: "quote", paragraphs: [HOSTILE], citation: HOSTILE },
        { type: "buttons", buttons: [{ text: HOSTILE, url: "/go" }] },
        { type: "table", header: [HOSTILE], rows: [[HOSTILE]] },
      ],
    });
    return pageCreateRow({ input_json, media: [mediaFact(5, { filename: HOSTILE })] });
  }

  it("renders markup and script in every slot as inert text", async () => {
    renderPage([hostileRow()]);
    const card = await openCard();
    const outline = within(card).getByTestId("ability-outline");
    expect((outline.textContent ?? "").split(HOSTILE).length - 1).toBe(SLOTS.length);
    expect(outline.querySelectorAll("img, script, iframe, object, embed, svg, [onerror]")).toHaveLength(0);
    expect(document.querySelectorAll("img, script[src], [onerror]")).toHaveLength(0);
    expect((window as unknown as { __pwn?: number }).__pwn).toBeUndefined();
  });

  it("renders the same strings as inert text in the card heading data too", async () => {
    // The site label and host are the site's words: the same treatment.
    renderPage([pageCreateRow({ site_label: HOSTILE })]);
    const card = await screen.findByRole("article");
    expect(card.querySelectorAll("img, script, [onerror]")).toHaveLength(0);
    expect((window as unknown as { __pwn?: number }).__pwn).toBeUndefined();
  });
});

// --- a request the card cannot show in full ----------------------------------------

describe("a request the card cannot show in full", () => {
  const NOT_SHOWABLE = "WPMgr cannot show this request in full, so it cannot be approved here. Decline it and ask the AI again.";

  async function expectNotApprovable(card: HTMLElement): Promise<void> {
    expect(card).toHaveTextContent(NOT_SHOWABLE);
    expect(within(card).queryByTestId("ability-outline")).toBeNull();
    expect(within(card).queryByTestId("layout-summary")).toBeNull();
    expect(within(card).getByRole("button", { name: "Approve" })).toBeDisabled();
    expect(within(card).getByRole("button", { name: "Decline" })).toBeEnabled();
  }

  it("a block this dashboard does not know (the API is newer) cannot be approved", async () => {
    const input_json = pageInput({ outline: [para, { type: "cover", text: "x" }] });
    renderPage([pageCreateRow({ input_json })]);
    await expectNotApprovable(await openCard());
  });

  it("an extra key on a known block cannot be approved, and is not dropped from view", async () => {
    const input_json = pageInput({ outline: [{ type: "paragraph", text: "Hello", style: "color:red" }] });
    renderPage([pageCreateRow({ input_json })]);
    const card = await openCard();
    await expectNotApprovable(card);
    expect(card.textContent).not.toContain("Hello");
  });

  it("a layout the control plane's rules refuse cannot be approved", async () => {
    // A group inside a group.
    const input_json = pageInput({ outline: [{ type: "group", children: [{ type: "group", children: [para] }] }] });
    renderPage([pageCreateRow({ input_json })]);
    await expectNotApprovable(await openCard());
  });

  it("a button link the rules refuse cannot be approved", async () => {
    const input_json = pageInput({
      outline: [{ type: "buttons", buttons: [{ text: "Go", url: "javascript:alert(1)" }] }],
    });
    renderPage([pageCreateRow({ input_json })]);
    await expectNotApprovable(await openCard());
  });

  it("an image with no fact from the site cannot be approved", async () => {
    const input_json = pageInput({ outline: [{ type: "image", attachment_id: 42, alt: "A" }] });
    // page_media is null on the wire when the stored facts are unreadable.
    renderPage([pageCreateRow({ input_json, media: null })]);
    await expectNotApprovable(await openCard());
  });

  it("an image whose fact is for another attachment cannot be approved", async () => {
    const input_json = pageInput({
      outline: [
        { type: "image", attachment_id: 42, alt: "A" },
        { type: "image", attachment_id: 43, alt: "B" },
      ],
    });
    // Built straight, bypassing the fixture builder's order check on purpose:
    // this is the wire after a fact went missing.
    const row = { ...pageCreateRow({ input_json, media: [mediaFact(42), mediaFact(43)] }), page_media: [mediaFact(42)] };
    renderPage([row]);
    await expectNotApprovable(await openCard());
  });

  it("a request with a layout and a sound set of facts can be approved", async () => {
    const input_json = pageInput({
      outline: [{ type: "image", attachment_id: 42, alt: "A" }, { type: "buttons", buttons: [{ text: "Go", url: "/go" }] }],
    });
    renderPage([pageCreateRow({ input_json, media: [mediaFact(42)] })]);
    const card = await openCard();
    expect(card).not.toHaveTextContent("cannot show this request in full");
    expect(within(card).getByRole("button", { name: "Approve" })).toBeEnabled();
  });
});

describe("approving a layout", () => {
  it("posts the row's own presented_digest to its own site", async () => {
    const { input_json, media } = everyNode();
    approveAbilityMock.mockReturnValue(ok(pageCreateRow({ input_json, media, state: "approved", decided_at: "2026-10-01T09:58:00Z" })));
    renderPage([pageCreateRow({ input_json, media, presented_digest: "d".repeat(64) })]);
    const card = await openCard();
    fireEvent.click(within(card).getByRole("button", { name: "Approve" }));
    await waitFor(() =>
      expect(approveAbilityMock).toHaveBeenCalledWith({
        path: { siteId: "site-1", requestId: "pc-1" },
        body: { presented_digest: "d".repeat(64) },
      }),
    );
  });
});

// --- the classic editor ----------------------------------------------------------------

describe("a classic-editor page", () => {
  it("shows the subset the classic editor can hold, with its Layout row", async () => {
    const input_json = pageInput({
      editor: "wordpress_classic",
      outline: [
        { type: "heading", level: 2, text: "Menu" },
        { type: "image", attachment_id: 42, alt: "A" },
        { type: "quote", paragraphs: ["Good."] },
        { type: "table", rows: [["a", "b"]] },
        { type: "separator" },
      ],
    });
    renderPage([pageCreateRow({ input_json, editor: "wordpress_classic", media: [mediaFact(42)] })]);
    const card = await openCard();
    expect(card).toHaveTextContent("Classic editor");
    expect(within(card).getByTestId("layout-summary")).toHaveTextContent("1 image · 1 table · 1 quote");
    expect(within(card).getByRole("button", { name: "Approve" })).toBeEnabled();
  });

  it("does not show columns, which the classic editor cannot hold", async () => {
    const input_json = pageInput({
      editor: "wordpress_classic",
      outline: [{ type: "columns", columns: [{ children: [para] }, { children: [para] }] }],
    });
    renderPage([pageCreateRow({ input_json, editor: "wordpress_classic" })]);
    const card = await openCard();
    expect(card).toHaveTextContent("cannot show this request in full");
    expect(within(card).getByRole("button", { name: "Approve" })).toBeDisabled();
  });
});

// --- what the owner reads when it did not work out -------------------------------------

describe("when a layout request did not work out", () => {
  const decided = { decided_at: "2026-10-01T09:58:00Z" };
  const layoutInput = pageInput({ outline: [{ type: "image", attachment_id: 42, alt: "A" }] });
  const fail = (outcome_code: string) =>
    pageCreateRow({ input_json: layoutInput, media: [mediaFact(42)], state: "failed", outcome: "refused", outcome_code, ...decided });

  it.each([
    ["preview_changed", "The site or one of the chosen images changed since you approved. Ask the AI to try again."],
    [
      "image_not_available",
      "An image the AI chose is no longer in the media library, or WPMgr may not use it. Ask the AI to pick another image.",
    ],
    [
      "image_url_unusable",
      "WordPress gave an unusual address for one of the images, so WPMgr stopped. Ask the AI to pick another image.",
    ],
    [
      "layout_needs_block_editor",
      "This site uses the classic editor, which has no columns, sections, buttons or spacing. Ask the AI for a simpler page.",
    ],
    ["layout_invalid", "The AI's page layout wasn't valid. Ask it to try again."],
    ["link_invalid", "The AI's page layout wasn't valid. Ask it to try again."],
    [
      "sanitiser_changed_new_content",
      "This site changes page content when saving it, often because of a plugin, in a way WPMgr can't approve. Ask the AI to simplify the page.",
    ],
  ])("%s says what to do next, and never the code", async (code, advice) => {
    renderPage([fail(code)]);
    const card = await openCard();
    expect(card).toHaveTextContent(advice);
    expect(card).toHaveTextContent("Nothing was created.");
    expect(card.textContent).not.toContain(code);
    expect(within(card).queryByRole("button", { name: "Approve" })).toBeNull();
  });

  it("the layout is still shown after the request has been decided", async () => {
    renderPage([fail("image_not_available")]);
    const card = await openCard();
    expect(within(card).getByTestId("layout-summary")).toHaveTextContent("1 image");
    expect(lines(within(card).getByTestId("ability-outline"))).toContain("Image · photo-42.jpg · 1200 × 800");
  });

  it.each([
    ["entry_changed", "the AI tool changed after you approved it"],
    ["agent_outdated", "the WPMgr plugin on the site is too old"],
  ])("not sent (%s) says nothing was created", async (reason, words) => {
    renderPage([
      pageCreateRow({
        input_json: layoutInput,
        media: [mediaFact(42)],
        state: "not_sent",
        outcome: "not_sent",
        not_sent_reason: reason,
        ...decided,
      }),
    ]);
    const card = await openCard();
    expect(card).toHaveTextContent(`Nothing was sent: ${words}. Nothing was created.`);
  });

  it("a created draft says so and offers undo", async () => {
    renderPage([
      pageCreateRow({
        input_json: layoutInput,
        media: [mediaFact(42)],
        state: "done",
        outcome: "created",
        created_post_id: 7,
        undo_offered: true,
        ...decided,
      }),
    ]);
    const card = await openCard();
    expect(card).toHaveTextContent("Draft created.");
    expect(within(card).getByRole("button", { name: "Undo" })).toBeEnabled();
  });
});

// --- the fixtures hold themselves to the database ---------------------------------------

describe("the fixture builder refuses a state the database does not allow", () => {
  const legal = () => pageCreateRow();

  it("accepts the rows used above", () => {
    expect(() => assertDbShape(legal())).not.toThrow();
  });

  it.each([
    ["a pending row with an outcome", { outcome: "created" }],
    ["a failed row with no outcome", { state: "failed", decided_at: "2026-10-01T09:58:00Z" }],
    ["a decided row with no decision time", { state: "declined" }],
    ["a pending row with a decision time", { decided_at: "2026-10-01T09:58:00Z" }],
    ["a created outcome with no post", { state: "done", outcome: "created", decided_at: "2026-10-01T09:58:00Z" }],
    ["a pending row with a created post", { created_post_id: 7 }],
    ["a not_sent reason on a failed row", { state: "failed", outcome: "refused", not_sent_reason: "entry_changed", decided_at: "2026-10-01T09:58:00Z" }],
    ["a page-create row with a REST route", { route_id: "wp-v2-pages-update-fields" }],
    ["a page-create row with rest-write card facts", { card_facts: {} as never }],
    ["a grant_via the table refuses", { grant_via: "mcp" }],
    ["a host with a space", { site_host: "my site.example" }],
    ["an undo on a pending row", { undo_state: "undone", undo_available_until: "2026-10-01T10:30:00Z" }],
    ["a window that closes before it opens", { expires_at: "2026-10-01T09:00:00Z" }],
    ["card copy version 1 for a layout outline", { input_json: pageInput({ outline: [{ type: "separator" }] }), card_copy_version: 1 }],
  ])("%s", (_name, over) => {
    expect(() => pageCreateRow(over as Partial<AbilityRequest>)).toThrow(/not a state the database allows/);
  });

  it("refuses page_media that does not follow the outline's images", () => {
    const input_json = pageInput({ outline: [{ type: "image", attachment_id: 42, alt: "" }] });
    expect(() => pageCreateRow({ input_json, media: [mediaFact(43)] })).toThrow(/page_media/);
    expect(() => pageCreateRow({ input_json, media: [mediaFact(42)] })).not.toThrow();
  });
});
