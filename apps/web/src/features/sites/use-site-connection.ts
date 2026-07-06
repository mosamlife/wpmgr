import {
  useMutation,
  useQueryClient,
  type UseMutationResult,
} from "@tanstack/react-query";
import { client } from "@wpmgr/api";

import { extractSiteLimitReached } from "@/lib/api";
import { sitesKeys } from "./use-sites";
import type { ConnectionState } from "./connection-state";

// Phase 5 — connection-lifecycle mutations.
//
// These endpoints shipped in Phase 3/4 but are NOT in the generated @wpmgr/api
// SDK yet (the generator hasn't been re-run against the new spec). Rather than
// regenerate the whole client and churn unrelated types, we call the shared
// `client` directly with explicit `url`s — the same `client` the generated SDK
// uses, so credentials/baseUrl/interceptors all apply identically.
//
//   POST /api/v1/sites                       → { site_id, enrollment_code, expires_at }
//   POST /api/v1/sites/:id/enrollment-codes  → { enrollment_code, expires_at }
//   POST /api/v1/sites/:id/revoke   { reason? }
//   POST /api/v1/sites/:id/archive  { reason? }
//   POST /api/v1/sites/:id/restore
//   POST /api/v1/sites/:id/cancel   → 204 (hard-delete pending site)

/**
 * Structured error thrown when POST /api/v1/sites returns 409 with
 * code:"site_url_exists". The details carry the existing site_id and its
 * current connection_state so the caller can offer a targeted affordance.
 */
export class SiteUrlExistsError extends Error {
  readonly code = "site_url_exists" as const;
  readonly siteId: string;
  readonly connectionState: ConnectionState;

  constructor(siteId: string, connectionState: ConnectionState) {
    super("A site with this URL already exists.");
    this.name = "SiteUrlExistsError";
    this.siteId = siteId;
    this.connectionState = connectionState;
  }
}

/** Result of creating a site (site-first enrollment flow). */
export interface CreateSiteResult {
  site_id: string;
  enrollment_code: string;
  expires_at: string;
}

/** A freshly-minted enrollment code (re-enroll / reconnect). */
export interface EnrollmentCode {
  enrollment_code: string;
  expires_at: string;
}

export interface CreateSiteInput {
  url: string;
  name?: string;
  group_id?: string;
  tags?: string[];
}

/** Normalize the raw client error (typed body or transport) into an Error. */
function toError(error: unknown, fallback: string): Error {
  if (error instanceof Error) return error;
  if (
    typeof error === "object" &&
    error !== null &&
    "message" in error &&
    typeof error.message === "string"
  ) {
    return new Error(error.message);
  }
  return new Error(fallback);
}

/**
 * Type guard: is this value the shape of an API error body?
 * The generated `ApiError` type has `code: string`, `message: string`, and
 * `details?: { [key: string]: unknown }`. We check code+message to narrow.
 */
function isApiErrorShape(
  value: unknown,
): value is { code: string; message: string; details?: Record<string, unknown> } {
  return (
    typeof value === "object" &&
    value !== null &&
    "code" in value &&
    typeof (value as Record<string, unknown>).code === "string" &&
    "message" in value &&
    typeof (value as Record<string, unknown>).message === "string"
  );
}

/**
 * Create a site (site-first flow). Returns the new site_id + the one-time
 * enrollment code the operator pastes into the agent. We invalidate the sites
 * lists so the new (pending_enrollment) row appears; the SSE `site.created`
 * event also triggers an invalidate, but doing it here too closes the gap if
 * the stream is momentarily reconnecting.
 *
 * On a URL collision the CP returns 409 with code:"site_url_exists" and
 * details:{site_id, connection_state}. We surface this as a typed
 * `SiteUrlExistsError` so the UrlStep can render a targeted reconnect
 * affordance instead of the raw message.
 */
export function useCreateSiteFirst(): UseMutationResult<
  CreateSiteResult,
  Error,
  CreateSiteInput
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: CreateSiteInput) => {
      const { data, error, response } = await client.post<{ 200: CreateSiteResult }>({
        url: "/api/v1/sites",
        body: input,
      });
      // 402 site_limit_reached (M16 Phase B): the tenant is at its hosted-
      // billing site cap. Run through the shared interceptor so the Add Site
      // dialog can swap to the consistent UpgradePrompt instead of a raw
      // error string.
      const limitReached = extractSiteLimitReached(response?.status, error);
      if (limitReached) throw limitReached;
      if (response?.status === 409 && isApiErrorShape(error)) {
        if (
          error.code === "site_url_exists" &&
          typeof error.details?.site_id === "string" &&
          typeof error.details?.connection_state === "string"
        ) {
          throw new SiteUrlExistsError(
            error.details.site_id,
            error.details.connection_state as ConnectionState,
          );
        }
      }
      if (error) throw toError(error, "Could not create the site");
      if (!data) throw new Error("Empty response");
      return data;
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: sitesKeys.lists() });
    },
  });
}

/**
 * Mint a fresh enrollment code for an existing site (re-enroll / reconnect /
 * code-expired). Does NOT change cardinality, so no list invalidation needed.
 */
export function useCreateEnrollmentCode(): UseMutationResult<
  EnrollmentCode,
  Error,
  { siteId: string }
> {
  return useMutation({
    mutationFn: async ({ siteId }) => {
      const { data, error } = await client.post<{ 200: EnrollmentCode }>({
        url: `/api/v1/sites/${encodeURIComponent(siteId)}/enrollment-codes`,
      });
      if (error) throw toError(error, "Could not generate an enrollment code");
      if (!data) throw new Error("Empty response");
      return data;
    },
  });
}

/**
 * Revoke a site (disconnect). The CP pushes the revoke to the agent on its next
 * heartbeat (≤60s) and moves the site to a revoked/archived state. The live row
 * update arrives over SSE; we still invalidate the detail so a focused detail
 * page reflects the change immediately even if the stream is reconnecting.
 */
export function useRevokeSite(): UseMutationResult<
  void,
  Error,
  { siteId: string; reason?: string }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ siteId, reason }) => {
      const { error } = await client.post({
        url: `/api/v1/sites/${encodeURIComponent(siteId)}/revoke`,
        body: reason ? { reason } : {},
      });
      if (error) throw toError(error, "Could not disconnect the site");
    },
    onSuccess: (_data, { siteId }) => {
      void queryClient.invalidateQueries({ queryKey: sitesKeys.detail(siteId) });
    },
  });
}

/**
 * Cancel enrollment for a never-connected (pending) site. The CP hard-deletes
 * the site record (only valid for `pending_enrollment` state; returns
 * 409 not_cancellable otherwise). On success, invalidate sites lists so the
 * row disappears immediately.
 */
export function useCancelEnrollment(): UseMutationResult<
  void,
  Error,
  { siteId: string }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ siteId }) => {
      const { error, response } = await client.post({
        url: `/api/v1/sites/${encodeURIComponent(siteId)}/cancel`,
      });
      // 404 means the site is already gone — treat as success.
      if (response?.status === 404) return;
      if (error) throw toError(error, "Could not cancel enrollment");
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: sitesKeys.lists() });
    },
  });
}

/** Archive a disconnected/revoked site (hidden from the default list). */
export function useArchiveSite(): UseMutationResult<
  void,
  Error,
  { siteId: string; reason?: string }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ siteId, reason }) => {
      const { error } = await client.post({
        url: `/api/v1/sites/${encodeURIComponent(siteId)}/archive`,
        body: reason ? { reason } : {},
      });
      if (error) throw toError(error, "Could not archive the site");
    },
    onSuccess: (_data, { siteId }) => {
      void queryClient.invalidateQueries({ queryKey: sitesKeys.lists() });
      void queryClient.invalidateQueries({ queryKey: sitesKeys.detail(siteId) });
    },
  });
}

/** Restore a revoked/archived site (the 60s Undo on disconnect, and reconnect). */
export function useRestoreSite(): UseMutationResult<
  void,
  Error,
  { siteId: string }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ siteId }) => {
      const { error } = await client.post({
        url: `/api/v1/sites/${encodeURIComponent(siteId)}/restore`,
      });
      if (error) throw toError(error, "Could not restore the site");
    },
    onSuccess: (_data, { siteId }) => {
      void queryClient.invalidateQueries({ queryKey: sitesKeys.lists() });
      void queryClient.invalidateQueries({ queryKey: sitesKeys.detail(siteId) });
    },
  });
}

/**
 * The shape returned by POST /api/v1/sites/:id/recheck on a 200 OK.
 * The backend refreshes liveness from the agent and echoes back the
 * updated connection snapshot.
 */
export interface RecheckResult {
  connection_state: string;
  last_seen_at?: string | null;
}

/**
 * Named error for the "agent didn't respond" 502 path. Distinct from a
 * generic network error so callers can show a calm, non-alarming message
 * instead of a generic failure toast.
 */
export class AgentUnreachableError extends Error {
  readonly code = "agent_unreachable" as const;
  constructor() {
    super("Agent didn't respond — it may just be quiet; the monitor will keep checking.");
    this.name = "AgentUnreachableError";
  }
}

/**
 * Force an immediate liveness refresh for a connected/degraded site.
 *
 * POST /api/v1/sites/:id/recheck
 *   200 → { connection_state, last_seen_at, ... } — agent answered; the CP
 *         also pushes a `site.state_changed` SSE frame which will patch the
 *         badge via the shared tenant bus. We ALSO invalidate the detail +
 *         list keys here so a focused detail page updates even if the SSE
 *         stream is momentarily reconnecting.
 *   502 { code: "agent_unreachable" } — the agent didn't respond within the
 *         probe window. This is NOT a fatal error: heartbeat-late sites on
 *         low-traffic servers are common. We surface a calm informational
 *         toast and leave the badge unchanged (we do NOT flip it to
 *         disconnected — that is the CP's responsibility after its own
 *         threshold elapses).
 *
 * Any other non-2xx (network error, 5xx) is re-thrown so Query surfaces it
 * in the normal mutation-error channel.
 */
export function useRecheckConnection(): UseMutationResult<
  RecheckResult,
  Error,
  { siteId: string }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ siteId }) => {
      const { data, error, response } = await client.post<{
        200: RecheckResult;
      }>({
        url: `/api/v1/sites/${encodeURIComponent(siteId)}/recheck`,
      });
      // 502 with agent_unreachable is an expected operational path, NOT a
      // crash. Surface it as a typed error so the onError handler can
      // distinguish it from a genuine transport failure.
      if (response?.status === 502 && isApiErrorShape(error) && error.code === "agent_unreachable") {
        throw new AgentUnreachableError();
      }
      if (error) throw toError(error, "Could not reach the agent");
      if (!data) throw new Error("Empty response from recheck");
      return data;
    },
    onSuccess: (_data, { siteId }) => {
      // Reconcile both the detail and the list so the badge updates on
      // whichever surface the operator is looking at. The SSE `site.state_changed`
      // frame will also patch the cache, but the invalidation closes the gap
      // when the stream is in its reconnect window.
      void queryClient.invalidateQueries({ queryKey: sitesKeys.detail(siteId) });
      void queryClient.invalidateQueries({ queryKey: sitesKeys.lists() });
    },
    // onError is intentionally left to the call site: the AgentUnreachableError
    // needs a non-destructive toast while a genuine error gets the standard
    // destructive treatment. See use in $siteId.tsx and sites-table.tsx.
  });
}
