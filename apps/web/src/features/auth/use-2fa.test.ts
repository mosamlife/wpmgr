import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createElement, type ReactNode } from "react";

// GH #215: the TOTP login-challenge (`POST /auth/2fa/totp`, `useTotpChallenge`)
// only branched on 410 (challenge_expired) and 401 (invalid_code /
// too_many_attempts). When a self-host's secrets-at-rest key changes, the
// server can no longer decrypt the operator's stored TOTP secret and returns
// HTTP 500 with the distinct domain code `totp_decrypt_failed` (still present
// in the JSON body per `httpx.Error` -- only the *message* is suppressed for
// KindInternal errors, the code is always surfaced). Before this fix, that
// 500 fell through to the generic `toError(result.error)` path and the route
// rendered "Verification failed. Please try again.", which reads as "wrong
// code" instead of a server configuration problem. These tests pin: the 500
// branch now surfaces a distinct `code`, and the existing 401/410 branches
// are unchanged (no regression).

const { postMock } = vi.hoisted(() => ({ postMock: vi.fn() }));

vi.mock("@wpmgr/api", () => ({
  client: { get: vi.fn(), post: postMock, patch: vi.fn(), delete: vi.fn() },
  getMe: vi.fn(),
  login: vi.fn(),
  logout: vi.fn(),
  register: vi.fn(),
}));

vi.mock("@/components/toast", () => ({
  toast: { success: vi.fn(), error: vi.fn(), warning: vi.fn(), info: vi.fn() },
}));

import { useTotpChallenge } from "./use-2fa";

function wrapper({ children }: { children: ReactNode }) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return createElement(QueryClientProvider, { client: qc }, children);
}

/** Reads the `.code` the mutation hook attaches via `Object.assign(new Error(...), { code })`. */
function errorCode(err: unknown): string | undefined {
  return (err as { code?: string } | null)?.code;
}

describe("useTotpChallenge error mapping — GH #215", () => {
  beforeEach(() => {
    postMock.mockReset();
  });

  it("surfaces a distinct `totp_decrypt_failed` code on a 500 with that domain code", async () => {
    postMock.mockResolvedValue({
      data: undefined,
      error: { code: "totp_decrypt_failed", message: "internal server error" },
      response: { status: 500 },
    });

    const { result } = renderHook(() => useTotpChallenge(), { wrapper });
    result.current.mutate({ challenge: "c1", code: "123456", remember_device: false });

    await waitFor(() => expect(result.current.isError).toBe(true));
    expect(errorCode(result.current.error)).toBe("totp_decrypt_failed");
    // Non-vacuous: against the pre-fix code this fell through to the generic
    // `toError()` path, which does NOT attach a `.code` at all.
    expect(errorCode(result.current.error)).not.toBeUndefined();
  });

  it("still surfaces `invalid_code` on a plain 401 (no regression)", async () => {
    postMock.mockResolvedValue({
      data: undefined,
      error: { code: "invalid_totp_code", message: "invalid TOTP code" },
      response: { status: 401 },
    });

    const { result } = renderHook(() => useTotpChallenge(), { wrapper });
    result.current.mutate({ challenge: "c1", code: "000000", remember_device: false });

    await waitFor(() => expect(result.current.isError).toBe(true));
    expect(errorCode(result.current.error)).toBe("invalid_code");
  });

  it("still surfaces `too_many_attempts` on a 401 with that domain code (no regression)", async () => {
    postMock.mockResolvedValue({
      data: undefined,
      error: { code: "too_many_attempts", message: "too many attempts" },
      response: { status: 401 },
    });

    const { result } = renderHook(() => useTotpChallenge(), { wrapper });
    result.current.mutate({ challenge: "c1", code: "000000", remember_device: false });

    await waitFor(() => expect(result.current.isError).toBe(true));
    expect(errorCode(result.current.error)).toBe("too_many_attempts");
  });

  it("still surfaces `challenge_expired` on a 410 (no regression)", async () => {
    postMock.mockResolvedValue({
      data: undefined,
      error: { code: "challenge_expired", message: "challenge expired" },
      response: { status: 410 },
    });

    const { result } = renderHook(() => useTotpChallenge(), { wrapper });
    result.current.mutate({ challenge: "c1", code: "000000", remember_device: false });

    await waitFor(() => expect(result.current.isError).toBe(true));
    expect(errorCode(result.current.error)).toBe("challenge_expired");
  });
});
