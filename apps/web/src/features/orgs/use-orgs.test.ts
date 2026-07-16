import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { createElement, type ReactNode } from "react";

// Mock the transport, toast, SSE reset, and router so useActivateOrg's and
// useDeleteOrg's onSuccess can be exercised without a network call, the real
// event stream, or a real TanStack Router instance (GH #186).
//
// `routerState` also backs GH #233's redirect check (`router.state.location`)
// -- it's a plain mutable object (not reassigned) so individual tests can set
// `routerState.location.pathname` before switching orgs, and `routerNavigateMock`
// lets those tests assert whether `router.navigate({ to: "/sites" })` fired.
const {
  deleteMock,
  postMock,
  toastSuccess,
  toastError,
  resetSiteStreamMock,
  routerInvalidateMock,
  routerNavigateMock,
  routerState,
} = vi.hoisted(() => ({
  deleteMock: vi.fn(),
  postMock: vi.fn(),
  toastSuccess: vi.fn(),
  toastError: vi.fn(),
  resetSiteStreamMock: vi.fn(),
  routerInvalidateMock: vi.fn(),
  routerNavigateMock: vi.fn(),
  routerState: { location: { pathname: "/dashboard" } },
}));
vi.mock("@wpmgr/api", () => ({
  client: { delete: deleteMock, get: vi.fn(), post: postMock, patch: vi.fn() },
}));
vi.mock("@/components/toast", () => ({
  toast: { success: toastSuccess, error: toastError },
}));
vi.mock("@/features/sites/use-site-events", () => ({ resetSiteStream: resetSiteStreamMock }));
vi.mock("@/router", () => ({
  router: {
    invalidate: routerInvalidateMock,
    navigate: routerNavigateMock,
    state: routerState,
  },
}));

import {
  mapDeleteOrgError,
  orgDeleteConfirmMatches,
  useActivateOrg,
  useDeleteOrg,
} from "./use-orgs";
import { sitesKeys, NotFoundError as SiteNotFoundError } from "@/features/sites/use-sites";

function hookWrapper({ children }: { children: ReactNode }) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return createElement(QueryClientProvider, { client: qc }, children);
}

/** Like hookWrapper, but shares a single QueryClient instance across renderHook calls. */
function wrapperFor(qc: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return createElement(QueryClientProvider, { client: qc }, children);
  };
}

// ---------------------------------------------------------------------------
// mapDeleteOrgError, GH #152 part 2: every documented DELETE /orgs/{orgId}
// refusal code must map to clear, actionable UI copy without a network call.
// ---------------------------------------------------------------------------

describe("mapDeleteOrgError", () => {
  it.each([
    ["confirm_name_required", "Type the organisation's name to confirm deletion."],
    [
      "confirm_name_mismatch",
      "That doesn't match the organisation's name. Type it exactly as shown, then try again.",
    ],
    [
      "cannot_delete_active_org",
      "Switch to another organisation first. You can't delete the one you're currently in.",
    ],
    ["billing_active", "Cancel the subscription before deleting this organisation."],
    [
      "restore_in_progress",
      "A restore is running on a site in this organisation. Wait for it to finish, then try again.",
    ],
    ["not_a_member", "You're not a member of this organisation."],
    ["insufficient_role", "Only the owner can delete this organisation."],
    ["org_already_deleted", "This organisation is already scheduled for deletion."],
    ["org_not_found", "This organisation could not be found."],
    ["invalid_org_id", "This organisation could not be found."],
    ["invalid_body", "Could not delete organisation: the request was malformed."],
  ] as const)("maps %s to a clear human message", (code, expected) => {
    expect(mapDeleteOrgError(code, "server said something unhelpful")).toBe(expected);
  });

  it("falls back to the server's own message for an undocumented code", () => {
    expect(mapDeleteOrgError("some_future_code", "a brand new refusal")).toBe(
      "a brand new refusal",
    );
  });

  it("falls back to a generic message when both code and server message are absent", () => {
    expect(mapDeleteOrgError(undefined, "")).toBe("Could not delete organisation.");
  });

  it("prefers the mapped copy over the server's own message for a known code", () => {
    // The server's wording is written for logs/API consumers ("switch to a
    // different organisation before deleting this one"); the UI must show the
    // house-style copy instead, never the raw server string.
    expect(
      mapDeleteOrgError(
        "cannot_delete_active_org",
        "switch to a different organisation before deleting this one",
      ),
    ).toBe("Switch to another organisation first. You can't delete the one you're currently in.");
  });
});

// ---------------------------------------------------------------------------
// orgDeleteConfirmMatches, the confirm-name enable gate. MUST be an exact,
// case-sensitive match with no client-side trim/normalize (the server itself
// trims outer whitespace only, then compares case-sensitively); the UI must
// never be more lenient than the server it is gating a call to.
// ---------------------------------------------------------------------------

describe("orgDeleteConfirmMatches", () => {
  it("matches an exact, case-sensitive equal string", () => {
    expect(orgDeleteConfirmMatches("Acme Corp", "Acme Corp")).toBe(true);
  });

  it("rejects a case mismatch (no client-side lowercasing)", () => {
    expect(orgDeleteConfirmMatches("acme corp", "Acme Corp")).toBe(false);
  });

  it("rejects leading/trailing whitespace (no client-side trimming)", () => {
    expect(orgDeleteConfirmMatches(" Acme Corp ", "Acme Corp")).toBe(false);
    expect(orgDeleteConfirmMatches("Acme Corp", " Acme Corp ")).toBe(false);
  });

  it("rejects an empty typed value against a non-empty name", () => {
    expect(orgDeleteConfirmMatches("", "Acme Corp")).toBe(false);
  });

  it("rejects a partial match", () => {
    expect(orgDeleteConfirmMatches("Acme", "Acme Corp")).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// useDeleteOrg onSuccess toast, GH #161 CodeRabbit review: a "hard" delete
// (empty org, gone immediately) must NOT claim to be recoverable during a
// grace window like a "soft" delete does.
// ---------------------------------------------------------------------------
describe("useDeleteOrg success toast", () => {
  beforeEach(() => {
    deleteMock.mockReset();
    toastSuccess.mockReset();
    toastError.mockReset();
    resetSiteStreamMock.mockReset();
    routerInvalidateMock.mockReset();
    routerNavigateMock.mockReset();
    routerState.location.pathname = "/dashboard";
  });

  it("hard delete shows a permanently-deleted toast with no grace-window description", async () => {
    deleteMock.mockResolvedValue({ data: { id: "o1", lane: "hard" }, error: undefined });
    const { result } = renderHook(() => useDeleteOrg(), { wrapper: hookWrapper });
    await result.current.mutateAsync({ orgId: "o1", confirmName: "Acme" });
    await waitFor(() => expect(toastSuccess).toHaveBeenCalledTimes(1));
    const hardCall = toastSuccess.mock.calls[0] as [string, unknown?] | undefined;
    expect(hardCall?.[0]).toBe('"Acme" has been permanently deleted');
    // No second arg: a hard delete has no recoverable grace window to describe.
    expect(hardCall?.[1]).toBeUndefined();
  });

  it("soft delete shows a scheduled toast with the grace-window description", async () => {
    deleteMock.mockResolvedValue({ data: { id: "o1", lane: "soft" }, error: undefined });
    const { result } = renderHook(() => useDeleteOrg(), { wrapper: hookWrapper });
    await result.current.mutateAsync({ orgId: "o1", confirmName: "Acme" });
    await waitFor(() => expect(toastSuccess).toHaveBeenCalledTimes(1));
    const softCall = toastSuccess.mock.calls[0] as
      | [string, { description?: string }?]
      | undefined;
    expect(softCall?.[0]).toBe('"Acme" is scheduled for permanent deletion');
    expect(softCall?.[1]?.description).toContain("recoverable");
  });
});

// ---------------------------------------------------------------------------
// GH #186: switching organizations must take effect in the UI immediately,
// without a full browser refresh, even when the target org is EMPTY. The
// bug: `queryClient.clear()` PASSIVELY empties the cache but never notifies
// or refetches an already-mounted QueryObserver, so a mounted Sites route
// kept rendering the PREVIOUS org's rows. For a populated target org this was
// incidentally masked by the SSE reset (an incoming site event invalidates
// and refetches) -- but an EMPTY target org has no sites, so no SSE event
// ever fires, and the observer stayed frozen until a hard reload. These
// tests never emit or depend on any SSE event, so they exercise exactly the
// empty-org path that exposed the bug.
// ---------------------------------------------------------------------------

describe("useActivateOrg actively refetches mounted queries — GH #186", () => {
  beforeEach(() => {
    postMock.mockReset();
    toastSuccess.mockReset();
    toastError.mockReset();
    resetSiteStreamMock.mockReset();
    routerInvalidateMock.mockReset();
    routerNavigateMock.mockReset();
    routerState.location.pathname = "/dashboard";
  });

  it("refetches a mounted org-scoped query on switch, with NO SSE event, proving the empty-target-org case", async () => {
    const qc = new QueryClient({
      defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
    });
    const wrapper = wrapperFor(qc);

    // A mounted, org-scoped query holding the PREVIOUS org's data -- mirrors
    // a live Sites route's useSites() staying mounted across the switch.
    const sitesQueryFn = vi
      .fn()
      .mockResolvedValueOnce(["old-org-site"])
      .mockResolvedValueOnce([]); // the target org is EMPTY (the reporter's exact repro)
    const sites = renderHook(
      () => useQuery({ queryKey: ["sites", "list"], queryFn: sitesQueryFn }),
      { wrapper },
    );
    await waitFor(() => expect(sites.result.current.data).toEqual(["old-org-site"]));
    expect(sitesQueryFn).toHaveBeenCalledTimes(1);

    postMock.mockResolvedValue({ data: { active_tenant_id: "org-2" }, error: undefined });
    const activate = renderHook(() => useActivateOrg(), { wrapper });
    await activate.result.current.mutateAsync("org-2");

    // Non-vacuous: against the old `queryClient.clear()` implementation this
    // assertion fails outright -- `clear()` never notifies a mounted
    // observer, so `sitesQueryFn` is never called a second time and `sites`
    // keeps showing the previous org's stale rows forever (verified directly
    // against real query-core semantics, not asserted on faith: a standalone
    // check confirmed `clear()` leaves a mounted observer's fetch count
    // unchanged while `resetQueries()` refetches it).
    await waitFor(() => expect(sitesQueryFn).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(sites.result.current.data).toEqual([]));

    // The SSE half (reset) and the route-loader half (router.invalidate)
    // must both still fire alongside the active cache reset.
    expect(resetSiteStreamMock).toHaveBeenCalledTimes(1);
    expect(routerInvalidateMock).toHaveBeenCalledTimes(1);
  });

  it("resets the mounted query's data to a fresh, non-stale value even before the empty org's refetch resolves", async () => {
    const qc = new QueryClient({
      defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
    });
    const wrapper = wrapperFor(qc);

    let resolveSecondFetch: (value: string[]) => void = () => {};
    const sitesQueryFn = vi
      .fn()
      .mockResolvedValueOnce(["old-org-site"])
      .mockImplementationOnce(
        () => new Promise<string[]>((resolve) => (resolveSecondFetch = resolve)),
      );
    const sites = renderHook(
      () => useQuery({ queryKey: ["sites", "list"], queryFn: sitesQueryFn }),
      { wrapper },
    );
    await waitFor(() => expect(sites.result.current.data).toEqual(["old-org-site"]));

    postMock.mockResolvedValue({ data: { active_tenant_id: "org-2" }, error: undefined });
    const activate = renderHook(() => useActivateOrg(), { wrapper });
    const activatePromise = activate.result.current.mutateAsync("org-2");

    // While the new org's fetch is still in flight, the observer must NOT be
    // showing the old org's rows as if they were current -- resetQueries()
    // clears the cached data back to undefined/pending instead of leaving
    // last-known-good data displayed under the new tenant.
    await waitFor(() => expect(sites.result.current.data).toBeUndefined());
    await waitFor(() => expect(sites.result.current.isFetching).toBe(true));

    resolveSecondFetch([]);
    await activatePromise;
    await waitFor(() => expect(sites.result.current.data).toEqual([]));
  });
});

describe("useDeleteOrg actively refetches mounted queries — GH #186", () => {
  beforeEach(() => {
    deleteMock.mockReset();
    toastSuccess.mockReset();
    toastError.mockReset();
    resetSiteStreamMock.mockReset();
    routerInvalidateMock.mockReset();
  });

  it("refetches a mounted org-scoped query after deleting the org, with no SSE event relied on", async () => {
    const qc = new QueryClient({
      defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
    });
    const wrapper = wrapperFor(qc);

    const membersQueryFn = vi
      .fn()
      .mockResolvedValueOnce(["old-org-member"])
      .mockResolvedValueOnce([]); // the reassigned active org has no members loaded yet
    const members = renderHook(
      () => useQuery({ queryKey: ["members", "list"], queryFn: membersQueryFn }),
      { wrapper },
    );
    await waitFor(() => expect(members.result.current.data).toEqual(["old-org-member"]));
    expect(membersQueryFn).toHaveBeenCalledTimes(1);

    deleteMock.mockResolvedValue({ data: { id: "o1", lane: "hard" }, error: undefined });
    const del = renderHook(() => useDeleteOrg(), { wrapper });
    await del.result.current.mutateAsync({ orgId: "o1", confirmName: "Acme" });

    // Non-vacuous for the same reason as useActivateOrg: this fails against
    // the old `queryClient.clear()` implementation.
    await waitFor(() => expect(membersQueryFn).toHaveBeenCalledTimes(2));
    expect(resetSiteStreamMock).toHaveBeenCalledTimes(1);
    expect(routerInvalidateMock).toHaveBeenCalledTimes(1);
  });
});

// ---------------------------------------------------------------------------
// GH #233: switching org while parked on a site-detail route (`/sites/$id/*`)
// for a site that belongs to the PREVIOUS org must not leave the operator
// staring at a raw "site not found" error. `resetQueries()` above already
// reset + refetched the mounted `useSite(siteId)` query under the new
// session; when that refetch comes back NotFound, `useActivateOrg` redirects
// to `/sites` (the new org's default list) instead of leaving the dead page
// up. These tests drive that mounted query directly (keyed on
// `sitesKeys.detail(siteId)`, exactly what `useSite` reads/writes) rather
// than mocking `useSite` itself, so the assertion exercises the real
// query-key contract between the two hooks.
// ---------------------------------------------------------------------------

describe("useActivateOrg redirects off a stale site-detail route — GH #233", () => {
  beforeEach(() => {
    postMock.mockReset();
    toastSuccess.mockReset();
    toastError.mockReset();
    resetSiteStreamMock.mockReset();
    routerInvalidateMock.mockReset();
    routerNavigateMock.mockReset();
    routerState.location.pathname = "/dashboard";
  });

  it("redirects to /sites when the site under the current route 404s in the new org", async () => {
    const qc = new QueryClient({
      defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
    });
    const wrapper = wrapperFor(qc);

    // Mirrors useSite("site-1") mounted by the site-detail layout: resolves
    // under the OLD org, then 404s once refetched under the NEW org.
    const siteQueryFn = vi
      .fn()
      .mockResolvedValueOnce({ id: "site-1", name: "Old org's site" })
      .mockRejectedValueOnce(new SiteNotFoundError("Site not found"));
    const site = renderHook(
      () => useQuery({ queryKey: sitesKeys.detail("site-1"), queryFn: siteQueryFn }),
      { wrapper },
    );
    await waitFor(() => expect(site.result.current.data).toEqual({ id: "site-1", name: "Old org's site" }));

    routerState.location.pathname = "/sites/site-1/health";

    postMock.mockResolvedValue({ data: { active_tenant_id: "org-2" }, error: undefined });
    const activate = renderHook(() => useActivateOrg(), { wrapper });
    await activate.result.current.mutateAsync("org-2");

    await waitFor(() => expect(site.result.current.error).toBeInstanceOf(SiteNotFoundError));
    expect(routerNavigateMock).toHaveBeenCalledTimes(1);
    expect(routerNavigateMock).toHaveBeenCalledWith({ to: "/sites" });
  });

  it("does NOT redirect when the site under the current route still exists in the new org", async () => {
    const qc = new QueryClient({
      defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
    });
    const wrapper = wrapperFor(qc);

    const siteQueryFn = vi
      .fn()
      .mockResolvedValueOnce({ id: "site-1", name: "Shared site" })
      .mockResolvedValueOnce({ id: "site-1", name: "Shared site" });
    const site = renderHook(
      () => useQuery({ queryKey: sitesKeys.detail("site-1"), queryFn: siteQueryFn }),
      { wrapper },
    );
    await waitFor(() => expect(site.result.current.data).toEqual({ id: "site-1", name: "Shared site" }));

    routerState.location.pathname = "/sites/site-1/health";

    postMock.mockResolvedValue({ data: { active_tenant_id: "org-2" }, error: undefined });
    const activate = renderHook(() => useActivateOrg(), { wrapper });
    await activate.result.current.mutateAsync("org-2");

    await waitFor(() => expect(siteQueryFn).toHaveBeenCalledTimes(2));
    expect(routerNavigateMock).not.toHaveBeenCalled();
  });

  it("does NOT redirect when switching org from a non-site route (e.g. /dashboard)", async () => {
    const qc = new QueryClient({
      defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
    });
    const wrapper = wrapperFor(qc);

    routerState.location.pathname = "/dashboard";

    postMock.mockResolvedValue({ data: { active_tenant_id: "org-2" }, error: undefined });
    const activate = renderHook(() => useActivateOrg(), { wrapper });
    await activate.result.current.mutateAsync("org-2");

    expect(routerNavigateMock).not.toHaveBeenCalled();
    expect(routerInvalidateMock).toHaveBeenCalledTimes(1);
  });

  it("does NOT redirect when switching org while already on the /sites list", async () => {
    const qc = new QueryClient({
      defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
    });
    const wrapper = wrapperFor(qc);

    routerState.location.pathname = "/sites";

    postMock.mockResolvedValue({ data: { active_tenant_id: "org-2" }, error: undefined });
    const activate = renderHook(() => useActivateOrg(), { wrapper });
    await activate.result.current.mutateAsync("org-2");

    expect(routerNavigateMock).not.toHaveBeenCalled();
  });
});
