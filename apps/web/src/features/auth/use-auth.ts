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

/** Raised when login is rejected because the email is not yet verified. */
export class EmailNotVerifiedError extends Error {
  constructor() {
    super("email_not_verified");
    this.name = "EmailNotVerifiedError";
  }
}

/**
 * Result of a login attempt. Either the session is fully established (me
 * present) or the server requires a second factor (challenge present).
 */
export type LoginResult =
  | { kind: "ok"; me: Me }
  | {
      kind: "2fa_required";
      challenge: string;
      factors: { totp: boolean; webauthn: boolean; recovery: boolean };
    };

export function useLogin(): UseMutationResult<LoginResult, Error, LoginRequest> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: LoginRequest): Promise<LoginResult> => {
      const { data, error, response } = await login({ body });
      if (response?.status === 401) {
        throw new UnauthorizedError("Invalid email or password");
      }
      if (response?.status === 403) {
        // Try to read the body code; if enumeration-safe backend always returns
        // the same 403, check for the email_not_verified code.
        const raw = error as unknown as Record<string, unknown> | null | undefined;
        const code = raw && typeof raw["code"] === "string" ? raw["code"] : "";
        if (code === "email_not_verified" || response.status === 403) {
          throw new EmailNotVerifiedError();
        }
      }
      // 202 = 2FA challenge required. The SDK treats 202 as a non-error
      // response and puts the body in `data`.
      if (response?.status === 202) {
        const raw = data as unknown as {
          two_factor_required: boolean;
          challenge: string;
          factors: { totp: boolean; webauthn: boolean; recovery: boolean };
        };
        if (raw?.two_factor_required) {
          return {
            kind: "2fa_required",
            challenge: raw.challenge,
            factors: raw.factors,
          };
        }
      }
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      return { kind: "ok", me: data };
    },
    onSuccess: (result) => {
      if (result.kind === "ok") {
        // Seed the cache so the guard/header see the user immediately.
        queryClient.setQueryData(authKeys.me, result.me);
      }
      // For 2fa_required we do NOT seed the cache — the session is not yet
      // established. The caller will navigate to the challenge page.
    },
  });
}

/**
 * Result shape for useRegister — covers both the first-account path (session
 * established immediately) and the normal self-serve path (pending email
 * verification, no session yet).
 */
export interface RegisterResult {
  /** Present when the backend established a session (first account only). */
  me?: Me;
  /** True when the account was created but email verification is required. */
  pending: boolean;
}

export function useRegister(): UseMutationResult<RegisterResult, Error, RegisterRequest> {
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

      // Normal (non-first) self-serve signup: backend returns { ok: true, pending: true }
      // without establishing a session.
      const raw = data as unknown as Record<string, unknown> | null | undefined;
      if (!raw || (raw["pending"] === true) || !("user" in raw && raw["user"])) {
        return { pending: true };
      }

      // First-account path: backend returns the Me object + sets a session cookie.
      return { me: data as unknown as Me, pending: false };
    },
    onSuccess: (result) => {
      if (result.me) {
        queryClient.setQueryData(authKeys.me, result.me);
      }
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

// ---------------------------------------------------------------------------
// Password-reset mutations — unauthenticated, hand-rolled (not in generated SDK).
// ---------------------------------------------------------------------------

/**
 * POST /auth/password/forgot
 *
 * Always returns 200 { ok: true } regardless of whether the address exists
 * (prevents email enumeration). The caller should show a neutral confirmation
 * without revealing whether the account was found.
 */
export function useForgotPassword(): UseMutationResult<void, Error, { email: string }> {
  return useMutation({
    mutationFn: async (body) => {
      const result = await client.post({
        url: "/auth/password/forgot",
        body,
        headers: { "Content-Type": "application/json" },
      });
      if (result.error !== undefined) throw toError(result.error);
    },
  });
}

/**
 * The result shape surfaced to the caller so the page can branch on HTTP status
 * without throwing. We surface the status rather than swallowing it into a
 * catch block because 400 / 410 / 429 are all non-exceptional outcomes from the
 * user's perspective — they need different UI messages, not a crash.
 */
export interface ResetPasswordResult {
  /** HTTP status from the backend: 200 success | 400 bad token | 410 expired | 429 rate-limited. */
  status: 200 | 400 | 410 | 429;
}

/**
 * POST /auth/password/reset { token, password }
 *
 * Returns a `ResetPasswordResult` with the HTTP status rather than throwing on
 * non-2xx (400/410/429) so the page can branch cleanly.
 */
export function useResetPassword(): UseMutationResult<
  ResetPasswordResult,
  Error,
  { token: string; password: string }
> {
  return useMutation({
    mutationFn: async (body) => {
      const result = await client.post({
        url: "/auth/password/reset",
        body,
        headers: { "Content-Type": "application/json" },
      });

      const status = result.response?.status;

      if (status === 400 || status === 410 || status === 429) {
        return { status };
      }

      if (result.error !== undefined) throw toError(result.error);

      return { status: 200 };
    },
  });
}

// ---------------------------------------------------------------------------
// Email-verification mutations — unauthenticated, hand-rolled.
// ---------------------------------------------------------------------------

/**
 * The result shape for useVerifyEmail so the page can branch on HTTP status
 * without throwing. 200 = verified + session established; 410 = invalid/expired
 * token; 429 = rate-limited.
 */
export interface VerifyEmailResult {
  status: 200 | 410 | 429;
  /** Present only when status === 200 — the verified user, now logged in. */
  me?: Me;
}

/**
 * POST /auth/verify-email { token }
 *
 * On 200 the backend returns the Me object and sets a session cookie.
 * On 410 the token is invalid or expired.
 * On 429 too many verification attempts.
 * Returns a VerifyEmailResult rather than throwing on non-2xx so the page can
 * branch cleanly (same pattern as useResetPassword).
 */
export function useVerifyEmail(): UseMutationResult<
  VerifyEmailResult,
  Error,
  { token: string }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body) => {
      const result = await client.post({
        url: "/auth/verify-email",
        body,
        headers: { "Content-Type": "application/json" },
      });

      const status = result.response?.status;

      if (status === 410 || status === 429) {
        return { status };
      }

      if (result.error !== undefined) throw toError(result.error);

      const me = result.data as Me;
      return { status: 200, me };
    },
    onSuccess: (result) => {
      if (result.status === 200 && result.me) {
        queryClient.setQueryData(authKeys.me, result.me);
      }
    },
  });
}

/**
 * POST /auth/verification/resend { email }
 *
 * Always returns 200 { ok: true } regardless of whether the email exists
 * (enumeration-safe). The page shows a neutral confirmation.
 */
export function useResendVerification(): UseMutationResult<void, Error, { email: string }> {
  return useMutation({
    mutationFn: async (body) => {
      const result = await client.post({
        url: "/auth/verification/resend",
        body,
        headers: { "Content-Type": "application/json" },
      });
      // Always 200 — ignore errors to stay enumeration-safe.
      if (result.error !== undefined && result.response?.status !== 200) {
        throw toError(result.error);
      }
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
 * Whether the current user is a superadmin (instance-level).
 * Reads `me.user.is_superadmin`; safe to call before the backend field ships
 * (returns false when the flag is absent).
 */
export function isSuperadmin(me: Me | null | undefined): boolean {
  return me?.user?.is_superadmin === true;
}

/**
 * Paths a superadmin (who has no org) may visit outside the Admin area. The
 * _authed gate keeps superadmins out of the tenant-scoped shell (which would
 * 403 or bounce them to the create-org screen), but their OWN personal account
 * settings — profile and 2FA/security — are per-user, not tenant-scoped, so
 * they must be reachable (otherwise a superadmin can never enable their own
 * 2FA). Everything else outside /admin stays redirected to /admin.
 */
export function isSuperadminAllowedPath(pathname: string): boolean {
  return (
    pathname.startsWith("/admin") ||
    pathname === "/settings/account" ||
    pathname === "/settings/security"
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
