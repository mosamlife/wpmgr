import { describe, expect, it, vi } from "vitest";
import { fireEvent, screen, within } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";
import { mockMutationResult, mockQueryResult } from "@/test/query-mocks";
import {
  CLOCK_403_DETAIL,
  CLOCK_403_RAW_ERROR,
  CORE_LEFT_AS_IS_DETAIL,
  CORE_ROLLBACK_UNDELIVERABLE_DETAIL,
  FIREWALL_403_DETAIL,
  FIREWALL_403_RAW_ERROR,
  HEALTH_CHECK_FAILED_REASON,
  PLUGIN_SITE_DOWN_DETAIL,
  ROLLBACK_RAW_ERROR,
} from "@/test/update-task-details";

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

// GH #553: `as_of` used to be stamped from the 60s site heartbeat, so it was
// practically never null. It now reads `components_updated_at`, which is only
// set when the inventory is genuinely (re)written, and it shipped with no
// backfill, so every site that existed before the fix reports `as_of: null`
// until its next sync. `null` is a real, common, temporary state here, not an
// absence to paper over with a generic "Never".
describe("AvailableUpdatesCard as_of honesty (GH #553)", () => {
  it("tells the operator the collection time is unknown, and how it resolves, instead of claiming it was never collected", () => {
    setup(payload({ as_of: null }), null);
    renderWithProviders(<AvailableUpdatesCard siteId="site-1" />);

    // Distinct from FreshnessBadge's generic "Never", which would wrongly
    // assert this site's inventory was never collected -- it was, we just
    // don't have a genuine collection time for it yet. Exactly once: the
    // header owns this state, the empty-state block below must not repeat it
    // (a site with no updates renders both regions at once).
    expect(screen.getAllByText(/inventory age unknown/i)).toHaveLength(1);
    expect(screen.queryByText(/^Never$/)).not.toBeInTheDocument();

    // States what resolves it, in operator terms, with no raw field names.
    expect(screen.getAllByText(/next sync/i)).toHaveLength(1);
    expect(screen.queryByText(/as_of/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/components_updated_at/i)).not.toBeInTheDocument();
  });

  it("does not show the unknown-age copy when a real collection time is present", () => {
    setup(payload({ as_of: "2026-08-04T10:00:00Z" }), null);
    renderWithProviders(<AvailableUpdatesCard siteId="site-1" />);

    expect(screen.queryAllByText(/inventory age unknown/i).length).toBe(0);
  });

  // The header is the single owner of the freshness/age claim: it is the
  // only place that renders in every query state (loading, error, loaded),
  // so a second copy in the "all managed components are up to date" block
  // would either duplicate it or, worse, drift from it.
  it("shows the unknown-age copy exactly once even when the empty state also renders", () => {
    setup(payload({ as_of: null, items: [], core_update: null }), null);
    renderWithProviders(<AvailableUpdatesCard siteId="site-1" />);

    // Sanity: this is genuinely the empty-state branch.
    expect(
      screen.getByText("All managed components are up to date"),
    ).toBeInTheDocument();
    expect(screen.getAllByText(/inventory age unknown/i)).toHaveLength(1);
  });

  // A query that is still loading has not established anything about the
  // inventory yet -- not its age, and not whether it is unknown. Claiming
  // "unknown, clears after next sync" here would be a guess dressed up as a
  // fact, the same failure mode this whole fix is about, just earlier.
  it("makes no age claim while the query is still loading", () => {
    mockedUseAvailableUpdates.mockReturnValue(
      mockQueryResult<SiteAvailableUpdates>({
        data: undefined,
        isPending: true,
        isSuccess: false,
        status: "pending",
      }),
    );
    mockedUseRefresh.mockReturnValue(mockMutationResult<void, void>({}));
    mockedUseSiteAgentUpdate.mockReturnValue(null);
    renderWithProviders(<AvailableUpdatesCard siteId="site-1" />);

    expect(screen.queryByText(/inventory age unknown/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/inventory age unavailable/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/^Never$/)).not.toBeInTheDocument();
  });

  // A failed request never loaded the inventory at all -- not the list, not
  // an age for it. That is a different fact from "we have the inventory but
  // not its collection time" (the unknown-age case above), and "clears after
  // the next sync" is actively wrong advice here: a sync is not what fixes a
  // request that did not load.
  it("shows a distinct unavailable state, never the unknown-age copy, when the request itself fails", () => {
    mockedUseAvailableUpdates.mockReturnValue(
      mockQueryResult<SiteAvailableUpdates>({
        data: undefined,
        isPending: false,
        isError: true,
        isSuccess: false,
        status: "error",
        error: new Error("network down"),
      }),
    );
    mockedUseRefresh.mockReturnValue(mockMutationResult<void, void>({}));
    mockedUseSiteAgentUpdate.mockReturnValue(null);
    renderWithProviders(<AvailableUpdatesCard siteId="site-1" />);

    expect(screen.getByText(/inventory age unavailable/i)).toBeInTheDocument();
    expect(screen.queryByText(/inventory age unknown/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/next sync/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/^Never$/)).not.toBeInTheDocument();

    // The body's own load-failure message still owns the "why" -- the header
    // state above says only that no age is available, not says why twice.
    expect(
      screen.getByText("Could not load available updates"),
    ).toBeInTheDocument();
  });
});

// GH #679, GH #415: a row that did not update says what happened.
//
// The control plane writes the sentence an operator acts on into the task's
// detail and keeps the raw reply in its error. The row used to print the error,
// so the reply from the site's firewall was the text on the card and the
// guidance never reached it. The raw reply is still there, one click away.
describe("AvailableUpdatesCard update outcomes (GH #679, GH #415)", () => {
  const AKISMET = {
    type: "plugin" as const,
    slug: "akismet/akismet.php",
    name: "Akismet",
    version: "5.0",
    new_version: "5.1",
    active: true,
  };

  function row(overrides: Partial<RowUpdate>): RowUpdate {
    return { ...IDLE_ROW, taskId: "task-1", ...overrides };
  }

  function renderPluginRow(update: RowUpdate) {
    setup(payload({ items: [AKISMET] }), null);
    mockedUseRowUpdate.mockReturnValue(update);
    return renderWithProviders(<AvailableUpdatesCard siteId="site-1" />);
  }

  function renderCoreRow(update: RowUpdate) {
    setup(
      payload({ core_update: { current_version: "6.6.2", new_version: "6.7.1" } }),
      null,
    );
    mockedUseCoreRowUpdate.mockReturnValue(update);
    return renderWithProviders(<AvailableUpdatesCard siteId="site-1" />);
  }

  it.each([
    ["a firewall block", FIREWALL_403_DETAIL, FIREWALL_403_RAW_ERROR],
    ["a clock difference", CLOCK_403_DETAIL, CLOCK_403_RAW_ERROR],
  ])(
    "shows the guidance for %s on the row, and the raw reply only under Show log",
    (_label, detail, rawError) => {
      renderPluginRow(
        row({ state: "failed", progress: detail, error: rawError }),
      );

      expect(screen.getByText(detail)).toBeInTheDocument();
      expect(screen.queryByText(/status 403 body=/)).not.toBeInTheDocument();
      expect(screen.queryByText(rawError)).not.toBeInTheDocument();

      const toggle = screen.getByRole("button", { name: /show log/i });
      expect(toggle).toHaveAttribute("aria-expanded", "false");
      fireEvent.click(toggle);

      expect(screen.getByText(rawError)).toBeInTheDocument();
      expect(
        screen.getByRole("button", { name: /hide log/i }),
      ).toHaveAttribute("aria-expanded", "true");
    },
  );

  it("shows the error itself when a failed row has no detail to lead with", () => {
    renderPluginRow(
      row({ state: "failed", progress: undefined, error: FIREWALL_403_RAW_ERROR }),
    );

    expect(screen.getByText(FIREWALL_403_RAW_ERROR)).toBeInTheDocument();
    // Nothing is hidden behind a log that would only repeat it.
    expect(
      screen.queryByRole("button", { name: /show log/i }),
    ).not.toBeInTheDocument();
  });

  it("says the update failed when the row has neither a detail nor an error", () => {
    renderPluginRow(row({ state: "failed" }));

    expect(screen.getByText("Update failed")).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: /show log/i }),
    ).not.toBeInTheDocument();
  });

  it("says why a row was rolled back instead of only that it was", () => {
    const detail = `rolled back: ${HEALTH_CHECK_FAILED_REASON}`;
    renderPluginRow(row({ state: "rolled_back", progress: detail }));

    expect(screen.getByText(detail)).toBeInTheDocument();
  });

  it("leads a plugin row that went down with the watchdog sentence, and keeps the rollback error behind the log", () => {
    renderPluginRow(
      row({
        state: "failed",
        progress: PLUGIN_SITE_DOWN_DETAIL,
        error: ROLLBACK_RAW_ERROR,
      }),
    );

    expect(screen.getByRole("alert")).toHaveTextContent(PLUGIN_SITE_DOWN_DETAIL);
    expect(screen.queryByText(ROLLBACK_RAW_ERROR)).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /show log/i }));
    expect(screen.getByText(ROLLBACK_RAW_ERROR)).toBeInTheDocument();
  });

  it("leads the WordPress core row with the sentence that says nothing restores core, never with a claim that recovery was attempted", () => {
    renderCoreRow(
      row({
        state: "failed",
        progress: CORE_ROLLBACK_UNDELIVERABLE_DETAIL,
        error: ROLLBACK_RAW_ERROR,
      }),
    );

    expect(screen.getByRole("alert")).toHaveTextContent(
      CORE_ROLLBACK_UNDELIVERABLE_DETAIL,
    );
    expect(
      screen.queryByText(/recovery was attempted/i),
    ).not.toBeInTheDocument();
    expect(screen.queryByText(ROLLBACK_RAW_ERROR)).not.toBeInTheDocument();
  });

  it("shows core left as is in full, as an ordinary failure", () => {
    renderCoreRow(
      row({
        state: "failed",
        progress: CORE_LEFT_AS_IS_DETAIL,
        error: HEALTH_CHECK_FAILED_REASON,
      }),
    );

    expect(screen.getByText(CORE_LEFT_AS_IS_DETAIL)).toBeInTheDocument();
    expect(screen.queryByText(HEALTH_CHECK_FAILED_REASON)).not.toBeInTheDocument();
  });

  it("leaves a succeeded row as Updated", () => {
    renderPluginRow(row({ state: "succeeded" }));

    expect(screen.getByText("Updated")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /show log/i })).not.toBeInTheDocument();
  });

  it("keeps a skipped row as Skipped and does not turn the control plane's note into a claim that the plugin is current", () => {
    // A skipped row says only what its status says. The note the control
    // plane stores with the task is not repeated as a claim about the version.
    renderPluginRow(row({ state: "skipped", progress: "already up to date" }));

    expect(screen.getByText("Skipped")).toBeInTheDocument();
    expect(screen.queryByText(/already up to date/i)).not.toBeInTheDocument();
  });
});
