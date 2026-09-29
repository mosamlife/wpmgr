import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createElement, type ReactNode } from "react";
import type { AssistantRequest, AssistantRequestList } from "@wpmgr/api";

// The REAL hooks (useQuery/useMutation) against a FAKED wire boundary — the
// four `@wpmgr/api` operation functions this feature calls — never the hooks
// themselves. Same pattern as features/sites/use-site-monitoring.test.ts:
// this is what makes a mutant mutationFn/queryFn (swallow the error, resolve
// as success on failure, send the wrong body) visible to a suite instead of
// only to a manual read.

const { listMock, listSiteMock, approveMock, declineMock } = vi.hoisted(() => ({
  listMock: vi.fn(),
  listSiteMock: vi.fn(),
  approveMock: vi.fn(),
  declineMock: vi.fn(),
}));

vi.mock("@wpmgr/api", () => ({
  client: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), delete: vi.fn() },
  listAssistantRequests: listMock,
  listSiteAssistantRequests: listSiteMock,
  approveAssistantRequest: approveMock,
  declineAssistantRequest: declineMock,
}));

import {
  useAssistantRequests,
  useSiteAssistantRequests,
  useApproveAssistantRequest,
  useDeclineAssistantRequest,
  assistantRequestKeys,
  AssistantRequestError,
} from "./use-ai-requests";

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

function list(overrides: Partial<AssistantRequestList> = {}): AssistantRequestList {
  return { requests: [], pending_count: 0, limit: 50, offset: 0, ...overrides };
}

function request(overrides: Partial<AssistantRequest> = {}): AssistantRequest {
  return {
    id: "req-1",
    site_id: "site-1",
    scope: "all",
    url: null,
    site_label: "Shop",
    site_host: "shop.test",
    grant_label: "Priya's laptop",
    grant_via: "oauth",
    setup_client: "Claude Code",
    presented_digest: "digest-abc",
    state: "approved_undispatched",
    created_at: "2026-09-29T09:41:00Z",
    expires_at: "2026-09-30T09:41:00Z",
    decided_at: "2026-09-29T09:43:00Z",
    decided_by_user_id: "user-1",
    decided_by_name: "Priya",
    decided_by_account_deleted: false,
    withdrawn_at: null,
    claimed_at: null,
    dispatch_attempts: 0,
    last_attempt_at: null,
    last_attempt_code: null,
    outcome: null,
    not_sent_reason: null,
    outcome_at: null,
    hosting_caches_cleared: null,
    hosting_caches_skipped: null,
    origin_only_confirmed: null,
    wpmgr_cdn: null,
    site_reported_text: null,
    ...overrides,
  };
}

beforeEach(() => {
  listMock.mockReset();
  listSiteMock.mockReset();
  approveMock.mockReset();
  declineMock.mockReset();
});

describe("useAssistantRequests — real hook against a faked transport", () => {
  it("resolves with the server body on success", async () => {
    listMock.mockResolvedValue({ data: list({ pending_count: 2 }), error: undefined, response: { status: 200 } });
    const qc = makeQueryClient();
    const { result } = renderHook(() => useAssistantRequests(), { wrapper: wrapperFor(qc) });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data?.pending_count).toBe(2);
  });

  it("never resolves as success when the server answers an error — the mutant this guards against returns {requests:[]} on error", async () => {
    listMock.mockResolvedValue({
      data: undefined,
      error: { code: "forbidden", message: "Your role cannot view AI requests." },
      response: { status: 403 },
    });
    const qc = makeQueryClient();
    const { result } = renderHook(() => useAssistantRequests(), { wrapper: wrapperFor(qc) });
    await waitFor(() => expect(result.current.isError).toBe(true));
    expect(result.current.data).toBeUndefined();
    expect(result.current.error).toBeInstanceOf(AssistantRequestError);
    expect((result.current.error as AssistantRequestError).code).toBe("forbidden");
    expect(result.current.error?.message).toBe("Your role cannot view AI requests.");
  });

  it("treats an empty 200 body as a failure, not a silent empty queue", async () => {
    listMock.mockResolvedValue({ data: undefined, error: undefined, response: { status: 200 } });
    const qc = makeQueryClient();
    const { result } = renderHook(() => useAssistantRequests(), { wrapper: wrapperFor(qc) });
    await waitFor(() => expect(result.current.isError).toBe(true));
    expect((result.current.error as AssistantRequestError).code).toBe("empty_response");
  });
});

describe("useSiteAssistantRequests", () => {
  it("stays disabled and fires nothing without a siteId", () => {
    const qc = makeQueryClient();
    renderHook(() => useSiteAssistantRequests(undefined), { wrapper: wrapperFor(qc) });
    expect(listSiteMock).not.toHaveBeenCalled();
  });

  it("passes the siteId through as a path param", async () => {
    listSiteMock.mockResolvedValue({ data: list(), error: undefined, response: { status: 200 } });
    const qc = makeQueryClient();
    const { result } = renderHook(() => useSiteAssistantRequests("site-9"), {
      wrapper: wrapperFor(qc),
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(listSiteMock).toHaveBeenCalledWith(
      expect.objectContaining({ path: { siteId: "site-9" } }),
    );
  });
});

describe("useApproveAssistantRequest", () => {
  it("sends presented_digest to the site-nested route and invalidates both keys on success", async () => {
    approveMock.mockResolvedValue({
      data: request({ state: "approved_undispatched" }),
      error: undefined,
      response: { status: 200 },
    });
    const qc = makeQueryClient();
    const invalidate = vi.spyOn(qc, "invalidateQueries");
    const { result } = renderHook(() => useApproveAssistantRequest(), { wrapper: wrapperFor(qc) });

    result.current.mutate({ siteId: "site-1", requestId: "req-1", presentedDigest: "the-digest" });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));

    expect(approveMock).toHaveBeenCalledWith({
      path: { siteId: "site-1", requestId: "req-1" },
      body: { presented_digest: "the-digest" },
    });
    expect(invalidate).toHaveBeenCalledWith(
      expect.objectContaining({ queryKey: assistantRequestKeys.all }),
    );
    expect(invalidate).toHaveBeenCalledWith(
      expect.objectContaining({ queryKey: assistantRequestKeys.site("site-1") }),
    );
  });

  it("never resolves as success on a 409 — a digest mismatch or a decided row stays an error", async () => {
    approveMock.mockResolvedValue({
      data: undefined,
      error: {
        code: "assistant_request_changed",
        message: "This request changed or was decided while you were reading it. Nothing ran.",
      },
      response: { status: 409 },
    });
    const qc = makeQueryClient();
    const { result } = renderHook(() => useApproveAssistantRequest(), { wrapper: wrapperFor(qc) });

    result.current.mutate({ siteId: "site-1", requestId: "req-1", presentedDigest: "stale" });
    await waitFor(() => expect(result.current.isError).toBe(true));
    expect(result.current.isSuccess).toBe(false);
    expect((result.current.error as AssistantRequestError).code).toBe("assistant_request_changed");
  });
});

describe("useDeclineAssistantRequest", () => {
  it("posts an empty JSON body to the site-nested decline route", async () => {
    declineMock.mockResolvedValue({
      data: request({ state: "rejected" }),
      error: undefined,
      response: { status: 200 },
    });
    const qc = makeQueryClient();
    const { result } = renderHook(() => useDeclineAssistantRequest(), { wrapper: wrapperFor(qc) });

    result.current.mutate({ siteId: "site-1", requestId: "req-1" });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));

    expect(declineMock).toHaveBeenCalledWith({
      path: { siteId: "site-1", requestId: "req-1" },
      body: {},
    });
  });
});
