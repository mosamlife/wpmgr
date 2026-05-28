import {
  useMutation,
  type UseMutationResult,
} from "@tanstack/react-query";

import { client, type Me } from "@wpmgr/api";
import { activeRole } from "@/features/auth/use-auth";

// Phase 5.5 — One-Click Login.
//
// The backend endpoint POST /api/v1/sites/{siteId}/autologin issues a short-
// lived, single-use redirect URL into the WordPress admin (handled by the
// agent plugin) via a signed JWT. The Hey-API generated SDK does NOT yet
// expose this operation because the canonical openapi.yaml (owned by the
// backend, in packages/openapi/) has not landed the autologin paths yet —
// task #26 in the backend backlog. Once it ships there, regenerate the SDK
// and replace `client.post(...)` below with the generated operation
// (`createAutoLogin` or similar). The shape used here mirrors the contract
// in PHASE-5.5.md so the swap is mechanical.
//
// Until then we hand-roll a typed call through the same runtime fetch client
// the generated SDK uses, so cookies, baseUrl, and interceptor config stay
// uniform.

/** Backend request body. `target_wp_user_login` defaults server-side to the
 *  first administrator; `redirect_to` defaults to `/wp-admin/`. */
export interface AutoLoginRequest {
  /** Optional WordPress user_login to log in as (admins/owners only). */
  target_wp_user_login?: string;
  /** Optional path on the WP site to land on (e.g. `/wp-admin/plugins.php`). */
  redirect_to?: string;
}

/** Successful 200 response. */
export interface AutoLoginResponse {
  /** Cross-origin URL on the WordPress site — open in a NEW TAB. */
  redirect_url: string;
  /** Single-use, short-lived: when the JWT inside expires (ISO-8601). */
  expires_at: string;
}

/** Mutation input bundles the path param with the body for convenience. */
export interface AutoLoginInput extends AutoLoginRequest {
  siteId: string;
}

/** Subset of well-known error codes the backend may return for autologin. */
export type AutoLoginErrorCode =
  | "rbac_denied"
  | "policy_disabled"
  | "2fa_required"
  | "rate_limited"
  | "validation_failed"
  | "not_found";

/**
 * Typed error raised by the hook so callers can branch on `code` and map to
 * UX strings. `message` carries the server's human-readable message (or our
 * fallback) and `retryAfterSeconds` is set when `code === "rate_limited"`.
 */
export class AutoLoginError extends Error {
  readonly code: AutoLoginErrorCode | "network" | "unknown";
  readonly status: number;
  readonly retryAfterSeconds: number | null;

  constructor(opts: {
    code: AutoLoginError["code"];
    message: string;
    status: number;
    retryAfterSeconds?: number | null;
  }) {
    super(opts.message);
    this.name = "AutoLoginError";
    this.code = opts.code;
    this.status = opts.status;
    this.retryAfterSeconds = opts.retryAfterSeconds ?? null;
  }
}

/** Raw shape of the backend error body for autologin. */
interface AutoLoginErrorBody {
  code?: string;
  message?: string;
  retry_after_seconds?: number;
}

function isErrorBody(value: unknown): value is AutoLoginErrorBody {
  return typeof value === "object" && value !== null;
}

function parseError(
  status: number,
  body: unknown,
): AutoLoginError {
  const b: AutoLoginErrorBody = isErrorBody(body) ? body : {};
  const code = (b.code ?? "unknown") as AutoLoginError["code"];
  const message = b.message ?? defaultMessageFor(code);
  return new AutoLoginError({
    code,
    message,
    status,
    retryAfterSeconds:
      typeof b.retry_after_seconds === "number" ? b.retry_after_seconds : null,
  });
}

function defaultMessageFor(code: string): string {
  switch (code) {
    case "rbac_denied":
      return "You don't have permission to log into this site.";
    case "policy_disabled":
      return "Auto-login is disabled for this site. Enable it in site settings.";
    case "2fa_required":
      return "Re-verify 2FA to continue.";
    case "rate_limited":
      return "Too many attempts. Please try again shortly.";
    case "validation_failed":
      return "The request was invalid.";
    case "not_found":
      return "Site not found.";
    default:
      return "Auto-login failed.";
  }
}

/** Friendly toast text for a given AutoLoginError. */
export function autoLoginErrorMessage(err: AutoLoginError): string {
  switch (err.code) {
    case "rbac_denied":
      return "You don't have permission to log into this site.";
    case "policy_disabled":
      return "Auto-login is disabled for this site. Enable it in site settings.";
    case "2fa_required":
      // V0: backend cannot actually emit this (2FA isn't built); handle just in case.
      return "Re-verify 2FA to continue.";
    case "rate_limited":
      return err.retryAfterSeconds
        ? `Too many attempts. Try again in ${err.retryAfterSeconds}s.`
        : "Too many attempts. Try again shortly.";
    case "network":
      return "Network error reaching the control plane.";
    default:
      return `Auto-login failed: ${err.message}`;
  }
}

/**
 * TanStack Query mutation hook. Resolves with `{redirect_url, expires_at}`
 * on success — the caller is responsible for opening the URL in a new tab
 * (we keep window.open out of the hook for testability).
 */
export function useAutoLogin(): UseMutationResult<
  AutoLoginResponse,
  AutoLoginError,
  AutoLoginInput
> {
  return useMutation({
    mutationFn: async ({ siteId, ...body }) => {
      // The generated client uses `{}` for an empty body; only include the
      // fields the caller provided so the backend gets a tidy payload.
      const payload: AutoLoginRequest = {};
      if (body.target_wp_user_login)
        payload.target_wp_user_login = body.target_wp_user_login;
      if (body.redirect_to) payload.redirect_to = body.redirect_to;

      // The Hey-API runtime indexes into the TData/TError generics with
      // `keyof T`, so we shape them as `{ [statusCode]: ... }` to match what
      // the generated SDK produces. The runtime returns `data` and `error`
      // already narrowed to the matching status' body.
      const result = await client.post<
        { 200: AutoLoginResponse },
        {
          400: AutoLoginErrorBody;
          403: AutoLoginErrorBody;
          409: AutoLoginErrorBody;
          422: AutoLoginErrorBody;
          429: AutoLoginErrorBody;
        }
      >({
        url: `/api/v1/sites/${encodeURIComponent(siteId)}/autologin`,
        body: payload,
        headers: { "Content-Type": "application/json" },
      });

      // Hey-API runtime returns { data, error, response } (no throw by default).
      const { data, error, response } = result;
      if (error || !data) {
        const status = response?.status ?? 0;
        if (status === 0) {
          throw new AutoLoginError({
            code: "network",
            message: "Network error",
            status: 0,
          });
        }
        throw parseError(status, error);
      }
      return data;
    },
    retry: false,
  });
}

/**
 * Whether the current user may use one-click auto-login. Mirrors the backend
 * default `PermSiteAutologin = admin+` (owner or admin). Defense-in-depth:
 * the backend re-checks the role on every call, regardless of the UI gate.
 */
export function canAutoLogin(me: Me | null | undefined): boolean {
  const role = activeRole(me);
  return role === "owner" || role === "admin";
}
