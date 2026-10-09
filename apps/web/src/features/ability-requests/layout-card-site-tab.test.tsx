import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, within } from "@testing-library/react";
import type { AbilityRequest } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";
import { mediaFact, pageCreateRow, pageInput } from "@/test/ability-request-rows";

import { AiEditingSection } from "./ai-editing-section";

// The site's Content tab: the same page-layout card, with the site's own
// address known, which is what makes "View image in WordPress" available.
// Rendered through the real router, the real QueryClient and the real hooks;
// only the @wpmgr/api wire boundary is faked.

const { editingMock, listSiteMock } = vi.hoisted(() => ({
  editingMock: vi.fn(),
  listSiteMock: vi.fn(),
}));

vi.mock("@wpmgr/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wpmgr/api")>();
  return { ...actual, getSiteContentEditing: editingMock, listSiteAbilityRequests: listSiteMock };
});

function ok(data: unknown) {
  return Promise.resolve({ data, error: undefined, response: { status: 200 } });
}

function renderTab(rows: AbilityRequest[], siteUrl: string | null | undefined) {
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
});

const input = pageInput({
  outline: [
    { type: "image", attachment_id: 42, alt: "Team" },
    { type: "paragraph", text: "Hello" },
    { type: "image", attachment_id: 7, alt: "" },
    { type: "image", attachment_id: 42, alt: "Team again", align: "wide" },
  ],
});
const media = [mediaFact(42, { filename: "team-photo.jpg" }), mediaFact(7, { filename: "menu.png", mime: "image/png" })];

async function openCard(): Promise<HTMLElement> {
  return screen.findByRole("article", { name: /Create a draft/ });
}

describe("View image in WordPress", () => {
  it("links each placed image to its edit screen on the site, in a new tab", async () => {
    renderTab([pageCreateRow({ input_json: input, media })], "https://example.com");
    const card = await openCard();
    const links = within(card).getAllByRole("link", { name: /View image in WordPress/ });
    expect(links.map((a) => a.getAttribute("href"))).toEqual([
      "https://example.com/wp-admin/post.php?post=42&action=edit",
      "https://example.com/wp-admin/post.php?post=7&action=edit",
      "https://example.com/wp-admin/post.php?post=42&action=edit",
    ]);
    for (const a of links) {
      expect(a.getAttribute("target")).toBe("_blank");
      expect(a.getAttribute("rel")).toBe("noopener noreferrer");
    }
  });

  it("tells the links apart for a screen reader by the file name", async () => {
    renderTab([pageCreateRow({ input_json: input, media })], "https://example.com");
    const card = await openCard();
    expect(within(card).getAllByRole("link", { name: "View image in WordPress (team-photo.jpg)" })).toHaveLength(2);
    expect(within(card).getByRole("link", { name: "View image in WordPress (menu.png)" })).toBeInTheDocument();
  });

  it("follows a WordPress that lives in a sub-directory", async () => {
    renderTab([pageCreateRow({ input_json: input, media })], "https://example.com/blog/");
    const links = within(await openCard()).getAllByRole("link", { name: /View image in WordPress/ });
    expect(links[0]?.getAttribute("href")).toBe("https://example.com/blog/wp-admin/post.php?post=42&action=edit");
  });

  it("is built from the site's address and an integer only, never from the AI's or the site's words", async () => {
    const hostile = 'x"><a href="https://evil.example">pwn</a>';
    renderTab(
      [pageCreateRow({ input_json: input, media: [mediaFact(42, { filename: hostile }), mediaFact(7)] })],
      "https://example.com",
    );
    const card = await openCard();
    const hrefs = within(card)
      .getAllByRole("link")
      .map((a) => a.getAttribute("href"));
    expect(hrefs).toHaveLength(3);
    for (const href of hrefs) expect(href).toMatch(/^https:\/\/example\.com\/wp-admin\/post\.php\?post=\d+&action=edit$/);
    expect(card.querySelectorAll('a[href*="evil"]')).toHaveLength(0);
  });

  it.each([
    ["no address", undefined],
    ["a null address", null],
    ["an empty address", ""],
    ["a javascript address", "javascript:alert(1)"],
    ["a data address", "data:text/html,x"],
    ["an address that is not a URL", "not a url"],
  ])("is not offered with %s", async (_name, siteUrl) => {
    renderTab([pageCreateRow({ input_json: input, media })], siteUrl);
    const card = await openCard();
    expect(within(card).getByTestId("layout-summary")).toHaveTextContent("3 images");
    expect(within(card).queryAllByRole("link")).toHaveLength(0);
  });

  it("still names every image by its file name when no link is offered", async () => {
    renderTab([pageCreateRow({ input_json: input, media })], undefined);
    const outline = within(await openCard()).getByTestId("ability-outline");
    expect(within(outline).getAllByText("team-photo.jpg")).toHaveLength(2);
    expect(within(outline).getByText("menu.png")).toBeInTheDocument();
  });
});
