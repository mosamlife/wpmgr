import { describe, it, expect, vi, beforeEach } from "vitest";
import type { ReactElement } from "react";
import { fireEvent, screen, waitFor } from "@testing-library/react";

import { renderWithProviders, type RenderWithProvidersOptions } from "@/test/render";
import { mockMutationResult, mockQueryResult } from "@/test/query-mocks";
import { ShellContext } from "@/components/layout/app-shell-context";

import { AlertConfigForm } from "./alert-config-form";
import { useAlertConfig, usePutAlertConfig } from "./use-uptime";
import { useEmailNotifySettings } from "@/features/email/use-email";
import type { EmailNotifySettings } from "@/features/email/use-email";
import { useMe } from "@/features/auth/use-auth";
import type { AlertConfig, AlertConfigUpdate, Me } from "@wpmgr/api";

// `StickySaveBar` (mounted unconditionally by `AlertConfigForm`) reads
// `useShellState`, which throws outside a real `<AppShell>`. `renderWithProviders`
// deliberately doesn't wire the shell (see its module doc), so this file
// supplies a minimal stub provider directly around the component under test.
function renderForm(
  ui: ReactElement,
  options?: RenderWithProvidersOptions,
) {
  return renderWithProviders(
    <ShellContext.Provider
      value={{
        collapsed: false,
        toggleCollapsed: vi.fn(),
        mobileOpen: false,
        setMobileOpen: vi.fn(),
      }}
    >
      {ui}
    </ShellContext.Provider>,
    options,
  );
}

// GH #247 (vulnerability alerting) — the alert channel form now edits four
// signals off one shared resource (recipients/webhook, downtime, security
// events, vulnerability alerts). These tests cover the three regressions
// this surface is most likely to reintroduce:
//   1. `notify_security` silently dropped on save (the exact CP bug #247
//      fixed on the backend — the FE must not reintroduce it by omitting
//      the field from a partial-looking edit).
//   2. The severity <select> not actually wired to `vuln_min_severity`, or
//      its "visible but disabled while off" gating drifting.
//   3. The instance-mailer warning banner (mirrored from
//      EmailNotifySettingsCard) not showing/hiding correctly.
//
// Only `useAlertConfig` / `usePutAlertConfig` and `useEmailNotifySettings`
// are mocked — the latter backs the warning banner only and would otherwise
// issue a real query.

vi.mock("./use-uptime", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./use-uptime")>();
  return {
    ...actual,
    useAlertConfig: vi.fn(),
    usePutAlertConfig: vi.fn(),
  };
});

vi.mock("@/features/email/use-email", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/email/use-email")>();
  return {
    ...actual,
    useEmailNotifySettings: vi.fn(),
  };
});

// Instance SMTP capability gating (5ae87b71): the "Configure SMTP" link in
// the instance-mailer banner below only renders for a principal
// `me.can_manage_instance_email` admits — everyone else sees plain text
// instead. `useMe` must be mocked (not left to hit the real, unmocked
// `getMe()`) so every existing assertion here states its principal
// explicitly rather than depending on whatever an unmocked query resolves
// to in jsdom.
vi.mock("@/features/auth/use-auth", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/auth/use-auth")>();
  return { ...actual, useMe: vi.fn() };
});

const mockedUseAlertConfig = vi.mocked(useAlertConfig);
const mockedUsePutAlertConfig = vi.mocked(usePutAlertConfig);
const mockedUseEmailNotifySettings = vi.mocked(useEmailNotifySettings);
const mockedUseMe = vi.mocked(useMe);

// Matches `usePutAlertConfig`'s optimistic-update context in use-uptime.ts.
type AlertConfigMutationContext = { previous: AlertConfig | null | undefined };

function mockPutAlertConfig(
  overrides: Partial<
    ReturnType<typeof usePutAlertConfig>
  > = {},
) {
  mockedUsePutAlertConfig.mockReturnValue(
    mockMutationResult<
      AlertConfig,
      AlertConfigUpdate,
      Error,
      AlertConfigMutationContext
    >(overrides),
  );
}

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
    // GH #291 Phase 3: required on the generated `AlertConfig` type. Most
    // tests in this file don't touch it, so a fixed default is fine; the
    // "application health alerts toggle" describe block below overrides it
    // directly, and `app-health-alert-prompt.test.tsx` covers the field's
    // other write path (the one-time upgrade prompt).
    app_alerts_enabled: false,
    ...overrides,
  };
}

function buildEmailSettings(
  overrides: Partial<EmailNotifySettings> = {},
): EmailNotifySettings {
  return {
    enabled: true,
    recipients: [],
    alert_on_failure: false,
    alert_throttle_minutes: 60,
    digest_enabled: false,
    digest_cadence: "daily",
    digest_day: 0,
    digest_hour: 8,
    timezone: "UTC",
    instance_mailer_configured: true,
    ...overrides,
  };
}

function buildMe(overrides: Partial<Me> = {}): Me {
  return {
    user: {
      id: "00000000-0000-0000-0000-000000000002",
      email: "owner@wpmgr.test",
      name: "Owner",
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
    },
    memberships: [],
    ...overrides,
  };
}

beforeEach(() => {
  vi.clearAllMocks();
  mockedUseEmailNotifySettings.mockReturnValue(
    mockQueryResult<EmailNotifySettings>({ data: buildEmailSettings() }),
  );
  // Default principal is instance-capable so every test in this file that
  // predates the capability gate (and doesn't care about it) keeps its
  // original "link always shows" behavior. The two capability-specific
  // describe blocks below override this per case.
  mockedUseMe.mockReturnValue(
    mockQueryResult<Me | null>({ data: buildMe({ can_manage_instance_email: true }) }),
  );
});

describe("AlertConfigForm — save always sends every signal field", () => {
  it("includes notify_security, notify_vulns, vuln_min_severity, and vuln_include_in_digest on save, even when only the recipients textarea was edited", async () => {
    const config = buildAlertConfig({
      email_recipients: ["ops@example.com"],
      notify_security: true,
      notify_vulns: true,
      vuln_min_severity: "medium",
      vuln_include_in_digest: false,
    });
    mockedUseAlertConfig.mockReturnValue(
      mockQueryResult<AlertConfig | null>({ data: config }),
    );
    const mutateMock = vi.fn();
    mockPutAlertConfig({ mutate: mutateMock });

    renderForm(<AlertConfigForm />);

    const recipients = await screen.findByLabelText("Email recipients");
    fireEvent.change(recipients, {
      target: { value: "ops@example.com\nnew@example.com" },
    });

    fireEvent.click(await screen.findByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(mutateMock).toHaveBeenCalledTimes(1));
    // The bug this guards against: a save that only "touches" recipients
    // must still resend the booleans/enum at their CURRENT form values —
    // never omit them and rely on the server to "preserve" what it already
    // had, which is exactly the silent-drop class of bug #247 fixed.
    expect(mutateMock).toHaveBeenCalledWith(
      expect.objectContaining({
        email_recipients: ["ops@example.com", "new@example.com"],
        notify_security: true,
        notify_vulns: true,
        vuln_min_severity: "medium",
        vuln_include_in_digest: false,
      }),
      expect.anything(),
    );
  });

  it("sends the flipped notify_security value when only the Security events switch is toggled", async () => {
    const config = buildAlertConfig({ notify_security: false });
    mockedUseAlertConfig.mockReturnValue(
      mockQueryResult<AlertConfig | null>({ data: config }),
    );
    const mutateMock = vi.fn();
    mockPutAlertConfig({ mutate: mutateMock });

    renderForm(<AlertConfigForm />);

    const securitySwitch = await screen.findByLabelText(
      "Email on high-severity security events",
    );
    expect(securitySwitch).toHaveAttribute("aria-checked", "false");
    fireEvent.click(securitySwitch);
    expect(securitySwitch).toHaveAttribute("aria-checked", "true");

    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(mutateMock).toHaveBeenCalledTimes(1));
    expect(mutateMock).toHaveBeenCalledWith(
      expect.objectContaining({ notify_security: true }),
      expect.anything(),
    );
  });
});

describe("AlertConfigForm - application health alerts toggle", () => {
  // GH #291 Phase 3: a dismissible one-time prompt (`app-health-alert-
  // prompt.tsx`) must never be the only way to turn this on, so this is the
  // toggle's permanent home, matching the same Controller + Switch pattern
  // as `notify_security` above.
  it("renders the Application health switch reflecting the current app_alerts_enabled value", async () => {
    const config = buildAlertConfig({ app_alerts_enabled: true });
    mockedUseAlertConfig.mockReturnValue(
      mockQueryResult<AlertConfig | null>({ data: config }),
    );
    mockPutAlertConfig();

    renderForm(<AlertConfigForm />);

    const appHealthSwitch = await screen.findByLabelText(
      "Enable application health alerts",
    );
    expect(appHealthSwitch).toHaveAttribute("aria-checked", "true");
  });

  it("sends the flipped app_alerts_enabled value when the Application health switch is toggled and saved", async () => {
    const config = buildAlertConfig({ app_alerts_enabled: false });
    mockedUseAlertConfig.mockReturnValue(
      mockQueryResult<AlertConfig | null>({ data: config }),
    );
    const mutateMock = vi.fn();
    mockPutAlertConfig({ mutate: mutateMock });

    renderForm(<AlertConfigForm />);

    const appHealthSwitch = await screen.findByLabelText(
      "Enable application health alerts",
    );
    expect(appHealthSwitch).toHaveAttribute("aria-checked", "false");
    fireEvent.click(appHealthSwitch);
    expect(appHealthSwitch).toHaveAttribute("aria-checked", "true");

    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(mutateMock).toHaveBeenCalledTimes(1));
    expect(mutateMock).toHaveBeenCalledWith(
      expect.objectContaining({ app_alerts_enabled: true }),
      expect.anything(),
    );
  });
});

describe("AlertConfigForm — vulnerability alerts severity select", () => {
  it("renders the configured minimum severity and submits a changed selection", async () => {
    const config = buildAlertConfig({
      notify_vulns: true,
      vuln_min_severity: "high",
    });
    mockedUseAlertConfig.mockReturnValue(
      mockQueryResult<AlertConfig | null>({ data: config }),
    );
    const mutateMock = vi.fn();
    mockPutAlertConfig({ mutate: mutateMock });

    renderForm(<AlertConfigForm />);

    const select = await screen.findByLabelText("Minimum severity");
    expect(select).toHaveValue("high");
    expect(select).toBeEnabled();

    fireEvent.change(select, { target: { value: "critical" } });
    expect(select).toHaveValue("critical");

    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(mutateMock).toHaveBeenCalledTimes(1));
    expect(mutateMock).toHaveBeenCalledWith(
      expect.objectContaining({ vuln_min_severity: "critical" }),
      expect.anything(),
    );
  });

  it("keeps the minimum-severity select and digest toggle visible but disabled while vulnerability alerts are off, without disabling the enable switch itself", async () => {
    const config = buildAlertConfig({
      notify_vulns: false,
      vuln_min_severity: "high",
      vuln_include_in_digest: true,
    });
    mockedUseAlertConfig.mockReturnValue(
      mockQueryResult<AlertConfig | null>({ data: config }),
    );
    mockPutAlertConfig();

    renderForm(<AlertConfigForm />);

    const enableSwitch = await screen.findByLabelText(
      "Enable vulnerability alerts",
    );
    const select = screen.getByLabelText("Minimum severity");
    const digestSwitch = screen.getByLabelText(
      "Include open findings in the email digest",
    );

    // Visible (not conditionally unmounted) ...
    expect(select).toBeInTheDocument();
    expect(digestSwitch).toBeInTheDocument();
    // ... but disabled, while the master switch stays interactive.
    expect(select).toBeDisabled();
    expect(digestSwitch).toBeDisabled();
    expect(enableSwitch).toBeEnabled();
  });
});

describe("AlertConfigForm — clearing recipients to zero (GH #706)", () => {
  // The exact regression: a customer with a SINGLE recipient could not clear
  // it and resorted to saving a fake address, because a client-only
  // non-empty refine blocked the submit entirely (no request ever went out).
  // Removing exactly one seeded recipient to zero is the only scenario that
  // actually exercises the old refine — a two-address test that drops to one
  // remaining address would pass against the broken code too.
  it("sends email_recipients: [] when the single seeded recipient is cleared and saved", async () => {
    const config = buildAlertConfig({ email_recipients: ["ops@example.com"] });
    mockedUseAlertConfig.mockReturnValue(
      mockQueryResult<AlertConfig | null>({ data: config }),
    );
    const mutateMock = vi.fn();
    mockPutAlertConfig({ mutate: mutateMock });

    renderForm(<AlertConfigForm />);

    const recipients = await screen.findByLabelText("Email recipients");
    fireEvent.change(recipients, { target: { value: "" } });
    fireEvent.blur(recipients);

    // The old refine's error must not appear, and Save must be reachable.
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(mutateMock).toHaveBeenCalledTimes(1));
    const [body] = mutateMock.mock.calls[0] as [AlertConfigUpdate];
    expect(body.email_recipients).toEqual([]);
  });

  it("shows a plain (non-error) empty state instead of a validation error when recipients are cleared", async () => {
    const config = buildAlertConfig({ email_recipients: ["ops@example.com"] });
    mockedUseAlertConfig.mockReturnValue(
      mockQueryResult<AlertConfig | null>({ data: config }),
    );
    mockPutAlertConfig();

    renderForm(<AlertConfigForm />);

    const recipients = await screen.findByLabelText("Email recipients");
    fireEvent.change(recipients, { target: { value: "" } });
    fireEvent.blur(recipients);

    expect(
      await screen.findByText(
        "No recipients configured. Email alerts are off until you add one.",
      ),
    ).toBeInTheDocument();
    // Never rendered as role="alert" — this is valid state, not a mistake.
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("round-trips a zero-recipient config after save: a refetch reflecting the save re-seeds the form empty without re-raising the old error", async () => {
    const config = buildAlertConfig({ email_recipients: ["ops@example.com"] });
    mockedUseAlertConfig.mockReturnValue(
      mockQueryResult<AlertConfig | null>({ data: config }),
    );
    mockPutAlertConfig();

    const { rerender } = renderForm(<AlertConfigForm />);

    // Simulate the server round trip: GET now reflects the saved empty list,
    // exactly like `useAlertConfig`'s cache after `usePutAlertConfig`
    // invalidates/refetches on settle.
    const cleared = buildAlertConfig({ email_recipients: [] });
    mockedUseAlertConfig.mockReturnValue(
      mockQueryResult<AlertConfig | null>({ data: cleared }),
    );
    rerender(
      <ShellContext.Provider
        value={{
          collapsed: false,
          toggleCollapsed: vi.fn(),
          mobileOpen: false,
          setMobileOpen: vi.fn(),
        }}
      >
        <AlertConfigForm />
      </ShellContext.Provider>,
    );

    const recipients = await screen.findByLabelText("Email recipients");
    await waitFor(() => expect(recipients).toHaveValue(""));
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.queryByText("No recipients")).not.toBeInTheDocument();
    expect(
      screen.getByText(
        "No recipients configured. Email alerts are off until you add one.",
      ),
    ).toBeInTheDocument();
  });
});

describe("AlertConfigForm — alert channel enabled switch (GH #706)", () => {
  it("reflects an enabled:true config and sends enabled:false when the switch is turned off and saved", async () => {
    const config = buildAlertConfig({ enabled: true });
    mockedUseAlertConfig.mockReturnValue(
      mockQueryResult<AlertConfig | null>({ data: config }),
    );
    const mutateMock = vi.fn();
    mockPutAlertConfig({ mutate: mutateMock });

    renderForm(<AlertConfigForm />);

    const enabledSwitch = await screen.findByLabelText("Alert channel enabled");
    expect(enabledSwitch).toHaveAttribute("aria-checked", "true");

    fireEvent.click(enabledSwitch);
    expect(enabledSwitch).toHaveAttribute("aria-checked", "false");

    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(mutateMock).toHaveBeenCalledTimes(1));
    expect(mutateMock).toHaveBeenCalledWith(
      expect.objectContaining({ enabled: false }),
      expect.anything(),
    );
  });

  it("reflects an enabled:false config and sends enabled:true when the switch is turned on and saved", async () => {
    const config = buildAlertConfig({ enabled: false });
    mockedUseAlertConfig.mockReturnValue(
      mockQueryResult<AlertConfig | null>({ data: config }),
    );
    const mutateMock = vi.fn();
    mockPutAlertConfig({ mutate: mutateMock });

    renderForm(<AlertConfigForm />);

    const enabledSwitch = await screen.findByLabelText("Alert channel enabled");
    expect(enabledSwitch).toHaveAttribute("aria-checked", "false");

    fireEvent.click(enabledSwitch);
    expect(enabledSwitch).toHaveAttribute("aria-checked", "true");

    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(mutateMock).toHaveBeenCalledTimes(1));
    expect(mutateMock).toHaveBeenCalledWith(
      expect.objectContaining({ enabled: true }),
      expect.anything(),
    );
  });
});

describe("AlertConfigForm — instance mailer warning banner", () => {
  it("shows the 'instance mailer not configured' banner with a link to /settings/smtp when instance email is not configured", async () => {
    mockedUseAlertConfig.mockReturnValue(
      mockQueryResult<AlertConfig | null>({ data: buildAlertConfig() }),
    );
    mockPutAlertConfig();
    mockedUseEmailNotifySettings.mockReturnValue(
      mockQueryResult<EmailNotifySettings>({
        data: buildEmailSettings({ instance_mailer_configured: false }),
      }),
    );
    // Capable principal (see beforeEach for the default) — the link only
    // renders for one the server admits; that gate is exercised on its own
    // below.

    renderForm(<AlertConfigForm />, { withRouter: true });

    expect(
      await screen.findByText("Instance mailer not configured."),
    ).toBeInTheDocument();
    const link = screen.getByRole("link", { name: "Configure SMTP" });
    expect(link).toHaveAttribute("href", "/settings/smtp");
  });

  it("hides the banner when instance email is configured", async () => {
    mockedUseAlertConfig.mockReturnValue(
      mockQueryResult<AlertConfig | null>({ data: buildAlertConfig() }),
    );
    mockPutAlertConfig();
    mockedUseEmailNotifySettings.mockReturnValue(
      mockQueryResult<EmailNotifySettings>({
        data: buildEmailSettings({ instance_mailer_configured: true }),
      }),
    );

    renderForm(<AlertConfigForm />);

    await screen.findByLabelText("Email recipients");
    expect(
      screen.queryByText("Instance mailer not configured."),
    ).not.toBeInTheDocument();
  });
});

// Instance SMTP capability gating (5ae87b71): the banner's "Configure SMTP"
// link is itself gated on `me.can_manage_instance_email` — a principal the
// server refuses gets the same warning text but no link, since it can only
// lead to a page that refuses them too.
describe("AlertConfigForm — instance mailer banner follows can_manage_instance_email", () => {
  beforeEach(() => {
    mockedUseAlertConfig.mockReturnValue(
      mockQueryResult<AlertConfig | null>({ data: buildAlertConfig() }),
    );
    mockPutAlertConfig();
    mockedUseEmailNotifySettings.mockReturnValue(
      mockQueryResult<EmailNotifySettings>({
        data: buildEmailSettings({ instance_mailer_configured: false }),
      }),
    );
  });

  it("shows plain text, no link, and tells the user to ask their instance administrator when the principal is not capable", async () => {
    mockedUseMe.mockReturnValue(
      mockQueryResult<Me | null>({ data: buildMe({ can_manage_instance_email: false }) }),
    );

    renderForm(<AlertConfigForm />, { withRouter: true });

    expect(
      await screen.findByText("Instance mailer not configured."),
    ).toBeInTheDocument();
    expect(screen.getByText(/Ask your instance administrator to configure it\./)).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Configure SMTP" })).not.toBeInTheDocument();
  });

  it("shows plain text, no link, when can_manage_instance_email is absent from the response", async () => {
    mockedUseMe.mockReturnValue(mockQueryResult<Me | null>({ data: buildMe() }));

    renderForm(<AlertConfigForm />, { withRouter: true });

    expect(
      await screen.findByText("Instance mailer not configured."),
    ).toBeInTheDocument();
    expect(screen.getByText(/Ask your instance administrator to configure it\./)).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Configure SMTP" })).not.toBeInTheDocument();
  });

  it("shows the link when the principal IS capable (sanity check the gate isn't just always-off)", async () => {
    mockedUseMe.mockReturnValue(
      mockQueryResult<Me | null>({ data: buildMe({ can_manage_instance_email: true }) }),
    );

    renderForm(<AlertConfigForm />, { withRouter: true });

    const link = await screen.findByRole("link", { name: "Configure SMTP" });
    expect(link).toHaveAttribute("href", "/settings/smtp");
    expect(
      screen.queryByText(/Ask your instance administrator to configure it\./),
    ).not.toBeInTheDocument();
  });
});
