import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, fireEvent } from "@testing-library/react";
import type { Me } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";
import { mockQueryResult } from "@/test/query-mocks";
import { UserMenu } from "@/components/layout/top-bar";

import { Route, SETTINGS_NAV_ITEMS } from "./route";

// Rendered via `Route.options.component` rather than a dedicated named
// export of the layout function: TanStack Router's vite plugin auto-splits
// each route's `component` into its own chunk, and adding a second named
// export of that same function for testability purposes would opt it back
// out of that split (the plugin warns "will not be code-split and will
// increase your bundle size" the moment such an export exists) — `Route` is
// already exported for route registration, so reading `.options.component`
// off it is free.
const SettingsLayout = Route.options.component!;

// GH nav-gap fix: the 2FA suite at /settings/security was fully built and
// worked by direct URL, but was unreachable for a superadmin — the Security
// item carried `orgOnly: true`, and a seeded superadmin has no membership
// (isOrgScoped(me) is false), so `SETTINGS_NAV_ITEMS`'s filter silently
// dropped it, and nothing linked to it from /admin or the user menu either.
// Fixed by (a) removing `orgOnly` from the Security item (it's a PERSONAL,
// per-user /auth/2fa/* setting, not an org setting) and (b) adding a direct
// link in the top-bar user menu.

vi.mock("@/features/auth/use-auth", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/auth/use-auth")>();
  return { ...actual, useMe: vi.fn() };
});

const { useMe } = await import("@/features/auth/use-auth");
const mockedUseMe = vi.mocked(useMe);

// A superadmin with NO membership — the exact shape that made this bug
// invisible: `isOrgScoped(me)` is false (no `memberships` entry matching
// `active_tenant_id`, and `active_tenant_id` itself is absent), so any item
// still marked `orgOnly` would be filtered out for this principal.
const SUPERADMIN_ME: Me = {
  user: {
    id: "00000000-0000-0000-0000-000000000001",
    email: "superadmin@wpmgr.test",
    name: "Superadmin",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    is_superadmin: true,
  },
  memberships: [],
};

beforeEach(() => {
  mockedUseMe.mockReturnValue(mockQueryResult<Me | null>({ data: SUPERADMIN_ME }));
});

describe("SETTINGS_NAV_ITEMS — Security is not orgOnly (regression guard)", () => {
  it("Security does not carry orgOnly (this exact flag is what filtered it out for a superadmin)", () => {
    const security = SETTINGS_NAV_ITEMS.find((item) => item.label === "Security");
    expect(security).toBeDefined();
    expect(security?.to).toBe("/settings/security");
    expect(security?.orgOnly).not.toBe(true);
  });

  it("still marks genuinely org-scoped items as orgOnly (this fix did not remove ALL gating)", () => {
    const organisation = SETTINGS_NAV_ITEMS.find((item) => item.label === "Organisation");
    expect(organisation?.orgOnly).toBe(true);
  });
});

// Instance SMTP capability gating (5ae87b71): the "Email / SMTP" settings
// nav item follows me.can_manage_instance_email rather than orgOnly — a
// superadmin has no organisation and must still see it when the server
// admits them, and an org member (even the owner) must NOT see it merely
// for being org-scoped.
describe("SETTINGS_NAV_ITEMS — Email / SMTP carries instanceEmailOnly, not orgOnly (regression guard)", () => {
  it("Email / SMTP is gated by instanceEmailOnly and not by orgOnly", () => {
    const smtp = SETTINGS_NAV_ITEMS.find((item) => item.label === "Email / SMTP");
    expect(smtp).toBeDefined();
    expect(smtp?.to).toBe("/settings/smtp");
    expect(smtp?.instanceEmailOnly).toBe(true);
    expect(smtp?.orgOnly).not.toBe(true);
  });
});

// GH #361: the vulnerability feed key follows the same authority as the instance
// email settings, so its entry carries the same flag and sits beside Email / SMTP.
// The layout and page behaviour is exercised through the real routes in
// -vuln-feed.test.tsx; this pins the item definition itself.
describe("SETTINGS_NAV_ITEMS — Vulnerability feed carries instanceEmailOnly, not orgOnly", () => {
  it("is gated by instanceEmailOnly and not by orgOnly, and sits directly after Email / SMTP", () => {
    const feed = SETTINGS_NAV_ITEMS.find((item) => item.label === "Vulnerability feed");
    expect(feed).toBeDefined();
    expect(feed?.to).toBe("/settings/vuln-feed");
    expect(feed?.instanceEmailOnly).toBe(true);
    expect(feed?.orgOnly).not.toBe(true);

    const labels = SETTINGS_NAV_ITEMS.map((item) => item.label);
    expect(labels.indexOf("Vulnerability feed")).toBe(labels.indexOf("Email / SMTP") + 1);
  });
});

describe("SettingsLayout — Email / SMTP link follows can_manage_instance_email", () => {
  // A tenant owner: org-scoped, so every orgOnly item would show, but that is
  // NOT what gates Email / SMTP — only the explicit capability does.
  const TENANT_ID = "00000000-0000-0000-0000-0000000000aa";
  const ORG_OWNER_ME: Me = {
    user: {
      id: "00000000-0000-0000-0000-000000000002",
      email: "owner@wpmgr.test",
      name: "Owner",
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
    },
    memberships: [{ user_id: "00000000-0000-0000-0000-000000000002", tenant_id: TENANT_ID, role: "owner" }],
    active_tenant_id: TENANT_ID,
  };

  it("shows Email / SMTP for an org owner the server reports as instance-capable", async () => {
    mockedUseMe.mockReturnValue(
      mockQueryResult<Me | null>({
        data: { ...ORG_OWNER_ME, can_manage_instance_email: true },
      }),
    );

    renderWithProviders(<SettingsLayout />, {
      withRouter: true,
      initialPath: "/settings/account",
    });

    const link = await screen.findByRole("link", { name: "Email / SMTP" });
    expect(link).toHaveAttribute("href", "/settings/smtp");
  });

  it("hides Email / SMTP for an org owner the server does NOT report as instance-capable (orgOnly items still show — proves this isn't just orgOnly)", async () => {
    mockedUseMe.mockReturnValue(
      mockQueryResult<Me | null>({
        data: { ...ORG_OWNER_ME, can_manage_instance_email: false },
      }),
    );

    renderWithProviders(<SettingsLayout />, {
      withRouter: true,
      initialPath: "/settings/account",
    });

    // An org-scoped item renders, proving orgOnly gating still works for
    // this principal — the absence of Email / SMTP below is the capability
    // gate specifically, not a broken orgOnly filter.
    await screen.findByRole("link", { name: "Organisation" });
    expect(screen.queryByRole("link", { name: "Email / SMTP" })).not.toBeInTheDocument();
  });

  it("hides Email / SMTP when can_manage_instance_email is absent from the response", async () => {
    mockedUseMe.mockReturnValue(mockQueryResult<Me | null>({ data: ORG_OWNER_ME }));

    renderWithProviders(<SettingsLayout />, {
      withRouter: true,
      initialPath: "/settings/account",
    });

    await screen.findByRole("link", { name: "Organisation" });
    expect(screen.queryByRole("link", { name: "Email / SMTP" })).not.toBeInTheDocument();
  });

  it("shows Email / SMTP for a superadmin with NO organisation, and the layout renders cleanly with every org-only item hidden", async () => {
    mockedUseMe.mockReturnValue(
      mockQueryResult<Me | null>({
        data: { ...SUPERADMIN_ME, can_manage_instance_email: true },
      }),
    );

    renderWithProviders(<SettingsLayout />, {
      withRouter: true,
      initialPath: "/settings/account",
    });

    const smtpLink = await screen.findByRole("link", { name: "Email / SMTP" });
    expect(smtpLink).toHaveAttribute("href", "/settings/smtp");
    // Every orgOnly item stays hidden for this principal — the layout does
    // not break just because instanceEmailOnly admits one more item.
    expect(screen.queryByRole("link", { name: "Organisation" })).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Billing" })).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Members" })).not.toBeInTheDocument();
    // The personal, always-visible items are still there alongside it.
    expect(screen.getByRole("link", { name: "Account" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Security" })).toBeInTheDocument();
  });
});

describe("SettingsLayout — Security renders for a superadmin with no membership", () => {
  it("renders a Security link for a superadmin (is_superadmin, isOrgScoped false), and still hides Organisation", async () => {
    renderWithProviders(<SettingsLayout />, {
      withRouter: true,
      initialPath: "/settings/account",
    });

    const securityLink = await screen.findByRole("link", { name: "Security" });
    expect(securityLink).toHaveAttribute("href", "/settings/security");

    // Sanity check: a genuinely org-scoped item is still absent for this
    // principal, proving the fix didn't just remove every filter.
    expect(screen.queryByRole("link", { name: "Organisation" })).not.toBeInTheDocument();
  });
});

describe("Top bar user menu — direct Security link (GH nav-gap fix)", () => {
  it("renders a Security item routed to /settings/security, one click from anywhere including /admin", async () => {
    renderWithProviders(<UserMenu />, {
      withRouter: true,
      initialPath: "/admin",
    });

    // First query after mount must be async (`findBy*`) — RouterProvider's
    // first paint resolves in a microtask, per src/test/render.tsx's module
    // doc; a bare `getByRole` here races an empty DOM.
    const trigger = await screen.findByRole("button", {
      name: "Account menu for Superadmin",
    });
    // The dropdown opens on pointerdown OR Enter/Space (Radix
    // DropdownMenuTrigger); Enter is the more deterministic path in jsdom.
    fireEvent.keyDown(trigger, { key: "Enter" });

    const securityItem = await screen.findByRole("menuitem", { name: /security/i });
    expect(securityItem).toHaveAttribute("href", "/settings/security");
  });
});
