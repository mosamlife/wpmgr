import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, fireEvent, screen, waitFor } from "@testing-library/react";
import {
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import type { ApiKey, ApiKeyCreated, Me } from "@wpmgr/api";

import { createTestQueryClient, renderWithProviders } from "@/test/render";
import { mockQueryResult, mockMutationResult } from "@/test/query-mocks";

import { Route as ApiKeysRoute, createApiKeySchema } from "./api-keys";
import { mapApiKeyError } from "@/features/api-keys/use-api-keys";
import { mapMemberError } from "@/features/orgs/use-members";

// `useMe` is a TanStack Query hook in production, and that is load-bearing for
// the org-switch test below: the switcher resets the cache and `me` changes
// value under a still-mounted page. A `vi.fn()` returning a literal cannot
// reproduce that -- swapping its return value re-renders nothing, because
// nothing is subscribed to it. So the stand-in is a real `useQuery` over a
// key the test seeds and then rewrites, which re-renders exactly the way the
// real hook does.
const ME_KEY = ["test", "me"] as const;

vi.mock("@/features/auth/use-auth", async (importOriginal) => {
  const actual =
    await importOriginal<typeof import("@/features/auth/use-auth")>();
  const { useQuery } = await import("@tanstack/react-query");
  return {
    ...actual,
    useMe: () =>
      useQuery({
        queryKey: ["test", "me"],
        queryFn: () => null,
        staleTime: Infinity,
      }),
  };
});

vi.mock("@/features/api-keys/use-api-keys", async (importOriginal) => {
  const actual =
    await importOriginal<typeof import("@/features/api-keys/use-api-keys")>();
  return {
    ...actual,
    useApiKeys: vi.fn(),
    useCreateApiKey: vi.fn(),
    useRevokeApiKey: vi.fn(),
  };
});

const { useApiKeys, useCreateApiKey, useRevokeApiKey } = await import(
  "@/features/api-keys/use-api-keys"
);

// GH #406 follow-up. Hiding the Owner <option> is not the same as refusing to
// send "owner": the schema still validated it, so a stale form value or a
// devtools edit produced a request the server then had to refuse. The enum is
// now gated the same way the option is.

describe("createApiKeySchema -- the enum is gated on the viewer being an owner", () => {
  it("an OWNER may select owner", () => {
    const parsed = createApiKeySchema(true).safeParse({
      name: "ci",
      role: "owner",
    });
    expect(parsed.success).toBe(true);
  });

  it("an ADMIN cannot: owner fails validation, so the client never sends it", () => {
    const parsed = createApiKeySchema(false).safeParse({
      name: "ci",
      role: "owner",
    });
    expect(parsed.success).toBe(false);
  });

  it("every non-owner role stays valid for an admin (the gate must not over-fire)", () => {
    const schema = createApiKeySchema(false);
    for (const role of ["admin", "operator", "viewer"] as const) {
      expect(schema.safeParse({ name: "ci", role }).success).toBe(true);
    }
  });

  it("both branches still require a name", () => {
    expect(createApiKeySchema(true).safeParse({ name: "" }).success).toBe(false);
    expect(createApiKeySchema(false).safeParse({ name: "" }).success).toBe(false);
  });
});

// Codes quoted from the handlers, not invented:
//   apps/api/internal/apikey/handler.go:59        apikey_role_exceeds_actor
//   apps/api/internal/auth/members_handler.go:215/:268  target_role_exceeds_actor
//   apps/api/internal/auth/members_handler.go     role_grant_exceeds_actor, last_owner

describe("coded 403s reach the user as copy, not as a generic failure", () => {
  it("apikey_role_exceeds_actor gets its own message", () => {
    const message = mapApiKeyError("apikey_role_exceeds_actor", "Request failed");
    expect(message).not.toBe("Request failed");
    expect(message).toMatch(/higher than your own/i);
  });

  it("target_role_exceeds_actor gets its own message", () => {
    const message = mapMemberError("target_role_exceeds_actor", "Request failed");
    expect(message).not.toBe("Request failed");
    expect(message).toMatch(/outranks you/i);
  });

  it("last_owner and role_grant_exceeds_actor were already returned by the server and are now mapped too", () => {
    expect(mapMemberError("last_owner", "Request failed")).toMatch(
      /last owner/i,
    );
    expect(mapMemberError("role_grant_exceeds_actor", "Request failed")).toMatch(
      /higher than your own/i,
    );
  });

  it("an undocumented code falls back to the server's own message rather than a blank error", () => {
    expect(mapMemberError("some_future_code", "Server said no")).toBe(
      "Server said no",
    );
    expect(mapApiKeyError("some_future_code", "Server said no")).toBe(
      "Server said no",
    );
  });

  it("a missing code falls back too", () => {
    expect(mapMemberError(undefined, "Server said no")).toBe("Server said no");
    expect(mapApiKeyError(undefined, "Server said no")).toBe("Server said no");
  });
});

// ---------------------------------------------------------------------------
// The page wiring, mounted.
//
// The pure tests above cover `createApiKeySchema` and the code->copy mappers,
// but nothing above touches the line that decides which branch the page asks
// for (api-keys.tsx: `const viewerIsOwner = activeRole(me) === "owner"`).
// Replacing that with `canManage(me)` -- the exact GH #406 escalation, since
// canManage is true for an admin -- leaves every pure test green. So the page
// is mounted here the same way `-members.owner-handover.test.tsx` mounts the
// members route.
// ---------------------------------------------------------------------------

const TENANT = "00000000-0000-0000-0000-0000000000aa";
const VIEWER_ID = "00000000-0000-0000-0000-000000000001";

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

const CREATED: ApiKeyCreated = {
  token: "wpm_live_deadbeef",
  api_key: {
    id: "00000000-0000-0000-0000-0000000000ff",
    name: "ci",
    prefix: "wpm_live",
    role: "operator",
    created_at: "2026-01-01T00:00:00Z",
  },
} as unknown as ApiKeyCreated;

let mutateAsync: ReturnType<typeof vi.fn>;

beforeEach(() => {
  mutateAsync = vi.fn().mockResolvedValue(CREATED);
  vi.mocked(useApiKeys).mockReturnValue(mockQueryResult<ApiKey[]>({ data: [] }));
  vi.mocked(useCreateApiKey).mockReturnValue(mockMutationResult({ mutateAsync }));
  vi.mocked(useRevokeApiKey).mockReturnValue(mockMutationResult({}));
});

/**
 * Attaches this file's exported `Route` singleton to a throwaway root route --
 * the same post-hoc wiring `routeTree.gen.ts` performs, collapsed to one path
 * segment so the real `_authed` session guard never runs. Identical to
 * `buildMembersRouter` in `-members.owner-handover.test.tsx:107`.
 */
function buildApiKeysRouter() {
  const rootRoute = createRootRoute({});
  type UpdateOptions = Parameters<typeof ApiKeysRoute.update>[0];
  const apiKeysRoute = ApiKeysRoute.update({
    id: "/settings/api-keys",
    path: "/settings/api-keys",
    getParentRoute: () => rootRoute,
  } as unknown as UpdateOptions);
  const routeTree = rootRoute.addChildren([apiKeysRoute]);
  return createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: ["/settings/api-keys"] }),
  });
}

async function renderAs(role: "owner" | "admin") {
  const queryClient = createTestQueryClient();
  queryClient.setQueryData(ME_KEY, meWithRole(role));
  const router = buildApiKeysRouter();
  renderWithProviders(<RouterProvider router={router} />, { queryClient });
  // RouterProvider's first paint is a microtask, so the first lookup awaits.
  const select = await screen.findByRole("combobox", { name: "Role" });
  return { queryClient, select };
}

function roleOptions(select: HTMLElement): string[] {
  return [...select.querySelectorAll("option")].map((o) => o.value);
}

describe("api keys page -- the Role select is gated on the viewer's own role", () => {
  it("an OWNER is offered Owner", async () => {
    const { select } = await renderAs("owner");
    expect(roleOptions(select)).toEqual([
      "owner",
      "admin",
      "operator",
      "viewer",
    ]);
  });

  it("an ADMIN is not offered Owner (canManage is true for an admin, so it is the wrong gate)", async () => {
    const { select } = await renderAs("admin");
    expect(roleOptions(select)).toEqual(["admin", "operator", "viewer"]);
  });
});

describe("api keys page -- the viewer's role drops while the form is open", () => {
  // Reachable without any devtools: the OrgSwitcher lives in the AppShell top
  // bar (_authed.tsx:70 -> top-bar.tsx:34), and useActivateOrg's onSuccess
  // resets the query cache and calls router.invalidate() without unmounting a
  // settings route -- the /sites/ bail-out in redirectIfSiteRouteWentStale
  // does not cover /settings/*. Owner in org A, admin in org B, Owner already
  // picked: the option vanishes, the native select falls back to its first
  // option ("Admin"), and the form still holds "owner".
  it("Create key still submits, with a role the new ceiling allows", async () => {
    const { queryClient, select } = await renderAs("owner");
    fireEvent.change(select, { target: { value: "owner" } });
    fireEvent.change(screen.getByLabelText("Key name"), {
      target: { value: "ci" },
    });

    act(() => {
      queryClient.setQueryData(ME_KEY, meWithRole("admin"));
    });

    await waitFor(() =>
      expect(roleOptions(select)).toEqual(["admin", "operator", "viewer"]),
    );
    fireEvent.click(screen.getByRole("button", { name: "Create key" }));

    await waitFor(() => expect(mutateAsync).toHaveBeenCalledTimes(1));
    const sent = mutateAsync.mock.calls[0]?.[0] as { role?: string };
    expect(sent.role).not.toBe("owner");
    // The control the user is looking at agrees with what was sent.
    expect((select as HTMLSelectElement).value).toBe(sent.role);
  });

  it("a role the ceiling refuses surfaces a visible error instead of a dead button", async () => {
    // Second line of defence, reached the way the schema's own comment names:
    // an out-of-ceiling value put into the select from outside the render.
    const { select } = await renderAs("admin");
    const smuggled = document.createElement("option");
    smuggled.value = "owner";
    smuggled.textContent = "Owner";
    select.appendChild(smuggled);
    fireEvent.change(select, { target: { value: "owner" } });
    fireEvent.change(screen.getByLabelText("Key name"), {
      target: { value: "ci" },
    });

    fireEvent.click(screen.getByRole("button", { name: "Create key" }));

    expect(
      await screen.findByText(/only an owner can create an owner key/i),
    ).toBeInTheDocument();
    expect(select).toHaveAttribute("aria-invalid", "true");
    expect(mutateAsync).not.toHaveBeenCalled();
  });
});
