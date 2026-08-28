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

const { listOrgContextVersionsMock, patchOrgContextMock } = vi.hoisted(() => ({
  listOrgContextVersionsMock: vi.fn(),
  patchOrgContextMock: vi.fn(),
}));

vi.mock("@wpmgr/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wpmgr/api")>();
  return {
    ...actual,
    listOrgContextVersions: listOrgContextVersionsMock,
    patchOrgContext: patchOrgContextMock,
  };
});

const { useOrgContextVersions, usePatchOrgContext } = await import("./use-context");

function wrapperFor(qc: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return createElement(QueryClientProvider, { client: qc }, children);
  };
}

beforeEach(() => {
  listOrgContextVersionsMock.mockReset();
  patchOrgContextMock.mockReset();
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

describe("usePatchOrgContext — a successful PATCH invalidates the version-history list", () => {
  // CodeRabbit finding on #566: a PATCH authors a new stored version row
  // (Decision 5) exactly like restore does, but only restore invalidated
  // contextKeys.orgVersions — the history list, mounted alongside the
  // editor, kept showing pre-save pages. This mounts BOTH hooks against one
  // shared QueryClient (the real coupling the bug lived in) and proves the
  // list actually refetches, not just that invalidateQueries was called —
  // an invalidation call that targeted the wrong key would still "pass" a
  // spy-based assertion while leaving the rendered list stale.
  it("makes the just-authored version show up in the still-mounted history list", async () => {
    listOrgContextVersionsMock
      .mockResolvedValueOnce({
        data: { items: [{ id: "v-1", version: 1, author_type: "user", provenance: "manual", created_at: "2026-08-01T00:00:00Z" }], next_cursor: 0 },
        error: undefined,
      })
      .mockResolvedValueOnce({
        data: {
          items: [
            { id: "v-2", version: 2, author_type: "user", provenance: "manual", created_at: "2026-08-02T00:00:00Z" },
            { id: "v-1", version: 1, author_type: "user", provenance: "manual", created_at: "2026-08-01T00:00:00Z" },
          ],
          next_cursor: 0,
        },
        error: undefined,
      });
    patchOrgContextMock.mockResolvedValue({
      data: { version: 2, restrictions: {}, guidance: {} },
      error: undefined,
    });

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const versions = renderHook(() => useOrgContextVersions("org-1"), { wrapper: wrapperFor(qc) });
    const patch = renderHook(() => usePatchOrgContext("org-1"), { wrapper: wrapperFor(qc) });

    await waitFor(() => expect(versions.result.current.items).toHaveLength(1));

    await patch.result.current.mutateAsync({ base_version: 1, restrictions: {}, guidance: {} });

    await waitFor(() => expect(versions.result.current.items).toHaveLength(2));
    expect(versions.result.current.items[0]?.version).toBe(2);
  });
});
