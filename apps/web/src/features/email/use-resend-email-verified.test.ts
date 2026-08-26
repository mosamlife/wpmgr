import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createElement, type ReactNode } from "react";
import type { ResendEmailResult, BulkResendResponse } from "@wpmgr/api";

// GH #528 (web half): the control plane now reports `verified` on both the
// single resend (ResendEmailResult, packages/openapi/openapi.yaml) and the
// per-item bulk resend result (BulkResendItemResult). `verified: false` means
// wpmgr sent the email but could not confirm the site's copy was still the
// exact message the operator picked (most commonly: the original send had
// failed, so there was no delivery id on record to compare against). That is
// NOT an error — the send happened — so it must render as a calm caveat
// (toast.warning), never toast.error/destructive, and it must never be
// dropped on the floor the way it was before this fix.
//
// resendEmailLog / bulkResendEmailLog are mocked at the @wpmgr/api boundary,
// the same pattern as use-cancel-update-run.test.ts, so this test exercises
// the real onSuccess branching in use-email.ts against the real response
// shapes rather than against fetch.

const {
  resendEmailLogMock,
  bulkResendEmailLogMock,
  toastSuccess,
  toastWarning,
  toastError,
} = vi.hoisted(() => ({
  resendEmailLogMock: vi.fn(),
  bulkResendEmailLogMock: vi.fn(),
  toastSuccess: vi.fn(),
  toastWarning: vi.fn(),
  toastError: vi.fn(),
}));

vi.mock("@wpmgr/api", () => ({
  resendEmailLog: resendEmailLogMock,
  bulkResendEmailLog: bulkResendEmailLogMock,
}));

vi.mock("@/components/toast", () => ({
  toast: {
    success: toastSuccess,
    warning: toastWarning,
    error: toastError,
    info: vi.fn(),
  },
}));

import { useResendEmail, useBulkResendEmail } from "./use-email";

const SITE_ID = "11111111-1111-1111-1111-111111111111";
const LOG_ID = "22222222-2222-2222-2222-222222222222";

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
  resendEmailLogMock.mockReset();
  bulkResendEmailLogMock.mockReset();
  toastSuccess.mockReset();
  toastWarning.mockReset();
  toastError.mockReset();
});

describe("useResendEmail — verified caveat (GH #528)", () => {
  it("ok + verified:false surfaces the unconfirmed caveat via toast.warning, and does NOT toast.success", async () => {
    const result: ResendEmailResult = { ok: true, verified: false, detail: null };
    resendEmailLogMock.mockResolvedValue({
      data: result,
      error: undefined,
      response: { status: 200 },
    });
    const { result: hook } = renderHook(() => useResendEmail(SITE_ID), {
      wrapper: wrapperFor(makeQueryClient()),
    });

    await hook.current.mutateAsync(LOG_ID);

    await waitFor(() => expect(toastWarning).toHaveBeenCalledTimes(1));
    const [title, opts] = toastWarning.mock.calls[0] as [
      string,
      { description?: string },
    ];
    expect(title).toBe("Email resent — unconfirmed");
    // Say what happened, plainly, and never a bare code.
    expect(opts.description).toMatch(/could not confirm/i);
    expect(opts.description).not.toMatch(/message_id_mismatch/i);
    expect(toastSuccess).not.toHaveBeenCalled();
    expect(toastError).not.toHaveBeenCalled();
  });

  it("ok + verified:true (unchanged path) toasts the plain success and never shows the caveat", async () => {
    const result: ResendEmailResult = { ok: true, verified: true, detail: null };
    resendEmailLogMock.mockResolvedValue({
      data: result,
      error: undefined,
      response: { status: 200 },
    });
    const { result: hook } = renderHook(() => useResendEmail(SITE_ID), {
      wrapper: wrapperFor(makeQueryClient()),
    });

    await hook.current.mutateAsync(LOG_ID);

    await waitFor(() => expect(toastSuccess).toHaveBeenCalledTimes(1));
    expect(toastSuccess).toHaveBeenCalledWith("Email queued for resend");
    expect(toastWarning).not.toHaveBeenCalled();
    expect(toastError).not.toHaveBeenCalled();
  });

  it("ok + verified omitted (older/edge response) also takes the unchanged success path, not the caveat", async () => {
    const result: ResendEmailResult = { ok: true, detail: null };
    resendEmailLogMock.mockResolvedValue({
      data: result,
      error: undefined,
      response: { status: 200 },
    });
    const { result: hook } = renderHook(() => useResendEmail(SITE_ID), {
      wrapper: wrapperFor(makeQueryClient()),
    });

    await hook.current.mutateAsync(LOG_ID);

    await waitFor(() => expect(toastSuccess).toHaveBeenCalledTimes(1));
    expect(toastWarning).not.toHaveBeenCalled();
  });
});

describe("useBulkResendEmail — mixed-verification batch (GH #528)", () => {
  it("all resends verified: unchanged plain success count, no caveat", async () => {
    const response: BulkResendResponse = {
      results: [
        { log_id: "a", ok: true, verified: true },
        { log_id: "b", ok: true, verified: true },
      ],
    };
    bulkResendEmailLogMock.mockResolvedValue({
      data: response,
      error: undefined,
      response: { status: 200 },
    });
    const { result: hook } = renderHook(() => useBulkResendEmail(SITE_ID), {
      wrapper: wrapperFor(makeQueryClient()),
    });

    await hook.current.mutateAsync(["a", "b"]);

    await waitFor(() => expect(toastSuccess).toHaveBeenCalledTimes(1));
    expect(toastSuccess).toHaveBeenCalledWith("2 emails queued for resend");
    expect(toastWarning).not.toHaveBeenCalled();
    expect(toastError).not.toHaveBeenCalled();
  });

  it("a mixed batch (0 failed, 1 unverified) does NOT read as uniformly fine: says how many, via toast.warning not toast.success", async () => {
    const response: BulkResendResponse = {
      results: [
        { log_id: "a", ok: true, verified: true },
        { log_id: "b", ok: true, verified: false },
      ],
    };
    bulkResendEmailLogMock.mockResolvedValue({
      data: response,
      error: undefined,
      response: { status: 200 },
    });
    const { result: hook } = renderHook(() => useBulkResendEmail(SITE_ID), {
      wrapper: wrapperFor(makeQueryClient()),
    });

    await hook.current.mutateAsync(["a", "b"]);

    await waitFor(() => expect(toastWarning).toHaveBeenCalledTimes(1));
    const [title, opts] = toastWarning.mock.calls[0] as [
      string,
      { description?: string },
    ];
    expect(title).toBe("2 emails queued for resend, 1 unconfirmed");
    expect(opts.description).toMatch(/could not confirm/i);
    expect(toastSuccess).not.toHaveBeenCalled();
    expect(toastError).not.toHaveBeenCalled();
  });

  it("a batch with real failures still errors, and folds the unverified count into the description rather than dropping it", async () => {
    const response: BulkResendResponse = {
      results: [
        { log_id: "a", ok: true, verified: false },
        { log_id: "b", ok: false, detail: "site not enrolled" },
      ],
    };
    bulkResendEmailLogMock.mockResolvedValue({
      data: response,
      error: undefined,
      response: { status: 200 },
    });
    const { result: hook } = renderHook(() => useBulkResendEmail(SITE_ID), {
      wrapper: wrapperFor(makeQueryClient()),
    });

    await hook.current.mutateAsync(["a", "b"]);

    await waitFor(() => expect(toastError).toHaveBeenCalledTimes(1));
    const [, opts] = toastError.mock.calls[0] as [string, { description?: string }];
    expect(opts.description).toContain("1 of the successes could not be confirmed");
    expect(toastWarning).not.toHaveBeenCalled();
    expect(toastSuccess).not.toHaveBeenCalled();
  });
});
