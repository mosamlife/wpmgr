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
  listSocialProviders,
  // connected accounts (settings/security)
  listMyIdentities,
  unlinkMyIdentity,
  setMyInitialPassword,
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
  // tags (GH #230 "rich tags", m100)
  listTags,
  createTag,
  updateTag,
  deleteTag,
  bulkApplyTags,
  // monitoring pause (GH #414, m117)
  pauseSiteMonitoring,
  resumeSiteMonitoring,
  // updates
  createUpdateRun,
  listUpdateRuns,
  getUpdateRun,
  retryUpdateRun,
  cancelScheduledUpdateRun,
  // backups
  createBackup,
  listBackups,
  getBackup,
  deleteBackup,
  bulkDeleteBackups,
  cancelBackup,
  lockBackup,
  unlockBackup,
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
  // error config (S1.2)
  getSiteErrorConfig,
  patchSiteErrorConfig,
  // activity log (ADR-037 Sprint 3)
  listSiteActivity,
  verifySiteActivity,
  // login protection (S2)
  getSiteLoginProtection,
  putSiteLoginProtection,
  unblockSiteIp,
  listSiteLoginEvents,
  // login whitelabel (M14)
  getSiteLoginBrand,
  putSiteLoginBrand,
  // autologin default user policy (GH #286)
  getSiteAutologinPolicy,
  putSiteAutologinPolicy,
  // performance suite (ADR-046 / m36)
  getPerfConfig,
  putPerfConfig,
  getCacheStats,
  purgeCache,
  preloadCache,
  enableCache,
  disableCache,
  cleanDatabase,
  listRucssResults,
  clearRucss,
  computeRucss,
  bulkPurgeCache,
  bulkConfigCache,
  // AI cache-clear requests (ai-requests, tracka-cache-purge-design)
  listAssistantRequests,
  listSiteAssistantRequests,
  approveAssistantRequest,
  declineAssistantRequest,
  // AI site-change requests (page create, approved per call)
  listSiteAbilityRequests,
  listAbilityRequests,
  approveAbilityRequest,
  declineAbilityRequest,
  undoAbilityRequest,
  getSiteContentEditing,
  enableSiteContentEditing,
  // AI readiness checklist (per-site card, fleet rollup, re-check)
  getSiteAiReadiness,
  getFleetAiReadiness,
  refreshSiteAiReadiness,
  // search-replace (#188)
  runSearchReplace,
  // db snapshots (#189)
  listDbSnapshots,
  createDbSnapshot,
  revertDbSnapshot,
  deleteDbSnapshot,
  // media cleaner (#190)
  scanUnusedMedia,
  isolateUnusedMedia,
  restoreIsolatedMedia,
  deleteIsolatedMedia,
  listQuarantinedMedia,
  // font results catalog (ADR-052 Phase 2 / m55)
  listFontResults,
  // RUM / Core Web Vitals (Phase 3b)
  getRumSummary,
  listRumResults,
  // RUM beacon key recovery (GH #174)
  rotateRumBeaconKey,
  // email (Phase 3c)
  listEmailProviders,
  getOrgEmailConfig,
  putOrgEmailConfig,
  getSiteEmailConfig,
  putSiteEmailConfig,
  sendTestEmail,
  listSiteEmailLog,
  exportSiteEmailLog,
  getSiteEmailLogEntry,
  getSiteEmailStats,
  listFleetEmailLog,
  getFleetEmailStats,
  // email Phase 4a — suppression + log actions
  listSiteEmailSuppression,
  addSiteEmailSuppression,
  deleteSiteEmailSuppression,
  listFleetEmailSuppression,
  addFleetEmailSuppression,
  deleteFleetEmailSuppression,
  resendEmailLog,
  bulkResendEmailLog,
  bulkDeleteEmailLog,
  // email webhook config (Phase 4b)
  putSiteEmailWebhookConfig,
  putOrgEmailWebhookConfig,
  // email sync (Phase 5)
  syncSiteEmailConfig,
  // email m62 — connections + notify settings
  listEmailConnections,
  putEmailConnection,
  deleteEmailConnection,
  getEmailNotifySettings,
  putEmailNotifySettings,
  // clients (m63 Foundation)
  listClients,
  createClient,
  updateClient,
  deleteClient,
  getClient,
  assignSitesToClient,
  // client reports (m64 Phase 2)
  getClientReportSchedule,
  putClientReportSchedule,
  listClientReports,
  generateClientReport,
  getClientReport,
  deleteClientReport,
  // client portal members (m66 Phase 3)
  listClientMembers,
  addClientMember,
  removeClientMember,
  listClientInvitations,
  revokeClientInvitation,
  regenerateClientInvitation,
  // portal endpoints (m66 Phase 3)
  getPortalOverview,
  listPortalSites,
  getPortalSiteUptime,
  listPortalSiteBackups,
  listPortalSiteUpdates,
  getPortalSiteVitals,
  listPortalReports,
  downloadPortalReport,
  // portal summary (m66 Phase 2 dashboard)
  getPortalSummary,
  // object cache (m68 Phase 0/1)
  getObjectCacheConfig,
  putObjectCacheConfig,
  testObjectCache,
  enableObjectCache,
  disableObjectCache,
  flushObjectCache,
  getObjectCacheStatsHistory,
  // email deliverability (fleet per-site reputation dashboard)
  getFleetEmailDeliverability,
  // screenshots (M72)
  refreshSiteScreenshot,
  // agent release freshness (agent-releases)
  getAgentLatestVersion,
  getFleetAgentVersions,
  // upstream agent-release mirror manual check (GH #322, admin console)
  checkAgentMirrorNow,
  // content inventory (Track B slice S1)
  getSiteContentInventory,
  refreshSiteContentInventory,
  getAdminContentFleetReport,
  // file manager (P1 read-only browser)
  getSiteFilesSettings,
  updateSiteFilesSettings,
  listSiteFiles,
  readSiteFileContent,
  prepareSiteFileDownload,
  // file manager (P2 write/upload)
  writeSiteFileContent,
  createSiteDirectory,
  renameSiteFile,
  deleteSiteFile,
  chmodSiteFile,
  prepareSiteFileUpload,
  applySiteFileUpload,
  // file manager (P3 advanced ops)
  createSiteFileArchive,
  extractSiteFileArchive,
  searchSiteFiles,
  listSiteFileVersions,
  restoreSiteFileVersion,
  // governed org/site context (ADR-064 S4)
  getOrgContext,
  patchOrgContext,
  listOrgContextVersions,
  getOrgContextVersion,
  diffOrgContextVersion,
  restoreOrgContextVersion,
  getSiteContext,
  patchSiteContext,
  listSiteContextVersions,
  getSiteContextVersion,
  diffSiteContextVersion,
  restoreSiteContextVersion,
  getEffectiveSiteContext,
} from "./generated/sdk.gen";

// --- Domain + request/response types ----------------------------------------
export type {
  Site,
  SiteCreate,
  SiteList,
  SiteComponent,
  SiteComponents,
  SiteTags,
  SiteKeystoreStatus,
  PairingCode,
  PairingCodeCreate,
  // updates
  UpdateItem,
  UpdateRun,
  UpdateRunCreate,
  UpdateRunList,
  UpdateRunRetryRequest,
  UpdateRunRetryResult,
  UpdateRunRetryExclusion,
  UpdateRunCancelResult,
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
  ConnectedAccounts,
  ConnectedIdentity,
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
  // tags (GH #230 "rich tags", m100)
  SiteTag,
  SiteTagList,
  SiteTagCreate,
  SiteTagUpdate,
  BulkTagApplyRequest,
  ListTagsData,
  ListTagsResponse,
  CreateTagData,
  CreateTagResponse,
  DeleteTagData,
  UpdateTagData,
  UpdateTagResponse,
  // monitoring pause (GH #414, m117)
  PauseMonitoringRequest,
  ResumeMonitoringRequest,
  MonitoringResult,
  MonitoringBulkResult,
  PauseSiteMonitoringData,
  PauseSiteMonitoringResponse,
  ResumeSiteMonitoringData,
  ResumeSiteMonitoringResponse,
  BulkApplyTagsData,
  BulkApplyTagsResponse,
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
  RetryUpdateRunData,
  RetryUpdateRunResponse,
  CancelScheduledUpdateRunData,
  CancelScheduledUpdateRunResponse,
  // backups
  BackupCreate,
  BackupSnapshot,
  BackupSnapshotList,
  BulkDeleteBackupsRequest,
  BulkDeleteBackupsResponse,
  BulkDeleteBackupsResultItem,
  BulkDeleteBackupsCounts,
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
  DeleteBackupData,
  CancelBackupData,
  CancelBackupResponse,
  LockBackupData,
  LockBackupResponse,
  UnlockBackupData,
  UnlockBackupResponse,
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
  PhpErrorFrame,
  PhpErrorList,
  PhpErrorSilence,
  GetSiteDiagnosticsData,
  GetSiteDiagnosticsResponse,
  RefreshSiteDiagnosticsData,
  ListSitePhpErrorsData,
  ListSitePhpErrorsResponse,
  SilenceSitePhpErrorData,
  // error config (S1.2)
  SiteErrorConfig,
  SiteErrorConfigUpdate,
  GetSiteErrorConfigData,
  GetSiteErrorConfigResponse,
  PatchSiteErrorConfigData,
  PatchSiteErrorConfigResponse,
  // activity log (ADR-037 Sprint 3)
  SiteActivityEvent,
  SiteActivityList,
  ActivityVerifyResult,
  ChainBreak,
  ListSiteActivityData,
  ListSiteActivityResponse,
  VerifySiteActivityData,
  VerifySiteActivityResponse,
  // login protection (S2)
  SecurityThresholds,
  SiteLoginProtectionConfig,
  SiteLoginProtectionConfigUpdate,
  UnblockIpRequest,
  UnblockIpResult,
  SiteLoginEvent,
  SiteLoginEventList,
  GetSiteLoginProtectionData,
  GetSiteLoginProtectionResponse,
  PutSiteLoginProtectionData,
  PutSiteLoginProtectionResponse,
  UnblockSiteIpData,
  UnblockSiteIpResponse,
  ListSiteLoginEventsData,
  ListSiteLoginEventsResponse,
  // login whitelabel (M14)
  SiteLoginBrand,
  SiteLoginBrandUpdate,
  GetSiteLoginBrandData,
  GetSiteLoginBrandResponse,
  PutSiteLoginBrandData,
  PutSiteLoginBrandResponse,
  // autologin default user policy (GH #286)
  SiteAutologinPolicy,
  SiteAutologinPolicyUpdate,
  GetSiteAutologinPolicyData,
  GetSiteAutologinPolicyResponse,
  PutSiteAutologinPolicyData,
  PutSiteAutologinPolicyResponse,
  // performance suite (ADR-046 / m36)
  PerfConfig,
  CdnCredentials,
  CacheStats,
  PurgeRequest,
  PerfActionResult,
  DbCleanResult,
  RucssResult,
  RucssResultList,
  RucssClearResult,
  BulkPurgeRequest,
  BulkConfigRequest,
  BulkResult,
  BulkResultList,
  GetPerfConfigData,
  GetPerfConfigResponse,
  PutPerfConfigData,
  PutPerfConfigResponse,
  GetCacheStatsData,
  GetCacheStatsResponse,
  PurgeCacheData,
  PurgeCacheResponse,
  PreloadCacheData,
  PreloadCacheResponse,
  EnableCacheData,
  EnableCacheResponse,
  DisableCacheData,
  DisableCacheResponse,
  CleanDatabaseData,
  CleanDatabaseResponse,
  ListRucssResultsData,
  ListRucssResultsResponse,
  ClearRucssData,
  ClearRucssResponse,
  BulkPurgeCacheData,
  BulkPurgeCacheResponse,
  BulkConfigCacheData,
  BulkConfigCacheResponse,
  ComputeRucssData,
  ComputeRucssResponse,
  // AI cache-clear requests (ai-requests, tracka-cache-purge-design).
  // AssistantRequest, AssistantRequestList and the four Response aliases are
  // NOT re-exported from here -- see the nullable-field patch below, which
  // exports corrected versions of all six under these same names.
  AssistantRequestApproveBody,
  ListAssistantRequestsData,
  ListSiteAssistantRequestsData,
  ApproveAssistantRequestData,
  DeclineAssistantRequestData,
  // AI site-change requests. AbilityRequest and AbilityRequestList are
  // exported below with the nullable-field patch.
  AbilityRequestApproveBody,
  AbilityRequestPageBuilder,
  AbilityRequestPageMedia,
  ContentEditingState,
  ListSiteAbilityRequestsData,
  ListAbilityRequestsData,
  ApproveAbilityRequestData,
  DeclineAbilityRequestData,
  UndoAbilityRequestData,
  GetSiteContentEditingData,
  EnableSiteContentEditingData,
  // AI readiness checklist. The status, check id and warning code unions are
  // the closed sets the control plane returns.
  SiteAiReadiness,
  AiReadinessStatus,
  AiReadinessWarning,
  AiReadinessWarningCode,
  AiReadinessFloors,
  AiReadinessCheckId,
  AiReadinessGroup,
  AiReadinessCheck,
  AiReadinessRefreshResult,
  FleetAiReadiness,
  FleetAiReadinessSite,
  GetSiteAiReadinessData,
  GetFleetAiReadinessData,
  RefreshSiteAiReadinessData,
  // search-replace (#188)
  SearchReplaceRequest,
  SearchReplaceResult,
  RunSearchReplaceData,
  RunSearchReplaceResponse,
  RunSearchReplaceResponses,
  // db snapshots (#189)
  DbSnapshotEntry,
  DbSnapshotList,
  DbSnapshotCreate,
  DbSnapshotCreateResult,
  DbSnapshotRevert,
  DbSnapshotRevertResult,
  ListDbSnapshotsData,
  ListDbSnapshotsResponse,
  CreateDbSnapshotData,
  CreateDbSnapshotResponse,
  RevertDbSnapshotData,
  RevertDbSnapshotResponse,
  DeleteDbSnapshotData,
  DeleteDbSnapshotResponse,
  // media cleaner (#190)
  MediaCleanCandidate,
  MediaCleanScanResult,
  MediaCleanIsolateRequest,
  MediaCleanIsolateResult,
  MediaCleanRestoreRequest,
  MediaCleanRestoreResult,
  MediaCleanDeleteRequest,
  MediaCleanDeleteResult,
  MediaCleanQuarantineEntry,
  MediaCleanQuarantineManifest,
  MediaCleanQuarantineList,
  ScanUnusedMediaData,
  ScanUnusedMediaResponse,
  IsolateUnusedMediaData,
  IsolateUnusedMediaResponse,
  RestoreIsolatedMediaData,
  RestoreIsolatedMediaResponse,
  DeleteIsolatedMediaData,
  DeleteIsolatedMediaResponse,
  ListQuarantinedMediaData,
  ListQuarantinedMediaResponse,
  // font results catalog (ADR-052 Phase 2 / m55)
  FontResult,
  FontResultList,
  ListFontResultsData,
  ListFontResultsResponse,
  // RUM / Core Web Vitals (Phase 3b)
  RumSummary,
  RumResult,
  RumResultList,
  RumMetricSummary,
  GetRumSummaryData,
  GetRumSummaryResponse,
  ListRumResultsData,
  ListRumResultsResponse,
  // RUM beacon key recovery (GH #174)
  RumBeaconRotateResult,
  RotateRumBeaconKeyData,
  RotateRumBeaconKeyResponse,
  // email (Phase 3c)
  SiteEmailConfig,
  PutEmailConfigRequest,
  EmailTestRequest,
  EmailTestResult,
  EmailProviderCatalog,
  EmailProviderSpec,
  EmailProviderField,
  SiteEmailLogEntry,
  EmailLogList,
  EmailLogDetail,
  EmailStatsByDay,
  EmailStatsByProvider,
  EmailStats,
  ListEmailProvidersData,
  ListEmailProvidersResponse,
  GetOrgEmailConfigData,
  GetOrgEmailConfigResponse,
  PutOrgEmailConfigData,
  PutOrgEmailConfigResponse,
  GetSiteEmailConfigData,
  GetSiteEmailConfigResponse,
  PutSiteEmailConfigData,
  PutSiteEmailConfigResponse,
  SendTestEmailData,
  SendTestEmailResponse,
  ListSiteEmailLogData,
  ListSiteEmailLogResponse,
  ExportSiteEmailLogData,
  ExportSiteEmailLogResponse,
  GetSiteEmailLogEntryData,
  GetSiteEmailLogEntryResponse,
  GetSiteEmailStatsData,
  GetSiteEmailStatsResponse,
  ListFleetEmailLogData,
  ListFleetEmailLogResponse,
  GetFleetEmailStatsData,
  GetFleetEmailStatsResponse,
  // email Phase 4a — suppression + log actions
  EmailSuppressionEntry,
  EmailSuppressionPage,
  AddSuppressionRequest,
  ResendEmailResult,
  BulkResendRequest,
  BulkResendItemResult,
  BulkResendResponse,
  BulkDeleteLogsRequest,
  BulkDeleteLogsResponse,
  ListSiteEmailSuppressionData,
  ListSiteEmailSuppressionResponse,
  AddSiteEmailSuppressionData,
  AddSiteEmailSuppressionResponse,
  DeleteSiteEmailSuppressionData,
  DeleteSiteEmailSuppressionResponse,
  ListFleetEmailSuppressionData,
  ListFleetEmailSuppressionResponse,
  AddFleetEmailSuppressionData,
  AddFleetEmailSuppressionResponse,
  DeleteFleetEmailSuppressionData,
  DeleteFleetEmailSuppressionResponse,
  ResendEmailLogData,
  ResendEmailLogResponse,
  BulkResendEmailLogData,
  BulkResendEmailLogResponse,
  BulkDeleteEmailLogData,
  BulkDeleteEmailLogResponse,
  // email webhook config (Phase 4b)
  PutEmailWebhookConfigRequest,
  EmailWebhookConfigResponse,
  PutSiteEmailWebhookConfigData,
  PutSiteEmailWebhookConfigResponse,
  PutOrgEmailWebhookConfigData,
  PutOrgEmailWebhookConfigResponse,
  // email sync (Phase 5)
  SyncSiteEmailConfigData,
  SyncSiteEmailConfigResponse,
  // email m62 — connections + notify settings + attachment metadata
  EmailConnection,
  EmailAttachmentMeta,
  EmailNotifySettings,
  PutEmailConnectionRequest,
  PutEmailNotifySettingsRequest,
  ListEmailConnectionsData,
  ListEmailConnectionsResponse,
  PutEmailConnectionData,
  PutEmailConnectionResponse,
  DeleteEmailConnectionData,
  DeleteEmailConnectionResponse,
  GetEmailNotifySettingsData,
  GetEmailNotifySettingsResponse,
  PutEmailNotifySettingsData,
  PutEmailNotifySettingsResponse,
  // clients (m63 Foundation)
  AgencyClient,
  AgencyClientList,
  CreateAgencyClientRequest,
  UpdateAgencyClientRequest,
  AssignSitesRequest,
  AssignSitesResponse,
  ListClientsData,
  ListClientsResponse,
  CreateClientData,
  CreateClientResponse,
  AssignSitesToClientData,
  AssignSitesToClientResponse,
  DeleteClientData,
  DeleteClientResponse,
  GetClientData,
  GetClientResponse,
  UpdateClientData,
  UpdateClientResponse,
  // client reports (m64 Phase 2)
  ClientReport,
  ClientReportList,
  ClientReportSchedule,
  ClientReportScheduleUpdate,
  ClientReportSectionFlags,
  GenerateClientReportRequest,
  GetClientReportScheduleData,
  GetClientReportScheduleResponse,
  PutClientReportScheduleData,
  PutClientReportScheduleResponse,
  ListClientReportsData,
  ListClientReportsResponse,
  GenerateClientReportData,
  GenerateClientReportResponse,
  GetClientReportData,
  GetClientReportResponse,
  DeleteClientReportData,
  DeleteClientReportResponse,
  // client portal members (m66 Phase 3)
  ClientMember,
  ClientMemberList,
  ClientMemberCreateRequest,
  ClientMemberInviteResult,
  ClientInvitation,
  ClientInvitationList,
  ListClientMembersData,
  ListClientMembersResponse,
  AddClientMemberData,
  AddClientMemberResponse,
  RemoveClientMemberData,
  ListClientInvitationsData,
  ListClientInvitationsResponse,
  RevokeClientInvitationData,
  RegenerateClientInvitationData,
  RegenerateClientInvitationResponse,
  // portal schemas (m66 Phase 3)
  MePortal,
  PrincipalRole,
  PortalOverview,
  PortalSite,
  PortalSiteList,
  PortalUptimeSummary,
  PortalIncident,
  PortalBackupItem,
  PortalBackupList,
  PortalUpdateItem,
  PortalUpdateList,
  PortalVitalMetric,
  PortalVitalsSummary,
  PortalReportItem,
  PortalReportList,
  PortalReportDownload,
  GetPortalOverviewResponse,
  ListPortalSitesResponse,
  GetPortalSiteUptimeData,
  GetPortalSiteUptimeResponse,
  ListPortalSiteBackupsData,
  ListPortalSiteBackupsResponse,
  ListPortalSiteUpdatesData,
  ListPortalSiteUpdatesResponse,
  GetPortalSiteVitalsData,
  GetPortalSiteVitalsResponse,
  ListPortalReportsResponse,
  DownloadPortalReportData,
  DownloadPortalReportResponse,
  // portal summary (m66 Phase 2 dashboard)
  PortalSummary,
  PortalSummaryTotals,
  PortalSummarySite,
  PortalSummaryLatestReport,
  PortalRecentWorkItem,
  PortalUptimeDay,
  PortalVitalsDistribution,
  PortalVitalsRatingCount,
  GetPortalSummaryData,
  GetPortalSummaryResponse,
  // object cache (m68 Phase 0/1)
  ObjectCacheConfig,
  ObjectCacheConfigPut,
  ObjectCacheTestResult,
  ObjectCacheStatsHistoryPoint,
  ObjectCacheStatsHistory,
  GetObjectCacheConfigData,
  GetObjectCacheConfigResponse,
  PutObjectCacheConfigData,
  PutObjectCacheConfigResponse,
  TestObjectCacheData,
  TestObjectCacheResponse,
  EnableObjectCacheData,
  EnableObjectCacheResponse,
  DisableObjectCacheData,
  DisableObjectCacheResponse,
  FlushObjectCacheData,
  FlushObjectCacheResponse,
  GetObjectCacheStatsHistoryData,
  GetObjectCacheStatsHistoryResponse,
  // email deliverability (fleet per-site reputation dashboard)
  SiteDeliveryItem,
  DeliverabilityReport,
  GetFleetEmailDeliverabilityData,
  GetFleetEmailDeliverabilityResponse,
  // screenshots (M72)
  RefreshSiteScreenshotData,
  RefreshSiteScreenshotResponse,
  // agent release freshness (agent-releases)
  AgentLatestVersion,
  FleetAgentCounts,
  FleetAgentSite,
  FleetAgentVersions,
  AgentMirrorStatus,
  GetAgentLatestVersionResponse,
  GetFleetAgentVersionsResponse,
  // upstream agent-release mirror manual check (GH #322, admin console)
  AgentMirrorCheckQueued,
  CheckAgentMirrorNowError,
  CheckAgentMirrorNowResponse,
  // file manager (P1 read-only browser)
  FileEntry,
  FileListResult,
  FileReadResult,
  FileDownloadRequest,
  FileDownloadResult,
  ContentInventoryPage,
  ContentInventoryRow,
  ContentInventoryEditor,
  ContentFleetReport,
  ContentFleetVerdictShare,
  ContentFleetBuilderShare,
  FileManagerSettings,
  UpdateFileManagerSettingsRequest,
  GetSiteFilesSettingsData,
  GetSiteFilesSettingsResponse,
  UpdateSiteFilesSettingsData,
  UpdateSiteFilesSettingsResponse,
  ListSiteFilesData,
  ListSiteFilesResponse,
  ReadSiteFileContentData,
  ReadSiteFileContentResponse,
  PrepareSiteFileDownloadData,
  PrepareSiteFileDownloadResponse,
  // file manager (P2 write/upload)
  WriteFileContentRequest,
  WriteFileResult,
  FileMkdirRequest,
  FileMkdirResult,
  FileRenameRequest,
  FileRenameResult,
  FileDeleteRequest,
  FileDeleteResult,
  FileChmodRequest,
  FileChmodResult,
  PrepareUploadRequest,
  PrepareUploadPresignedPut,
  PrepareUploadResult,
  ApplyUploadRequest,
  ApplyUploadResult,
  WriteSiteFileContentData,
  WriteSiteFileContentResponse,
  CreateSiteDirectoryData,
  CreateSiteDirectoryResponse,
  RenameSiteFileData,
  RenameSiteFileResponse,
  DeleteSiteFileData,
  DeleteSiteFileResponse,
  ChmodSiteFileData,
  ChmodSiteFileResponse,
  PrepareSiteFileUploadData,
  PrepareSiteFileUploadResponse,
  ApplySiteFileUploadData,
  ApplySiteFileUploadResponse,
  // file manager (P3 advanced ops)
  FileArchiveCreateRequest,
  FileArchiveCreateResult,
  FileExtractRequest,
  FileExtractResult,
  FileSearchMatch,
  FileSearchResult,
  FileVersion,
  FileVersionsResult,
  FileVersionRestoreRequest,
  FileVersionRestoreResult,
  CreateSiteFileArchiveData,
  CreateSiteFileArchiveResponse,
  ExtractSiteFileArchiveData,
  ExtractSiteFileArchiveResponse,
  SearchSiteFilesData,
  SearchSiteFilesResponse,
  ListSiteFileVersionsData,
  ListSiteFileVersionsResponse,
  RestoreSiteFileVersionData,
  RestoreSiteFileVersionResponse,
  // governed org/site context (ADR-064 S4)
  RestrictionSet,
  GuidanceSet,
  GovContext,
  GovContextVersionSummary,
  GovContextVersionList,
  GovContextVersionItem,
  GovContextListDiff,
  GovContextFieldDiff,
  GovContextSnapshotDiff,
  GovContextDiff,
  GovContextLayerContribution,
  GovContextEffective,
  PatchGovContextRequest,
  // admin-billing (superadmin billing-admin panel: accounts / account detail / revenue)
  AdminAccountTiles,
  AdminAccountListItem,
  AdminAccountsResponse,
  AdminAccountUsageMeter,
  AdminAccountEntitlementValues,
  AdminAccountUsage,
  AdminAccountSubscription,
  AdminAccountTimelineEntry,
  AdminAccountMember,
  AdminAccountSite,
  AdminAccountDetail,
  AdminRevenueTiles,
  AdminPlanDistributionRow,
  AdminCompedRow,
  AdminPastDueRow,
  AdminRecentBillingEvent,
  AdminRevenueResponse,
  AdminBillingAck,
  AdminCompAccountRequest,
  AdminReasonRequest,
  AdminSetOverridesRequest,
  AdminExtendGraceRequest,
  AdminForceStateRequest,
  ListAdminAccountsData,
  ListAdminAccountsResponse,
  GetAdminAccountData,
  GetAdminAccountResponse,
  CompAdminAccountData,
  CompAdminAccountResponse,
  RevokeAdminAccountCompData,
  RevokeAdminAccountCompResponse,
  SetAdminAccountOverridesData,
  SetAdminAccountOverridesResponse,
  ExtendAdminAccountGraceData,
  ExtendAdminAccountGraceResponse,
  SuspendAdminAccountData,
  SuspendAdminAccountResponse,
  RestoreAdminAccountData,
  RestoreAdminAccountResponse,
  ForceAdminAccountStateData,
  ForceAdminAccountStateResponse,
  GetAdminRevenueResponse,
} from "./generated/types.gen";

// --- AI cache-clear requests: nullable-field patch --------------------------
//
// packages/openapi/openapi.yaml declares `openapi: 3.1.0`. Under 3.1, schemas
// are JSON Schema 2020-12, where nullability is `type: [T, "null"]`; the
// OpenAPI-3.0-only `nullable: true` sidecar the AssistantRequest schema uses
// is not a JSON Schema keyword there, and @hey-api/openapi-ts (this package's
// generator, see openapi-ts.config.ts) does not special-case it, so every
// field below is generated with no `| null` even though it is genuinely
// nullable on the wire: apps/api/internal/api/gen/oas_schemas_gen.go's
// AssistantRequest struct types every one of them Nil*/Opt*, matching
// tracka-cache-purge-design-v7 §2.6's card-state table (a pending row has
// decided_at/outcome/etc. all null). Confirmed by running this package's own
// `generate` against the current spec: byte-identical output, so this is a
// spec-authoring bug (the 3.0-style keyword in a 3.1 document), not stale
// codegen -- out of this package's path (packages/openapi/openapi.yaml is
// backend-owned) to fix at the source. Until it is, the facade corrects the
// nullability here rather than let app code trust a type that lies about
// what the server actually sends.
import type {
  AssistantRequest as GeneratedAssistantRequest,
  AssistantRequestList as GeneratedAssistantRequestList,
} from "./generated/types.gen";

type PatchedAssistantRequestFields = {
  url: GeneratedAssistantRequest["url"] | null;
  setup_client: GeneratedAssistantRequest["setup_client"] | null;
  decided_at: GeneratedAssistantRequest["decided_at"] | null;
  decided_by_user_id: GeneratedAssistantRequest["decided_by_user_id"] | null;
  decided_by_name: GeneratedAssistantRequest["decided_by_name"] | null;
  withdrawn_at: GeneratedAssistantRequest["withdrawn_at"] | null;
  claimed_at: GeneratedAssistantRequest["claimed_at"] | null;
  last_attempt_at: GeneratedAssistantRequest["last_attempt_at"] | null;
  last_attempt_code: GeneratedAssistantRequest["last_attempt_code"] | null;
  outcome: GeneratedAssistantRequest["outcome"] | null;
  not_sent_reason: GeneratedAssistantRequest["not_sent_reason"] | null;
  outcome_at: GeneratedAssistantRequest["outcome_at"] | null;
  hosting_caches_cleared: GeneratedAssistantRequest["hosting_caches_cleared"] | null;
  hosting_caches_skipped: GeneratedAssistantRequest["hosting_caches_skipped"] | null;
  origin_only_confirmed: GeneratedAssistantRequest["origin_only_confirmed"] | null;
  wpmgr_cdn: GeneratedAssistantRequest["wpmgr_cdn"] | null;
  site_reported_text: GeneratedAssistantRequest["site_reported_text"] | null;
};

export type AssistantRequest = Omit<
  GeneratedAssistantRequest,
  keyof PatchedAssistantRequestFields
> &
  PatchedAssistantRequestFields;

export type AssistantRequestList = Omit<GeneratedAssistantRequestList, "requests"> & {
  requests: AssistantRequest[];
};

export type ListAssistantRequestsResponse = AssistantRequestList;
export type ListSiteAssistantRequestsResponse = AssistantRequestList;
export type ApproveAssistantRequestResponse = AssistantRequest;
export type DeclineAssistantRequestResponse = AssistantRequest;

// --- AI site-change requests: nullable-field patch --------------------------
// Same spec-authoring gap as AssistantRequest above: `nullable: true` is not a
// JSON Schema 2020-12 keyword, so the generated type omits `| null` for fields
// the server sends as JSON null (apps/api/internal/abilityrequest/handler.go's
// RequestDTO types them as pointers without omitempty).
import type {
  AbilityRequest as GeneratedAbilityRequest,
  AbilityRequestList as GeneratedAbilityRequestList,
  AbilityRequestOrgList as GeneratedAbilityRequestOrgList,
  ContentEditingState as GeneratedContentEditingState,
} from "./generated/types.gen";

type PatchedAbilityRequestFields = {
  title_excerpt?: GeneratedAbilityRequest["title_excerpt"] | null;
  editor?: GeneratedAbilityRequest["editor"] | null;
  post_type?: GeneratedAbilityRequest["post_type"] | null;
  setup_client?: GeneratedAbilityRequest["setup_client"] | null;
  decided_at?: GeneratedAbilityRequest["decided_at"] | null;
  outcome?: GeneratedAbilityRequest["outcome"] | null;
  outcome_code?: GeneratedAbilityRequest["outcome_code"] | null;
  not_sent_reason?: GeneratedAbilityRequest["not_sent_reason"] | null;
  created_post_id?: GeneratedAbilityRequest["created_post_id"] | null;
  trashed?: GeneratedAbilityRequest["trashed"] | null;
  undo_state?: GeneratedAbilityRequest["undo_state"] | null;
  undo_available_until?: GeneratedAbilityRequest["undo_available_until"] | null;
  // page_media is a nil slice without omitempty in RequestDTO, so the wire
  // carries null for every row that places no image.
  page_media?: GeneratedAbilityRequest["page_media"] | null;
  // page_builder is a nil pointer without omitempty in RequestDTO, so the wire
  // carries null for a page in a WordPress editor and every other ability.
  page_builder?: GeneratedAbilityRequest["page_builder"] | null;
};

export type AbilityRequest = Omit<GeneratedAbilityRequest, keyof PatchedAbilityRequestFields> &
  PatchedAbilityRequestFields;

export type AbilityRequestList = Omit<GeneratedAbilityRequestList, "requests"> & {
  requests: AbilityRequest[];
};

export type AbilityRequestOrgList = Omit<GeneratedAbilityRequestOrgList, "requests"> & {
  requests: AbilityRequest[];
};

export type ListAbilityRequestsResponse = AbilityRequestOrgList;
export type ListSiteAbilityRequestsResponse = AbilityRequestList;
export type ApproveAbilityRequestResponse = AbilityRequest;
export type DeclineAbilityRequestResponse = AbilityRequest;
export type UndoAbilityRequestResponse = AbilityRequest;
export type GetSiteContentEditingResponse = GeneratedContentEditingState;
export type EnableSiteContentEditingResponse = GeneratedContentEditingState;
