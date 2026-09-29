import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen } from "@testing-library/react";
import type { Site } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";
import { mockQueryResult } from "@/test/query-mocks";
import { useSite } from "@/features/sites/use-sites";
import type { ScheduleRun } from "@/features/backups/use-schedule-runs";

import { ScheduleRunDetailView } from "./$runId";

// GH #791 adv-review finding 11 — the schedule-run detail page showed
// "Last error: X" twice for a running-and-retrying run: once in the status
// banner above the Summary card, and again in the Summary card's own
// "Error" row. Fixed by making the Error row read only the terminal `error`
// field; the banner is the run's single "Last error" surface while running.

vi.mock("@/features/sites/use-sites", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/sites/use-sites")>();
  return { ...actual, useSite: vi.fn() };
});

const mockedUseSite = vi.mocked(useSite);

function buildSite(overrides: Partial<Site> = {}): Site {
  return {
    id: "site-1",
    tenant_id: "t1",
    url: "https://example.com",
    name: "Example",
    status: "active",
    enrolled: true,
    wp_version: "6.8",
    php_version: "8.3",
    health_status: "healthy",
    connection_state: "connected",
    multisite: false,
    tags: [],
    ...overrides,
  } as unknown as Site;
}

function buildRun(overrides: Partial<ScheduleRun> = {}): ScheduleRun {
  return {
    id: "run-1",
    tenant_id: "t1",
    site_id: "site-1",
    schedule_id: "sched-1",
    snapshot_id: "snap-1",
    scheduled_for: "2026-09-29T00:00:00Z",
    status: "running",
    kind: "full",
    error: null,
    attempt_error: null,
    triggered_by: null,
    triggered_by_email: null,
    triggered_by_name: null,
    created_at: "2026-09-29T00:00:00Z",
    started_at: "2026-09-29T00:00:05Z",
    finished_at: null,
    updated_at: "2026-09-29T00:00:05Z",
    ...overrides,
  };
}

beforeEach(() => {
  vi.clearAllMocks();
  mockedUseSite.mockReturnValue(mockQueryResult<Site>({ data: buildSite() }));
});

describe("ScheduleRunDetailView — GH #791 attempt_error (adv finding 11)", () => {
  it("shows 'Last error' exactly once for a running run with an attempt_error", async () => {
    const run = buildRun({
      status: "running",
      attempt_error: "Could not connect to the site.",
    });
    renderWithProviders(<ScheduleRunDetailView run={run} />, { withRouter: true });

    // The test router's first paint is async — findBy* first.
    const matches = await screen.findAllByText(
      /Last error: Could not connect to the site\./,
    );
    expect(matches).toHaveLength(1);
  });

  it("does not show the running attempt_error banner or row once the run is not running", async () => {
    const run = buildRun({
      status: "running",
      attempt_error: null,
    });
    renderWithProviders(<ScheduleRunDetailView run={run} />, { withRouter: true });
    await screen.findByText("Summary");
    expect(screen.queryByText(/Last error:/)).not.toBeInTheDocument();
  });

  it("never shows the 'Last error' (attempt_error) phrasing for a failed run", async () => {
    const run = buildRun({
      status: "failed",
      error: "Backup failed: the WPMgr agent on this site stopped with an error.",
      attempt_error: null,
      finished_at: "2026-09-29T00:05:00Z",
    });
    renderWithProviders(<ScheduleRunDetailView run={run} />, { withRouter: true });

    await screen.findByText("Summary");
    expect(screen.queryByText(/Last error:/)).not.toBeInTheDocument();
  });
});
