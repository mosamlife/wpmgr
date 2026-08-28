import { describe, it, expect, beforeEach, vi } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createElement, type ReactNode } from "react";

// CodeRabbit finding on #566: a missing response body must not render as
// "this history has nothing in it" — the same conflation behind the
// updates-card "Never" bug, the as_of heartbeat substitution, and
// facts_unavailable's omitempty. Mocked at the @wpmgr/api boundary (the
// generated SDK function), matching use-admin-agent-mirror.test.ts's
// convention, so this exercises the real response-shape branch, not a
// hand-built fixture of use-context.ts's own internals.

const { listOrgContextVersionsMock } = vi.hoisted(() => ({
  listOrgContextVersionsMock: vi.fn(),
}));

vi.mock("@wpmgr/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wpmgr/api")>();
  return { ...actual, listOrgContextVersions: listOrgContextVersionsMock };
});

const { useOrgContextVersions } = await import("./use-context");

function wrapperFor(qc: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return createElement(QueryClientProvider, { client: qc }, children);
  };
}

beforeEach(() => {
  listOrgContextVersionsMock.mockReset();
});

describe("useOrgContextVersions — a missing body is a failure, never an empty history", () => {
  it("surfaces an error, not an empty items array, when data and error are both absent", async () => {
    listOrgContextVersionsMock.mockResolvedValue({ data: undefined, error: undefined });
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const { result } = renderHook(() => useOrgContextVersions("org-1"), {
      wrapper: wrapperFor(qc),
    });

    await waitFor(() => expect(result.current.isError).toBe(true));
    // Non-vacuous: the malformed-response version and the genuinely-empty
    // version must not collapse into the same observable state.
    expect(result.current.items).toEqual([]);
    expect(result.current.error?.message).toBe("Empty response");
  });

  it("still returns a genuinely empty items array on a real empty page (must not over-block)", async () => {
    listOrgContextVersionsMock.mockResolvedValue({
      data: { items: [], next_cursor: 0 },
      error: undefined,
    });
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const { result } = renderHook(() => useOrgContextVersions("org-1"), {
      wrapper: wrapperFor(qc),
    });

    await waitFor(() => expect(result.current.isPending).toBe(false));
    expect(result.current.isError).toBe(false);
    expect(result.current.items).toEqual([]);
  });
});
