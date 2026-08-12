import { describe, it, expect } from "vitest";

import { createApiKeySchema } from "./api-keys";
import { mapApiKeyError } from "@/features/api-keys/use-api-keys";
import { mapMemberError } from "@/features/orgs/use-members";

// GH #406 follow-up. Hiding the Owner <option> is not the same as refusing to
// send "owner": the schema still validated it, so a stale form value or a
// devtools edit produced a request the server then had to refuse. The enum is
// now gated the same way the option is.

describe("createApiKeySchema -- the enum is gated on the viewer being an owner", () => {
  it("an OWNER may select owner", () => {
    const parsed = createApiKeySchema(true).safeParse({
      name: "ci",
      role: "owner",
    });
    expect(parsed.success).toBe(true);
  });

  it("an ADMIN cannot: owner fails validation, so the client never sends it", () => {
    const parsed = createApiKeySchema(false).safeParse({
      name: "ci",
      role: "owner",
    });
    expect(parsed.success).toBe(false);
  });

  it("every non-owner role stays valid for an admin (the gate must not over-fire)", () => {
    const schema = createApiKeySchema(false);
    for (const role of ["admin", "operator", "viewer"] as const) {
      expect(schema.safeParse({ name: "ci", role }).success).toBe(true);
    }
  });

  it("both branches still require a name", () => {
    expect(createApiKeySchema(true).safeParse({ name: "" }).success).toBe(false);
    expect(createApiKeySchema(false).safeParse({ name: "" }).success).toBe(false);
  });
});

// Codes quoted from the handlers, not invented:
//   apps/api/internal/apikey/handler.go:59        apikey_role_exceeds_actor
//   apps/api/internal/auth/members_handler.go:215/:268  target_role_exceeds_actor
//   apps/api/internal/auth/members_handler.go     role_grant_exceeds_actor, last_owner

describe("coded 403s reach the user as copy, not as a generic failure", () => {
  it("apikey_role_exceeds_actor gets its own message", () => {
    const message = mapApiKeyError("apikey_role_exceeds_actor", "Request failed");
    expect(message).not.toBe("Request failed");
    expect(message).toMatch(/higher than your own/i);
  });

  it("target_role_exceeds_actor gets its own message", () => {
    const message = mapMemberError("target_role_exceeds_actor", "Request failed");
    expect(message).not.toBe("Request failed");
    expect(message).toMatch(/outranks you/i);
  });

  it("last_owner and role_grant_exceeds_actor were already returned by the server and are now mapped too", () => {
    expect(mapMemberError("last_owner", "Request failed")).toMatch(
      /last owner/i,
    );
    expect(mapMemberError("role_grant_exceeds_actor", "Request failed")).toMatch(
      /higher than your own/i,
    );
  });

  it("an undocumented code falls back to the server's own message rather than a blank error", () => {
    expect(mapMemberError("some_future_code", "Server said no")).toBe(
      "Server said no",
    );
    expect(mapApiKeyError("some_future_code", "Server said no")).toBe(
      "Server said no",
    );
  });

  it("a missing code falls back too", () => {
    expect(mapMemberError(undefined, "Server said no")).toBe("Server said no");
    expect(mapApiKeyError(undefined, "Server said no")).toBe("Server said no");
  });
});
