import { describe, it, expect, vi } from "vitest";
import { screen } from "@testing-library/react";
import type { AlertConfig } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";
import { mockQueryResult } from "@/test/query-mocks";

import { AppHealthAlertingNotice } from "./app-health-alerting-notice";
import { useAlertConfig } from "./use-uptime";

// GH #291 Phase 3, Task 2, the tenant/per-site app-health alerting surface.
// Named coverage: the required "what unknown means" + "only a genuine
// WordPress error triggers an alert" explanation is present, the
// scope-specific content matches what actually belongs at that scope (the
// tenant-wide switch now lives in the form, not this panel; the per-site
// health path and mute remain genuinely unshipped), and the On/Off badge
// tracks the tenant's real `AlertConfig.app_alerts_enabled`, the same query
// the form and the upgrade prompt read, so it can't silently drift into a
// stale or backwards label again.

vi.mock("./use-uptime", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./use-uptime")>();
  return { ...actual, useAlertConfig: vi.fn() };
});

const mockedUseAlertConfig = vi.mocked(useAlertConfig);

function buildAlertConfig(overrides: Partial<AlertConfig> = {}): AlertConfig {
  return {
    email_recipients: ["ops@example.com"],
    webhook_url: "",
    webhook_configured: false,
    enabled: true,
    notify_security: false,
    notify_vulns: false,
    vuln_min_severity: "high",
    vuln_include_in_digest: true,
    app_alerts_enabled: false,
    ...overrides,
  };
}

describe("AppHealthAlertingNotice", () => {
  it("explains what 'unknown' means and that only a genuine WordPress error alerts", () => {
    mockedUseAlertConfig.mockReturnValue(
      mockQueryResult<AlertConfig | null>({ data: buildAlertConfig() }),
    );
    renderWithProviders(<AppHealthAlertingNotice scope="tenant" />);

    expect(
      screen.getByText(/reported as unknown, not broken/),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/an HTTP 500, or a page that carries WordPress's own fatal-error signature/),
    ).toBeInTheDocument();
  });

  it("points to the Application health section above instead of listing the tenant-wide switch as planned", () => {
    mockedUseAlertConfig.mockReturnValue(
      mockQueryResult<AlertConfig | null>({ data: buildAlertConfig() }),
    );
    renderWithProviders(<AppHealthAlertingNotice scope="tenant" />);

    expect(
      screen.getByText(/Application health section above/),
    ).toBeInTheDocument();
    expect(screen.queryByText(/custom health-check path/)).not.toBeInTheDocument();
    expect(screen.queryByText("Planned")).not.toBeInTheDocument();
  });

  it("lists the per-site health path and mute controls as planned at site scope", () => {
    mockedUseAlertConfig.mockReturnValue(
      mockQueryResult<AlertConfig | null>({ data: buildAlertConfig() }),
    );
    renderWithProviders(<AppHealthAlertingNotice scope="site" />);

    expect(screen.getByText("Planned for this section:")).toBeInTheDocument();
    expect(screen.getByText(/custom health-check path for this site/)).toBeInTheDocument();
    expect(screen.getByText(/mute switch to stop this specific site/)).toBeInTheDocument();
    expect(
      screen.queryByText(/Application health section above/),
    ).not.toBeInTheDocument();
  });

  it("shows an Off badge when application health alerting is off for the tenant", () => {
    mockedUseAlertConfig.mockReturnValue(
      mockQueryResult<AlertConfig | null>({
        data: buildAlertConfig({ app_alerts_enabled: false }),
      }),
    );
    renderWithProviders(<AppHealthAlertingNotice scope="tenant" />);

    expect(screen.getByText("Off")).toBeInTheDocument();
    expect(screen.queryByText("On")).not.toBeInTheDocument();
  });

  it("shows an On badge when application health alerting is on for the tenant", () => {
    mockedUseAlertConfig.mockReturnValue(
      mockQueryResult<AlertConfig | null>({
        data: buildAlertConfig({ app_alerts_enabled: true }),
      }),
    );
    renderWithProviders(<AppHealthAlertingNotice scope="tenant" />);

    expect(screen.getByText("On")).toBeInTheDocument();
    expect(screen.queryByText("Off")).not.toBeInTheDocument();
  });

  it("shows no badge while the tenant's alert config is still loading", () => {
    mockedUseAlertConfig.mockReturnValue(
      mockQueryResult<AlertConfig | null>({
        data: undefined,
        isPending: true,
        isSuccess: false,
      }),
    );
    renderWithProviders(<AppHealthAlertingNotice scope="tenant" />);

    expect(screen.queryByText("On")).not.toBeInTheDocument();
    expect(screen.queryByText("Off")).not.toBeInTheDocument();
  });
});
