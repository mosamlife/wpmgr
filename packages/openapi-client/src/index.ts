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
  // updates
  createUpdateRun,
  listUpdateRuns,
  getUpdateRun,
  // backups
  createBackup,
  listBackups,
  getBackup,
  createRestore,
  getBackupSchedule,
  getBackupSqlInspection,
  putBackupSchedule,
  // monitoring (M5)
  getSiteUptime,
  getUptimeSummary,
  getAlertConfig,
  putAlertConfig,
  // destinations (ADR-036 P1)
  listSiteDestinations,
  createSiteDestination,
  getSiteDestination,
  updateSiteDestination,
  deleteSiteDestination,
  testSiteDestination,
  // diagnostics + php errors (ADR-037 Sprint 2)
  getSiteDiagnostics,
  refreshSiteDiagnostics,
  listSitePhpErrors,
  silenceSitePhpError,
  // activity log (ADR-037 Sprint 3)
  listSiteActivity,
  verifySiteActivity,
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
  // updates
  UpdateItem,
  UpdateRun,
  UpdateRunCreate,
  UpdateRunList,
  UpdateTask,
  UpdateEvent,
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
  // updates request/response shapes
  CreateUpdateRunData,
  CreateUpdateRunResponse,
  ListUpdateRunsData,
  ListUpdateRunsResponse,
  GetUpdateRunData,
  GetUpdateRunResponse,
  // backups
  BackupCreate,
  BackupSnapshot,
  BackupSnapshotList,
  BackupManifestEntry,
  BackupSnapshotDetail,
  RestoreCreate,
  SqlInspection,
  BackupSchedule,
  BackupScheduleUpdate,
  // backups request/response shapes
  CreateBackupData,
  CreateBackupResponse,
  ListBackupsData,
  ListBackupsResponse,
  GetBackupData,
  GetBackupResponse,
  CreateRestoreData,
  CreateRestoreResponse,
  GetBackupScheduleData,
  GetBackupScheduleResponse,
  GetBackupSqlInspectionData,
  GetBackupSqlInspectionResponse,
  PutBackupScheduleData,
  PutBackupScheduleResponse,
  // monitoring (M5)
  UptimeStatus,
  UptimePoint,
  UptimeSummary,
  UptimeSummaryItem,
  AlertConfig,
  AlertConfigUpdate,
  GetSiteUptimeData,
  GetSiteUptimeResponse,
  GetUptimeSummaryResponse,
  GetAlertConfigResponse,
  PutAlertConfigData,
  PutAlertConfigResponse,
  // destinations (ADR-036 P1)
  SiteDestination,
  SiteDestinationList,
  SiteDestinationCreate,
  SiteDestinationUpdate,
  SiteDestinationTest,
  SiteDestinationTestResult,
  SiteDestinationKind,
  ListSiteDestinationsData,
  ListSiteDestinationsResponse,
  CreateSiteDestinationData,
  CreateSiteDestinationResponse,
  GetSiteDestinationData,
  GetSiteDestinationResponse,
  UpdateSiteDestinationData,
  UpdateSiteDestinationResponse,
  DeleteSiteDestinationData,
  TestSiteDestinationData,
  TestSiteDestinationResponse,
  // diagnostics + php errors (ADR-037 Sprint 2)
  SiteDiagnosticsCard,
  SiteDiagnosticsList,
  PhpError,
  PhpErrorList,
  PhpErrorSilence,
  GetSiteDiagnosticsData,
  GetSiteDiagnosticsResponse,
  RefreshSiteDiagnosticsData,
  ListSitePhpErrorsData,
  ListSitePhpErrorsResponse,
  SilenceSitePhpErrorData,
  // activity log (ADR-037 Sprint 3)
  SiteActivityEvent,
  SiteActivityList,
  ActivityVerifyResult,
  ListSiteActivityData,
  ListSiteActivityResponse,
  VerifySiteActivityData,
  VerifySiteActivityResponse,
} from "./generated/types.gen";
