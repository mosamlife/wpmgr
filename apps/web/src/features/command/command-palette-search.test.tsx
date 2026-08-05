import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import type { Site } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";
import { mockQueryResult } from "@/test/query-mocks";
import type { UseSitesOptions } from "@/features/sites/use-sites";
import { recordRecentSite } from "@/features/command/use-recent-sites";

import { CommandPalette } from "./command-palette";

// GH #349. The command palette searches the WHOLE organisation.
//
// THE REPORTED BUG: an agency with 24 enrolled sites pressed the palette
// shortcut, typed "iacop", and got "No matches." The palette rendered
// `recentSites`, a localStorage list of sites this browser had previously
// OPENED. With 24 enrolled and none visited, "No matches." was the correct
// answer to the wrong question, and it was also indistinguishable from a
// request that had not happened yet or had failed.
//
// Every test below fails against the pre-change palette except the two marked
// KEEP guard: recents were the one thing the old implementation got right and
// must survive. The GH #322 rollout gate has its own file
// (command-palette.test.tsx) and is the regression guard for that entry.

class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}
vi.stubGlobal("ResizeObserver", ResizeObserverStub);
Element.prototype.scrollIntoView = function scrollIntoView() {};

const useSitesMock =
  vi.fn<(options?: UseSitesOptions) => ReturnType<typeof mockQueryResult<Site[]>>>();

vi.mock("@/features/auth/use-auth", async () => {
  const actual =
    await vi.importActual<typeof import("@/features/auth/use-auth")>(
      "@/features/auth/use-auth",
    );
  return {
    ...actual,
    useMe: () => ({ data: undefined }),
    useLogout: () => ({ mutate: vi.fn() }),
  };
});

vi.mock("@/features/fleet/use-fleet-agents", () => ({
  useFleetAgentVersions: () => ({ data: undefined }),
}));

vi.mock("@/features/sites/use-sites", async (importOriginal) => {
  const actual =
    await importOriginal<typeof import("@/features/sites/use-sites")>();
  return {
    ...actual,
    useSites: (options?: UseSitesOptions) => useSitesMock(options),
  };
});

vi.mock("@/features/backups/use-bulk-backup", () => ({
  useBulkBackup: () => vi.fn(),
}));

function buildSite(overrides: Partial<Site> = {}): Site {
  return {
    id: "site-1",
    tenant_id: "t1",
    url: "https://iacop.example.com",
    name: "Iacop",
    status: "active",
    wp_version: "6.8",
    php_version: "8.3",
    health_status: "healthy",
    multisite: false,
    tags: [],
    ...overrides,
  } as unknown as Site;
}

/**
 * Tells the palette's two `useSites()` calls apart: the bare one (which feeds
 * "Run backup on all sites") and the SEARCH one, recognised by its `q` option.
 */
function mockSearch(result: ReturnType<typeof mockQueryResult<Site[]>>) {
  useSitesMock.mockImplementation((options?: UseSitesOptions) =>
    options && "q" in options ? result : mockQueryResult<Site[]>({ data: [] }),
  );
}

/** Type into the palette's search box. */
function typeTerm(term: string) {
  const input = screen.getByPlaceholderText("Search sites, runs, snapshots");
  fireEvent.change(input, { target: { value: term } });
}

/** Every option the SEARCH call was made with, in order. */
function searchCalls(): (UseSitesOptions | undefined)[] {
  return useSitesMock.mock.calls
    .map(([options]) => options)
    .filter((options) => options && "q" in options);
}

beforeEach(() => {
  vi.clearAllMocks();
  mockSearch(mockQueryResult<Site[]>({ data: [] }));
});

afterEach(() => {
  window.localStorage.clear();
});

describe("CommandPalette site search (GH #349)", () => {
  it("T1: finds a site the operator has NEVER opened (the reported case)", async () => {
    // Nothing in recents. This is precisely the reporter's state: sites
    // enrolled, none visited, so the old palette had nothing to offer and said
    // "No matches."
    mockSearch(
      mockQueryResult<Site[]>({
        data: [
          buildSite({
            id: "s-iacop",
            name: "Iacop",
            url: "https://iacop.example.com",
          }),
        ],
      }),
    );

    renderWithProviders(<CommandPalette open onClose={vi.fn()} />);
    typeTerm("iacop");

    expect(await screen.findByText("Go to Iacop")).toBeInTheDocument();
    expect(screen.queryByText("No matches.")).not.toBeInTheDocument();
  });

  it("T1b: sends the typed term to the server as `q` rather than filtering what is already loaded", async () => {
    renderWithProviders(<CommandPalette open onClose={vi.fn()} />);
    typeTerm("iacop");

    await waitFor(() => {
      expect(searchCalls().some((options) => options?.q === "iacop")).toBe(true);
    });
  });

  it("T1c: shows a server result whose visible label does not contain the term, because the SERVER decided it matches", async () => {
    // The contract also matches on URL and tags. A result whose name shares no
    // characters with the term must still be shown: re-filtering the server's
    // answer in the browser is the defect class this change is undoing.
    mockSearch(
      mockQueryResult<Site[]>({
        data: [
          buildSite({
            id: "s-tagged",
            name: "Northwind",
            url: "https://northwind.test",
            tags: ["iacop"],
          }),
        ],
      }),
    );

    renderWithProviders(<CommandPalette open onClose={vi.fn()} />);
    typeTerm("iacop");

    expect(await screen.findByText("Go to Northwind")).toBeInTheDocument();
  });

  it("T2 (KEEP guard): an empty box still shows recently visited sites, and searches for nothing", async () => {
    recordRecentSite({ id: "s-recent", url: "https://recent.example.com" });

    renderWithProviders(<CommandPalette open onClose={vi.fn()} />);

    expect(
      await screen.findByText("Go to recent.example.com"),
    ).toBeInTheDocument();
    // The search query stays disabled while there is nothing to search for.
    expect(searchCalls().length).toBeGreaterThan(0);
    expect(searchCalls().every((options) => options?.enabled === false)).toBe(
      true,
    );
  });

  it("T2b: typing replaces recents with the organisation-wide answer", async () => {
    recordRecentSite({ id: "s-recent", url: "https://recent.example.com" });
    mockSearch(
      mockQueryResult<Site[]>({
        data: [buildSite({ id: "s-iacop", name: "Iacop" })],
      }),
    );

    renderWithProviders(<CommandPalette open onClose={vi.fn()} />);
    expect(
      await screen.findByText("Go to recent.example.com"),
    ).toBeInTheDocument();

    typeTerm("iacop");

    expect(await screen.findByText("Go to Iacop")).toBeInTheDocument();
    expect(
      screen.queryByText("Go to recent.example.com"),
    ).not.toBeInTheDocument();
  });

  it("T3a: says it is searching while the request is in flight, never 'No matches.'", async () => {
    mockSearch(
      mockQueryResult<Site[]>({
        data: undefined,
        isPending: true,
        isFetching: true,
        isSuccess: false,
        status: "pending",
      }),
    );

    renderWithProviders(<CommandPalette open onClose={vi.fn()} />);
    typeTerm("iacop");

    expect(await screen.findByText("Searching sites...")).toBeInTheDocument();
    expect(screen.queryByText("No matches.")).not.toBeInTheDocument();
    expect(screen.queryByText(/No sites match/)).not.toBeInTheDocument();
  });

  it("T3b: says the request failed, offers a retry, and never claims nothing matched", async () => {
    const refetch = vi.fn();
    mockSearch(
      mockQueryResult<Site[]>({
        data: undefined,
        isError: true,
        isSuccess: false,
        status: "error",
        error: new Error("network down"),
        refetch,
      }),
    );

    renderWithProviders(<CommandPalette open onClose={vi.fn()} />);
    typeTerm("iacop");

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Could not search sites.");
    expect(screen.queryByText("No matches.")).not.toBeInTheDocument();
    expect(screen.queryByText(/No sites match/)).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(refetch).toHaveBeenCalled();
  });

  it("T3c: only a settled, successful, empty answer says nothing matched, and it names the term", async () => {
    mockSearch(mockQueryResult<Site[]>({ data: [] }));

    renderWithProviders(<CommandPalette open onClose={vi.fn()} />);
    typeTerm("zzzz");

    expect(
      await screen.findByText('No sites match "zzzz".'),
    ).toBeInTheDocument();
    expect(screen.queryByText("Searching sites...")).not.toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("T3d: the three states are told apart on screen", async () => {
    // Same term, three different server states, three different lines. The bug
    // was that all three rendered the same sentence, so an operator could not
    // tell "not asked yet" from "asked and failed" from "asked and nothing
    // matched".
    const lines = new Set<string>();

    const cases: [ReturnType<typeof mockQueryResult<Site[]>>, string | RegExp][] =
      [
        [
          mockQueryResult<Site[]>({
            data: undefined,
            isPending: true,
            isFetching: true,
            isSuccess: false,
            status: "pending",
          }),
          "Searching sites...",
        ],
        [
          mockQueryResult<Site[]>({
            data: undefined,
            isError: true,
            isSuccess: false,
            status: "error",
            error: new Error("boom"),
          }),
          /Could not search sites\./,
        ],
        [mockQueryResult<Site[]>({ data: [] }), 'No sites match "iacop".'],
      ];

    for (const [state, expected] of cases) {
      mockSearch(state);
      const { unmount } = renderWithProviders(
        <CommandPalette open onClose={vi.fn()} />,
      );
      typeTerm("iacop");
      // Waiting on the state's OWN line, not on "some status element", is what
      // makes this meaningful: the in-flight line shows first in every case
      // (the debounce window is itself in flight), so a test that grabbed the
      // first status it saw would pass while the bug was present.
      await screen.findByText(expected);
      const node = screen.queryByRole("status") ?? screen.getByRole("alert");
      lines.add(node.textContent ?? "");
      unmount();
    }

    expect(lines.size).toBe(3);
  });

  it("keeps the fleet-wide verbs reachable while a search is running", async () => {
    // A search must not take the palette over: "Run backup on all sites" and
    // the other command groups still filter and select as before.
    mockSearch(
      mockQueryResult<Site[]>({
        data: [buildSite({ id: "s-1", name: "Backup Cafe" })],
      }),
    );

    renderWithProviders(<CommandPalette open onClose={vi.fn()} />);
    typeTerm("backup");

    expect(
      await screen.findByText("Run backup on all sites"),
    ).toBeInTheDocument();
    expect(screen.getByText("Go to Backups")).toBeInTheDocument();
  });
});
