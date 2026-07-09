import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { renderHook, act } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createElement, type ReactNode } from "react";
import type { Site } from "@wpmgr/api";

// GH #187 (FE half) — `useRefreshScreenshot` must never end in a silent
// no-op. Before this fix, when the completion poll exhausted its 15 x 3s
// window with the server still reporting "pending" (the common self-host
// case: the media-encoder that performs the capture isn't running), the
// mutation's onSuccess poll loop just `return`ed — the card stayed on the
// "capturing" spinner forever and the operator got no further feedback at
// all. These tests pin: (b) poll exhaustion clears the optimistic state and
// fires a warning toast (the regression lock), and (c) the mutationFn maps
// 409/500/501/503 to distinct, correct messages.

const { refreshMock, toastWarning, toastSuccess, toastError, toastInfo } =
  vi.hoisted(() => ({
    refreshMock: vi.fn(),
    toastWarning: vi.fn(),
    toastSuccess: vi.fn(),
    toastError: vi.fn(),
    toastInfo: vi.fn(),
  }));

vi.mock("@wpmgr/api", () => ({
  client: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), delete: vi.fn() },
  listSites: vi.fn(),
  getSite: vi.fn(),
  deleteSite: vi.fn(),
  createPairingCode: vi.fn(),
  setSiteTags: vi.fn(),
  refreshSiteScreenshot: refreshMock,
}));

vi.mock("@/components/toast", () => ({
  toast: {
    warning: toastWarning,
    success: toastSuccess,
    error: toastError,
    info: toastInfo,
  },
}));

import { useRefreshScreenshot, sitesKeys } from "./use-sites";

function buildSite(overrides: Partial<Site> = {}): Site {
  return {
    id: "site-1",
    tenant_id: "tenant-1",
    url: "https://example.com",
    name: "Example",
    status: "active",
    wp_version: "6.8",
    php_version: "8.3",
    health_status: "healthy",
    multisite: false,
    tags: [],
    ...overrides,
  } as unknown as Site;
}

function makeQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  });
}

function wrapperFor(qc: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return createElement(QueryClientProvider, { client: qc }, children);
  };
}

beforeEach(() => {
  refreshMock.mockReset();
  toastWarning.mockReset();
  toastSuccess.mockReset();
  toastError.mockReset();
  toastInfo.mockReset();
});

describe("useRefreshScreenshot — poll exhaustion never ends silently (GH #187)", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it("clears the optimistic 'capturing' state to 'failed' and fires a warning toast once the 45s poll window exhausts with status still pending", async () => {
    vi.useFakeTimers();
    const qc = makeQueryClient();
    // Seed the list cache exactly as `useSites()` would have it before the
    // refresh — a real, ready screenshot from a prior capture.
    qc.setQueryData<Site[]>(sitesKeys.lists(), [
      buildSite({ screenshot_status: "ready", screenshot_url: "https://cdn/old.webp" }),
    ]);
    refreshMock.mockResolvedValue({
      data: undefined,
      error: undefined,
      response: { status: 202 },
    });

    const { result } = renderHook(() => useRefreshScreenshot(), {
      wrapper: wrapperFor(qc),
    });

    await act(async () => {
      result.current.mutate("site-1");
      // Flush the mocked network promise + onMutate/onSuccess microtask
      // chain before any timer exists to advance.
      await Promise.resolve();
      await Promise.resolve();
      await Promise.resolve();
    });

    // Optimistic patch landed: spinner state, stale url cleared.
    const afterMutate = qc.getQueryData<Site[]>(sitesKeys.lists());
    expect(afterMutate?.[0]?.screenshot_status).toBe("pending");
    expect(afterMutate?.[0]?.screenshot_url).toBeUndefined();

    // Drive the poll through all 15 ticks (3s apart = 45s) with the server
    // NEVER reporting a terminal status — the exact "stuck job" scenario
    // (self-host media-encoder not running) that #187 reports.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(45_000);
    });

    const afterExhaustion = qc.getQueryData<Site[]>(sitesKeys.lists());
    // THE FIX: the card must leave the spinner state. Against the old code
    // (a silent `return` on exhaustion with no cache patch) this assertion
    // fails — screenshot_status would still read "pending" forever.
    expect(afterExhaustion?.[0]?.screenshot_status).toBe("failed");

    // THE FIX: an operator-visible signal must fire — never silence.
    expect(toastWarning).toHaveBeenCalledTimes(1);
    const [title, opts] = toastWarning.mock.calls[0] as [string, { description?: string }?];
    expect(title).toMatch(/didn't finish/i);
    expect(opts?.description).toMatch(/media-encoder/i);

    // No other toast fired — the warning is the one and only terminal signal.
    expect(toastSuccess).not.toHaveBeenCalled();
    expect(toastError).not.toHaveBeenCalled();
  });

  it("does NOT force 'failed' or warn when the server reports a terminal status before the poll window exhausts", async () => {
    vi.useFakeTimers();
    const qc = makeQueryClient();
    qc.setQueryData<Site[]>(sitesKeys.lists(), [buildSite()]);
    refreshMock.mockResolvedValue({
      data: undefined,
      error: undefined,
      response: { status: 202 },
    });

    const { result } = renderHook(() => useRefreshScreenshot(), {
      wrapper: wrapperFor(qc),
    });

    await act(async () => {
      result.current.mutate("site-1");
      await Promise.resolve();
      await Promise.resolve();
      await Promise.resolve();
    });

    // Simulate the SSE/refetch path landing a real "ready" screenshot after
    // the first poll tick fires (before exhaustion).
    await act(async () => {
      await vi.advanceTimersByTimeAsync(3000);
      qc.setQueryData<Site[]>(sitesKeys.lists(), [
        buildSite({
          screenshot_status: "ready",
          screenshot_url: "https://cdn/new.webp",
        }),
      ]);
      // Give the poll loop the rest of the window; it must see the terminal
      // status on its next tick and stop rather than overwriting it.
      await vi.advanceTimersByTimeAsync(45_000);
    });

    const final = qc.getQueryData<Site[]>(sitesKeys.lists());
    expect(final?.[0]?.screenshot_status).toBe("ready");
    expect(final?.[0]?.screenshot_url).toBe("https://cdn/new.webp");
    expect(toastWarning).not.toHaveBeenCalled();
  });
});

describe("useRefreshScreenshot — mutationFn error mapping (GH #187)", () => {
  it("maps 409 to a not-enrolled message", async () => {
    refreshMock.mockResolvedValue({ error: undefined, response: { status: 409 } });
    const { result } = renderHook(() => useRefreshScreenshot(), {
      wrapper: wrapperFor(makeQueryClient()),
    });
    await expect(result.current.mutateAsync("site-1")).rejects.toThrow(
      /not enrolled/i,
    );
  });

  it("maps 503 to 'the screenshot service isn't running' (worker not up)", async () => {
    refreshMock.mockResolvedValue({ error: undefined, response: { status: 503 } });
    const { result } = renderHook(() => useRefreshScreenshot(), {
      wrapper: wrapperFor(makeQueryClient()),
    });
    await expect(result.current.mutateAsync("site-1")).rejects.toThrow(
      "The screenshot service isn't running.",
    );
  });

  it("maps 501 to the not-configured message", async () => {
    refreshMock.mockResolvedValue({ error: undefined, response: { status: 501 } });
    const { result } = renderHook(() => useRefreshScreenshot(), {
      wrapper: wrapperFor(makeQueryClient()),
    });
    await expect(result.current.mutateAsync("site-1")).rejects.toThrow(
      "Screenshots aren't configured on this server.",
    );
  });

  it("maps 500 to the SAME not-configured message as 501 (previously fell through to a generic error)", async () => {
    refreshMock.mockResolvedValue({ error: undefined, response: { status: 500 } });
    const { result } = renderHook(() => useRefreshScreenshot(), {
      wrapper: wrapperFor(makeQueryClient()),
    });
    await expect(result.current.mutateAsync("site-1")).rejects.toThrow(
      "Screenshots aren't configured on this server.",
    );
  });
});
