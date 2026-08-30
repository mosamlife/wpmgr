import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";
import { ShellContext, type ShellState } from "@/components/layout/app-shell-context";
import { Sidebar } from "@/components/layout/sidebar";
import { useMe } from "@/features/auth/use-auth";
import { useSites } from "@/features/sites/use-sites";
import { mockQueryResult } from "@/test/query-mocks";
import type { Site } from "@wpmgr/api";

// FOUND BY THE MUTATION SWEEP, AND IT IS THE ONE THE OWNER ASKED FOR.
//
// Repointing the sidebar's "AI" entry from /ai to /email left the entire suite
// green. The reported complaint was "I can't see in the ui what's the route for
// this all ai settings? it's not in the sidebar at all" -- so a silent
// regression of this exact nav entry reproduces the bug the slice exists to
// fix, and nothing would have said a word.
//
// There was no sidebar test in this codebase at all before this file
// (components/layout carried only top-bar-helpers.test.ts), so nav placement
// for EVERY entry was unpinned. This covers the AI entry and the two placement
// decisions either side of it.

vi.mock("@/features/auth/use-auth", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/auth/use-auth")>();
  return { ...actual, useMe: vi.fn() };
});
vi.mock("@/features/sites/use-sites", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/sites/use-sites")>();
  return { ...actual, useSites: vi.fn() };
});

const mockedMe = vi.mocked(useMe);
const mockedSites = vi.mocked(useSites);

const SHELL: ShellState = {
  collapsed: false,
  toggleCollapsed: () => {},
  mobileOpen: false,
  setMobileOpen: () => {},
};

beforeEach(() => {
  vi.clearAllMocks();
  // A normal tenant operator, not a superadmin: the superadmin branch renders a
  // completely different nav and would make every assertion below vacuous.
  mockedMe.mockReturnValue(
    mockQueryResult({ data: { user: { is_superadmin: false } } }) as ReturnType<typeof useMe>,
  );
  mockedSites.mockReturnValue(mockQueryResult<Site[]>({ data: [] }));
});

function renderSidebar() {
  return renderWithProviders(
    <ShellContext.Provider value={SHELL}>
      <Sidebar />
    </ShellContext.Provider>,
    { withRouter: true, initialPath: "/sites" },
  );
}

describe("the sidebar's AI entry", () => {
  it("renders the sidebar for a tenant operator at all", async () => {
    // Guard against the whole file passing vacuously if the render throws into
    // an empty tree or the superadmin branch takes over.
    renderSidebar();
    expect(await screen.findByRole("navigation", { name: /primary/i })).toBeInTheDocument();
    // Matched on href rather than accessible name: the Sites leaf carries a
    // live count badge, so its name is not a stable string to assert on.
    const hrefs = screen.getAllByRole("link").map((a) => a.getAttribute("href"));
    expect(hrefs).toContain("/sites");
    expect(hrefs).toContain("/email");
  });

  it("carries an entry pointing at /ai", async () => {
    renderSidebar();
    await screen.findByRole("navigation", { name: /primary/i });

    // MATCHED ON THE ROUTE, NOT THE LABEL. An earlier version of this asserted
    // the accessible name was exactly "AI", and the over-fire check caught it:
    // renaming the entry to "AI connections" is correct work, and it reddened.
    // A guard that blocks correct work gets switched off, and then it guards
    // nothing. What must not regress is that /ai is reachable from the nav.
    const aiLink = screen
      .getAllByRole("link")
      .find((a) => a.getAttribute("href") === "/ai");
    expect(aiLink, "no sidebar link points at /ai").toBeDefined();
    // And it has to be labelled, or it is reachable only by people who read
    // markup.
    expect((aiLink?.textContent ?? "").trim().length).toBeGreaterThan(0);
  });

  it("keeps the wizard out of the sidebar", async () => {
    // /ai/connect hangs off the /ai page's primary action. The house rule is
    // that an authenticated route reached only from another page stays out of
    // the nav, and this pins the decision rather than leaving it to memory.
    renderSidebar();
    await screen.findByRole("navigation", { name: /primary/i });
    const hrefs = screen
      .getAllByRole("link")
      .map((a) => a.getAttribute("href"))
      .filter((h): h is string => h !== null);
    expect(hrefs.length).toBeGreaterThan(3);
    expect(hrefs).not.toContain("/ai/connect");
  });

  it("keeps the OAuth consent screen out of the sidebar", async () => {
    // /connect/ai is a redirect target an external client sends the browser to,
    // never a destination. Putting it in the nav would offer a page that only
    // works when it arrives carrying an authorization request.
    renderSidebar();
    await screen.findByRole("navigation", { name: /primary/i });
    const hrefs = screen
      .getAllByRole("link")
      .map((a) => a.getAttribute("href"))
      .filter((h): h is string => h !== null);
    expect(hrefs).not.toContain("/connect/ai");
  });
});
