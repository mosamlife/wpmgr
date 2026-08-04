import { describe, it, expect, vi } from "vitest";
import { screen } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";

import { PortalUptimeCard } from "./portal-uptime-card";
import { PortalBackupsCard } from "./portal-backups-card";
import { PortalUpdatesCard } from "./portal-updates-card";
import { PortalVitalsCard } from "./portal-vitals-card";
import type {
  PortalUptimeSummary,
  PortalBackupItem,
  PortalUpdateItem,
  PortalVitalsSummary,
} from "./use-portal";

// P1 outcome test — GH #170 Wave 5.
//
// Before this file, `src/features/portal/` had NO render test at all — the
// client-facing portal (a customer-visible surface, not just an operator
// one) was entirely untested beyond whatever the SDK types enforced. A
// regression that swapped a real prop-read for a hardcoded placeholder (or
// dropped the empty-state branch for a customer with no data yet) would pass
// every existing test. These cards are pure props-in/JSX-out (no data hooks
// of their own — the parent route resolves the query and passes
// data/isLoading/isError down), so no hook mocking is needed here.

describe("PortalUptimeCard — shows the real uptime/latency/incident data", () => {
  const onRetry = vi.fn();

  it("renders the real uptime percentage, latency, and incident rows for a customer with data", () => {
    const data: PortalUptimeSummary = {
      range: "30d",
      uptime_pct: 99.87,
      avg_latency_ms: 214,
      tls_expires_at: "2026-09-01T00:00:00Z",
      incidents: [
        { started_at: "2026-07-01T02:00:00Z", ended_at: "2026-07-01T02:05:00Z", duration_seconds: 300 },
      ],
    };

    renderWithProviders(
      <PortalUptimeCard
        data={data}
        isLoading={false}
        isError={false}
        error={null}
        onRetry={onRetry}
        isRetrying={false}
      />,
    );

    // Non-vacuous: a hardcoded placeholder (e.g. always "100.00%") fails.
    expect(screen.getByText("99.87%")).toBeInTheDocument();
    expect(screen.getByText("214")).toBeInTheDocument();
    expect(screen.getByText("1 incident")).toBeInTheDocument();
  });

  it("shows an empty/no-incidents state for a customer with a clean uptime record", () => {
    const data: PortalUptimeSummary = {
      range: "30d",
      uptime_pct: 100,
      avg_latency_ms: 120,
      incidents: [],
    };

    renderWithProviders(
      <PortalUptimeCard
        data={data}
        isLoading={false}
        isError={false}
        error={null}
        onRetry={onRetry}
        isRetrying={false}
      />,
    );

    expect(screen.getByText("No incidents in this period")).toBeInTheDocument();
  });

  it("renders PageError (not the data view) when the query failed", () => {
    renderWithProviders(
      <PortalUptimeCard
        data={undefined}
        isLoading={false}
        isError
        error={new Error("network down")}
        onRetry={onRetry}
        isRetrying={false}
      />,
    );

    expect(screen.getByText("Could not load uptime data.")).toBeInTheDocument();
    expect(screen.queryByText(/%$/)).not.toBeInTheDocument();
  });
});

describe("PortalBackupsCard — shows the real backup inventory", () => {
  const onRetry = vi.fn();

  it("renders the real status/kind/backup date for each item", () => {
    const items: PortalBackupItem[] = [
      {
        id: "b1",
        kind: "full",
        status: "completed",
        size_bytes: 52_428_800, // 50 MB
        created_at: "2026-07-05T00:00:00Z",
        completed_at: "2026-07-05T00:10:00Z",
      },
    ];

    renderWithProviders(
      <PortalBackupsCard
        items={items}
        isLoading={false}
        isError={false}
        error={null}
        onRetry={onRetry}
        isRetrying={false}
      />,
    );

    expect(screen.getByText("completed")).toBeInTheDocument();
    expect(screen.getByText("Full")).toBeInTheDocument();
    expect(screen.getByText("50.0 MB")).toBeInTheDocument();
    expect(screen.getByText("1 shown")).toBeInTheDocument();
  });

  it("shows the honest 'no completed backups yet' empty state for a customer with none", () => {
    renderWithProviders(
      <PortalBackupsCard
        items={[]}
        isLoading={false}
        isError={false}
        error={null}
        onRetry={onRetry}
        isRetrying={false}
      />,
    );

    expect(screen.getByText("No completed backups yet.")).toBeInTheDocument();
  });
});

describe("PortalUpdatesCard — shows the real applied-updates log", () => {
  const onRetry = vi.fn();

  it("renders the real plugin/theme/core update rows with from -> to versions", () => {
    const items: PortalUpdateItem[] = [
      {
        type: "plugin",
        name: "Example Plugin",
        status: "succeeded",
        from_version: "2.1.0",
        to_version: "2.2.0",
        finished_at: "2026-07-06T00:00:00Z",
      },
    ];

    renderWithProviders(
      <PortalUpdatesCard
        items={items}
        isLoading={false}
        isError={false}
        error={null}
        onRetry={onRetry}
        isRetrying={false}
      />,
    );

    expect(screen.getByText("Plugin")).toBeInTheDocument();
    expect(screen.getByText("Example Plugin")).toBeInTheDocument();
    expect(screen.getByText("2.1.0 → 2.2.0")).toBeInTheDocument();
  });

  it("shows the empty state when no updates have been recorded", () => {
    renderWithProviders(
      <PortalUpdatesCard
        items={[]}
        isLoading={false}
        isError={false}
        error={null}
        onRetry={onRetry}
        isRetrying={false}
      />,
    );

    expect(screen.getByText("No updates recorded yet.")).toBeInTheDocument();
  });
});

describe("PortalVitalsCard — shows the real Core Web Vitals p75s", () => {
  const onRetry = vi.fn();

  it("renders the real p75 values and ratings, not a placeholder", () => {
    // WIRE VALUES, not display values. The API sends milli-units for EVERY
    // metric, so a CLS of 0.18 arrives as 180. The previous version of this
    // test passed `p75: 0.18` here, which no server ever sends, and so it
    // agreed with a component that also forgot to divide. Two wrongs
    // cancelling is why a client-facing bug shipped with a green test.
    const data: PortalVitalsSummary = {
      range: "28d",
      metrics: [
        { metric: "lcp", p75: 2100, rating: "good", samples: 5000 },
        { metric: "cls", p75: 180, rating: "needs-improvement", samples: 4200 },
      ],
    };

    renderWithProviders(
      <PortalVitalsCard
        data={data}
        isLoading={false}
        isError={false}
        error={null}
        onRetry={onRetry}
        isRetrying={false}
      />,
    );

    expect(screen.getByText("2.10 s")).toBeInTheDocument();
    expect(screen.getByText("0.180")).toBeInTheDocument();
    expect(screen.getByText("Good")).toBeInTheDocument();
    expect(screen.getByText("Needs improvement")).toBeInTheDocument();
  });

  it("never shows a client a CLS scaled by 1000", () => {
    // The exact reported shape: a GOOD CLS of 0.1, which the API sends as 100.
    // It rendered as "100.000" beside a badge reading "Good", which is a
    // number no CLS can take and which contradicts its own rating.
    const data: PortalVitalsSummary = {
      range: "28d",
      metrics: [
        { metric: "cls", p75: 100, rating: "good", samples: 3000 },
      ],
    };

    renderWithProviders(
      <PortalVitalsCard
        data={data}
        isLoading={false}
        isError={false}
        error={null}
        onRetry={onRetry}
        isRetrying={false}
      />,
    );

    expect(screen.getByText("0.100")).toBeInTheDocument();
    expect(screen.queryByText("100.000")).not.toBeInTheDocument();
  });

  it("formats a sub-second timing metric in milliseconds", () => {
    // formatLcpP75 switches at 1000, so this pins the other side of the
    // branch and proves the portal did not simply become a seconds-only view.
    const data: PortalVitalsSummary = {
      range: "28d",
      metrics: [{ metric: "inp", p75: 180, rating: "good", samples: 900 }],
    };

    renderWithProviders(
      <PortalVitalsCard
        data={data}
        isLoading={false}
        isError={false}
        error={null}
        onRetry={onRetry}
        isRetrying={false}
      />,
    );

    expect(screen.getByText("180 ms")).toBeInTheDocument();
  });

  it("shows the 'no field data' empty state for a customer with no RUM samples yet", () => {
    renderWithProviders(
      <PortalVitalsCard
        data={{ range: "28d", metrics: [] }}
        isLoading={false}
        isError={false}
        error={null}
        onRetry={onRetry}
        isRetrying={false}
      />,
    );

    expect(
      screen.getByText(/No field data available yet/),
    ).toBeInTheDocument();
  });
});
