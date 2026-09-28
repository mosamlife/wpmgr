import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, fireEvent, waitFor } from "@testing-library/react";
import type { Me, Site, SiteAutologinPolicy } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";
import { mockMutationResult, mockQueryResult } from "@/test/query-mocks";

import { SiteShell } from "./$siteId";
import { useMe } from "@/features/auth/use-auth";
import { useSaveDefaultLoginUser } from "@/features/sites/use-autologin-policy";
import {
  usePauseMonitoring,
  useResumeMonitoring,
} from "@/features/sites/use-site-monitoring";
// Real, unmocked export: `use-site-connection` itself is never mocked in this
// file (only the `@wpmgr/api` wire boundary is), so this is the exact class
// the hook's mutationFn throws.
import { SiteUrlRedirectsError } from "@/features/sites/use-site-connection";
import { toast } from "@/components/toast";

// GH #755 round 2 (T2, the required router-rendered test). Every earlier
// slice's coverage mocked `use-site-connection` itself, which proves the
// hook's own contract but never proves the copy actually reaches the
// screen through the real mutation, the real onError branch in $siteId.tsx,
// and the real toast call. This mounts the REAL `SiteShell` and the REAL
// `useRecheckConnection` hook, and fakes only the wire boundary
// (`@wpmgr/api`'s `client.post`), so a regression in either the hook's
// branching or the component's onError handler shows up here.

const { postMock } = vi.hoisted(() => ({ postMock: vi.fn() }));

vi.mock("@wpmgr/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wpmgr/api")>();
  return { ...actual, client: { ...actual.client, post: postMock } };
});

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

vi.mock("@/features/sites/use-site-monitoring", async (importOriginal) => {
  const actual = await importOriginal<
    typeof import("@/features/sites/use-site-monitoring")
  >();
  return { ...actual, usePauseMonitoring: vi.fn(), useResumeMonitoring: vi.fn() };
});

// Unrelated to redirect-copy reachability, same stubs as
// -siteId-pause.test.tsx.
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
const mockedUsePauseMonitoring = vi.mocked(usePauseMonitoring);
const mockedUseResumeMonitoring = vi.mocked(useResumeMonitoring);
const mockedToastError = vi.mocked(toast.error);

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
  postMock.mockReset();
  mockedUseMe.mockReturnValue(mockQueryResult<Me | null>({ data: buildMe() }));
  mockedUseSaveDefaultLoginUser.mockReturnValue({
    policy: mockQueryResult<SiteAutologinPolicy>({ data: undefined }),
    save: vi.fn(),
  });
  mockedUsePauseMonitoring.mockReturnValue(mockMutationResult({}));
  mockedUseResumeMonitoring.mockReturnValue(mockMutationResult({}));
});

async function clickRecheck() {
  const button = await screen.findByRole("button", {
    name: /re-check connection/i,
  });
  fireEvent.click(button);
}

describe("SiteShell re-check: renders the server's redirect copy verbatim (GH #755 round 2)", () => {
  it("shows the full server message for copy A (target the saved address will move to), with no Reconnect wording anywhere", async () => {
    const message =
      "Couldn't reach the agent. https://example.com redirects to https://www.example.com, so no command was sent. If WordPress on the site reports https://www.example.com as its address, the saved address updates to https://www.example.com automatically at the site's next daily check-in.";
    postMock.mockResolvedValue({
      data: undefined,
      error: {
        code: "site_url_redirects",
        message,
        details: {
          from: "https://example.com",
          to: "https://www.example.com/wp-json/wpmgr/v1/command/metadata",
          suggested_url: "https://www.example.com",
        },
      },
      response: { status: 502 },
    });

    const { queryClient } = renderWithProviders(
      <SiteShell site={buildSite()} siteId="site-1" />,
      { withRouter: true },
    );

    await clickRecheck();

    await waitFor(() => expect(mockedToastError).toHaveBeenCalledTimes(1));
    const [title, opts] = mockedToastError.mock.calls[0]!;
    expect(title).toBe("Re-check failed");
    expect(opts?.description).toBe(message);
    expect(opts?.description).toContain("so no command was sent");
    expect(opts?.description).toContain("https://www.example.com");
    expect(opts?.description).not.toContain("Reconnect");

    // Nothing rendered on screen names the retired remedy either.
    expect(document.body.textContent).not.toContain("Reconnect");

    // The rendered copy alone can't tell a SiteUrlRedirectsError from
    // toError's generic 502 fallback: the server always fills `message`, so
    // both paths produce the same toast text (see use-site-connection.ts).
    // Read the actual rejection off the mutation cache instead, which is the
    // one place the branch under test (error.code === "site_url_redirects")
    // is still observable: remove it and this goes red even though the toast
    // above still passes.
    const mutations = queryClient.getMutationCache().getAll();
    expect(mutations).toHaveLength(1);
    expect(mutations[0]?.state.error).toBeInstanceOf(SiteUrlRedirectsError);
  });

  it("shows the full server message for copy D (the site redirects its command address back to itself), with no Reconnect wording anywhere", async () => {
    const message =
      "Couldn't reach the agent. https://example.com redirects its command address back to itself (HTTP 301), so no command was sent. Exempt /wp-json/wpmgr/ from the redirect on the site or its CDN.";
    postMock.mockResolvedValue({
      data: undefined,
      error: {
        code: "site_url_redirects",
        message,
        details: {
          from: "https://example.com",
          to: "https://example.com/wp-json/wpmgr/v1/command/metadata",
        },
      },
      response: { status: 502 },
    });

    const { queryClient } = renderWithProviders(
      <SiteShell site={buildSite()} siteId="site-1" />,
      { withRouter: true },
    );

    await clickRecheck();

    await waitFor(() => expect(mockedToastError).toHaveBeenCalledTimes(1));
    const [title, opts] = mockedToastError.mock.calls[0]!;
    expect(title).toBe("Re-check failed");
    expect(opts?.description).toBe(message);
    expect(opts?.description).toContain("so no command was sent");
    expect(opts?.description).not.toContain("Reconnect");
    expect(document.body.textContent).not.toContain("Reconnect");

    // See the copy-A case above for why the rendered toast alone can't catch
    // a regression here.
    const mutations = queryClient.getMutationCache().getAll();
    expect(mutations).toHaveLength(1);
    expect(mutations[0]?.state.error).toBeInstanceOf(SiteUrlRedirectsError);
  });
});
