import { describe, it, expect } from "vitest";
import {
  isSuperadminAllowedPath,
  canWriteSiteContext,
  canManageInstanceEmail,
} from "./use-auth";
import type { Me } from "@wpmgr/api";

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

  // Instance SMTP capability gating (5ae87b71): the settings allow-list
  // grew exactly one entry, /settings/smtp — the exact string, admitted
  // regardless of the actual capability (this function only names a
  // PATH a superadmin may attempt to reach; the page itself refuses a
  // superadmin the server does not admit via can_manage_instance_email).
  it("allows /settings/smtp (the instance SMTP relay)", () => {
    expect(isSuperadminAllowedPath("/settings/smtp")).toBe(true);
  });

  it("admits /settings/smtp exactly, and nothing merely prefixed by it", () => {
    // A prefix-match bug here would silently open every /settings/smtp*
    // path, not just the one route the server actually gates this way.
    expect(isSuperadminAllowedPath("/settings/smtp-other")).toBe(false);
    expect(isSuperadminAllowedPath("/settings/smtp/anything")).toBe(false);
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

// Instance SMTP capability gating (5ae87b71): canManageInstanceEmail reads
// me.can_manage_instance_email directly and nothing else — the server
// computes it from the same decision that gates GET/PUT
// /api/v1/settings/smtp, so the client must never re-derive it from role or
// org state.
describe("canManageInstanceEmail", () => {
  it("is true only when the server reports can_manage_instance_email: true", () => {
    expect(
      canManageInstanceEmail({ can_manage_instance_email: true } as unknown as Me),
    ).toBe(true);
  });

  it("is false when the server reports can_manage_instance_email: false", () => {
    expect(
      canManageInstanceEmail({ can_manage_instance_email: false } as unknown as Me),
    ).toBe(false);
  });

  it("is false when the field is absent (older API, or a pre-session Me response) — refused, never defaulted open", () => {
    expect(canManageInstanceEmail({ memberships: [] } as unknown as Me)).toBe(false);
  });

  it("is false for a null/undefined me", () => {
    expect(canManageInstanceEmail(null)).toBe(false);
    expect(canManageInstanceEmail(undefined)).toBe(false);
  });
});

// Greptile P1 #2 on #566, "Site collaborators remain read-only": canOperate
// only ever reads me.memberships, so a genuine site-scoped collaborator (no
// org membership at all) always read as unauthorized through it even when
// their own share role (Me.role) permits site-context write. This is the
// first helper in this codebase to gate a site-collaborator-visible WRITE
// action by Me.scope/Me.role directly (noted here and in the PR thread as an
// established pattern, not a silent one-off — nothing else in this codebase
// does the equivalent; every other me.scope === "site" site either excludes
// the collaborator outright or degrades to read-only).
function meOrgScoped(role: "owner" | "admin" | "operator" | "viewer"): Me {
  const tenant = "00000000-0000-0000-0000-0000000000aa";
  return {
    active_tenant_id: tenant,
    memberships: [{ tenant_id: tenant, role, tenant_name: "Acme" }],
  } as unknown as Me;
}

function meSiteScoped(role: Me["role"]): Me {
  return { scope: "site", role, memberships: [] } as unknown as Me;
}

describe("canWriteSiteContext — ADR-064 Decision 6 (site-scope write is not org-membership-gated)", () => {
  it("an org-scoped operator/admin/owner may write (canOperate's existing entitlement, unaffected)", () => {
    expect(canWriteSiteContext(meOrgScoped("owner"))).toBe(true);
    expect(canWriteSiteContext(meOrgScoped("admin"))).toBe(true);
    expect(canWriteSiteContext(meOrgScoped("operator"))).toBe(true);
  });

  it("an org-scoped viewer may not write", () => {
    expect(canWriteSiteContext(meOrgScoped("viewer"))).toBe(false);
  });

  it("a genuine SITE-scoped collaborator with an operator share role may write their own site (the fix)", () => {
    expect(canWriteSiteContext(meSiteScoped("operator"))).toBe(true);
  });

  it("a site-scoped VIEWER collaborator may not write", () => {
    expect(canWriteSiteContext(meSiteScoped("viewer"))).toBe(false);
  });

  // Security review finding (F2): apps/api/internal/middleware/auth.go:178-180
  // clamps a site share role at or above admin down to operator before it
  // ever reaches a Me response, specifically so scope==="site" can never
  // carry role: "owner"/"admin" — the server cannot produce that state. This
  // asserts the SERVER's actual ceiling, not a client-invented one: even if
  // a malformed/future response somehow smuggled owner or admin through
  // scope==="site", this client refuses it rather than trusting a shape the
  // real server never sends.
  it("refuses owner/admin under scope==='site' — a shape the server's own clamp can never produce", () => {
    expect(canWriteSiteContext(meSiteScoped("owner"))).toBe(false);
    expect(canWriteSiteContext(meSiteScoped("admin"))).toBe(false);
  });

  it("an unrecognised or absent Me.scope resolves to false — refused, never a default and never 'site because it's narrower'", () => {
    // Absent scope entirely (older response shape).
    expect(
      canWriteSiteContext({ role: "owner", memberships: [] } as unknown as Me),
    ).toBe(false);
    // Empty-string scope (PrincipalRole/scope's own "unauthenticated" value).
    expect(
      canWriteSiteContext({ scope: "", role: "owner", memberships: [] } as unknown as Me),
    ).toBe(false);
    // A future/unknown scope value this function has never heard of — must
    // NOT fall through to either the org or the site branch.
    expect(
      canWriteSiteContext({
        scope: "portal",
        role: "owner",
        memberships: [],
      } as unknown as Me),
    ).toBe(false);
  });

  // Security review finding (F3): scope==="site" explicitly includes portal
  // (client-report) principals per the generated Me.scope doc comment
  // (types.gen.ts:939, "site for collaborators/portal principals"), and
  // role: "client" is a real value that reaches this code, not a
  // hypothetical. It resolves to false today because "client" matches none
  // of the three checked strings, but nothing held that down before this
  // test — "happens to be false" is not "tested to be false".
  it("refuses a portal (client) principal, even though it is a real scope==='site' shape", () => {
    expect(canWriteSiteContext(meSiteScoped("client"))).toBe(false);
  });

  it("null/undefined me is refused outright", () => {
    expect(canWriteSiteContext(null)).toBe(false);
    expect(canWriteSiteContext(undefined)).toBe(false);
  });
});
