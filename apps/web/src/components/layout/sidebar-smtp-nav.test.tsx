import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";
import { ShellContext, type ShellState } from "@/components/layout/app-shell-context";
import { Sidebar } from "@/components/layout/sidebar";
import { useMe } from "@/features/auth/use-auth";
import { useSites } from "@/features/sites/use-sites";
import { mockQueryResult } from "@/test/query-mocks";
import type { Me, Site } from "@wpmgr/api";

// Instance SMTP capability gating (5ae87b71): the superadmin console's
// "Email / SMTP" entry follows `me.can_manage_instance_email`, the same
// instance-authority decision the server uses to gate
// /api/v1/settings/smtp. This is the superadmin-console half of that gate —
// the tenant-facing settings left-nav half is covered by
// routes/_authed/settings/-route.test.tsx.

vi.mock("@/features/auth/use-auth", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/auth/use-auth")>();
  return { ...actual, useMe: vi.fn() };
});
vi.mock("@/features/sites/use-sites", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/sites/use-sites")>();
  return { ...actual, useSites: vi.fn() };
});

const mockedMe = vi.mocked(useMe);
const mockedSites = vi.mocked(useSites);

const SHELL: ShellState = {
  collapsed: false,
  toggleCollapsed: () => {},
  mobileOpen: false,
  setMobileOpen: () => {},
};

const SUPERADMIN_BASE: Me = {
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

function renderSidebarAs(me: Me) {
  mockedMe.mockReturnValue(mockQueryResult({ data: me }) as ReturnType<typeof useMe>);
  return renderWithProviders(
    <ShellContext.Provider value={SHELL}>
      <Sidebar />
    </ShellContext.Provider>,
    { withRouter: true, initialPath: "/admin" },
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  mockedSites.mockReturnValue(mockQueryResult<Site[]>({ data: [] }));
});

describe("Sidebar admin console — Email / SMTP entry follows can_manage_instance_email", () => {
  it("shows the Email / SMTP entry, routed to /settings/smtp, for a superadmin the server admits", async () => {
    renderSidebarAs({ ...SUPERADMIN_BASE, can_manage_instance_email: true });

    await screen.findByRole("navigation", { name: /primary/i });
    const link = screen
      .getAllByRole("link")
      .find((a) => a.getAttribute("href") === "/settings/smtp");
    expect(link, "no admin nav link points at /settings/smtp").toBeDefined();
    expect((link?.textContent ?? "").trim()).toBe("Email / SMTP");
  });

  it("hides the Email / SMTP entry when can_manage_instance_email is explicitly false", async () => {
    renderSidebarAs({ ...SUPERADMIN_BASE, can_manage_instance_email: false });

    await screen.findByRole("navigation", { name: /primary/i });
    const hrefs = screen
      .getAllByRole("link")
      .map((a) => a.getAttribute("href"))
      .filter((h): h is string => h !== null);
    expect(hrefs).not.toContain("/settings/smtp");
  });

  it("hides the Email / SMTP entry when can_manage_instance_email is absent (older API / pre-session response)", async () => {
    // SUPERADMIN_BASE carries no can_manage_instance_email field at all —
    // the exact shape an older server response, or a response captured
    // before the session exists, produces. Refused rather than defaulted
    // open.
    renderSidebarAs(SUPERADMIN_BASE);

    await screen.findByRole("navigation", { name: /primary/i });
    const hrefs = screen
      .getAllByRole("link")
      .map((a) => a.getAttribute("href"))
      .filter((h): h is string => h !== null);
    expect(hrefs).not.toContain("/settings/smtp");
  });

  it("still renders the rest of the admin console when the entry is hidden (the gate does not break the nav)", async () => {
    renderSidebarAs({ ...SUPERADMIN_BASE, can_manage_instance_email: false });

    const usersLink = await screen.findByRole("link", { name: "Users" });
    expect(usersLink).toHaveAttribute("href", "/admin");
    expect(screen.getByRole("link", { name: "Accounts" })).toHaveAttribute(
      "href",
      "/admin/accounts",
    );
  });
});
