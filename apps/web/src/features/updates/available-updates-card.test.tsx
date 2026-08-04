import { describe, expect, it, vi } from "vitest";
import { screen, within } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";
import { mockMutationResult, mockQueryResult } from "@/test/query-mocks";

import { AvailableUpdatesCard } from "./available-updates-card";
import type { UpdateRun, UpdateRunCreate } from "@wpmgr/api";

import type { SiteAvailableUpdates } from "./types";
import type { RowUpdate } from "./use-row-update";
import type { SiteAgentUpdate } from "./use-site-agent-update";
import { useAvailableUpdates, useRefreshSiteUpdates } from "./use-available-updates";
import { useSiteAgentUpdate } from "./use-site-agent-update";
import { useCoreRowUpdate, useRowUpdate } from "./use-row-update";
import { useCreateUpdateRun } from "./use-updates";

// GH #314: the card used to render "All up to date" with a green check for
// `total === 0`, but `total` only ever counted the components WPMgr manages.
// The control plane strips the WPMgr agent from that projection on purpose
// (0.61.97), so a site whose agent was behind read as fully current here while
// its own wp-admin was offering the agent update.

vi.mock("./use-available-updates", async (importOriginal) => {
  const actual =
    await importOriginal<typeof import("./use-available-updates")>();
  return {
    ...actual,
    useAvailableUpdates: vi.fn(),
    useRefreshSiteUpdates: vi.fn(),
  };
});

vi.mock("./use-site-agent-update", async (importOriginal) => {
  const actual =
    await importOriginal<typeof import("./use-site-agent-update")>();
  return { ...actual, useSiteAgentUpdate: vi.fn() };
});

vi.mock("./use-row-update", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./use-row-update")>();
  return { ...actual, useRowUpdate: vi.fn(), useCoreRowUpdate: vi.fn() };
});

vi.mock("./use-updates", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./use-updates")>();
  return { ...actual, useCreateUpdateRun: vi.fn() };
});

const mockedUseAvailableUpdates = vi.mocked(useAvailableUpdates);
const mockedUseRefresh = vi.mocked(useRefreshSiteUpdates);
const mockedUseSiteAgentUpdate = vi.mocked(useSiteAgentUpdate);
const mockedUseRowUpdate = vi.mocked(useRowUpdate);
const mockedUseCoreRowUpdate = vi.mocked(useCoreRowUpdate);
const mockedUseCreateUpdateRun = vi.mocked(useCreateUpdateRun);

const IDLE_ROW: RowUpdate = {
  state: "idle",
  runId: null,
  taskId: null,
  isStarting: false,
  trigger: vi.fn(),
  retry: vi.fn(),
};

const OUTDATED_AGENT: SiteAgentUpdate = {
  status: "outdated",
  version: "0.61.100",
  latestVersion: "0.61.121",
  referenceSource: "published",
  channelAvailable: false,
};

function setup(
  data: SiteAvailableUpdates,
  agent: SiteAgentUpdate | null,
): void {
  mockedUseAvailableUpdates.mockReturnValue(
    mockQueryResult<SiteAvailableUpdates>({ data }),
  );
  mockedUseRefresh.mockReturnValue(
    mockMutationResult<void, void>({}),
  );
  mockedUseSiteAgentUpdate.mockReturnValue(agent);
  mockedUseRowUpdate.mockReturnValue(IDLE_ROW);
  mockedUseCoreRowUpdate.mockReturnValue(IDLE_ROW);
  mockedUseCreateUpdateRun.mockReturnValue(
    mockMutationResult<UpdateRun, UpdateRunCreate>({}),
  );
}

function payload(
  overrides: Partial<SiteAvailableUpdates> = {},
): SiteAvailableUpdates {
  return {
    site_id: "site-1",
    core_update: null,
    items: [],
    as_of: "2026-08-04T10:00:00Z",
    ...overrides,
  };
}

describe("AvailableUpdatesCard agent honesty (GH #314)", () => {
  it("never claims everything is up to date when it only knows the managed components are", () => {
    setup(payload(), OUTDATED_AGENT);
    renderWithProviders(<AvailableUpdatesCard siteId="site-1" />);

    // The exact reported string must not come back.
    expect(screen.queryByText("All up to date")).not.toBeInTheDocument();
    expect(
      screen.getByText("All managed components are up to date"),
    ).toBeInTheDocument();
    expect(screen.getByTestId("agent-update-notice")).toBeInTheDocument();
  });

  it("scopes the header badge to the managed set too", () => {
    setup(payload(), OUTDATED_AGENT);
    renderWithProviders(<AvailableUpdatesCard siteId="site-1" />);

    expect(screen.getByText("No managed updates")).toBeInTheDocument();
    expect(screen.queryByText("Up to date")).not.toBeInTheDocument();
  });

  it("keeps the qualified wording when the agent is current, so the claim never depends on a second query", () => {
    setup(payload(), { ...OUTDATED_AGENT, status: "current", version: "0.61.121" });
    renderWithProviders(<AvailableUpdatesCard siteId="site-1" />);

    expect(screen.queryByText("All up to date")).not.toBeInTheDocument();
    expect(
      screen.getByText("All managed components are up to date"),
    ).toBeInTheDocument();
  });

  it("shows no agent line at all when the classification is unavailable", () => {
    setup(payload(), null);
    renderWithProviders(<AvailableUpdatesCard siteId="site-1" />);

    expect(screen.queryByTestId("agent-update-notice")).not.toBeInTheDocument();
    expect(
      screen.getByText("All managed components are up to date"),
    ).toBeInTheDocument();
  });

  it("shows the agent line alongside the managed list when there are updates too", () => {
    setup(
      payload({
        items: [
          {
            type: "plugin",
            slug: "akismet/akismet.php",
            name: "Akismet",
            version: "5.0",
            new_version: "5.1",
            active: true,
          },
        ],
      }),
      OUTDATED_AGENT,
    );
    renderWithProviders(<AvailableUpdatesCard siteId="site-1" />);

    expect(screen.getByText("Akismet")).toBeInTheDocument();
    expect(screen.getByTestId("agent-update-notice")).toBeInTheDocument();
  });

  // ── The safety property (GH #314 requirement 1, see #255) ────────────────

  it("never lets the agent enter a bulk selection or an update run", () => {
    setup(
      payload({
        core_update: { current_version: "6.7", new_version: "6.8" },
        items: [
          {
            type: "plugin",
            slug: "akismet/akismet.php",
            name: "Akismet",
            version: "5.0",
            new_version: "5.1",
            active: true,
          },
        ],
      }),
      OUTDATED_AGENT,
    );
    renderWithProviders(<AvailableUpdatesCard siteId="site-1" />);

    // The agent line carries no selection control of its own.
    const notice = screen.getByTestId("agent-update-notice");
    expect(within(notice).queryByRole("checkbox")).not.toBeInTheDocument();
    expect(within(notice).queryByRole("button")).not.toBeInTheDocument();

    // And it is not counted as a target: core + one plugin is 2, not 3.
    expect(
      screen.getByRole("button", { name: "Update all (2)" }),
    ).toBeInTheDocument();

    // It is also outside the list the bulk footer acts on.
    const list = screen.getByRole("list", { name: "Updates available" });
    expect(within(list).queryByTestId("agent-update-notice")).not.toBeInTheDocument();
    expect(within(list).queryByText("WPMgr agent")).not.toBeInTheDocument();
  });
});
