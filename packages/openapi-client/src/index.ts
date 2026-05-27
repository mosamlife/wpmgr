// @wpmgr/api — thin, swappable facade over the generated Hey API client.
//
// Everything the app consumes flows through THIS module, never through
// `./generated/*` directly. That keeps the code generator (ADR-013: Hey API)
// an implementation detail we can swap without touching app code.
//
// Regenerate the `./generated` tree from packages/openapi/openapi.yaml with:
//   pnpm --filter @wpmgr/api generate
// The generated output is committed.

// --- Runtime client + config -------------------------------------------------
export { client } from "./generated/client.gen";
export type { Config, ClientOptions } from "./generated/client";

// --- Operation functions (SDK) ----------------------------------------------
export {
  getHealthz,
  getReadyz,
  listTenants,
  createTenant,
  getTenant,
  listSites,
  createSite,
  getSite,
  deleteSite,
} from "./generated/sdk.gen";

// --- Domain + request/response types ----------------------------------------
export type {
  Site,
  SiteCreate,
  SiteList,
  Tenant,
  TenantCreate,
  TenantList,
  Health,
  Readiness,
  Error as ApiError,
  ListSitesData,
  ListSitesResponse,
  GetSiteData,
  GetSiteResponse,
  DeleteSiteData,
  CreateSiteData,
  CreateSiteResponse,
} from "./generated/types.gen";
