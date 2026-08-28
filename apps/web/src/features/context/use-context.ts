import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from "@tanstack/react-query";
import {
  getEffectiveSiteContext,
  getOrgContext as apiGetOrgContext,
  patchOrgContext as apiPatchOrgContext,
  getSiteContext as apiGetSiteContext,
  patchSiteContext as apiPatchSiteContext,
  type ApiError,
  type GovContext,
  type GovContextEffective,
  type PatchGovContextRequest,
} from "@wpmgr/api";

import { toError } from "@/features/auth/use-auth";

// ADR-064 (governed org/site context) — S5.
//
// Stage A covered ONLY the effective-context preview (Decision 8, Screen 1).
// Stage B adds the org/site context read+write hooks below (Decision
// 6/13) — org and site editors, and the three write-path refusals Decision
// 4/10/13 define. Version history, diff and restore (Decision 5) are not yet
// built; grow `contextKeys` further (e.g. `contextKeys.orgVersions(orgId)`)
// rather than starting a second key factory when that lands.

export const contextKeys = {
  all: ["gov-context"] as const,
  effective: (siteId: string) => [...contextKeys.all, "effective", siteId] as const,
  org: (orgId: string) => [...contextKeys.all, "org", orgId] as const,
  site: (siteId: string) => [...contextKeys.all, "site", siteId] as const,
};

/**
 * Raised when `GET .../context/effective` returns `503 context_unavailable`
 * (ADR-064 Decision 14): resolution could not complete, so the call is
 * refused rather than handed an empty, partial, or stale-but-unmarked
 * result. This is a distinct, EXPECTED outcome, never coerced into the
 * generic error path — the UI renders a "could not load" state for it, which
 * must never look like the (different) "site has no context yet" state.
 */
export class ContextUnavailableError extends Error {
  constructor(message = "Context could not be resolved.") {
    super(message);
    this.name = "ContextUnavailableError";
  }
}

async function fetchEffectiveContext(siteId: string): Promise<GovContextEffective> {
  const { data, error, response } = await getEffectiveSiteContext({
    path: { siteId },
  });
  // Branch on 503 BEFORE the generic `if (error)` below, matching this
  // repo's EmailNotVerifiedError convention (features/auth/use-auth.ts) —
  // a named error class, not a generic throw, so the component can tell
  // "could not load" apart from every other failure.
  if (response?.status === 503) {
    throw new ContextUnavailableError(error?.message);
  }
  if (error) throw toError(error);
  if (!data) throw new Error("Empty response");
  return data;
}

/**
 * `GET /api/v1/sites/{siteId}/context/effective` — ADR-064 Decision 8's
 * preview: every surviving layer (1-6, in precedence order; layer 7/learned
 * memory is never present), the read-time union of layers 1-3's
 * restrictions, and the byte accounting from Decision 9.
 *
 * Render exactly what this returns. Do NOT concatenate the layers into a
 * merged prose block in the browser — Decision 8 requires the preview to
 * call the same resolution function the model-facing assembly path calls,
 * never a second implementation of the same walk, and a client-side
 * re-assembly would be exactly that: a second path that can silently drift
 * from the real one.
 */
export function useEffectiveSiteContext(
  siteId: string,
): UseQueryResult<GovContextEffective, Error> {
  return useQuery({
    queryKey: contextKeys.effective(siteId),
    queryFn: () => fetchEffectiveContext(siteId),
  });
}

// ── Org + site context editors (Stage B) ────────────────────────────────
//
// PATCH .../context has exactly three write refusals (ADR-064 Decision 13):
// two `409`s sharing an HTTP status but distinguished by `code`
// (`context_widen_forbidden` vs `context_version_conflict`), and one `422`
// (`context_secret_detected`). Each gets its own error class carrying the
// server's structured `details` so the editor can flag the specific field
// (widen) or offer a reload-and-retry action (conflict) — never a generic
// throw that loses the distinction. `code` strings and `details` keys below
// are quoted from `apps/api/internal/govcontext/{widen,secretscan,service}.go`
// on the S4 branch, not inferred from the OpenAPI shape.

/**
 * `409 context_widen_forbidden` (Decision 4): the proposed restrictions would
 * remove something a higher layer set. `field` is always a RestrictionSet key
 * (`forbidden_tools` | `forbidden_domains` | `forbidden_topics`) — the widen
 * check never runs over guidance (ADR-064 Decision 1: "wider"/"narrower" are
 * not defined relations over free text).
 */
export class ContextWidenForbiddenError extends Error {
  field: string;
  layer: number;
  layerName: string;
  removedItems: string[];
  constructor(
    message: string,
    details: { field: string; layer: number; layerName: string; removedItems: string[] },
  ) {
    super(message);
    this.name = "ContextWidenForbiddenError";
    this.field = details.field;
    this.layer = details.layer;
    this.layerName = details.layerName;
    this.removedItems = details.removedItems;
  }
}

/**
 * `422 context_secret_detected` (Decision 10): the proposed snapshot contains
 * a value shaped like a credential. `category` names the shape
 * (`aws_access_key`, `private_key_block`, `database_connection_string`,
 * `bearer_token`, `api_key_assignment`, `high_entropy_secret`) — the server
 * deliberately never reports WHICH field matched (`DetectSecret` scans the
 * whole snapshot as a flat string list), so there is no field to flag inline,
 * only the banner.
 */
export class ContextSecretDetectedError extends Error {
  category: string;
  constructor(message: string, category: string) {
    super(message);
    this.name = "ContextSecretDetectedError";
    this.category = category;
  }
}

/**
 * `409 context_version_conflict` (ADR-064 open question 2, answered by S4):
 * the request's `base_version` no longer matches the current version — either
 * a stale read (another write landed first) or a lost race on `restore`. The
 * fix is never a client-side merge (ADR-064's Consequences section forecloses
 * merge-based conflict resolution entirely) — only reread and retry.
 */
export class ContextVersionConflictError extends Error {
  currentVersion: number;
  suppliedBaseVersion?: number;
  constructor(message: string, currentVersion: number, suppliedBaseVersion?: number) {
    super(message);
    this.name = "ContextVersionConflictError";
    this.currentVersion = currentVersion;
    this.suppliedBaseVersion = suppliedBaseVersion;
  }
}

function detailString(details: Record<string, unknown> | undefined, key: string): string {
  const v = details?.[key];
  return typeof v === "string" ? v : "";
}

function detailNumber(details: Record<string, unknown> | undefined, key: string): number {
  const v = details?.[key];
  return typeof v === "number" ? v : 0;
}

function detailStringArray(details: Record<string, unknown> | undefined, key: string): string[] {
  const v = details?.[key];
  return Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : [];
}

/**
 * Maps a PATCH .../context error onto the three named refusal classes above
 * by `code` (never by HTTP status alone — both 409s share one status), or
 * falls back to the same generic `toError` every other domain hook uses.
 */
function toPatchContextError(error: ApiError | undefined): Error {
  if (!error) return new Error("Request failed");
  const details = error.details;
  if (error.code === "context_widen_forbidden") {
    return new ContextWidenForbiddenError(error.message, {
      field: detailString(details, "field"),
      layer: detailNumber(details, "layer"),
      layerName: detailString(details, "layer_name"),
      removedItems: detailStringArray(details, "removed_items"),
    });
  }
  if (error.code === "context_version_conflict") {
    return new ContextVersionConflictError(
      error.message,
      detailNumber(details, "current_version"),
      details && "supplied_base_version" in details
        ? detailNumber(details, "supplied_base_version")
        : undefined,
    );
  }
  if (error.code === "context_secret_detected") {
    return new ContextSecretDetectedError(error.message, detailString(details, "category"));
  }
  return toError(error);
}

// --- organisation context (layer 2) -----------------------------------------

/**
 * `GET /api/v1/orgs/{orgId}/context` — the organisation's current context.
 * `version: 0` is a legitimate empty state ("nothing authored yet"), never a
 * 404 (ADR-064 Decision 3/13).
 */
export function useOrgContext(orgId: string): UseQueryResult<GovContext, Error> {
  return useQuery({
    queryKey: contextKeys.org(orgId),
    queryFn: async () => {
      const { data, error } = await apiGetOrgContext({ path: { orgId } });
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
  });
}

/**
 * `PATCH /api/v1/orgs/{orgId}/context` — author a new organisation context
 * version. Always send the FULL current `restrictions`/`guidance` (the server
 * replaces each key wholesale, never deep-merges — `PatchGovContextRequest`'s
 * own doc comment). On success, seeds the query cache with the new version so
 * the form's `base_version` is immediately correct for the next save.
 */
export function usePatchOrgContext(
  orgId: string,
): UseMutationResult<GovContext, Error, PatchGovContextRequest> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: PatchGovContextRequest) => {
      const { data, error } = await apiPatchOrgContext({ path: { orgId }, body });
      if (error) throw toPatchContextError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
    onSuccess: (updated) => {
      queryClient.setQueryData(contextKeys.org(orgId), updated);
      // Layer 2 changed, so every site's resolved effective context under
      // this org may have too (Decision 2: a write to a layer beneath the
      // materialised preview invalidates it immediately). We don't know
      // which sites are open from here, so mark every cached preview stale
      // rather than guess which one(s) to target — cheap, and "pull is the
      // truth" means a mounted preview just refetches on next focus/mount.
      void queryClient.invalidateQueries({ queryKey: [...contextKeys.all, "effective"] });
    },
  });
}

// --- site context (layer 3) -------------------------------------------------

/** `GET /api/v1/sites/{siteId}/context` — the site's current context (layer 3). */
export function useSiteContext(siteId: string): UseQueryResult<GovContext, Error> {
  return useQuery({
    queryKey: contextKeys.site(siteId),
    queryFn: async () => {
      const { data, error } = await apiGetSiteContext({ path: { siteId } });
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
  });
}

/** `PATCH /api/v1/sites/{siteId}/context` — site-scope sibling of {@link usePatchOrgContext}. */
export function usePatchSiteContext(
  siteId: string,
): UseMutationResult<GovContext, Error, PatchGovContextRequest> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: PatchGovContextRequest) => {
      const { data, error } = await apiPatchSiteContext({ path: { siteId }, body });
      if (error) throw toPatchContextError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
    onSuccess: (updated) => {
      queryClient.setQueryData(contextKeys.site(siteId), updated);
      // Layer 3 changed — this site's effective-context preview (Decision 8,
      // Screen 1) is stale (Decision 2). Targeted, since we know the site.
      void queryClient.invalidateQueries({ queryKey: contextKeys.effective(siteId) });
    },
  });
}
