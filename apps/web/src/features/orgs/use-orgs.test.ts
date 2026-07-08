import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createElement, type ReactNode } from "react";

// Mock the transport, toast, and SSE reset so useDeleteOrg's onSuccess can be
// exercised without a network call or touching the real event stream.
const { deleteMock, toastSuccess, toastError } = vi.hoisted(() => ({
  deleteMock: vi.fn(),
  toastSuccess: vi.fn(),
  toastError: vi.fn(),
}));
vi.mock("@wpmgr/api", () => ({
  client: { delete: deleteMock, get: vi.fn(), post: vi.fn(), patch: vi.fn() },
}));
vi.mock("@/components/toast", () => ({
  toast: { success: toastSuccess, error: toastError },
}));
vi.mock("@/features/sites/use-site-events", () => ({ resetSiteStream: vi.fn() }));

import { mapDeleteOrgError, orgDeleteConfirmMatches, useDeleteOrg } from "./use-orgs";

function hookWrapper({ children }: { children: ReactNode }) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return createElement(QueryClientProvider, { client: qc }, children);
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
  });

  it("hard delete shows a permanently-deleted toast with no grace-window description", async () => {
    deleteMock.mockResolvedValue({ data: { id: "o1", lane: "hard" }, error: undefined });
    const { result } = renderHook(() => useDeleteOrg(), { wrapper: hookWrapper });
    await result.current.mutateAsync({ orgId: "o1", confirmName: "Acme" });
    await waitFor(() => expect(toastSuccess).toHaveBeenCalledTimes(1));
    expect(toastSuccess).toHaveBeenCalledWith('"Acme" has been permanently deleted');
    // No second arg: a hard delete has no recoverable grace window to describe.
    expect(toastSuccess.mock.calls[0][1]).toBeUndefined();
  });

  it("soft delete shows a scheduled toast with the grace-window description", async () => {
    deleteMock.mockResolvedValue({ data: { id: "o1", lane: "soft" }, error: undefined });
    const { result } = renderHook(() => useDeleteOrg(), { wrapper: hookWrapper });
    await result.current.mutateAsync({ orgId: "o1", confirmName: "Acme" });
    await waitFor(() => expect(toastSuccess).toHaveBeenCalledTimes(1));
    expect(toastSuccess).toHaveBeenCalledWith(
      '"Acme" is scheduled for permanent deletion',
      expect.objectContaining({ description: expect.stringContaining("recoverable") }),
    );
  });
});
