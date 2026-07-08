import { describe, it, expect } from "vitest";
import { isSuperadminAllowedPath } from "./use-auth";

// A superadmin has no org and is pinned to /admin by the _authed gate, EXCEPT
// their own per-user account settings (profile + 2FA) — otherwise they can
// never enable their own 2FA (GH admin-panel report).
describe("isSuperadminAllowedPath", () => {
  it("allows the admin area and its children", () => {
    expect(isSuperadminAllowedPath("/admin")).toBe(true);
    expect(isSuperadminAllowedPath("/admin/accounts")).toBe(true);
    expect(isSuperadminAllowedPath("/admin/accounts/abc123")).toBe(true);
  });

  it("allows the superadmin's own personal account + security settings", () => {
    expect(isSuperadminAllowedPath("/settings/account")).toBe(true);
    expect(isSuperadminAllowedPath("/settings/security")).toBe(true);
  });

  it("still keeps the superadmin OUT of the tenant-scoped shell", () => {
    expect(isSuperadminAllowedPath("/")).toBe(false);
    expect(isSuperadminAllowedPath("/sites")).toBe(false);
    expect(isSuperadminAllowedPath("/uptime")).toBe(false);
    // org-scoped settings pages must stay blocked (they'd 403 with no org)
    expect(isSuperadminAllowedPath("/settings/organization")).toBe(false);
    expect(isSuperadminAllowedPath("/settings/billing")).toBe(false);
    expect(isSuperadminAllowedPath("/settings/members")).toBe(false);
  });
});
