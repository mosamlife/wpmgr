import {
  useQuery,
  useMutation,
  useQueryClient,
  type QueryClient,
  type UseQueryResult,
  type UseMutationResult,
} from "@tanstack/react-query";
import {
  client,
  getMe,
  login,
  logout,
  register,
  type Me,
  type LoginRequest,
  type RegisterRequest,
  type ApiError,
} from "@wpmgr/api";

import { toast } from "@/components/toast";

// Auth/session is SERVER state, so it lives in TanStack Query (not Zustand) per
// the ADRs. The single source of truth is `GET /auth/me`, backed by the
// HttpOnly `wpmgr_session` cookie that the generated client sends automatically
// (credentials: "include"). A 401 means "not authenticated".

export const authKeys = {
  me: ["auth", "me"] as const,
};

/** Raised when an auth endpoint returns 401 (no/expired session). */
export class UnauthorizedError extends Error {
  constructor(message = "Not authenticated") {
    super(message);
    this.name = "UnauthorizedError";
  }
}

/**
 * Fetch the current user. Returns `null` on 401 (unauthenticated) instead of
 * throwing, so callers can branch on presence. Other transport/HTTP errors
 * still throw.
 */
async function fetchMe(): Promise<Me | null> {
  const { data, error, response } = await getMe();
  if (response?.status === 401) return null;
  if (error) throw toError(error);
  return data ?? null;
}

/**
 * Loader-friendly variant used by the `_authed` route guard. Reads from (or
 * populates) the query cache so we don't refetch on every navigation, and
 * returns `null` when unauthenticated.
 */
export async function ensureMe(queryClient: QueryClient): Promise<Me | null> {
  return queryClient.ensureQueryData({
    queryKey: authKeys.me,
    queryFn: fetchMe,
  });
}

export function useMe(): UseQueryResult<Me | null, Error> {
  return useQuery({
    queryKey: authKeys.me,
    queryFn: fetchMe,
    staleTime: 5 * 60_000,
    retry: false,
  });
}

export function useLogin(): UseMutationResult<Me, Error, LoginRequest> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: LoginRequest) => {
      const { data, error, response } = await login({ body });
      if (response?.status === 401) {
        throw new UnauthorizedError("Invalid email or password");
      }
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
    onSuccess: (me) => {
      // Seed the cache so the guard/header see the user immediately.
      queryClient.setQueryData(authKeys.me, me);
    },
  });
}

export function useRegister(): UseMutationResult<Me, Error, RegisterRequest> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: RegisterRequest) => {
      const { data, error, response } = await register({ body });
      if (response?.status === 403) {
        throw new Error("Open registration is closed: ask an admin to invite you.");
      }
      if (response?.status === 409) {
        throw new Error("An account with that email already exists.");
      }
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
    onSuccess: (me) => {
      queryClient.setQueryData(authKeys.me, me);
    },
  });
}

export function useLogout(): UseMutationResult<void, Error, void> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async () => {
      const { error, response } = await logout();
      // 401 just means we were already logged out — treat as success.
      if (error && response?.status !== 401) throw toError(error);
    },
    onSuccess: () => {
      // Drop ALL server state on logout (sites, members, keys, me, ...).
      queryClient.clear();
    },
  });
}

// ---------------------------------------------------------------------------
// Account mutations — hand-rolled Gin endpoints (not in the generated SDK).
// Both call through the configured Hey API client (credentials: "include").
// ---------------------------------------------------------------------------

/** PATCH /auth/me — update the display name. */
export function useUpdateProfile(): UseMutationResult<
  Me,
  Error,
  { name: string }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body) => {
      const result = await client.patch({
        url: "/auth/me",
        body,
        headers: { "Content-Type": "application/json" },
      });
      if (result.error !== undefined) throw toError(result.error);
      return result.data as Me;
    },
    onSuccess: (updated) => {
      queryClient.setQueryData(authKeys.me, updated);
      toast.success("Profile saved");
    },
  });
}

/** POST /auth/me/password — change the account password. */
export function useChangePassword(): UseMutationResult<
  void,
  Error,
  { current_password: string; new_password: string }
> {
  return useMutation({
    mutationFn: async (body) => {
      const result = await client.post({
        url: "/auth/me/password",
        body,
        headers: { "Content-Type": "application/json" },
      });
      if (result.error !== undefined) throw toError(result.error);
    },
    onSuccess: () => {
      toast.success("Password updated");
    },
  });
}

/** Normalize the generated `Error` body (or anything) into an Error instance. */
export function toError(error: unknown): Error {
  if (error instanceof Error) return error;
  if (isApiError(error)) return new Error(error.message);
  return new Error("Request failed");
}

function isApiError(value: unknown): value is ApiError {
  return (
    typeof value === "object" &&
    value !== null &&
    "message" in value &&
    typeof value.message === "string"
  );
}

/**
 * Active role of the user in their active tenant.
 *
 * FIXED (M5.7): no longer falls back to memberships[0] — that was unsafe
 * because the first membership may belong to a different org than the one the
 * session has active. We now require `active_tenant_id` to be set and match
 * a membership; if there is no matching membership (site-scoped collaborator)
 * we return null so `canManage`/`canOperate` correctly gate org-level controls.
 */
export function activeRole(
  me: Me | null | undefined,
): Me["memberships"][number]["role"] | null {
  if (!me) return null;
  if (!me.active_tenant_id) return null;
  const membership = me.memberships.find(
    (m) => m.tenant_id === me.active_tenant_id,
  );
  return membership?.role ?? null;
}

/**
 * Whether the active principal has a full org membership (as opposed to a
 * site-scoped collaborator who accessed the org via a share).
 *
 * An org-scoped member has their active_tenant_id present in me.memberships.
 * A site-scoped collaborator does not — their active org came from a share,
 * not a membership.
 */
export function isOrgScoped(me: Me | null | undefined): boolean {
  if (!me?.active_tenant_id) return false;
  return me.memberships.some((m) => m.tenant_id === me.active_tenant_id);
}

/** Whether the user may manage API keys / members (owner or admin). */
export function canManage(me: Me | null | undefined): boolean {
  // Site-scoped collaborators never have org management permissions.
  if (!isOrgScoped(me)) return false;
  const role = activeRole(me);
  return role === "owner" || role === "admin";
}

/**
 * Whether the user may perform operator-level actions such as generating site
 * pairing codes (owner, admin, or operator). The backend enforces this too.
 */
export function canOperate(me: Me | null | undefined): boolean {
  const role = activeRole(me);
  // Org-scoped: any of owner/admin/operator. Site-scoped: any share role
  // except viewer is operator-capable (server enforces per-site).
  return role === "owner" || role === "admin" || role === "operator";
}
