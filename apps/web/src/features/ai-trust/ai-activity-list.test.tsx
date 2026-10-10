import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, screen, within } from "@testing-library/react";
import type { AbilityRequest, AiActivityItem } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";
import { autoApproved, pageCreateRow, pageEditRow, personApproved } from "@/test/ability-request-rows";

import { AiActivityList } from "./ai-activity-list";

// The AI activity feed (/ai/activity, approval tiers design 8.7) lists every
// request a person or a site's setting approved, from the same table the cards
// read, so a page edit (wpmgr/page-edit) shows up in it beside a page creation.
// Each row is WPMgr's own title and status line, and Details opens the same
// card the queues show. Rendered through the real hooks and the real
// QueryClient; only the @wpmgr/api wire boundary is faked. The items are the
// shape of AIActivityItem in packages/openapi/openapi.yaml: `kind` is
// ability_request and `request` is an AbilityRequest, held to the states the
// database allows by test/ability-request-rows.ts.

const { activityMock } = vi.hoisted(() => ({ activityMock: vi.fn() }));

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));

vi.mock("@wpmgr/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wpmgr/api")>();
  return { ...actual, listAiActivity: activityMock };
});

function ok(data: unknown) {
  return Promise.resolve({ data, error: undefined, response: { status: 200 } });
}

beforeEach(() => {
  activityMock.mockReset();
});

const flat = (el: Element | null | undefined) => (el?.textContent ?? "").replace(/\s+/g, " ").trim();
const inDays = (n: number) => new Date(Date.now() + n * 86_400_000).toISOString();

const CREATED_AT = "2026-10-01T09:00:00Z";
const CREATED_DECIDED = "2026-10-01T09:01:00Z";
const EDITED_AT = "2026-10-01T09:55:00Z";
const EDITED_DECIDED = "2026-10-01T09:56:00Z";

/** A draft page a setting approved and the site created (post 418, the post the edits below change). */
function created(over: Partial<AbilityRequest> = {}): AbilityRequest {
  return pageCreateRow({
    id: "pc-1",
    created_at: CREATED_AT,
    expires_at: "2026-10-01T10:00:00Z",
    state: "done",
    outcome: "created",
    created_post_id: 418,
    decided_at: CREATED_DECIDED,
    undo_state: "available",
    undo_available_until: inDays(13),
    undo_offered: true,
    ...autoApproved(),
    ...over,
  });
}

/** A change to that draft a setting approved and the site applied. */
function edited(over: Partial<AbilityRequest> = {}): AbilityRequest {
  return pageEditRow({
    id: "pe-1",
    created_at: EDITED_AT,
    state: "done",
    outcome: "applied",
    decided_at: EDITED_DECIDED,
    undo_state: "available",
    undo_available_until: inDays(13),
    undo_offered: true,
    ...autoApproved(),
    ...over,
  });
}

function item(request: AbilityRequest): AiActivityItem {
  return { kind: "ability_request", request };
}

function renderFeed(requests: AbilityRequest[]) {
  activityMock.mockReturnValue(ok({ items: requests.map(item), next_cursor: null }));
  return renderWithProviders(
    <AiActivityList filters={{ filter: "all" }} currentUserId="user-1" refetchInterval={30_000} />,
  );
}

async function rowOf(title: string): Promise<HTMLElement> {
  const li = (await screen.findByText(title)).closest("li");
  if (li === null) throw new Error(`no row titled ${title}`);
  return li;
}

describe("a page edit in the feed", () => {
  it("is titled by what it did and says how it ended, not as a page creation", async () => {
    renderFeed([edited()]);
    const row = await rowOf("Change a draft in Elementor · Shop One");
    expect(flat(row)).toContain("Draft changed in Elementor.");
    expect(flat(row)).not.toContain("Create a draft page");
    expect(flat(row)).not.toContain("Draft created.");
  });

  it("says how a failed page edit ended in its own words", async () => {
    renderFeed([
      edited({
        state: "failed",
        outcome: "refused",
        outcome_code: "conflict",
        outcome_detail: "editor_open",
        undo_state: null,
        undo_available_until: null,
        undo_offered: false,
      }),
    ]);
    const row = await rowOf("Change a draft in Elementor · Shop One");
    expect(flat(row)).toContain("Someone has this page open in Elementor. Nothing changed.");
  });

  it("says a change ran automatically when a setting approved it, and not when a person did", async () => {
    renderFeed([edited({ id: "pe-auto" }), edited({ id: "pe-person", site_label: "Shop Two", ...personApproved() })]);
    const auto = await rowOf("Change a draft in Elementor · Shop One");
    const person = await rowOf("Change a draft in Elementor · Shop Two");
    expect(within(auto).getByTestId("ran-automatically")).toHaveTextContent("Ran automatically");
    expect(within(person).queryByTestId("ran-automatically")).toBeNull();
  });

  it("opens the page edit's own card under Details, with what allowed it and its undo", async () => {
    renderFeed([edited()]);
    const row = await rowOf("Change a draft in Elementor · Shop One");
    fireEvent.click(within(row).getByRole("button", { name: "Details: Change a draft in Elementor · Shop One" }));
    const card = await within(row).findByRole("article", { name: "Change a draft in Elementor · Shop One" });
    expect(within(card).getByText("Allowed by", { selector: "dt" })).toBeInTheDocument();
    expect(within(card).getByRole("button", { name: "Undo this change" })).toBeEnabled();
  });
});

describe("a page creation in the feed", () => {
  it("still reads as a page creation", async () => {
    renderFeed([created()]);
    const row = await rowOf("Create a draft page · Shop One");
    expect(flat(row)).toContain("Draft created.");
    expect(within(row).getByTestId("ran-automatically")).toBeInTheDocument();
  });

  it("says in its Details how many of the AI's later changes to the draft its undo also covers", async () => {
    renderFeed([edited(), created()]);
    const row = await rowOf("Create a draft page · Shop One");
    fireEvent.click(within(row).getByRole("button", { name: "Details: Create a draft page · Shop One" }));
    const card = await within(row).findByRole("article", { name: "Create a draft page · Shop One" });
    expect(flat(within(card).getByTestId("also-covers"))).toBe("Also covers the 1 AI change made since.");
    expect(within(card).getByRole("button", { name: "Move draft to trash" })).toBeEnabled();
  });

  it("says nothing of later changes when the draft has none", async () => {
    renderFeed([created()]);
    const row = await rowOf("Create a draft page · Shop One");
    fireEvent.click(within(row).getByRole("button", { name: "Details: Create a draft page · Shop One" }));
    const card = await within(row).findByRole("article", { name: "Create a draft page · Shop One" });
    expect(within(card).queryByTestId("also-covers")).toBeNull();
    expect(within(card).getByRole("button", { name: "Undo" })).toBeEnabled();
  });
});
