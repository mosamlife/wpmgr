import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen } from "@testing-library/react";
import {
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import type { Me } from "@wpmgr/api";

import { createTestQueryClient, renderWithProviders } from "@/test/render";
import { mockQueryResult, mockMutationResult } from "@/test/query-mocks";
import type { Member } from "@/features/orgs/use-members";

import { Route as MembersRoute, canActOnMemberRow } from "./members";

// GH #406 follow-up. The escalation fix correctly stopped an ADMIN from
// touching the owner's row, but it applied that same block to an OWNER, so an
// org that reached two owners through the dashboard could never return to one:
// every owner row rendered as static text with no role picker and no Remove.
//
// The API permits owner-on-owner. The refusal in
// apps/api/internal/auth/members_handler.go:215 and :268 is
// `target_role_exceeds_actor` -- "you cannot change the role of a member who
// outranks you" -- and an owner does not outrank an owner. So the UI was
// stricter than the API. These tests pin the four-cell matrix so neither half
// can drift: the owner half must stay open, the admin half must stay shut.

vi.mock("@/features/auth/use-auth", async (importOriginal) => {
  const actual =
    await importOriginal<typeof import("@/features/auth/use-auth")>();
  return { ...actual, useMe: vi.fn() };
});

vi.mock("@/features/orgs/use-members", async (importOriginal) => {
  const actual =
    await importOriginal<typeof import("@/features/orgs/use-members")>();
  return {
    ...actual,
    useMembers: vi.fn(),
    useUpdateMemberRole: vi.fn(),
    useRemoveMember: vi.fn(),
    useInviteMember: vi.fn(),
  };
});

const { useMe } = await import("@/features/auth/use-auth");
const {
  useMembers,
  useUpdateMemberRole,
  useRemoveMember,
  useInviteMember,
} = await import("@/features/orgs/use-members");

const mockedUseMe = vi.mocked(useMe);
const mockedUseMembers = vi.mocked(useMembers);

const TENANT = "00000000-0000-0000-0000-0000000000aa";
const VIEWER_ID = "00000000-0000-0000-0000-000000000001";
const OTHER_OWNER_ID = "00000000-0000-0000-0000-000000000002";
const ADMIN_ID = "00000000-0000-0000-0000-000000000003";

function meWithRole(role: "owner" | "admin"): Me {
  return {
    user: {
      id: VIEWER_ID,
      email: "viewer@wpmgr.test",
      name: "Viewer",
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
    },
    active_tenant_id: TENANT,
    memberships: [{ tenant_id: TENANT, role, tenant_name: "Acme" }],
  } as unknown as Me;
}

const CO_OWNER: Member = {
  user_id: OTHER_OWNER_ID,
  tenant_id: TENANT,
  role: "owner",
  email: "co-owner@wpmgr.test",
};

const AN_ADMIN: Member = {
  user_id: ADMIN_ID,
  tenant_id: TENANT,
  role: "admin",
  email: "an-admin@wpmgr.test",
};

beforeEach(() => {
  vi.mocked(useUpdateMemberRole).mockReturnValue(mockMutationResult({}));
  vi.mocked(useRemoveMember).mockReturnValue(mockMutationResult({}));
  vi.mocked(useInviteMember).mockReturnValue(mockMutationResult({}));
});

/**
 * Attaches this file's exported `Route` singleton to a throwaway root route --
 * the same post-hoc wiring `routeTree.gen.ts` performs, collapsed to a single
 * path segment so the real `_authed` session guard never runs. Identical to
 * `buildBillingRouter` in `-billing.test.tsx`.
 *
 * Mounting the real route rather than reading `Route.options.component` off it
 * is deliberate: the vite router plugin rewrites `component` into a split
 * wrapper, and rendering that wrapper directly produced an empty DOM.
 */
function buildMembersRouter() {
  const rootRoute = createRootRoute({});
  type UpdateOptions = Parameters<typeof MembersRoute.update>[0];
  const membersRoute = MembersRoute.update({
    id: "/settings/members",
    path: "/settings/members",
    getParentRoute: () => rootRoute,
  } as unknown as UpdateOptions);
  const routeTree = rootRoute.addChildren([membersRoute]);
  return createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: ["/settings/members"] }),
  });
}

async function renderAs(role: "owner" | "admin", members: Member[]) {
  mockedUseMe.mockReturnValue(
    mockQueryResult<Me | null>({ data: meWithRole(role) }),
  );
  mockedUseMembers.mockReturnValue(mockQueryResult<Member[]>({ data: members }));
  renderWithProviders(<RouterProvider router={buildMembersRouter()} />, {
    queryClient: createTestQueryClient(),
  });
  // RouterProvider's first paint is a microtask, so the first lookup awaits.
  await screen.findByRole("table");
}

describe("members page -- owner/admin x owner/admin row matrix", () => {
  it("OWNER viewing a CO-OWNER: role picker and Remove are both present (handover is possible)", async () => {
    await renderAs("owner", [CO_OWNER]);
    expect(
      screen.getByRole("combobox", { name: `Role for ${CO_OWNER.email}` }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: `Remove ${CO_OWNER.email}` }),
    ).toBeInTheDocument();
  });

  it("OWNER viewing an ADMIN: role picker and Remove are both present", async () => {
    await renderAs("owner", [AN_ADMIN]);
    expect(
      screen.getByRole("combobox", { name: `Role for ${AN_ADMIN.email}` }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: `Remove ${AN_ADMIN.email}` }),
    ).toBeInTheDocument();
  });

  it("ADMIN viewing the OWNER: static text only -- no picker, no Remove (this is the GH #406 fix)", async () => {
    await renderAs("admin", [CO_OWNER]);
    expect(
      screen.queryByRole("combobox", { name: `Role for ${CO_OWNER.email}` }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: `Remove ${CO_OWNER.email}` }),
    ).not.toBeInTheDocument();
  });

  it("ADMIN viewing another ADMIN: role picker and Remove are both present", async () => {
    await renderAs("admin", [AN_ADMIN]);
    expect(
      screen.getByRole("combobox", { name: `Role for ${AN_ADMIN.email}` }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: `Remove ${AN_ADMIN.email}` }),
    ).toBeInTheDocument();
  });
});

describe("members page -- the Owner option in the role picker", () => {
  it("an OWNER can nominate a co-owner (Owner option is offered)", async () => {
    await renderAs("owner", [AN_ADMIN]);
    const select = screen.getByRole("combobox", {
      name: `Role for ${AN_ADMIN.email}`,
    });
    expect(
      [...select.querySelectorAll("option")].map((o) => o.value),
    ).toEqual(["owner", "admin", "operator", "viewer"]);
  });

  it("an ADMIN is not offered Owner (an admin self-promoting was the escalation)", async () => {
    await renderAs("admin", [AN_ADMIN]);
    const select = screen.getByRole("combobox", {
      name: `Role for ${AN_ADMIN.email}`,
    });
    expect(
      [...select.querySelectorAll("option")].map((o) => o.value),
    ).toEqual(["admin", "operator", "viewer"]);
  });
});

describe("canActOnMemberRow -- the gate itself", () => {
  it("is closed on the owner row for an admin and open for an owner", () => {
    const base = { manage: true, isCurrentUser: false } as const;
    expect(
      canActOnMemberRow({ ...base, viewerIsOwner: true, targetRole: "owner" }),
    ).toBe(true);
    expect(
      canActOnMemberRow({ ...base, viewerIsOwner: false, targetRole: "owner" }),
    ).toBe(false);
  });

  it("never opens for a viewer who cannot manage, whatever their role claim", () => {
    expect(
      canActOnMemberRow({
        manage: false,
        viewerIsOwner: true,
        isCurrentUser: false,
        targetRole: "admin",
      }),
    ).toBe(false);
  });

  it("stays closed on the viewer's own row, so an owner cannot demote themselves into a zero-owner org", () => {
    expect(
      canActOnMemberRow({
        manage: true,
        viewerIsOwner: true,
        isCurrentUser: true,
        targetRole: "owner",
      }),
    ).toBe(false);
  });
});
