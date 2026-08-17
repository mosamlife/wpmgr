import { describe, it, expect, vi } from "vitest";
import { screen } from "@testing-library/react";
import type { EmailNotifySettings } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";
import { mockQueryResult, mockMutationResult } from "@/test/query-mocks";

// GH #381, phase 3: the Notifications page promised "Send an alert email
// when a delivery failure is detected on a site" with no mention that
// WPMgr can only detect a failure on a site whose agent is new enough to
// report it. A self-hosted user with a working instance mailer (so the
// OTHER banner, instance_mailer_configured, never fired) went ten days
// with alerting silently unable to ever trigger because every one of their
// sites was on an older agent. Phase 2 added `failure_detection` to
// GET /api/v1/email/notify-settings; this phase surfaces it.
//
// GH #381 phase 2 fix: coverage is routed-OR-new-enough-agent, not
// agent-version-alone. A site WPMgr actively routes mail through has always
// been able to report a delivery failure regardless of agent version, so a
// fully-routed fleet on old agents is fully covered, not 0% covered. The
// "fully routed, real placeholder version" test below is the regression
// test for the shipped bug: it asserts the full-coverage state renders for
// exactly that fleet shape, using the real production placeholder
// ("999.0.0", MinAgentVersionForFailureDetection in
// apps/api/internal/email/service.go) rather than a plausible fake.

const { useEmailNotifySettingsMock, usePutEmailNotifySettingsMock } = vi.hoisted(
  () => ({
    useEmailNotifySettingsMock: vi.fn(),
    usePutEmailNotifySettingsMock: vi.fn(),
  }),
);

vi.mock("./use-email", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./use-email")>();
  return {
    ...actual,
    useEmailNotifySettings: useEmailNotifySettingsMock,
    usePutEmailNotifySettings: usePutEmailNotifySettingsMock,
  };
});

import { EmailNotifySettingsCard } from "./email-notify-settings-card";

function buildSettings(
  overrides: Partial<EmailNotifySettings> = {},
): EmailNotifySettings {
  return {
    enabled: true,
    recipients: ["ops@example.com"],
    alert_on_failure: true,
    alert_throttle_minutes: 60,
    digest_enabled: false,
    digest_cadence: "daily",
    digest_day: 0,
    digest_hour: 8,
    timezone: "UTC",
    instance_mailer_configured: true,
    failure_detection: {
      sites_total: 10,
      sites_covered: 10,
      sites_routed: 0,
      min_agent_version_unrouted: "1.4.0",
    },
    ...overrides,
  };
}

function mockSettings(data: EmailNotifySettings | null) {
  useEmailNotifySettingsMock.mockReturnValue(
    mockQueryResult<EmailNotifySettings | null>({ data }),
  );
  usePutEmailNotifySettingsMock.mockReturnValue(
    mockMutationResult<EmailNotifySettings, unknown>({}),
  );
}

describe("EmailNotifySettingsCard failure-detection coverage (GH #381)", () => {
  it("shows a zero-coverage warning when no site can report delivery failures", async () => {
    mockSettings(
      buildSettings({
        failure_detection: {
          sites_total: 6,
          sites_covered: 0,
          sites_routed: 0,
          min_agent_version_unrouted: "1.4.0",
        },
      }),
    );

    renderWithProviders(<EmailNotifySettingsCard />, { withRouter: true });

    expect(
      await screen.findByText(/No connected site can report a delivery failure\./),
    ).toBeInTheDocument();
    expect(screen.getByText(/1\.4\.0/)).toBeInTheDocument();
    expect(
      screen.queryByText(/Covering all/),
    ).not.toBeInTheDocument();
  });

  it("shows the zero-coverage warning for a genuinely zero-coverage tenant (nothing routed, nothing new enough) — the banner must still be reachable", async () => {
    mockSettings(
      buildSettings({
        failure_detection: {
          sites_total: 4,
          sites_covered: 0,
          sites_routed: 0,
          min_agent_version_unrouted: "1.4.0",
        },
      }),
    );

    renderWithProviders(<EmailNotifySettingsCard />, { withRouter: true });

    expect(
      await screen.findByText(/No connected site can report a delivery failure\./),
    ).toBeInTheDocument();
    expect(screen.getByText(/1\.4\.0/)).toBeInTheDocument();
    expect(screen.queryByText(/Covering all/)).not.toBeInTheDocument();
  });

  // Regression test for the shipped bug: a fully-routed fleet, all on agents
  // below the real (unreachable-until-shipped) placeholder version, is fully
  // covered because routing does not depend on agent version. Before the
  // GH #381 phase 2 fix, sites_covered was computed from agent_version alone,
  // so this exact fleet shape reported 0 of N covered and told customers to
  // update to an impossible version.
  it("shows full coverage for a fully-routed fleet under the real production placeholder version, not the zero-coverage warning", async () => {
    mockSettings(
      buildSettings({
        failure_detection: {
          sites_total: 12,
          sites_covered: 12,
          sites_routed: 12,
          min_agent_version_unrouted: "999.0.0",
        },
      }),
    );

    renderWithProviders(<EmailNotifySettingsCard />, { withRouter: true });

    expect(
      await screen.findByText(/Covering all 12 connected sites/),
    ).toBeInTheDocument();
    expect(
      screen.queryByText(/No connected site can report a delivery failure\./),
    ).not.toBeInTheDocument();
    expect(screen.queryByText(/999\.0\.0/)).not.toBeInTheDocument();
    expect(
      screen.queryByText(/connected sites can report a delivery failure \(/),
    ).not.toBeInTheDocument();
  });

  it("shows partial coverage as N of M", async () => {
    mockSettings(
      buildSettings({
        failure_detection: {
          sites_total: 8,
          sites_covered: 3,
          sites_routed: 0,
          min_agent_version_unrouted: "1.4.0",
        },
      }),
    );

    renderWithProviders(<EmailNotifySettingsCard />, { withRouter: true });

    expect(
      await screen.findByText(/3 of 8 connected sites can report a delivery/),
    ).toBeInTheDocument();
    expect(
      screen.queryByText(/No connected site can report a delivery failure\./),
    ).not.toBeInTheDocument();
  });

  it("shows the quiet full-coverage line and not the warning banner", async () => {
    mockSettings(
      buildSettings({
        failure_detection: {
          sites_total: 5,
          sites_covered: 5,
          sites_routed: 0,
          min_agent_version_unrouted: "1.4.0",
        },
      }),
    );

    renderWithProviders(<EmailNotifySettingsCard />, { withRouter: true });

    expect(
      await screen.findByText("Covering all 5 connected sites."),
    ).toBeInTheDocument();
    expect(
      screen.queryByText(/No connected site can report a delivery failure\./),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByText(/connected sites can report a delivery failure\. The rest/),
    ).not.toBeInTheDocument();
  });

  it("still renders the instance_mailer_configured banner independently, and both can show at once", async () => {
    mockSettings(
      buildSettings({
        instance_mailer_configured: false,
        failure_detection: {
          sites_total: 4,
          sites_covered: 0,
          sites_routed: 0,
          min_agent_version_unrouted: "1.4.0",
        },
      }),
    );

    renderWithProviders(<EmailNotifySettingsCard />, { withRouter: true });

    expect(
      await screen.findByText("Instance mailer not configured."),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/No connected site can report a delivery failure\./),
    ).toBeInTheDocument();
  });

  it("shows no coverage message when failure_detection is absent (older API)", async () => {
    const { failure_detection: _omit, ...withoutFailureDetection } =
      buildSettings();
    mockSettings(withoutFailureDetection);

    renderWithProviders(<EmailNotifySettingsCard />, { withRouter: true });

    expect(
      await screen.findByText(
        "Send an alert email when a delivery failure is detected on a site.",
      ),
    ).toBeInTheDocument();
    expect(
      screen.queryByText(/No connected site can report a delivery failure\./),
    ).not.toBeInTheDocument();
    expect(screen.queryByText(/Covering all/)).not.toBeInTheDocument();
    expect(
      screen.queryByText(/connected sites can report a delivery failure\. The rest/),
    ).not.toBeInTheDocument();
  });

  it("shows no coverage message for a tenant with zero sites", async () => {
    mockSettings(
      buildSettings({
        failure_detection: {
          sites_total: 0,
          sites_covered: 0,
          sites_routed: 0,
          min_agent_version_unrouted: "1.4.0",
        },
      }),
    );

    renderWithProviders(<EmailNotifySettingsCard />, { withRouter: true });

    expect(
      await screen.findByText(
        "Send an alert email when a delivery failure is detected on a site.",
      ),
    ).toBeInTheDocument();
    expect(
      screen.queryByText(/No connected site can report a delivery failure\./),
    ).not.toBeInTheDocument();
    expect(screen.queryByText(/Covering all/)).not.toBeInTheDocument();
  });
});
