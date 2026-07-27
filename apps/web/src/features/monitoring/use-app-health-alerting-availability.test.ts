import { describe, it, expect, vi } from "vitest";
import { renderHook } from "@testing-library/react";
import type { AlertConfig } from "@wpmgr/api";

import { mockQueryResult } from "@/test/query-mocks";

import { useAlertConfig } from "./use-uptime";
import { useAppHealthAlertingAvailable } from "./use-app-health-alerting-availability";

// GH #291 Phase 3 — direct regression coverage for the hook that gates the
// one-time upgrade prompt (`app-health-alert-prompt.tsx`).
//
// This is the test class whose absence let a hard-coded `return false` ship
// to production unnoticed: it exercises the REAL hook body against a mocked
// `useAlertConfig()` (the data-fetch boundary), never a mock of
// `useAppHealthAlertingAvailable` itself standing in for it. A regression
// that reintroduces a hard-coded constant, in either direction, fails here
// immediately.

vi.mock("./use-uptime", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./use-uptime")>();
  return { ...actual, useAlertConfig: vi.fn() };
});

const mockedUseAlertConfig = vi.mocked(useAlertConfig);

function buildAlertConfig(overrides: Partial<AlertConfig> = {}): AlertConfig {
  return {
    email_recipients: [],
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

describe("useAppHealthAlertingAvailable", () => {
  it("is true once the tenant's alert config has loaded with application health alerting off", () => {
    mockedUseAlertConfig.mockReturnValue(
      mockQueryResult<AlertConfig | null>({
        data: buildAlertConfig({ app_alerts_enabled: false }),
      }),
    );

    const { result } = renderHook(() => useAppHealthAlertingAvailable());

    expect(result.current).toBe(true);
  });

  it("is false once application health alerting is already on for the tenant", () => {
    mockedUseAlertConfig.mockReturnValue(
      mockQueryResult<AlertConfig | null>({
        data: buildAlertConfig({ app_alerts_enabled: true }),
      }),
    );

    const { result } = renderHook(() => useAppHealthAlertingAvailable());

    expect(result.current).toBe(false);
  });

  it("is false while the tenant's alert config is still loading", () => {
    mockedUseAlertConfig.mockReturnValue(
      mockQueryResult<AlertConfig | null>({
        data: undefined,
        isPending: true,
        isSuccess: false,
      }),
    );

    const { result } = renderHook(() => useAppHealthAlertingAvailable());

    expect(result.current).toBe(false);
  });

  it("is false when the alert config query has errored", () => {
    mockedUseAlertConfig.mockReturnValue(
      mockQueryResult<AlertConfig | null>({
        data: undefined,
        isError: true,
        isSuccess: false,
        error: new Error("network error"),
      }),
    );

    const { result } = renderHook(() => useAppHealthAlertingAvailable());

    expect(result.current).toBe(false);
  });
});
