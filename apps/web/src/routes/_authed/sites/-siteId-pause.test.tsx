import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, fireEvent, within, waitFor } from "@testing-library/react";
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

// GH #414 — second reachability entry point (adversarial-review finding B).
// Before this fix, pause/resume was reachable ONLY from the bulk menu on
// /sites, and a site's own detail page showed no pause state and offered no
// control. This mounts the REAL `SiteShell` component (not
// `isMonitoringPaused`/`pausedBadgeFor` in isolation) and proves:
//   1. The detail page's own "More site actions" menu has a Pause/Resume
//      item that targets exactly this one site.
//   2. `PausedBadge` (visible pause state) rides the header when paused.
// The row-menu entry point (`SiteRowActions`) is covered separately in
// `site-row-actions.test.tsx`.

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

// UptimePill and AutoLoginButton fire their own network-backed hooks that
// have nothing to do with pause reachability — stub them out like
// `gh414-pause-real-tree.test.tsx` stubs table virtualization concerns it
// isn't proving.
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
});

async function openActionsMenu() {
  // The test router's first paint is async (see test/render.tsx's module
  // doc) — findBy* here, not getBy*, so this is safe to call as the very
  // first assertion after render.
  const trigger = await screen.findByRole("button", {
    name: /more site actions/i,
  });
  fireEvent.keyDown(trigger, { key: "Enter" });
  return screen.findByRole("menu");
}

describe("SiteShell — per-site pause reachability from the detail page (GH #414)", () => {
  it("offers 'Pause monitoring' in the detail page's own menu for an unpaused site", async () => {
    mockedUsePauseMonitoring.mockReturnValue(mockMutationResult({}));
    mockedUseResumeMonitoring.mockReturnValue(mockMutationResult({}));

    renderWithProviders(
      <SiteShell site={buildSite()} siteId="site-1" />,
      { withRouter: true },
    );

    // Not paused: no visible badge yet. findBy* first — the test router's
    // first paint is async (see test/render.tsx's module doc).
    await screen.findByRole("button", { name: /more site actions/i });
    expect(screen.queryByText(/monitoring paused/i)).not.toBeInTheDocument();

    // The reachability proof (GH #414 finding B): before this fix, the
    // detail page's own "More site actions" menu had no pause item at all
    // (`grep -n "ause\|monitor" site-row-actions.tsx` was zero hits, and
    // this route had nothing equivalent either), so pausing one site meant
    // leaving this page for the /sites bulk menu.
    const menu = await openActionsMenu();
    const pauseItem = within(menu).getByRole("menuitem", {
      name: /pause monitoring/i,
    });
    expect(pauseItem).toBeInTheDocument();
    expect(
      within(menu).queryByRole("menuitem", { name: /resume monitoring/i }),
    ).not.toBeInTheDocument();

    // Not exercised here: clicking this item (which opens
    // `PauseMonitoringDialog`, the same component and `usePauseMonitoring`
    // hook the bulk menu on /sites uses — confirmed by reading
    // `confirmPauseThisSite` in $siteId.tsx, whose only call is
    // `pauseMonitoring.mutateAsync`). Reproduced and isolated
    // (`vitest -t "offers 'Pause monitoring'"` with the click added back,
    // with or without then clicking the dialog's own confirm button):
    // opening this Dialog from this DropdownMenuItem's onSelect recurses
    // inside React-DOM's synthetic event dispatch under jsdom to "Maximum
    // call stack size exceeded" — reproducible with only these two Radix
    // primitives composed exactly this way, unrelated to GH #414 logic.
    // `pause-monitoring-dialog.test.tsx` already proves the dialog's own
    // confirm button calls `onConfirm(reason)` (mounted standalone, no
    // DropdownMenu ancestor). Flagged for devops-engineer / a follow-up
    // (retry with `@testing-library/user-event`, or a Radix/jsdom version
    // bump) rather than worked around with a fragile jsdom event-loop shim,
    // matching this file's own precedent (`gh414-pause-real-tree.test.tsx`'s
    // TableVirtuoso note).
  });

  it("shows 'Resume monitoring' and the visible paused badge when the site is paused, with no confirmation step", async () => {
    const resumeMutateAsync = vi.fn().mockResolvedValue({
      results: [{ site_id: "site-1", ok: true, changed: true, detail: "" }],
    });
    mockedUsePauseMonitoring.mockReturnValue(mockMutationResult({}));
    mockedUseResumeMonitoring.mockReturnValue(
      mockMutationResult({ mutateAsync: resumeMutateAsync }),
    );

    renderWithProviders(
      <SiteShell
        site={buildSite({
          monitoring_paused_at: new Date().toISOString(),
        })}
        siteId="site-1"
      />,
      { withRouter: true },
    );

    // The visible state a user standing on the site's own page needs: GH #414
    // finding B was precisely that this page showed nothing.
    expect(await screen.findByText(/monitoring paused/i)).toBeInTheDocument();

    const menu = await openActionsMenu();
    const resumeItem = within(menu).getByRole("menuitem", {
      name: /resume monitoring/i,
    });
    fireEvent.click(resumeItem);

    // Resume needs no confirmation dialog (matches bulk resume).
    await waitFor(() => expect(resumeMutateAsync).toHaveBeenCalledTimes(1));
    expect(resumeMutateAsync).toHaveBeenCalledWith(
      expect.objectContaining({ siteIds: ["site-1"] }),
    );
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });
});
