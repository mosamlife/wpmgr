import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createElement, type ReactNode } from "react";
import type { ListSitesData } from "@wpmgr/api";

// GH #349. What `useSites` actually puts on the wire.
//
// The route-level tests assert the page's behaviour; this file asserts the
// REQUEST, which is where the bug lived. `useSites()` used to call
// `listSites({})`: no limit, so the control plane's default 50, ordered by
// created_at DESC, with the free-text search applied afterwards in the
// browser. An agency past 50 sites searched their newest 50 and was told
// nothing else matched.
//
// Every test in this file fails against the pre-change hook.

const { listSitesMock } = vi.hoisted(() => ({ listSitesMock: vi.fn() }));

vi.mock("@wpmgr/api", () => ({
  client: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), delete: vi.fn() },
  listSites: listSitesMock,
  getSite: vi.fn(),
  deleteSite: vi.fn(),
  createPairingCode: vi.fn(),
  setSiteTags: vi.fn(),
  refreshSiteScreenshot: vi.fn(),
}));

vi.mock("@/components/toast", () => ({
  toast: { warning: vi.fn(), success: vi.fn(), error: vi.fn(), info: vi.fn() },
}));

import {
  useSites,
  sitesQueryOptions,
  sitesKeys,
  DEFAULT_SITES_LIMIT,
  type UseSitesOptions,
} from "./use-sites";

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

/** The query object of the most recent listSites call. */
async function sentQuery(): Promise<NonNullable<ListSitesData["query"]>> {
  await waitFor(() => {
    expect(listSitesMock).toHaveBeenCalled();
  });
  const call = listSitesMock.mock.calls.at(-1) as
    | [{ query?: NonNullable<ListSitesData["query"]> }]
    | undefined;
  return call?.[0]?.query ?? {};
}

async function renderUseSites(options?: UseSitesOptions) {
  renderHook(() => useSites(options), { wrapper: wrapperFor(makeQueryClient()) });
  return sentQuery();
}

beforeEach(() => {
  listSitesMock.mockReset();
  listSitesMock.mockResolvedValue({
    data: { items: [] },
    error: undefined,
    response: { status: 200 },
  });
});

describe("useSites request (GH #349)", () => {
  it("sends the free-text term as `q` instead of filtering the response", async () => {
    expect(await renderUseSites({ q: "iacop" })).toMatchObject({ q: "iacop" });
  });

  it("trims the term, and treats a whitespace-only one as no search at all", async () => {
    expect(await renderUseSites({ q: "  iacop  " })).toMatchObject({
      q: "iacop",
    });
    listSitesMock.mockClear();
    expect(await renderUseSites({ q: "   " })).not.toHaveProperty("q");
  });

  it("sends the chosen order as `sort`, and sends nothing when the default is wanted", async () => {
    expect(await renderUseSites({ sort: "-last_seen" })).toMatchObject({
      sort: "-last_seen",
    });
    listSitesMock.mockClear();
    // Absent means the server's own default (-created_at). Sending nothing is
    // how the list behaved before the axis existed, so an untouched control
    // changes no request.
    expect(await renderUseSites()).not.toHaveProperty("sort");
  });

  it("asks for the contract maximum rather than taking the server's default page of 50", async () => {
    // THE CAP. A list the operator is told is searched and ordered across
    // their organisation must not quietly stop at 50.
    expect(await renderUseSites()).toMatchObject({ limit: 200 });
    expect(DEFAULT_SITES_LIMIT).toBe(200);
  });

  it("lets a caller ask for a smaller page (the command palette does)", async () => {
    expect(await renderUseSites({ q: "iacop", limit: 20 })).toMatchObject({
      q: "iacop",
      limit: 20,
    });
  });

  it("composes search and order with the filters that were already server-side", async () => {
    expect(
      await renderUseSites({
        view: "archived",
        clientId: "c-1",
        tags: ["prod", "eu"],
        tagsMatch: "all",
        q: "iacop",
        sort: "name",
      }),
    ).toMatchObject({
      state: "archived",
      clientId: "c-1",
      tags: ["prod", "eu"],
      tags_match: "all",
      q: "iacop",
      sort: "name",
      limit: 200,
    });
  });

  it("does not fire at all when disabled, so an empty palette box searches for nothing", async () => {
    renderHook(() => useSites({ q: "", enabled: false }), {
      wrapper: wrapperFor(makeQueryClient()),
    });
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(listSitesMock).not.toHaveBeenCalled();
  });
});

describe("useSites cache identity (GH #349)", () => {
  it("gives each term and each order its own cache entry", () => {
    const base = JSON.stringify(sitesKeys.list("active"));
    expect(JSON.stringify(sitesKeys.list("active", undefined, undefined, "any", { q: "a" })))
      .not.toBe(base);
    expect(
      JSON.stringify(
        sitesKeys.list("active", undefined, undefined, "any", { sort: "name" }),
      ),
    ).not.toBe(base);
  });

  it("keeps the route loader's prefetch on the SAME key a default useSites() reads", async () => {
    // The Sites route prefetches `sitesQueryOptions()` and the page then calls
    // `useSites({ view: "active", ... })` with no term and no order. If those
    // two keys drifted apart, the prefetch would be dead weight and the page
    // would fire a second identical request on every cold load.
    expect(JSON.stringify(sitesQueryOptions().queryKey)).toBe(
      JSON.stringify(sitesKeys.list("active")),
    );

    // ...and the prefetch must ask for the same page size, or the two would
    // return different rows.
    const qc = makeQueryClient();
    await qc.prefetchQuery(sitesQueryOptions());
    expect(await sentQuery()).toMatchObject({ limit: 200 });
  });
});
