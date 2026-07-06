// App-side wiring for the generated @wpmgr/api client.
//
// The generated client already defaults its baseUrl to "/api" (see
// packages/openapi-client/src/client.config.ts), which Vite proxies to the
// backend (vite.config.ts). We re-assert it here so the intent is explicit and
// lives next to the rest of the data layer. App code imports operations and
// types from "@wpmgr/api" — never from its ./generated internals.
import { client } from "@wpmgr/api";

export function configureApiClient(): void {
  // Empty baseUrl: operation paths already include their real prefixes
  // (/auth/*, /api/v1/*, /enroll, …) and are served from the same origin,
  // routed to the backend by nginx (prod) / the Vite proxy (dev).
  client.setConfig({
    baseUrl: "",
    credentials: "include",
  });
}

export type {
  Site,
  SiteList,
  SiteComponent,
  SiteComponents,
  PairingCode,
  PairingCodeCreate,
  ApiError,
} from "@wpmgr/api";

// ---------------------------------------------------------------------------
// 402 site-limit interceptor (M16 Phase B) — the shared handler every
// site-create mutation error path runs through so the "upgrade" experience is
// identical everywhere a hosted-billing site cap can be hit (Add Site dialog,
// pairing-code creation, restore/re-enroll of an archived site, ...).
//
// Contract (apps/api/internal/billing/service.go CheckSiteCreate): a 402 with
// body { code: "site_limit_reached", message, details: { limit, usage, plan } }.
// Only ever returned when the instance runs with WPMGR_HOSTED on; self-hosted
// installs never see this shape.
// ---------------------------------------------------------------------------

/**
 * Thrown by a site-create mutation when the tenant is at its hosted-billing
 * site cap. Carries the details `UpgradePrompt` needs so no call site has to
 * re-parse the error body itself.
 */
export class SiteLimitReachedError extends Error {
  readonly code = "site_limit_reached" as const;
  readonly limit: number;
  readonly usage: number;
  readonly plan: string;

  constructor(limit: number, usage: number, plan: string) {
    super(
      `This workspace is at its site limit (${usage}/${limit} on the ${plan} plan).`,
    );
    this.name = "SiteLimitReachedError";
    this.limit = limit;
    this.usage = usage;
    this.plan = plan;
  }
}

/**
 * Recognizes the 402 `site_limit_reached` shape from ANY endpoint's error
 * body and returns a typed `SiteLimitReachedError`, or `null` when the
 * response doesn't match. Pure (no network, no React) so it is fully
 * unit-testable in isolation — see lib/api.test.ts.
 */
export function extractSiteLimitReached(
  status: number | undefined,
  error: unknown,
): SiteLimitReachedError | null {
  if (status !== 402) return null;
  if (typeof error !== "object" || error === null) return null;
  const body = error as Record<string, unknown>;
  if (body.code !== "site_limit_reached") return null;
  const details =
    typeof body.details === "object" && body.details !== null
      ? (body.details as Record<string, unknown>)
      : null;
  if (
    typeof details?.limit !== "number" ||
    typeof details?.usage !== "number" ||
    typeof details?.plan !== "string"
  ) {
    return null;
  }
  return new SiteLimitReachedError(details.limit, details.usage, details.plan);
}
