import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import type { AbilityRequest } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";
import { editFacts, elementorFacts, pageCreateRow, pageEditRow, pageInput } from "@/test/ability-request-rows";

import { AiEditingSection } from "./ai-editing-section";

// What an AI page edit's card says once the change is made, and how a person
// undoes it: one change at a time, the newest first, for fourteen days (section
// 5.3 of the BF-E design, the owner's acceptance copy). Rendered through the
// real router, the real QueryClient and the real hooks; only the @wpmgr/api
// wire boundary is faked. Whether an undo is offered is the server's word
// (undo_offered: true only on the newest applied edit of a page that has not
// been undone, apps/api/internal/abilityrequest/undo.go UndoOffered); the card
// shows exactly that, plus its own clock for the end of the window.

const { editingMock, listSiteMock, undoMock } = vi.hoisted(() => ({
  editingMock: vi.fn(),
  listSiteMock: vi.fn(),
  undoMock: vi.fn(),
}));

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));

vi.mock("@wpmgr/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wpmgr/api")>();
  return {
    ...actual,
    getSiteContentEditing: editingMock,
    listSiteAbilityRequests: listSiteMock,
    undoAbilityRequest: undoMock,
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
  undoMock.mockReset();
  vi.mocked(toast.success).mockClear();
  vi.mocked(toast.error).mockClear();
});

const flat = (el: Element | null | undefined) => (el?.textContent ?? "").replace(/\s+/g, " ").trim();
const DAY = 86_400_000;
const inDays = (n: number) => new Date(Date.now() + n * DAY).toISOString();

/** An applied page edit of page 418 with an undo that is open (or not) in the window. */
function applied(id: string, over: Partial<AbilityRequest> = {}): AbilityRequest {
  return pageEditRow({
    id,
    state: "done",
    outcome: "applied",
    decided_at: "2026-10-01T09:58:00Z",
    undo_state: "available",
    undo_available_until: inDays(13),
    undo_offered: false,
    ...over,
  });
}

/** The newest edit of the page, which the server offers the undo on, and the one before it. */
function twoEdits(): { newest: AbilityRequest; older: AbilityRequest } {
  return {
    newest: applied("pe-new", { undo_offered: true }),
    older: applied("pe-old", { created_at: "2026-10-01T09:20:00Z", decided_at: "2026-10-01T09:25:00Z" }),
  };
}

async function cardsOf(count: number): Promise<HTMLElement[]> {
  const cards = await screen.findAllByRole("article", { name: /Change a draft in Elementor/ });
  expect(cards).toHaveLength(count);
  return cards;
}

// --- the change is done ----------------------------------------------------------

describe("an applied page edit", () => {
  it("says the draft was changed, and links to the page in Elementor and its preview", async () => {
    renderTab([applied("pe-new", { undo_offered: true })]);
    const [card] = await cardsOf(1);
    const line = within(card!).getByTestId("status-line");
    expect(flat(line)).toBe("Draft changed in Elementor.");
    expect(line).toHaveAttribute("data-tone", "neutral");
    const open = within(card!).getByRole("link", { name: "Open in Elementor" });
    expect(open.getAttribute("href")).toBe("https://example.com/wp-admin/post.php?post=418&action=elementor");
    const preview = within(card!).getByRole("link", { name: "Preview" });
    expect(preview.getAttribute("href")).toBe("https://example.com/?page_id=418&preview=true");
    for (const link of [open, preview]) {
      expect(link.getAttribute("target")).toBe("_blank");
      expect(link.getAttribute("rel")).toBe("noopener noreferrer");
    }
    expect(within(card!).queryByRole("button", { name: "Approve" })).toBeNull();
  });

  it("offers no link when the site's address is not a plain web address", async () => {
    renderTab([applied("pe-new", { undo_offered: true })], "javascript:alert(1)");
    const [card] = await cardsOf(1);
    expect(within(card!).queryAllByRole("link")).toHaveLength(0);
    expect(flat(within(card!).getByTestId("status-line"))).toBe("Draft changed in Elementor.");
  });
});

// --- undo, newest first, for fourteen days ----------------------------------------

describe("undoing page edits one at a time", () => {
  it("offers Undo this change on the newest applied edit only, and asks to undo the later one first on the older", async () => {
    const { newest, older } = twoEdits();
    renderTab([newest, older]);
    const [first, second] = await cardsOf(2);
    expect(within(first!).getByRole("button", { name: "Undo this change" })).toBeEnabled();
    expect(within(first!).queryByTestId("undo-later-first")).toBeNull();
    expect(within(second!).queryByRole("button", { name: /Undo/ })).toBeNull();
    const wait = within(second!).getByTestId("undo-later-first");
    expect(flat(wait)).toBe("Undo the later change first.");
    expect(wait).toHaveClass("text-muted-foreground");
  });

  it("takes the offer from the server's word, not from the order of the list", async () => {
    // The same two edits with the offer on the one listed second.
    const { newest, older } = twoEdits();
    renderTab([{ ...newest, undo_offered: false }, { ...older, undo_offered: true }]);
    const [first, second] = await cardsOf(2);
    expect(within(first!).queryByRole("button", { name: /Undo/ })).toBeNull();
    expect(flat(within(first!).getByTestId("undo-later-first"))).toBe("Undo the later change first.");
    expect(within(second!).getByRole("button", { name: "Undo this change" })).toBeEnabled();
  });

  it("says Undo is no longer available on every edit once the fourteen days are over", async () => {
    const expired = inDays(-1);
    const { newest, older } = twoEdits();
    // Even where the server still offered it, the window's end on this clock decides.
    renderTab([
      { ...newest, undo_available_until: expired },
      { ...older, undo_available_until: expired },
    ]);
    const cards = await cardsOf(2);
    for (const card of cards) {
      expect(flat(within(card).getByTestId("undo-expired"))).toBe("Undo is no longer available (14 days).");
      expect(within(card).queryByRole("button", { name: /Undo/ })).toBeNull();
      expect(within(card).queryByTestId("undo-later-first")).toBeNull();
    }
  });

  it("says Undo is not available for an applied edit that kept no copy to undo with", async () => {
    renderTab([applied("pe-new", { undo_state: undefined, undo_available_until: undefined })]);
    const [card] = await cardsOf(1);
    expect(flat(within(card!).getByTestId("undo-none"))).toBe("Undo is not available for this change.");
    expect(within(card!).queryByRole("button", { name: /Undo/ })).toBeNull();
  });
});

// --- the confirmation and the undo itself -------------------------------------------

describe("the confirmation to undo a page edit", () => {
  async function askToUndo(): Promise<HTMLElement> {
    const { newest, older } = twoEdits();
    renderTab([newest, older]);
    const [first] = await cardsOf(2);
    fireEvent.click(within(first!).getByRole("button", { name: "Undo this change" }));
    return screen.findByRole("dialog");
  }

  it("says what is put back and what stays, and focuses Cancel", async () => {
    const dialog = await askToUndo();
    expect(within(dialog).getByRole("heading", { name: "Undo this change?" })).toBeInTheDocument();
    expect(flat(dialog.querySelector("p"))).toBe(
      'WPMgr puts back what this change wrote on "Spring sale". Changes approved after it that touched other parts of the page, like a featured image, stay.',
    );
    await waitFor(() => expect(within(dialog).getByRole("button", { name: "Cancel" })).toHaveFocus());
    expect(within(dialog).getByRole("button", { name: "Undo" })).toBeEnabled();
  });

  it("changes nothing when it is cancelled", async () => {
    const dialog = await askToUndo();
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(undoMock).not.toHaveBeenCalled();
  });

  it("undoes this change and no other, shows it running, then offers the undo of the one before it", async () => {
    const { newest, older } = twoEdits();
    renderTab([newest, older]);
    const [first, second] = await cardsOf(2);
    // The undo is slow: the card says so until the site answers.
    let settle: (v: unknown) => void = () => undefined;
    undoMock.mockReturnValue(new Promise((resolve) => (settle = resolve)));
    fireEvent.click(within(first!).getByRole("button", { name: "Undo this change" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Undo" }));

    await waitFor(() =>
      expect(undoMock).toHaveBeenCalledWith({ path: { siteId: "site-1", requestId: "pe-new" }, body: {} }),
    );
    expect(undoMock).toHaveBeenCalledTimes(1);
    const running = await within(first!).findByRole("button", { name: "Undoing…" });
    expect(running).toBeDisabled();

    // The site has put the page back: the newest edit is undone and the one before it is now the newest.
    const undone = applied("pe-new", { undo_state: "undone" });
    listSiteMock.mockReturnValue(ok({ requests: [undone, { ...older, undo_offered: true }], limit: 50, offset: 0 }));
    settle({ data: undone, error: undefined, response: { status: 200 } });

    await waitFor(() =>
      expect(flat(within(first!).getByTestId("status-line"))).toBe(
        "Change undone. The page is back as it was before this change.",
      ),
    );
    expect(within(first!).queryByRole("button", { name: /Undo/ })).toBeNull();
    expect(await within(second!).findByRole("button", { name: "Undo this change" })).toBeEnabled();
    expect(within(second!).queryByTestId("undo-later-first")).toBeNull();
    expect(toast.success).toHaveBeenCalledWith("Change undone.");
  });
});

// --- the page the AI created, with changes made to it since --------------------------

describe("the page creation of a draft that has had changes since", () => {
  /** A done page creation of draft 418 made before the two edits above. */
  function creation(over: Partial<AbilityRequest> = {}): AbilityRequest {
    return pageCreateRow({
      id: "pc-1",
      input_json: pageInput({ editor: "builder:elementor", outline: [{ type: "heading", level: 2, text: "Spring sale" }] }),
      page_builder: elementorFacts(),
      state: "done",
      outcome: "created",
      created_post_id: 418,
      created_at: "2026-10-01T09:00:00Z",
      decided_at: "2026-10-01T09:05:00Z",
      undo_state: "available",
      undo_available_until: inDays(13),
      undo_offered: true,
      ...over,
    });
  }

  it("keeps Move draft to trash and says it also covers the changes made since", async () => {
    const { newest, older } = twoEdits();
    renderTab([newest, older, creation()]);
    const create = await screen.findByRole("article", { name: /Create a draft/ });
    expect(flat(within(create).getByTestId("also-covers"))).toBe("Also covers the 2 AI changes made since.");
    expect(within(create).getByRole("button", { name: "Move draft to trash" })).toBeEnabled();
    expect(within(create).queryByRole("button", { name: "Undo" })).toBeNull();
  });

  it("says Also covers the 1 AI change for one change", async () => {
    renderTab([twoEdits().newest, creation()]);
    const create = await screen.findByRole("article", { name: /Create a draft/ });
    expect(flat(within(create).getByTestId("also-covers"))).toBe("Also covers the 1 AI change made since.");
  });

  it("offers a plain Undo, with no line about changes, when nothing was changed since", async () => {
    renderTab([creation()]);
    const create = await screen.findByRole("article", { name: /Create a draft/ });
    expect(within(create).queryByTestId("also-covers")).toBeNull();
    expect(within(create).getByRole("button", { name: "Undo" })).toBeEnabled();
  });

  it("does not count a change made before the page was created, to another page, or not applied", async () => {
    const { newest } = twoEdits();
    renderTab([
      applied("pe-early", { created_at: "2026-10-01T08:00:00Z", decided_at: "2026-10-01T08:05:00Z" }),
      applied("pe-other", { post_id: 500, page_edit: editFacts({ post: { id: 500, from_the_site: { title: "Other" } } }) }),
      {
        ...newest,
        id: "pe-failed",
        state: "failed",
        outcome: "refused",
        outcome_code: "snapshot_failed",
        undo_state: undefined,
        undo_available_until: undefined,
        undo_offered: false,
      },
      creation(),
    ]);
    const create = await screen.findByRole("article", { name: /Create a draft/ });
    expect(within(create).queryByTestId("also-covers")).toBeNull();
    expect(within(create).getByRole("button", { name: "Undo" })).toBeEnabled();
  });

  it("says why the draft was not moved to the trash when someone edited it in WordPress after the changes", async () => {
    const { newest, older } = twoEdits();
    renderTab([
      newest,
      older,
      creation({ undo_state: "refused_conflict", undo_offered: false }),
    ]);
    const create = await screen.findByRole("article", { name: /Create a draft/ });
    expect(
      within(create).getByText("WPMgr won't move this draft to the trash: someone edited it in WordPress after WPMgr's changes."),
    ).toBeInTheDocument();
    expect(within(create).queryByRole("button", { name: /trash|Undo/ })).toBeNull();
  });
});
