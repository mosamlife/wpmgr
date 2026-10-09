import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";

import { createTestQueryClient } from "@/test/render";

import { failResult, fleetSite, okResult } from "./readiness-fixtures";
import {
  AiReadinessRefreshError,
  toRefreshError,
  useAiReadinessRollup,
  useBoundedPolling,
} from "./use-ai-readiness";

// The owner's contract for the refetch window after "Check again": every 15
// seconds, at most 8 times. Written out, not imported, so a change to the
// implementation's constants is a change a test has to be told about.
const POLL_MS = 15_000;
const MAX_POLLS = 8;

const getFleet = vi.fn();

vi.mock("@wpmgr/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wpmgr/api")>();
  return {
    ...actual,
    getFleetAiReadiness: (...a: unknown[]): unknown => getFleet(...a),
  };
});

beforeEach(() => {
  getFleet.mockReset();
});

afterEach(() => {
  vi.useRealTimers();
});

describe("toRefreshError", () => {
  // 409 site_unreachable: apps/api/internal/aireadiness/service.go RequestRefresh
  // (domain.Conflict) through the {code, message} envelope in httpx/respond.go.
  it("maps 409 site_unreachable to the unreachable message", () => {
    const err = toRefreshError({ code: "site_unreachable", message: "site agent heartbeat is stale" }, 409);
    expect(err).toBeInstanceOf(AiReadinessRefreshError);
    expect(err.kind).toBe("unreachable");
    expect(err.message).toBe(
      "WPMgr could not reach this site, so nothing was checked. Try again when the site is back online.",
    );
  });

  // 403 insufficient_permission: apps/api/internal/authz/middleware.go RequirePermission.
  it("maps 403 to the permission message", () => {
    const err = toRefreshError({ code: "insufficient_permission", message: "your role does not permit this action" }, 403);
    expect(err.kind).toBe("forbidden");
    expect(err.message).toBe("You do not have permission to run this check.");
  });

  it("does not treat a 409 with another code as the site being unreachable", () => {
    const err = toRefreshError({ code: "something_else", message: "a different conflict" }, 409);
    expect(err.kind).toBe("other");
    expect(err.message).toBe("a different conflict");
  });

  // 503 ai_readiness_refresh_unavailable: service.go RequestRefresh.
  it("keeps the server's own message for any other failure", () => {
    const err = toRefreshError(
      { code: "ai_readiness_refresh_unavailable", message: "Asking the site to report again is not available on this install." },
      503,
    );
    expect(err.kind).toBe("other");
    expect(err.message).toBe("Asking the site to report again is not available on this install.");
  });

  it("falls back to a connection hint when the body carries no message", () => {
    const err = toRefreshError(undefined, undefined);
    expect(err.kind).toBe("other");
    expect(err.message).toBe("Check your connection and try again.");
  });
});

describe("useBoundedPolling", () => {
  it("does nothing until started", () => {
    vi.useFakeTimers();
    const refetch = vi.fn();
    renderHook(() => useBoundedPolling(refetch));
    vi.advanceTimersByTime(POLL_MS * 20);
    expect(refetch).not.toHaveBeenCalled();
  });

  it("refetches every interval, exactly MAX_POLLS times, then stops", () => {
    vi.useFakeTimers();
    const refetch = vi.fn();
    const { result } = renderHook(() => useBoundedPolling(refetch));
    act(() => result.current.start());
    expect(result.current.polling).toBe(true);

    for (let i = 1; i <= MAX_POLLS; i += 1) {
      act(() => {
        vi.advanceTimersByTime(POLL_MS);
      });
      expect(refetch).toHaveBeenCalledTimes(i);
    }
    expect(result.current.polling).toBe(false);

    act(() => {
      vi.advanceTimersByTime(POLL_MS * 20);
    });
    expect(refetch).toHaveBeenCalledTimes(MAX_POLLS);
  });

  it("starting again opens a fresh window", () => {
    vi.useFakeTimers();
    const refetch = vi.fn();
    const { result } = renderHook(() => useBoundedPolling(refetch, 1000, 2));
    // One tick per act: the next timer is armed by the render the last one caused.
    const tick = () =>
      act(() => {
        vi.advanceTimersByTime(1000);
      });

    act(() => result.current.start());
    tick();
    tick();
    tick();
    expect(refetch).toHaveBeenCalledTimes(2);

    act(() => result.current.start());
    tick();
    tick();
    tick();
    expect(refetch).toHaveBeenCalledTimes(4);
  });

  it("stops when the component unmounts", () => {
    vi.useFakeTimers();
    const refetch = vi.fn();
    const { result, unmount } = renderHook(() => useBoundedPolling(refetch));
    act(() => result.current.start());
    unmount();
    vi.advanceTimersByTime(POLL_MS * 20);
    expect(refetch).not.toHaveBeenCalled();
  });
});

describe("useAiReadinessRollup", () => {
  function wrapper() {
    const queryClient = createTestQueryClient();
    return function Wrapper({ children }: { children: ReactNode }) {
      return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
    };
  }

  it("is loading until the rollup arrives, then ready and keyed by site", async () => {
    getFleet.mockResolvedValue(okResult({ sites: [fleetSite({ site_id: "s-1", status: "incomplete" })] }));
    const { result } = renderHook(() => useAiReadinessRollup(), { wrapper: wrapper() });
    expect(result.current).toEqual({ state: "loading" });
    await waitFor(() => expect(result.current.state).toBe("ready"));
    if (result.current.state !== "ready") throw new Error("expected ready");
    expect(result.current.bySite.get("s-1")?.status).toBe("incomplete");
  });

  // 403 insufficient_permission: apps/api/internal/authz/middleware.go.
  it("is unavailable, not thrown, when the rollup is refused", async () => {
    getFleet.mockResolvedValue(failResult(403, "insufficient_permission", "your role does not permit this action"));
    const { result } = renderHook(() => useAiReadinessRollup(), { wrapper: wrapper() });
    await waitFor(() => expect(result.current).toEqual({ state: "unavailable" }));
  });

  it("is unavailable when the request itself fails", async () => {
    getFleet.mockRejectedValue(new TypeError("Failed to fetch"));
    const { result } = renderHook(() => useAiReadinessRollup(), { wrapper: wrapper() });
    await waitFor(() => expect(result.current).toEqual({ state: "unavailable" }));
  });
});
