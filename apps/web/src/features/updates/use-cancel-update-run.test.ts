import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createElement, type ReactNode } from "react";
import type { UpdateRun } from "@wpmgr/api";

// GH #463 — useCancelUpdateRun is the hook wiring the run-detail page's
// Cancel button to `POST /updates/runs/{id}/cancel`
// (apps/api/internal/update/handler.go:59,
// operationId cancelScheduledUpdateRun, openapi.yaml:6420). Before this
// change the facade (packages/openapi-client/src/index.ts) did not
// re-export the operation, so `@wpmgr/api` had no `cancelScheduledUpdateRun`
// for this hook to import at all — this file (and the hook it tests)
// literally could not exist.
//
// cancelScheduledUpdateRun is mocked at the @wpmgr/api boundary, exactly
// like use-admin-agent-mirror.test.ts, and NOT by driving the real
// generated client against a relative URL: doing that under jsdom throws an
// unhandled rejection that fails the suite while the summary line still
// reports every test passing (see that file's own note). Mocking the SDK
// function keeps this test asserting on the REAL response shapes
// (UpdateRunCancelResult / Error, from openapi.yaml) without touching fetch.

const {
  cancelScheduledUpdateRunMock,
  toastSuccess,
  toastWarning,
  toastError,
} = vi.hoisted(() => ({
  cancelScheduledUpdateRunMock: vi.fn(),
  toastSuccess: vi.fn(),
  toastWarning: vi.fn(),
  toastError: vi.fn(),
}));

vi.mock("@wpmgr/api", () => ({
  cancelScheduledUpdateRun: cancelScheduledUpdateRunMock,
}));

vi.mock("@/components/toast", () => ({
  toast: {
    success: toastSuccess,
    warning: toastWarning,
    error: toastError,
    info: vi.fn(),
  },
}));

import { useCancelUpdateRun, updatesKeys } from "./use-updates";

const RUN_ID = "11111111-1111-1111-1111-111111111111";
const TENANT_ID = "22222222-2222-2222-2222-222222222222";

function makeQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
}

function wrapperFor(qc: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return createElement(QueryClientProvider, { client: qc }, children);
  };
}

function haltedRun(): UpdateRun {
  return {
    id: RUN_ID,
    tenant_id: TENANT_ID,
    status: "halted",
    dry_run: false,
    created_at: "2026-08-19T00:00:00Z",
    updated_at: "2026-08-19T02:00:00Z",
    scheduled_at: "2026-08-19T02:00:00Z",
    tasks: [],
  };
}

beforeEach(() => {
  cancelScheduledUpdateRunMock.mockReset();
  toastSuccess.mockReset();
  toastWarning.mockReset();
  toastError.mockReset();
});

describe("useCancelUpdateRun", () => {
  it("200 resolves to 'cancelled', seeds the detail cache with the returned run, and toasts success (not an error, not a warning)", async () => {
    const run = haltedRun();
    cancelScheduledUpdateRunMock.mockResolvedValue({
      data: { run, cancelled_tasks: 3 },
      error: undefined,
      response: { status: 200 },
    });
    const qc = makeQueryClient();
    const { result } = renderHook(() => useCancelUpdateRun(RUN_ID), {
      wrapper: wrapperFor(qc),
    });

    const outcome = await result.current.mutateAsync();

    expect(outcome).toEqual({ kind: "cancelled", run, cancelledTasks: 3 });
    // The view must reflect the new state without a manual refresh: the
    // detail cache is seeded synchronously from the response.
    expect(qc.getQueryData(updatesKeys.detail(RUN_ID))).toEqual(run);
    await waitFor(() => expect(toastSuccess).toHaveBeenCalledTimes(1));
    const [title, opts] = toastSuccess.mock.calls[0] as [
      string,
      { description?: string },
    ];
    expect(title).toBe("Run cancelled");
    expect(opts.description).toContain("3 updates called back");
    expect(toastWarning).not.toHaveBeenCalled();
    expect(toastError).not.toHaveBeenCalled();
  });

  it("200 with zero cancelled tasks still reads as success, not an error", async () => {
    const run = haltedRun();
    cancelScheduledUpdateRunMock.mockResolvedValue({
      data: { run, cancelled_tasks: 0 },
      error: undefined,
      response: { status: 200 },
    });
    const { result } = renderHook(() => useCancelUpdateRun(RUN_ID), {
      wrapper: wrapperFor(makeQueryClient()),
    });
    const outcome = await result.current.mutateAsync();
    expect(outcome).toEqual({ kind: "cancelled", run, cancelledTasks: 0 });
    await waitFor(() =>
      expect(toastSuccess).toHaveBeenCalledWith(
        "Run cancelled",
        expect.objectContaining({
          description: "Nothing was sent to any site.",
        }),
      ),
    );
    expect(toastError).not.toHaveBeenCalled();
  });

  // apps/api/internal/update/service.go CancelScheduledRun: the 409 is "the
  // WHOLE FEATURE, not an edge case" — the run left `scheduled` between the
  // page loading and the click (it fired, or someone else cancelled it
  // first). The server names the status the run is actually in
  // (domain.Conflict("run_not_cancellable", "this run has already left the
  // scheduled state (now "+status+") and cannot be cancelled; use halt if it
  // is running")), and the client MUST render that verbatim as "too late",
  // never as a generic failure.
  it("409 run_not_cancellable resolves to 'too_late' with the server's own message, and warns rather than errors", async () => {
    const serverMessage =
      "this run has already left the scheduled state (now dispatching) and cannot be cancelled; use halt if it is running";
    cancelScheduledUpdateRunMock.mockResolvedValue({
      data: undefined,
      error: { code: "run_not_cancellable", message: serverMessage },
      response: { status: 409 },
    });
    const { result } = renderHook(() => useCancelUpdateRun(RUN_ID), {
      wrapper: wrapperFor(makeQueryClient()),
    });

    const outcome = await result.current.mutateAsync();

    expect(outcome).toEqual({ kind: "too_late", message: serverMessage });
    await waitFor(() =>
      expect(toastWarning).toHaveBeenCalledWith(
        "Too late to cancel",
        { description: serverMessage },
      ),
    );
    // The whole point of #463's 409: this must never render as a failure.
    expect(toastError).not.toHaveBeenCalled();
  });

  it("409 still triggers a re-read of the run so a stale countdown doesn't sit next to a dead button", async () => {
    cancelScheduledUpdateRunMock.mockResolvedValue({
      data: undefined,
      error: { code: "run_not_cancellable", message: "already fired" },
      response: { status: 409 },
    });
    const qc = makeQueryClient();
    const invalidateSpy = vi.spyOn(qc, "invalidateQueries");
    const { result } = renderHook(() => useCancelUpdateRun(RUN_ID), {
      wrapper: wrapperFor(qc),
    });
    await result.current.mutateAsync();
    await waitFor(() =>
      expect(invalidateSpy).toHaveBeenCalledWith(
        expect.objectContaining({ queryKey: updatesKeys.detail(RUN_ID) }),
      ),
    );
  });

  it("404 (run not found) is a genuine failure, distinct from the 409 'too late' outcome", async () => {
    cancelScheduledUpdateRunMock.mockResolvedValue({
      data: undefined,
      error: { code: "not_found", message: "Update run not found" },
      response: { status: 404 },
    });
    const { result } = renderHook(() => useCancelUpdateRun(RUN_ID), {
      wrapper: wrapperFor(makeQueryClient()),
    });
    await expect(result.current.mutateAsync()).rejects.toThrow(
      "Update run not found",
    );
    await waitFor(() => expect(toastError).toHaveBeenCalled());
    expect(toastWarning).not.toHaveBeenCalled();
  });

  it("a network/5xx failure rejects and toasts an error, never a warning", async () => {
    cancelScheduledUpdateRunMock.mockResolvedValue({
      data: undefined,
      error: undefined,
      response: undefined,
    });
    const { result } = renderHook(() => useCancelUpdateRun(RUN_ID), {
      wrapper: wrapperFor(makeQueryClient()),
    });
    await expect(result.current.mutateAsync()).rejects.toBeTruthy();
    await waitFor(() => expect(toastError).toHaveBeenCalled());
    expect(toastWarning).not.toHaveBeenCalled();
  });
});
