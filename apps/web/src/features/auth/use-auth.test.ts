import { describe, it, expect } from "vitest";
import {
  hasOrg,
  isSuperadminAllowedPath,
  canWriteSiteContext,
  canManageInstanceEmail,
} from "./use-auth";
import type { Me } from "@wpmgr/api";

const TENANT_ID = "00000000-0000-0000-0000-0000000000aa";
const USER_ID = "00000000-0000-0000-0000-000000000001";

// A superadmin Me. With no overrides it belongs to no organisation.
function superadminMe(overrides: Partial<Me> = {}): Me {
  return {
    user: {
      id: USER_ID,
      email: "operator@wpmgr.test",
      name: "Operator",
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
      is_superadmin: true,
    },
    memberships: [],
    ...overrides,
  };
}

const NO_ORG = superadminMe();
const MEMBER_OF_ORG = superadminMe({
  memberships: [{ user_id: USER_ID, tenant_id: TENANT_ID, role: "owner" }],
  active_tenant_id: TENANT_ID,
});
// Reaches an organisation only through a share: no membership row, but the
// server reports that organisation as the active one.
const ACTIVE_TENANT_ONLY = superadminMe({ active_tenant_id: TENANT_ID });

// The one definition of "has an organisation". The create-organisation screen,
// the superadmin gate and the sidebar all ask it.
describe("hasOrg", () => {
  it("is false for a missing user", () => {
    expect(hasOrg(null)).toBe(false);
    expect(hasOrg(undefined)).toBe(false);
  });

  it("is false with no membership and no active tenant", () => {
    expect(hasOrg(NO_ORG)).toBe(false);
  });

  it("is true with a membership, whatever the role", () => {
    expect(hasOrg(MEMBER_OF_ORG)).toBe(true);
    expect(
      hasOrg(
        superadminMe({
          memberships: [{ user_id: USER_ID, tenant_id: TENANT_ID, role: "viewer" }],
        }),
      ),
    ).toBe(true);
  });

  it("is true with only an active tenant (a share, not a membership)", () => {
    expect(hasOrg(ACTIVE_TENANT_ONLY)).toBe(true);
  });
});

// A superadmin with no organisation is pinned to /admin by the _authed gate,
// EXCEPT their own per-user account settings (profile + 2FA), otherwise they
// can never enable their own 2FA (GH admin-panel report).
describe("isSuperadminAllowedPath, for a superadmin with no organisation", () => {
  it("allows the admin area and its children", () => {
    expect(isSuperadminAllowedPath(NO_ORG, "/admin")).toBe(true);
    expect(isSuperadminAllowedPath(NO_ORG, "/admin/accounts")).toBe(true);
    expect(isSuperadminAllowedPath(NO_ORG, "/admin/accounts/abc123")).toBe(true);
  });

  it("allows the superadmin's own personal account + security settings", () => {
    expect(isSuperadminAllowedPath(NO_ORG, "/settings/account")).toBe(true);
    expect(isSuperadminAllowedPath(NO_ORG, "/settings/security")).toBe(true);
  });

  // Instance SMTP capability gating (5ae87b71): the settings allow-list
  // grew exactly one entry, /settings/smtp — the exact string, admitted
  // regardless of the actual capability (this function only names a
  // PATH a superadmin may attempt to reach; the page itself refuses a
  // superadmin the server does not admit via can_manage_instance_email).
  it("allows /settings/smtp (the instance SMTP relay)", () => {
    expect(isSuperadminAllowedPath(NO_ORG, "/settings/smtp")).toBe(true);
  });

  it("admits /settings/smtp exactly, and nothing merely prefixed by it", () => {
    // A prefix-match bug here would silently open every /settings/smtp*
    // path, not just the one route the server actually gates this way.
    expect(isSuperadminAllowedPath(NO_ORG, "/settings/smtp-other")).toBe(false);
    expect(isSuperadminAllowedPath(NO_ORG, "/settings/smtp/anything")).toBe(false);
  });

  // GH #361: the vulnerability feed key follows the same authority as the
  // instance email settings, and the settings nav offers it beside Email / SMTP.
  // A superadmin with no organisation sees that entry in the settings layout, so
  // the path it links to must open for them. Exact string, like /settings/smtp.
  it("allows /settings/vuln-feed (the instance vulnerability feed key), and only that exact path", () => {
    expect(isSuperadminAllowedPath(NO_ORG, "/settings/vuln-feed")).toBe(true);
    expect(isSuperadminAllowedPath(NO_ORG, "/settings/vuln-feed-other")).toBe(false);
    expect(isSuperadminAllowedPath(NO_ORG, "/settings/vuln-feed/anything")).toBe(false);
  });

  it("still keeps the superadmin OUT of the tenant-scoped shell", () => {
    expect(isSuperadminAllowedPath(NO_ORG, "/")).toBe(false);
    expect(isSuperadminAllowedPath(NO_ORG, "/sites")).toBe(false);
    expect(isSuperadminAllowedPath(NO_ORG, "/uptime")).toBe(false);
    // org-scoped settings pages must stay blocked (they'd 403 with no org)
    expect(isSuperadminAllowedPath(NO_ORG, "/settings/organization")).toBe(false);
    expect(isSuperadminAllowedPath(NO_ORG, "/settings/billing")).toBe(false);
    expect(isSuperadminAllowedPath(NO_ORG, "/settings/members")).toBe(false);
  });
});

// GH #434. The same account, once it belongs to an organisation, is a member of
// it like anyone else and the gate has nothing to hold back.
describe("isSuperadminAllowedPath, for a superadmin who belongs to an organisation", () => {
  it("allows the tenant-scoped shell, not only the admin area", () => {
    for (const path of [
      "/",
      "/sites",
      "/sites/abc123",
      "/uptime",
      "/settings/organization",
      "/settings/billing",
      "/settings/members",
      "/admin",
      "/admin/accounts",
    ]) {
      expect(isSuperadminAllowedPath(MEMBER_OF_ORG, path), path).toBe(true);
    }
  });

  it("allows it for an active tenant reached through a share, too", () => {
    expect(isSuperadminAllowedPath(ACTIVE_TENANT_ONLY, "/sites")).toBe(true);
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
