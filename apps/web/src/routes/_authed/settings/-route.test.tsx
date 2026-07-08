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
