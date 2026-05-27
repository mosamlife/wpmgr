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
  // auth
  register,
  login,
  logout,
  getMe,
  oidcLogin,
  oidcCallback,
  // members
  listMembers,
  inviteMember,
  // api keys
  listApiKeys,
  createApiKey,
  revokeApiKey,
  // audit
  listAudit,
  verifyAudit,
  // tenants
  listTenants,
  createTenant,
  getTenant,
  // sites
  listSites,
  createSite,
  getSite,
  deleteSite,
  createPairingCode,
  setSiteTags,
} from "./generated/sdk.gen";

// --- Domain + request/response types ----------------------------------------
export type {
  Site,
  SiteCreate,
  SiteList,
  SiteComponent,
  SiteComponents,
  SiteTags,
  PairingCode,
  PairingCodeCreate,
  Tenant,
  TenantCreate,
  TenantList,
  Health,
  Readiness,
  Error as ApiError,
  // auth
  Me,
  User,
  Role,
  Membership,
  MembershipList,
  LoginRequest,
  RegisterRequest,
  InviteRequest,
  // api keys
  ApiKey,
  ApiKeyList,
  ApiKeyCreate,
  ApiKeyCreated,
  // audit
  AuditEntry,
  AuditList,
  AuditVerify,
  // request/response shapes
  ListSitesData,
  ListSitesResponse,
  GetSiteData,
  GetSiteResponse,
  DeleteSiteData,
  CreateSiteData,
  CreateSiteResponse,
  CreatePairingCodeData,
  CreatePairingCodeResponse,
  SetSiteTagsData,
  SetSiteTagsResponse,
  LoginData,
  RegisterData,
  GetMeResponse,
  ListApiKeysResponse,
  CreateApiKeyData,
  CreateApiKeyResponse,
  RevokeApiKeyData,
} from "./generated/types.gen";
