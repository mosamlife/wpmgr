import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { getEffectiveSiteContext, type GovContextEffective } from "@wpmgr/api";

import { toError } from "@/features/auth/use-auth";

// ADR-064 (governed org/site context) — S5 Stage A.
//
// This module currently covers ONLY the effective-context preview (Decision
// 8, Screen 1). Stage B adds the org/site context read+edit hooks, version
// history, diff and restore (Decision 5/13) alongside this file — grow the
// `contextKeys` factory below (e.g. `contextKeys.org(orgId)`,
// `contextKeys.siteVersions(siteId)`) rather than starting a second one.

export const contextKeys = {
  all: ["gov-context"] as const,
  effective: (siteId: string) => [...contextKeys.all, "effective", siteId] as const,
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
