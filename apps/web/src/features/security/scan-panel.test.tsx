import { describe, it, expect, vi } from "vitest";
import { screen } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";
import { mockMutationResult, mockQueryResult } from "@/test/query-mocks";

import { ScanPanel } from "./scan-panel";
import {
  useScanRuns,
  useScanRun,
  useStartScan,
  useScanFindings,
  type ScanRun,
  type ScanFinding,
} from "./use-scan";

// P1 outcome test — GH #170 Wave 5.
//
// `security-card.test.ts` covers `deriveScanStatus` (pure state -> pill
// mapping) in isolation; nothing ever rendered `ScanPanel`, so a regression
// that left the pill on "Scanning" after the run actually completed (e.g.
// read the stale list-row status instead of the live-polled detail) would
// pass every existing test. This renders the real component twice — once
// mid-scan, once after the run transitions to done — and asserts the pill
// (and its live/pulsing indicator) actually flips.

vi.mock("./use-scan", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./use-scan")>();
  return {
    ...actual,
    useScanRuns: vi.fn(),
    useScanRun: vi.fn(),
    useStartScan: vi.fn(),
    useScanFindings: vi.fn(),
  };
});

const mockedUseScanRuns = vi.mocked(useScanRuns);
const mockedUseScanRun = vi.mocked(useScanRun);
const mockedUseStartScan = vi.mocked(useStartScan);
const mockedUseScanFindings = vi.mocked(useScanFindings);

function buildRun(overrides: Partial<ScanRun> = {}): ScanRun {
  return {
    id: "run-1",
    kind: "core",
    status: "scanning",
    files_scanned: 1200,
    wp_version: "6.7",
    locale: "en_US",
    error: null,
    finding_counts: null,
    created_at: "2026-07-08T00:00:00Z",
    started_at: "2026-07-08T00:00:01Z",
    finished_at: null,
    ...overrides,
  };
}

describe("ScanPanel — exits the 'Scanning…' pill on completion", () => {
  it("shows a live, pulsing 'Scanning' pill mid-run, then flips to a terminal 'No issues found' pill with no pulse once the run completes", () => {
    mockedUseStartScan.mockReturnValue(
      mockMutationResult<ScanRun, "core" | "files" | "full">({}),
    );
    mockedUseScanFindings.mockReturnValue(
      mockQueryResult<ScanFinding[]>({ data: [] }),
    );

    // The OUTER list (`useScanRuns`) is pinned to "scanning" for the whole
    // test and NEVER updated — it only refetches on its own poll interval,
    // which may lag behind the per-run detail poll. This is deliberate: it
    // isolates the exact invariant `LatestRunStatus` documents ("Subscribe to
    // the live-polling run detail so status advances in real time") — the
    // pill must track the live-polled `useScanRun` DETAIL, not the
    // possibly-stale list row. A version that reads the list row instead
    // would stay stuck on "Scanning" even after the assertions below update
    // only the detail.
    const scanningRun = buildRun({ status: "scanning" });
    mockedUseScanRuns.mockReturnValue(
      mockQueryResult<ScanRun[]>({ data: [scanningRun] }),
    );
    mockedUseScanRun.mockReturnValue(
      mockQueryResult<ScanRun>({ data: scanningRun }),
    );

    const { container, rerender } = renderWithProviders(
      <ScanPanel siteId="site-1" canWrite />,
    );

    // Mid-scan: "Scanning" copy + a live pulsing dot (LiveIndicator), and the
    // findings table has NOT mounted yet (no completed run).
    expect(screen.getByText("Scanning")).toBeInTheDocument();
    expect(container.querySelector(".animate-pulse")).not.toBeNull();
    expect(screen.queryByText("No issues found")).not.toBeInTheDocument();

    // The live-polled DETAIL resolves to done with zero findings — the list
    // query is untouched (still "scanning"). Re-render the SAME component
    // (simulating the poll resolving), not a fresh mount.
    const doneRun = buildRun({
      status: "done",
      finding_counts: {},
      finished_at: "2026-07-08T00:02:00Z",
    });
    mockedUseScanRun.mockReturnValue(
      mockQueryResult<ScanRun>({ data: doneRun }),
    );

    rerender(<ScanPanel siteId="site-1" canWrite />);

    // Non-vacuous: a version that keeps rendering the LiveIndicator/"Scanning"
    // pill after the run reaches a terminal state (the stuck-pill bug class)
    // fails both assertions below.
    expect(screen.queryByText("Scanning")).not.toBeInTheDocument();
    expect(container.querySelector(".animate-pulse")).toBeNull();
    expect(screen.getByText("No issues found")).toBeInTheDocument();
    expect(screen.getByText("Complete")).toBeInTheDocument();
  });
});
