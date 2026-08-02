import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createElement, type ReactNode } from "react";

// GH #322: useCheckAgentMirrorNow must turn each of the manual-trigger
// endpoint's REAL outcomes into the exact typed result (and toast) the
// design specifies, and MUST NOT treat "rate limited" or "already running"
// as errors (C3/C5): both are the mirror truthfully reporting that nothing
// new happened, not a failure.
//
// checkAgentMirrorNow is mocked at the @wpmgr/api boundary (the generated
// SDK function), not at client.post: the whole point of moving onto the
// generated operation is that the URL can no longer be typo'd independently
// of the OpenAPI contract, so these tests assert on the real response
// shapes (AgentMirrorCheckQueued / Error) rather than a hand-built fixture.

const { checkAgentMirrorNowMock, toastSuccess, toastError, toastInfo } = vi.hoisted(() => ({
  checkAgentMirrorNowMock: vi.fn(),
  toastSuccess: vi.fn(),
  toastError: vi.fn(),
  toastInfo: vi.fn(),
}));

vi.mock("@wpmgr/api", () => ({
  checkAgentMirrorNow: checkAgentMirrorNowMock,
}));

vi.mock("@/components/toast", () => ({
  toast: { success: toastSuccess, error: toastError, info: toastInfo, warning: vi.fn() },
}));

import { useCheckAgentMirrorNow } from "./use-admin-agent-mirror";

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

beforeEach(() => {
  checkAgentMirrorNowMock.mockReset();
  toastSuccess.mockReset();
  toastError.mockReset();
  toastInfo.mockReset();
});

describe("useCheckAgentMirrorNow", () => {
  it("202 resolves to 'queued' and toasts the control-plane message verbatim", async () => {
    checkAgentMirrorNowMock.mockResolvedValue({
      data: {
        status: "queued",
        queued_at: "2026-08-02T10:00:00Z",
        message: "A mirror run has been queued. It has not run yet; refresh the fleet agent view for the result.",
      },
      error: undefined,
      response: { status: 202 },
    });
    const { result } = renderHook(() => useCheckAgentMirrorNow(), {
      wrapper: wrapperFor(makeQueryClient()),
    });
    const outcome = await result.current.mutateAsync();
    expect(outcome).toEqual({
      kind: "queued",
      message: "A mirror run has been queued. It has not run yet; refresh the fleet agent view for the result.",
    });
    await waitFor(() =>
      expect(toastSuccess).toHaveBeenCalledWith(
        "A mirror run has been queued. It has not run yet; refresh the fleet agent view for the result.",
      ),
    );
    expect(toastError).not.toHaveBeenCalled();
  });

  it("409 agent_mirror_check_in_flight resolves to 'already_running' as an INFO toast, never an error", async () => {
    checkAgentMirrorNowMock.mockResolvedValue({
      data: undefined,
      error: {
        code: "agent_mirror_check_in_flight",
        message: "a mirror check is already queued or running",
      },
      response: { status: 409 },
    });
    const { result } = renderHook(() => useCheckAgentMirrorNow(), {
      wrapper: wrapperFor(makeQueryClient()),
    });
    const outcome = await result.current.mutateAsync();
    expect(outcome).toEqual({ kind: "already_running" });
    await waitFor(() =>
      expect(toastInfo).toHaveBeenCalledWith(
        "A check is already running. Its result will appear on the fleet agent view when it finishes.",
      ),
    );
    expect(toastError).not.toHaveBeenCalled();
  });

  it("429 agent_mirror_rate_limited resolves to 'rate_limited' as an INFO toast (C5: never an error) and reads retry_after_seconds", async () => {
    checkAgentMirrorNowMock.mockResolvedValue({
      data: undefined,
      error: {
        code: "agent_mirror_rate_limited",
        message: "the upstream release was last requested 12m ago",
        details: {
          retry_after_seconds: 480,
          next_check_after: "2026-08-02T10:20:00Z",
          last_request_at: "2026-08-02T10:04:00Z",
        },
      },
      response: { status: 429 },
    });
    const { result } = renderHook(() => useCheckAgentMirrorNow(), {
      wrapper: wrapperFor(makeQueryClient()),
    });
    const outcome = await result.current.mutateAsync();
    expect(outcome).toEqual({ kind: "rate_limited", retryAfterSeconds: 480 });
    await waitFor(() =>
      expect(toastInfo).toHaveBeenCalledWith(
        "Not checked. The mirror must wait 8 minutes before its next upstream request. The scheduled check still runs.",
      ),
    );
    expect(toastError).not.toHaveBeenCalled();
  });

  it("429 without retry_after_seconds still gives the honest 'not checked' message, not a fabricated number", async () => {
    checkAgentMirrorNowMock.mockResolvedValue({
      data: undefined,
      error: { code: "agent_mirror_rate_limited", message: "rate limited" },
      response: { status: 429 },
    });
    const { result } = renderHook(() => useCheckAgentMirrorNow(), {
      wrapper: wrapperFor(makeQueryClient()),
    });
    const outcome = await result.current.mutateAsync();
    expect(outcome).toEqual({ kind: "rate_limited", retryAfterSeconds: undefined });
    await waitFor(() =>
      expect(toastInfo).toHaveBeenCalledWith(
        "Not checked. The mirror must wait before its next upstream request. The scheduled check still runs.",
      ),
    );
  });

  it("429 under a minute reads in seconds, not '0 minutes'", async () => {
    checkAgentMirrorNowMock.mockResolvedValue({
      data: undefined,
      error: {
        code: "agent_mirror_rate_limited",
        message: "rate limited",
        details: { retry_after_seconds: 45 },
      },
      response: { status: 429 },
    });
    const { result } = renderHook(() => useCheckAgentMirrorNow(), {
      wrapper: wrapperFor(makeQueryClient()),
    });
    await result.current.mutateAsync();
    await waitFor(() =>
      expect(toastInfo).toHaveBeenCalledWith(
        "Not checked. The mirror must wait 45 seconds before its next upstream request. The scheduled check still runs.",
      ),
    );
  });

  it("503 agent_mirror_disabled is a genuine failure, not a silent no-op", async () => {
    checkAgentMirrorNowMock.mockResolvedValue({
      data: undefined,
      error: {
        code: "agent_mirror_disabled",
        message: "the upstream agent-release mirror is disabled on this install (WPMGR_UPDATE_AGENT_MIRROR_ENABLED)",
      },
      response: { status: 503 },
    });
    const { result } = renderHook(() => useCheckAgentMirrorNow(), {
      wrapper: wrapperFor(makeQueryClient()),
    });
    await expect(result.current.mutateAsync()).rejects.toThrow(
      "the upstream agent-release mirror is disabled on this install (WPMGR_UPDATE_AGENT_MIRROR_ENABLED)",
    );
    await waitFor(() => expect(toastError).toHaveBeenCalled());
  });

  it("503 agent_mirror_not_configured is a genuine failure", async () => {
    checkAgentMirrorNowMock.mockResolvedValue({
      data: undefined,
      error: {
        code: "agent_mirror_not_configured",
        message: "the upstream agent-release mirror is enabled but cannot run (object storage is not configured, WPMGR_S3_*)",
      },
      response: { status: 503 },
    });
    const { result } = renderHook(() => useCheckAgentMirrorNow(), {
      wrapper: wrapperFor(makeQueryClient()),
    });
    await expect(result.current.mutateAsync()).rejects.toThrow(
      "the upstream agent-release mirror is enabled but cannot run (object storage is not configured, WPMGR_S3_*)",
    );
  });

  it("403 superadmin_required is a genuine failure with the server's message", async () => {
    checkAgentMirrorNowMock.mockResolvedValue({
      data: undefined,
      error: { code: "superadmin_required", message: "superadmin access required" },
      response: { status: 403 },
    });
    const { result } = renderHook(() => useCheckAgentMirrorNow(), {
      wrapper: wrapperFor(makeQueryClient()),
    });
    await expect(result.current.mutateAsync()).rejects.toThrow("superadmin access required");
  });

  it("an unlabelled network/5xx failure (empty error message) falls back to the fixed, non-parameterised sentence", async () => {
    checkAgentMirrorNowMock.mockResolvedValue({
      data: undefined,
      error: { code: "internal", message: "" },
      response: { status: 500 },
    });
    const { result } = renderHook(() => useCheckAgentMirrorNow(), {
      wrapper: wrapperFor(makeQueryClient()),
    });
    await expect(result.current.mutateAsync()).rejects.toBeTruthy();
    await waitFor(() =>
      expect(toastError).toHaveBeenCalledWith(
        "Could not queue a check. Try again shortly, or see the control plane logs.",
      ),
    );
  });

  it("no error body at all (network drop) still rejects and shows the fixed sentence", async () => {
    checkAgentMirrorNowMock.mockResolvedValue({
      data: undefined,
      error: undefined,
      response: undefined,
    });
    const { result } = renderHook(() => useCheckAgentMirrorNow(), {
      wrapper: wrapperFor(makeQueryClient()),
    });
    await expect(result.current.mutateAsync()).rejects.toBeTruthy();
    await waitFor(() => expect(toastError).toHaveBeenCalled());
  });
});
