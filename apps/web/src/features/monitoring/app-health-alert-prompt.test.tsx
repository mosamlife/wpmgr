import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, fireEvent } from "@testing-library/react";
import type { Me, AlertConfig, AlertConfigUpdate } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";
import { mockQueryResult, mockMutationResult } from "@/test/query-mocks";
import { useMe } from "@/features/auth/use-auth";

import {
  AppHealthAlertUpgradePrompt,
  AppHealthAlertUpgradePromptView,
} from "./app-health-alert-prompt";
import { useAlertConfig, usePutAlertConfig } from "./use-uptime";

// GH #291 Phase 3 — the reporter-requested one-time upgrade prompt.
//
// Two groups of tests:
//   - The VIEW: exercised directly with injected props, so the copy,
//     structure, and both actions are proven correct independent of the
//     gating logic.
//   - The CONTAINER: proves the four gating rules (feature availability,
//     tenant resolved, role, dismissal) against the REAL
//     `useAppHealthAlertingAvailable()` hook — only `use-uptime`'s
//     `useAlertConfig`/`usePutAlertConfig` are mocked, at the data-fetch
//     boundary. This is the test class that was missing before: a container
//     test that mocks `useAppHealthAlertingAvailable` directly can never
//     notice a regression INSIDE that hook (e.g. a reintroduced hard-coded
//     `return false`) — exactly how the dead Phase 3 prompt shipped
//     unnoticed in the first place.

vi.mock("@/features/auth/use-auth", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/auth/use-auth")>();
  return { ...actual, useMe: vi.fn() };
});

vi.mock("./use-uptime", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./use-uptime")>();
  return { ...actual, useAlertConfig: vi.fn(), usePutAlertConfig: vi.fn() };
});

const mockedUseMe = vi.mocked(useMe);
const mockedUseAlertConfig = vi.mocked(useAlertConfig);
const mockedUsePutAlertConfig = vi.mocked(usePutAlertConfig);

function buildMe(overrides: Partial<Me> = {}): Me {
  return {
    user: {
      id: "u1",
      email: "owner@example.com",
      name: "Owner",
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
    },
    memberships: [{ user_id: "u1", tenant_id: "t1", role: "owner" }],
    active_tenant_id: "t1",
    hosted: true,
    ...overrides,
  };
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
    app_alerts_enabled: false,
    ...overrides,
  };
}

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
    >({ mutate: vi.fn(), ...overrides }),
  );
}

describe("AppHealthAlertUpgradePromptView", () => {
  it("renders calm, specific copy with no em or en dashes", () => {
    renderWithProviders(
      <AppHealthAlertUpgradePromptView onEnable={vi.fn()} onDismiss={vi.fn()} />,
    );

    const heading = screen.getByRole("heading", {
      name: "Application health alerting is available",
    });
    expect(heading).toBeInTheDocument();

    const region = screen.getByRole("region", {
      name: "Application health alerting is available",
    });
    expect(region.textContent).not.toMatch(/[–—]/); // en dash / em dash
  });

  it("explains why alerting starts off", () => {
    renderWithProviders(
      <AppHealthAlertUpgradePromptView onEnable={vi.fn()} onDismiss={vi.fn()} />,
    );

    expect(
      screen.getByText(/some sites may have been quietly broken for a while/),
    ).toBeInTheDocument();
  });

  it("offers both actions honestly: enable, and leave it off", () => {
    const onEnable = vi.fn();
    const onDismiss = vi.fn();
    renderWithProviders(
      <AppHealthAlertUpgradePromptView onEnable={onEnable} onDismiss={onDismiss} />,
    );

    fireEvent.click(screen.getByRole("button", { name: "Enable app health alerts" }));
    expect(onEnable).toHaveBeenCalledTimes(1);

    fireEvent.click(screen.getByRole("button", { name: "Leave it off" }));
    expect(onDismiss).toHaveBeenCalledTimes(1);
  });

  it("disables both actions while the enable mutation is in flight", () => {
    renderWithProviders(
      <AppHealthAlertUpgradePromptView
        onEnable={vi.fn()}
        onDismiss={vi.fn()}
        isEnabling
      />,
    );

    expect(screen.getByRole("button", { name: "Enabling…" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Leave it off" })).toBeDisabled();
  });
});

describe("AppHealthAlertUpgradePrompt (gating, against the real availability hook)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    window.localStorage.clear();
    mockedUseMe.mockReturnValue({ data: buildMe() } as ReturnType<typeof useMe>);
    mockPutAlertConfig();
  });

  it("renders for an operator once application health alerting is off for the tenant and not yet dismissed", () => {
    mockedUseAlertConfig.mockReturnValue(
      mockQueryResult<AlertConfig | null>({
        data: buildAlertConfig({ app_alerts_enabled: false }),
      }),
    );

    renderWithProviders(<AppHealthAlertUpgradePrompt />);

    expect(
      screen.getByText("Application health alerting is available"),
    ).toBeInTheDocument();
  });

  it("does not render while the tenant's alert config is still loading", () => {
    mockedUseAlertConfig.mockReturnValue(
      mockQueryResult<AlertConfig | null>({
        data: undefined,
        isPending: true,
        isSuccess: false,
      }),
    );

    renderWithProviders(<AppHealthAlertUpgradePrompt />);

    expect(
      screen.queryByText("Application health alerting is available"),
    ).not.toBeInTheDocument();
  });

  it("does not render when application health alerting is already on for the tenant", () => {
    mockedUseAlertConfig.mockReturnValue(
      mockQueryResult<AlertConfig | null>({
        data: buildAlertConfig({ app_alerts_enabled: true }),
      }),
    );

    renderWithProviders(<AppHealthAlertUpgradePrompt />);

    expect(
      screen.queryByText("Application health alerting is available"),
    ).not.toBeInTheDocument();
  });

  it("does not render for a user who lacks permission to change it, even when alerting is off", () => {
    mockedUseAlertConfig.mockReturnValue(
      mockQueryResult<AlertConfig | null>({
        data: buildAlertConfig({ app_alerts_enabled: false }),
      }),
    );
    mockedUseMe.mockReturnValue({
      data: buildMe({
        memberships: [{ user_id: "u1", tenant_id: "t1", role: "viewer" }],
      }),
    } as ReturnType<typeof useMe>);

    renderWithProviders(<AppHealthAlertUpgradePrompt />);

    expect(
      screen.queryByText("Application health alerting is available"),
    ).not.toBeInTheDocument();
  });

  it("does not render again once dismissed", () => {
    mockedUseAlertConfig.mockReturnValue(
      mockQueryResult<AlertConfig | null>({
        data: buildAlertConfig({ app_alerts_enabled: false }),
      }),
    );
    window.localStorage.setItem(
      "wpmgr.app-health-alert-prompt.dismissed.t1",
      "true",
    );

    renderWithProviders(<AppHealthAlertUpgradePrompt />);

    expect(
      screen.queryByText("Application health alerting is available"),
    ).not.toBeInTheDocument();
  });

  it("persists dismissal when 'Leave it off' is clicked", () => {
    mockedUseAlertConfig.mockReturnValue(
      mockQueryResult<AlertConfig | null>({
        data: buildAlertConfig({ app_alerts_enabled: false }),
      }),
    );

    renderWithProviders(<AppHealthAlertUpgradePrompt />);

    fireEvent.click(screen.getByRole("button", { name: "Leave it off" }));

    expect(
      window.localStorage.getItem("wpmgr.app-health-alert-prompt.dismissed.t1"),
    ).toBe("true");
    expect(
      screen.queryByText("Application health alerting is available"),
    ).not.toBeInTheDocument();
  });

  it("saves app_alerts_enabled=true (preserving every other field) and dismisses on success when 'Enable app health alerts' is clicked", () => {
    mockedUseAlertConfig.mockReturnValue(
      mockQueryResult<AlertConfig | null>({
        data: buildAlertConfig({ app_alerts_enabled: false }),
      }),
    );
    // Cast at the mock boundary (matches `query-mocks.ts`'s own convention):
    // the real `UseMutateFunction`'s `onSuccess` carries TanStack Query's
    // full internal (data, variables, onMutateResult, context) signature,
    // which this fake has no use for beyond invoking the success callback.
    const mutateMock = vi.fn(
      (_vars: AlertConfigUpdate, opts?: { onSuccess?: () => void }) => {
        opts?.onSuccess?.();
      },
    ) as unknown as ReturnType<typeof usePutAlertConfig>["mutate"];
    mockPutAlertConfig({ mutate: mutateMock });

    renderWithProviders(<AppHealthAlertUpgradePrompt />);

    fireEvent.click(
      screen.getByRole("button", { name: "Enable app health alerts" }),
    );

    // Only the changed field is sent — every other alert-config field
    // (recipients, webhook, downtime/security/vulnerability settings) is
    // omitted so the backend's merge preserves it (see the field's own doc
    // comment on AlertConfigUpdate).
    expect(mutateMock).toHaveBeenCalledWith(
      { app_alerts_enabled: true },
      expect.anything(),
    );
    expect(
      window.localStorage.getItem("wpmgr.app-health-alert-prompt.dismissed.t1"),
    ).toBe("true");
  });
});
