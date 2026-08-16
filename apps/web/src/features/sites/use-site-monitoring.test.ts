import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, act } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createElement, type ReactNode } from "react";
import type { MonitoringBulkResult } from "@wpmgr/api";

// GH #414 phase 4b — `use-site-monitoring.ts` had ZERO executed test
// coverage. The only test that imports it,
// `routes/_authed/sites/-siteId-pause.test.tsx`, does
// `vi.mock("@/features/sites/use-site-monitoring", ...)` and replaces
// `usePauseMonitoring`/`useResumeMonitoring` with bare `vi.fn()`s — so
// `MonitoringRequestError`, `toMonitoringError`, the 403 -> `principal_required`
// fallback and the `empty_response` guard were all unexecuted code.
//
// These tests exercise the REAL hooks (`usePauseMonitoring` /
// `useResumeMonitoring`, real `useMutation`, real `QueryClient`) against a
// faked WIRE boundary — `pauseSiteMonitoring` / `resumeSiteMonitoring` from
// `@wpmgr/api` — never the hook itself. An adversary proved two mutant
// mutationFns were invisible to the suite:
//   1. Throwing unconditionally ahead of the `if (error)` check.
//   2. `if (error) return { results: [] } as unknown as MonitoringBulkResult`
//      — a server error resolving as success, `onSuccess` firing, and the
//      sites tree invalidated for a change that never happened.
// The "never resolves as success" and "never invalidates on error" tests
// below are aimed squarely at variant 2.

const { pauseMock, resumeMock } = vi.hoisted(() => ({
  pauseMock: vi.fn(),
  resumeMock: vi.fn(),
}));

vi.mock("@wpmgr/api", () => ({
  client: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), delete: vi.fn() },
  listSites: vi.fn(),
  getSite: vi.fn(),
  deleteSite: vi.fn(),
  createPairingCode: vi.fn(),
  setSiteTags: vi.fn(),
  refreshSiteScreenshot: vi.fn(),
  pauseSiteMonitoring: pauseMock,
  resumeSiteMonitoring: resumeMock,
}));

vi.mock("@/components/toast", () => ({
  toast: { success: vi.fn(), error: vi.fn(), info: vi.fn(), warning: vi.fn() },
}));

import { usePauseMonitoring, useResumeMonitoring, MonitoringRequestError } from "./use-site-monitoring";
import { sitesKeys } from "./use-sites";

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

function okResult(overrides: Partial<MonitoringBulkResult> = {}): MonitoringBulkResult {
  return {
    results: [{ site_id: "site-1", ok: true, changed: true, detail: "" }],
    changed_count: 1,
    ...overrides,
  } as unknown as MonitoringBulkResult;
}

beforeEach(() => {
  pauseMock.mockReset();
  resumeMock.mockReset();
});

describe("usePauseMonitoring — real hook against a faked transport (GH #414)", () => {
  it("sends site_ids/reason/resume_at, resolves with the server body, and invalidates the sites tree", async () => {
    pauseMock.mockResolvedValue({ data: okResult(), error: undefined, response: { status: 200 } });
    const qc = makeQueryClient();
    const invalidateSpy = vi.spyOn(qc, "invalidateQueries");
    const { result } = renderHook(() => usePauseMonitoring(), { wrapper: wrapperFor(qc) });

    let resolved: MonitoringBulkResult | undefined;
    await act(async () => {
      resolved = await result.current.mutateAsync({
        siteIds: ["site-1", "site-2"],
        reason: "maintenance",
        resumeAt: "2026-09-01T00:00:00Z",
      });
    });

    expect(pauseMock).toHaveBeenCalledTimes(1);
    expect(pauseMock.mock.calls[0]?.[0]).toMatchObject({
      body: {
        site_ids: ["site-1", "site-2"],
        reason: "maintenance",
        resume_at: "2026-09-01T00:00:00Z",
      },
    });
    expect(resolved?.changed_count).toBe(1);
    expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: sitesKeys.all });
  });

  it("omits reason/resume_at from the body when they are not supplied", async () => {
    pauseMock.mockResolvedValue({
      data: okResult({ results: [], changed_count: 0 }),
      error: undefined,
      response: { status: 200 },
    });
    const { result } = renderHook(() => usePauseMonitoring(), {
      wrapper: wrapperFor(makeQueryClient()),
    });

    await act(async () => {
      await result.current.mutateAsync({ siteIds: ["site-1"] });
    });

    const body = (pauseMock.mock.calls[0]?.[0] as { body: Record<string, unknown> }).body;
    expect(body).toEqual({ site_ids: ["site-1"] });
  });

  it("a 403 with no `code` on the wire rejects via the principal_required fallback, and never invalidates", async () => {
    // Bare 403, no machine code in the body — exactly the shape
    // `toMonitoringError`'s `status === 403 ? "principal_required" : ...`
    // fallback exists for.
    pauseMock.mockResolvedValue({
      data: undefined,
      error: { message: "Forbidden" },
      response: { status: 403 },
    });
    const qc = makeQueryClient();
    const invalidateSpy = vi.spyOn(qc, "invalidateQueries");
    const { result } = renderHook(() => usePauseMonitoring(), { wrapper: wrapperFor(qc) });

    await expect(result.current.mutateAsync({ siteIds: ["site-1"] })).rejects.toMatchObject({
      name: "MonitoringRequestError",
      code: "principal_required",
      status: 403,
    });
    expect(invalidateSpy).not.toHaveBeenCalled();
  });

  it("a 422 with a known server code rejects with that code and its sentence, and never invalidates", async () => {
    pauseMock.mockResolvedValue({
      data: undefined,
      error: { code: "request_too_large", message: "too big" },
      response: { status: 422 },
    });
    const qc = makeQueryClient();
    const invalidateSpy = vi.spyOn(qc, "invalidateQueries");
    const { result } = renderHook(() => usePauseMonitoring(), { wrapper: wrapperFor(qc) });

    let caught: MonitoringRequestError | undefined;
    try {
      await result.current.mutateAsync({ siteIds: ["site-1"] });
    } catch (err) {
      caught = err as MonitoringRequestError;
    }
    expect(caught).toBeInstanceOf(MonitoringRequestError);
    expect(caught?.code).toBe("request_too_large");
    expect(caught?.status).toBe(422);
    expect(caught?.message).toMatch(/too large/i);
    expect(invalidateSpy).not.toHaveBeenCalled();
  });

  it("an error response can NEVER resolve as success, whatever body rides alongside it (GH #414 adversarial finding)", async () => {
    // The exact shape of the adversary's second mutant: `error` present AND a
    // `results: [], changed_count: 0` body sitting right next to it that
    // would satisfy `MonitoringBulkResult`'s shape if returned instead of
    // thrown.
    pauseMock.mockResolvedValue({
      data: okResult({ results: [], changed_count: 0 }),
      error: { code: "principal_required", message: "Forbidden" },
      response: { status: 403 },
    });
    const qc = makeQueryClient();
    const invalidateSpy = vi.spyOn(qc, "invalidateQueries");
    const { result } = renderHook(() => usePauseMonitoring(), { wrapper: wrapperFor(qc) });

    await expect(result.current.mutateAsync({ siteIds: ["site-1"] })).rejects.toBeInstanceOf(
      MonitoringRequestError,
    );
    // THE finding: onSuccess must not fire, and the sites tree must not be
    // invalidated, for a request the server refused.
    expect(invalidateSpy).not.toHaveBeenCalled();
  });

  it("an empty response (no error, no data) rejects via the empty_response guard", async () => {
    pauseMock.mockResolvedValue({ data: undefined, error: undefined, response: { status: 200 } });
    const qc = makeQueryClient();
    const invalidateSpy = vi.spyOn(qc, "invalidateQueries");
    const { result } = renderHook(() => usePauseMonitoring(), { wrapper: wrapperFor(qc) });

    await expect(result.current.mutateAsync({ siteIds: ["site-1"] })).rejects.toMatchObject({
      name: "MonitoringRequestError",
      code: "empty_response",
    });
    expect(invalidateSpy).not.toHaveBeenCalled();
  });
});

describe("useResumeMonitoring — real hook against a faked transport (GH #414)", () => {
  it("sends only site_ids, resolves with the server body, and invalidates the sites tree", async () => {
    resumeMock.mockResolvedValue({ data: okResult(), error: undefined, response: { status: 200 } });
    const qc = makeQueryClient();
    const invalidateSpy = vi.spyOn(qc, "invalidateQueries");
    const { result } = renderHook(() => useResumeMonitoring(), { wrapper: wrapperFor(qc) });

    let resolved: MonitoringBulkResult | undefined;
    await act(async () => {
      resolved = await result.current.mutateAsync({ siteIds: ["site-1", "site-2"] });
    });

    expect(resumeMock).toHaveBeenCalledTimes(1);
    expect(resumeMock.mock.calls[0]?.[0]).toEqual({ body: { site_ids: ["site-1", "site-2"] } });
    expect(resolved?.changed_count).toBe(1);
    expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: sitesKeys.all });
  });

  it("a 403 with no `code` on the wire rejects via the principal_required fallback, and never invalidates", async () => {
    resumeMock.mockResolvedValue({
      data: undefined,
      error: { message: "Forbidden" },
      response: { status: 403 },
    });
    const qc = makeQueryClient();
    const invalidateSpy = vi.spyOn(qc, "invalidateQueries");
    const { result } = renderHook(() => useResumeMonitoring(), { wrapper: wrapperFor(qc) });

    await expect(result.current.mutateAsync({ siteIds: ["site-1"] })).rejects.toMatchObject({
      name: "MonitoringRequestError",
      code: "principal_required",
      status: 403,
    });
    expect(invalidateSpy).not.toHaveBeenCalled();
  });

  it("a 500 with no parsed error body rejects via the empty_response guard, and never invalidates", async () => {
    resumeMock.mockResolvedValue({
      data: undefined,
      error: undefined,
      response: { status: 500 },
    });
    const qc = makeQueryClient();
    const invalidateSpy = vi.spyOn(qc, "invalidateQueries");
    const { result } = renderHook(() => useResumeMonitoring(), { wrapper: wrapperFor(qc) });

    // No `error` body at all here (network/proxy 500) — `error` is falsy so
    // the mutationFn falls through to the `empty_response` guard, which is
    // itself an intentional case: a 500 with no parsed error body must not
    // read as a silent success either.
    await expect(result.current.mutateAsync({ siteIds: ["site-1"] })).rejects.toMatchObject({
      name: "MonitoringRequestError",
      code: "empty_response",
    });
    expect(invalidateSpy).not.toHaveBeenCalled();
  });

  it("an error response can NEVER resolve as success, whatever body rides alongside it (GH #414 adversarial finding, resume path)", async () => {
    resumeMock.mockResolvedValue({
      data: okResult({ results: [], changed_count: 0 }),
      error: { code: "principal_required", message: "Forbidden" },
      response: { status: 403 },
    });
    const qc = makeQueryClient();
    const invalidateSpy = vi.spyOn(qc, "invalidateQueries");
    const { result } = renderHook(() => useResumeMonitoring(), { wrapper: wrapperFor(qc) });

    await expect(result.current.mutateAsync({ siteIds: ["site-1"] })).rejects.toBeInstanceOf(
      MonitoringRequestError,
    );
    expect(invalidateSpy).not.toHaveBeenCalled();
  });
});
