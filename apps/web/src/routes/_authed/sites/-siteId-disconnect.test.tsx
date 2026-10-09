import type { ReactNode } from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, fireEvent, within } from "@testing-library/react";
import type { Me, Site, SiteAutologinPolicy } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";
import { mockMutationResult, mockQueryResult } from "@/test/query-mocks";

import { SiteShell } from "./$siteId";
import { useMe } from "@/features/auth/use-auth";
import { useSaveDefaultLoginUser } from "@/features/sites/use-autologin-policy";
import {
  useRevokeSite,
  useArchiveSite,
  useRestoreSite,
  useCreateEnrollmentCode,
  useRecheckConnection,
} from "@/features/sites/use-site-connection";
import {
  usePauseMonitoring,
  useResumeMonitoring,
} from "@/features/sites/use-site-monitoring";

// What the Disconnect confirmation on a site's own page tells the operator.
// Disconnect only revokes the agent's access (POST /api/v1/sites/{id}/revoke,
// connection_handler.go): the site stays in the default sites list, labelled as
// disconnected, with its history kept, and the detail page then offers
// Reconnect and Archive. Archiving is a separate action with its own
// confirmation, so this one must not say the site "is archived".
//
// Mounts the REAL `SiteShell` and opens the dialog from the page's own
// "Disconnect site" menu item. The data hooks around the page are stubbed, the
// same set -siteId-pause.test.tsx stubs; the dialog text under test is the
// page's own.
//
// The actions menu is the one other piece replaced. Opening a Dialog from a
// Radix DropdownMenuItem recurses to "Maximum call stack size exceeded" under
// jsdom (the note in -siteId-pause.test.tsx has the isolation), so the menu
// chrome is a plain list of buttons here: an item still calls the page's
// `onSelect`, which is the only thing this test needs from it.

vi.mock("@/components/ui/dropdown-menu", () => ({
  DropdownMenu: ({ children }: { children: ReactNode }) => <>{children}</>,
  DropdownMenuTrigger: ({ children }: { children: ReactNode }) => <>{children}</>,
  DropdownMenuContent: ({ children }: { children: ReactNode }) => (
    <div role="menu">{children}</div>
  ),
  DropdownMenuItem: ({
    children,
    onSelect,
    onClick,
    disabled,
  }: {
    children: ReactNode;
    onSelect?: (event: Event) => void;
    onClick?: () => void;
    disabled?: boolean;
  }) => (
    <button
      type="button"
      role="menuitem"
      disabled={disabled}
      onClick={() => {
        onClick?.();
        onSelect?.(new Event("select"));
      }}
    >
      {children}
    </button>
  ),
  DropdownMenuSeparator: () => <hr />,
}));

vi.mock("@/features/auth/use-auth", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/auth/use-auth")>();
  return { ...actual, useMe: vi.fn() };
});

vi.mock("@/features/sites/use-autologin-policy", async (importOriginal) => {
  const actual = await importOriginal<
    typeof import("@/features/sites/use-autologin-policy")
  >();
  return { ...actual, useSaveDefaultLoginUser: vi.fn() };
});

vi.mock("@/features/sites/use-site-connection", async (importOriginal) => {
  const actual = await importOriginal<
    typeof import("@/features/sites/use-site-connection")
  >();
  return {
    ...actual,
    useRevokeSite: vi.fn(),
    useArchiveSite: vi.fn(),
    useRestoreSite: vi.fn(),
    useCreateEnrollmentCode: vi.fn(),
    useRecheckConnection: vi.fn(),
  };
});

vi.mock("@/features/sites/use-site-monitoring", async (importOriginal) => {
  const actual = await importOriginal<
    typeof import("@/features/sites/use-site-monitoring")
  >();
  return { ...actual, usePauseMonitoring: vi.fn(), useResumeMonitoring: vi.fn() };
});

// Network-backed widgets in the page header that have nothing to do with the
// dialog text.
vi.mock("@/features/monitoring/uptime-pill", () => ({
  UptimePill: () => null,
}));
vi.mock("@/features/sites/auto-login-button", () => ({
  AutoLoginButton: () => null,
}));

vi.mock("@/components/toast", () => ({
  toast: {
    success: vi.fn(),
    error: vi.fn(),
    warning: vi.fn(),
    info: vi.fn(),
    destructive: vi.fn(),
  },
}));

const mockedUseMe = vi.mocked(useMe);
const mockedUseSaveDefaultLoginUser = vi.mocked(useSaveDefaultLoginUser);
const mockedUseRevokeSite = vi.mocked(useRevokeSite);
const mockedUseArchiveSite = vi.mocked(useArchiveSite);
const mockedUseRestoreSite = vi.mocked(useRestoreSite);
const mockedUseCreateEnrollmentCode = vi.mocked(useCreateEnrollmentCode);
const mockedUseRecheckConnection = vi.mocked(useRecheckConnection);
const mockedUsePauseMonitoring = vi.mocked(usePauseMonitoring);
const mockedUseResumeMonitoring = vi.mocked(useResumeMonitoring);

function buildMe(): Me {
  return {
    user: {
      id: "u1",
      email: "owner@example.com",
      name: "Owner",
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
    },
    memberships: [{ user_id: "u1", tenant_id: "t1", role: "admin" }],
    active_tenant_id: "t1",
    hosted: true,
  } as unknown as Me;
}

function buildSite(overrides: Partial<Site> = {}): Site {
  return {
    id: "site-1",
    tenant_id: "t1",
    url: "https://example.com",
    name: "Example",
    status: "active",
    enrolled: true,
    wp_version: "6.8",
    php_version: "8.3",
    health_status: "healthy",
    connection_state: "connected",
    multisite: false,
    tags: [],
    ...overrides,
  } as unknown as Site;
}

beforeEach(() => {
  vi.clearAllMocks();
  mockedUseMe.mockReturnValue(mockQueryResult<Me | null>({ data: buildMe() }));
  mockedUseSaveDefaultLoginUser.mockReturnValue({
    policy: mockQueryResult<SiteAutologinPolicy>({ data: undefined }),
    save: vi.fn(),
  });
  mockedUseRevokeSite.mockReturnValue(mockMutationResult({}));
  mockedUseArchiveSite.mockReturnValue(mockMutationResult({}));
  mockedUseRestoreSite.mockReturnValue(mockMutationResult({}));
  mockedUseCreateEnrollmentCode.mockReturnValue(mockMutationResult({}));
  mockedUseRecheckConnection.mockReturnValue(mockMutationResult({}));
  mockedUsePauseMonitoring.mockReturnValue(mockMutationResult({}));
  mockedUseResumeMonitoring.mockReturnValue(mockMutationResult({}));
});

describe("SiteShell: the Disconnect confirmation", () => {
  it("says the site stays in the list as disconnected with its history kept, not that it is archived", async () => {
    renderWithProviders(<SiteShell site={buildSite()} siteId="site-1" />, {
      withRouter: true,
    });

    // The test router's first paint is async (see test/render.tsx's module
    // doc), so the first lookup is a findBy.
    const menu = await screen.findByRole("menu");
    fireEvent.click(
      within(menu).getByRole("menuitem", { name: /disconnect site/i }),
    );

    const dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent("Disconnect example.com");
    expect(dialog).toHaveTextContent(
      "Backups and monitoring stop. The site stays in your sites list as disconnected, with its full history kept. You can reconnect it or archive it later.",
    );
    expect(dialog).not.toHaveTextContent(/is archived/i);
  });
});
