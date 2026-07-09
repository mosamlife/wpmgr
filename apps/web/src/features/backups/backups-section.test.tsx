import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen } from "@testing-library/react";
import type { Me } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";
import { mockQueryResult } from "@/test/query-mocks";

import { BackupsSection } from "./backups-section";
import { useBackups, useBackupSchedule } from "./use-backups";
import { useRestoreRuns } from "./use-restores";
import { useScheduleRuns } from "./use-schedule-runs";
import { useMe } from "@/features/auth/use-auth";
import type { BackupSnapshot } from "@wpmgr/api";

// GH #188 caller test — the snapshot row's "View" link is the single most
// common way an operator reaches the snapshot-detail route. Before the fix
// it pointed at the top-level, siteId-less `/backups/$snapshotId` route
// (see routes/_authed/sites/$siteId.backups.$snapshotId.tsx and
// top-bar-helpers.test.ts for the breadcrumb half of this regression lock).
// This test pins that the row now links to the SITE-NESTED route with BOTH
// params wired.

vi.mock("./use-backups", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./use-backups")>();
  return { ...actual, useBackups: vi.fn(), useBackupSchedule: vi.fn() };
});

vi.mock("./use-restores", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./use-restores")>();
  return { ...actual, useRestoreRuns: vi.fn() };
});

vi.mock("./use-schedule-runs", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./use-schedule-runs")>();
  return { ...actual, useScheduleRuns: vi.fn() };
});

vi.mock("@/features/auth/use-auth", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/auth/use-auth")>();
  return { ...actual, useMe: vi.fn() };
});

const mockedUseBackups = vi.mocked(useBackups);
const mockedUseBackupSchedule = vi.mocked(useBackupSchedule);
const mockedUseRestoreRuns = vi.mocked(useRestoreRuns);
const mockedUseScheduleRuns = vi.mocked(useScheduleRuns);
const mockedUseMe = vi.mocked(useMe);

function buildSnapshot(overrides: Partial<BackupSnapshot> = {}): BackupSnapshot {
  return {
    id: "01hzxysnapshot0000000000",
    tenant_id: "tenant-1",
    site_id: "site-42",
    kind: "full",
    status: "completed",
    created_at: "2026-07-01T00:00:00Z",
    updated_at: "2026-07-01T00:05:00Z",
    ...overrides,
  };
}

beforeEach(() => {
  vi.clearAllMocks();
  mockedUseMe.mockReturnValue(mockQueryResult<Me | null>({ data: null }));
  mockedUseRestoreRuns.mockReturnValue(mockQueryResult({ data: [] }));
  mockedUseScheduleRuns.mockReturnValue(
    mockQueryResult({ data: { all: [], upcoming: [], past: [] } }),
  );
  mockedUseBackupSchedule.mockReturnValue(mockQueryResult({ data: null }));
});

describe("BackupsSection — snapshot row navigation (GH #188)", () => {
  it("links a single snapshot's View button to the site-nested route with both siteId and snapshotId params", async () => {
    const snapshot = buildSnapshot({ id: "snap-single", site_id: "site-42" });
    mockedUseBackups.mockReturnValue(mockQueryResult({ data: [snapshot] }));

    renderWithProviders(<BackupsSection siteId="site-42" canOperate={false} />, {
      withRouter: true,
      initialPath: "/sites/site-42/backups",
    });

    const link = await screen.findByRole("link", { name: "View" });
    expect(link).toHaveAttribute("href", "/sites/site-42/backups/snap-single");
  });

  it("links each member of an incremental chain's View button to the site-nested route (not the old fleet-level /backups/$snapshotId)", async () => {
    // Two members of the same chain (gen 0 base + gen 1 increment) so
    // SnapshotList renders the ChainGroupRows/ChainMemberRow path instead of
    // SingletonRow — a second, independent call site for the same link.
    const base = buildSnapshot({
      id: "snap-base",
      site_id: "site-42",
      chain_id: "chain-1",
      is_incremental: false,
      generation: 0,
    });
    const incr = buildSnapshot({
      id: "snap-incr",
      site_id: "site-42",
      chain_id: "chain-1",
      is_incremental: true,
      generation: 1,
      created_at: "2026-07-02T00:00:00Z",
    });
    mockedUseBackups.mockReturnValue(mockQueryResult({ data: [base, incr] }));

    renderWithProviders(<BackupsSection siteId="site-42" canOperate={false} />, {
      withRouter: true,
      initialPath: "/sites/site-42/backups",
    });

    // The chain parent row shows the TIP (highest generation) collapsed;
    // its View link must carry the tip's id and the site's id.
    const tipLink = await screen.findByRole("link", { name: "View" });
    expect(tipLink).toHaveAttribute("href", "/sites/site-42/backups/snap-incr");
    expect(tipLink.getAttribute("href")).not.toMatch(/^\/backups\//);
  });
});
