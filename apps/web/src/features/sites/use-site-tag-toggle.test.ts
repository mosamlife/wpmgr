import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, act } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createElement, type ReactNode } from "react";
import type { Site } from "@wpmgr/api";

// GH #230 "rich tags" — adversarial-verify HIGH: the single-site tag-picker
// computed its replace-set toggle from the `site` PROP (the sites LIST cache
// at render time). `useSetSiteTags` only optimistically patched the DETAIL
// cache, never the list caches, so two rapid toggles both read the same
// stale array before either PUT resolved — the second PUT silently dropped
// the first toggle (last-write-wins on a replace-set POST).
//
// This test proves the fix: toggle A then immediately toggle B, BEFORE A's
// PUT resolves. The final (second) PUT payload must carry BOTH tags — never
// just B (which would mean A got dropped).

const { setSiteTagsMock } = vi.hoisted(() => ({ setSiteTagsMock: vi.fn() }));

vi.mock("@wpmgr/api", () => ({
  client: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), delete: vi.fn() },
  listSites: vi.fn(),
  getSite: vi.fn(),
  deleteSite: vi.fn(),
  createPairingCode: vi.fn(),
  setSiteTags: setSiteTagsMock,
  refreshSiteScreenshot: vi.fn(),
}));

vi.mock("@/components/toast", () => ({
  toast: { success: vi.fn(), error: vi.fn(), info: vi.fn(), warning: vi.fn() },
}));

import { useSiteTagToggle } from "./use-site-tag-toggle";
import { getCachedSiteTags, sitesKeys } from "./use-sites";

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
    tags: ["existing"],
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

/** A controllable, deferred mock response for one setSiteTags call. */
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((res) => {
    resolve = res;
  });
  return { promise, resolve };
}

beforeEach(() => {
  setSiteTagsMock.mockReset();
});

describe("useSiteTagToggle — GH #230 lost-update race (adversarial-verify HIGH)", () => {
  it("toggle A then quickly toggle B (before A resolves): the final PUT payload contains BOTH tags", async () => {
    const site = buildSite({ id: "site-1", tags: ["existing"] });
    const qc = makeQueryClient();

    const first = deferred<{ data: Site; error: undefined; response: { status: number } }>();
    const second = deferred<{ data: Site; error: undefined; response: { status: number } }>();
    setSiteTagsMock.mockImplementationOnce(() => first.promise);
    setSiteTagsMock.mockImplementationOnce(() => second.promise);

    const { result } = renderHook(() => useSiteTagToggle(site), { wrapper: wrapperFor(qc) });

    // Toggle A, then IMMEDIATELY toggle B — synchronously, no await between —
    // simulating the fastest possible double-click. Neither PUT has resolved
    // yet at this point.
    act(() => {
      result.current.toggleTag("A");
      result.current.toggleTag("B");
    });

    // Flush the microtask queue so A's queued chain reaches its
    // `mutateAsync` call — this does NOT resolve either PUT (both `first`
    // and `second` deferred promises are still pending), it just lets the
    // promise chain progress as far as it can without a real network tick.
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(setSiteTagsMock).toHaveBeenCalledTimes(1);
    // The FIRST call must be exactly A applied to the original set — the
    // second toggle must NOT have fired yet (it's queued behind the first).
    expect(setSiteTagsMock.mock.calls[0]?.[0]).toMatchObject({
      path: { siteId: "site-1" },
      body: { tags: ["existing", "A"] },
    });

    // Resolve the first PUT now.
    await act(async () => {
      first.resolve({
        data: { ...site, tags: ["existing", "A"] },
        error: undefined,
        response: { status: 200 },
      });
      // Flush the mutateAsync resolution chain + the queued second toggle's
      // "compute from resolved current" step + its own mutateAsync call.
      await Promise.resolve();
      await Promise.resolve();
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(setSiteTagsMock).toHaveBeenCalledTimes(2);
    // THE FIX: the second (final) PUT carries BOTH A and B — proving B's
    // toggle was computed from A's resolved result, not a stale snapshot.
    const secondCall = setSiteTagsMock.mock.calls[1]?.[0] as {
      path: { siteId: string };
      body: { tags: string[] };
    };
    expect(secondCall.path.siteId).toBe("site-1");
    expect([...secondCall.body.tags].sort()).toEqual(["A", "B", "existing"].sort());

    // Let the second PUT resolve too, so the test doesn't leave a dangling
    // unhandled promise warning.
    await act(async () => {
      second.resolve({
        data: { ...site, tags: secondCall.body.tags },
        error: undefined,
        response: { status: 200 },
      });
      await Promise.resolve();
      await Promise.resolve();
    });
  });

  it("getCachedSiteTags reflects the optimistic patch immediately after onMutate (list cache, not just detail)", async () => {
    const site = buildSite({ id: "site-1", tags: ["existing"] });
    const qc = makeQueryClient();
    // Seed a sites LIST cache (as the table/grid would have it) — no detail
    // cache entry exists yet, which is the common case for a row the
    // operator has never opened the detail page for.
    qc.setQueryData<Site[]>(sitesKeys.list("active"), [site]);

    const pending = deferred<{ data: Site; error: undefined; response: { status: number } }>();
    setSiteTagsMock.mockImplementationOnce(() => pending.promise);

    const { result } = renderHook(() => useSiteTagToggle(site), { wrapper: wrapperFor(qc) });

    act(() => {
      result.current.toggleTag("A");
    });
    // Flush onMutate's microtask.
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });

    // The list cache (not the detail cache — none exists) must already show
    // the toggle, and a fresh read must see it too.
    const patchedList = qc.getQueryData<Site[]>(sitesKeys.list("active"));
    expect(patchedList?.[0]?.tags).toEqual(["existing", "A"]);
    expect(getCachedSiteTags(qc, "site-1", site.tags)).toEqual(["existing", "A"]);

    await act(async () => {
      pending.resolve({
        data: { ...site, tags: ["existing", "A"] },
        error: undefined,
        response: { status: 200 },
      });
      await Promise.resolve();
      await Promise.resolve();
    });
  });
});
