import { describe, it, expect } from "vitest";

import { mapDeleteOrgError, orgDeleteConfirmMatches } from "./use-orgs";

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
