package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/activity"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentplugin"
	"github.com/mosamlife/wpmgr/apps/api/internal/autologin"
	"github.com/mosamlife/wpmgr/apps/api/internal/backup"
	"github.com/mosamlife/wpmgr/apps/api/internal/diagnostics"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/loginbrand"
	mediasvc "github.com/mosamlife/wpmgr/apps/api/internal/media/service"
	"github.com/mosamlife/wpmgr/apps/api/internal/perf"
	"github.com/mosamlife/wpmgr/apps/api/internal/scan"
	"github.com/mosamlife/wpmgr/apps/api/internal/security"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	"github.com/mosamlife/wpmgr/apps/api/internal/update"
	"github.com/mosamlife/wpmgr/apps/api/internal/uptime"
	"github.com/mosamlife/wpmgr/apps/api/internal/vuln"
	"github.com/mosamlife/wpmgr/apps/api/internal/wpversion"
)

// uptimeSiteAdapter adapts the site service to the uptime package's SiteVerifier
// (tenant-ownership check + site enumeration for the summary) and SiteLookup
// (site name for alert rendering). It keeps the uptime package free of a site
// import.
type uptimeSiteAdapter struct {
	svc *site.Service
}

func newUptimeSiteAdapter(svc *site.Service) *uptimeSiteAdapter { return &uptimeSiteAdapter{svc: svc} }

// VerifySite confirms the site belongs to tenantID (RLS-scoped Get). A
// not-found (including a foreign-tenant site hidden by RLS) returns ok=false,
// not an error, so the handler maps it to 404.
func (a *uptimeSiteAdapter) VerifySite(ctx context.Context, tenantID, siteID uuid.UUID) (string, bool, error) {
	s, err := a.svc.Get(ctx, tenantID, siteID)
	if err != nil {
		if de, ok := domain.AsDomain(err); ok && de.Kind == domain.KindNotFound {
			return "", false, nil
		}
		return "", false, err
	}
	return s.Name, true, nil
}

// ListSiteIDs returns all non-archived site IDs in the tenant (for the uptime
// summary). Uses ListAllSiteIDs to avoid the 500-row cap in site.List.
func (a *uptimeSiteAdapter) ListSiteIDs(ctx context.Context, tenantID uuid.UUID) ([]uuid.UUID, error) {
	return a.svc.ListAllSiteIDs(ctx, tenantID)
}

// SiteName resolves a site's display name for alert rendering; an unresolvable
// site degrades to an empty name (the worker falls back to the URL).
func (a *uptimeSiteAdapter) SiteName(ctx context.Context, tenantID, siteID uuid.UUID) string {
	s, err := a.svc.Get(ctx, tenantID, siteID)
	if err != nil {
		return ""
	}
	return s.Name
}

var (
	_ uptime.SiteVerifier = (*uptimeSiteAdapter)(nil)
	_ uptime.SiteLookup   = (*uptimeSiteAdapter)(nil)
)

// siteLookup adapts the site service to the update package's SiteLookup
// interface, translating site.Site (with its JSONB component inventory) into the
// update.SiteInfo the orchestrator/worker need. It keeps the update package free
// of a site import (the dependency points site<-update only, no cycle).
type siteLookup struct {
	svc *site.Service
}

func newSiteLookup(svc *site.Service) *siteLookup { return &siteLookup{svc: svc} }

func (l *siteLookup) GetSiteInfo(ctx context.Context, tenantID, siteID uuid.UUID) (update.SiteInfo, error) {
	s, err := l.svc.Get(ctx, tenantID, siteID)
	if err != nil {
		return update.SiteInfo{}, err
	}
	return toSiteInfo(s), nil
}

// AgentVersion returns the agent version the site last reported over its
// signed metadata push. It is the ONLY signal the agent self-update channel
// accepts as proof an upgrade landed (update.AgentConfirmWorker): the new code
// speaking for itself, over a channel the old code cannot fake once it has
// been replaced. A not-found site propagates as a domain.NotFound error so the
// confirmation worker can tell "this site is gone" from "this site has not
// reported yet".
func (l *siteLookup) AgentVersion(ctx context.Context, tenantID, siteID uuid.UUID) (string, error) {
	s, err := l.svc.Get(ctx, tenantID, siteID)
	if err != nil {
		return "", err
	}
	return s.AgentVersion, nil
}

// AgentSelfUpdateResult returns the agent's own account of its last self-update
// apply beat, as replayed on the site's signed metadata push and stored in the
// site's inventory blob. It satisfies update.AgentApplyResultLookup.
//
// The apply runs in a WordPress cron request that has no control-plane response
// to ride on, so this record is the only way a FAILED or EXPIRED apply is ever
// heard about. The confirmation worker uses it to explain a timeout: without it
// "the cron run never happened" and "the cron run happened and the upgrade
// failed" are the same silence.
//
// Best-effort throughout: a site that never staged an upgrade, and an agent old
// enough not to send the record, both return false with no error.
func (l *siteLookup) AgentSelfUpdateResult(ctx context.Context, tenantID, siteID uuid.UUID) (update.AgentApplyResult, bool, error) {
	s, err := l.svc.Get(ctx, tenantID, siteID)
	if err != nil {
		return update.AgentApplyResult{}, false, err
	}
	r := s.ParsedAgentSelfUpdate()
	if r == nil {
		return update.AgentApplyResult{}, false, nil
	}
	out := update.AgentApplyResult{
		Status:      r.Status,
		FromVersion: r.FromVersion,
		ToVersion:   r.ToVersion,
		Detail:      r.Detail,
		ApplyID:     r.ApplyID,
	}
	if r.At > 0 {
		out.At = time.Unix(r.At, 0).UTC()
	}
	return out, true, nil
}

func (l *siteLookup) ListSiteInfoByTag(ctx context.Context, tenantID uuid.UUID, tag string) ([]update.SiteInfo, error) {
	sites, err := l.svc.List(ctx, site.ListInput{TenantID: tenantID, AnyTags: []string{tag}, Limit: 200, Offset: 0})
	if err != nil {
		return nil, err
	}
	out := make([]update.SiteInfo, 0, len(sites))
	for _, s := range sites {
		if s.EnrolledAt == nil {
			continue // only enrolled sites can receive signed commands.
		}
		out = append(out, toSiteInfo(s))
	}
	return out, nil
}

// siteRefreshAdapter adapts update.RiverEnqueuer to site.RefreshEnqueuer (the
// site handler's interface). It keeps the site package free of an update
// import. The Source value is forwarded into the River args for audit.
type siteRefreshAdapter struct {
	enq *update.RiverEnqueuer
}

func newSiteRefreshAdapter(enq *update.RiverEnqueuer) *siteRefreshAdapter {
	return &siteRefreshAdapter{enq: enq}
}

func (a *siteRefreshAdapter) EnqueueRefresh(ctx context.Context, tenantID, siteID uuid.UUID, siteURL, source string) error {
	return a.enq.EnqueueRefresh(ctx, update.RefreshInventoryArgs{
		TenantID: tenantID,
		SiteID:   siteID,
		SiteURL:  siteURL,
		Source:   source,
	})
}

var _ site.RefreshEnqueuer = (*siteRefreshAdapter)(nil)

// backupSiteLookup adapts the site service to the backup package's SiteLookup,
// surfacing the agent URL, enrollment status, and the site's age PUBLIC
// recipient (backups are encrypted to it client-side on the agent).
type backupSiteLookup struct {
	svc *site.Service
}

func newBackupSiteLookup(svc *site.Service) *backupSiteLookup { return &backupSiteLookup{svc: svc} }

func (l *backupSiteLookup) GetBackupSiteInfo(ctx context.Context, tenantID, siteID uuid.UUID) (backup.SiteInfo, error) {
	s, err := l.svc.Get(ctx, tenantID, siteID)
	if err != nil {
		return backup.SiteInfo{}, err
	}
	return backup.SiteInfo{
		ID:              s.ID,
		URL:             s.URL,
		Enrolled:        s.EnrolledAt != nil,
		AgeRecipient:    s.AgeRecipient,
		WpTimezone:      s.WpTimezone,
		WpGmtOffset:     s.WpGmtOffset,
		ConnectionState: string(s.ConnectionState),
	}, nil
}

// ListSiteIDs returns all non-archived site IDs for the tenant (used by fleet
// endpoints to build the full candidate set for org-scoped principals). Uses
// ListAllSiteIDs to avoid the 500-row cap in site.List.
func (l *backupSiteLookup) ListSiteIDs(ctx context.Context, tenantID uuid.UUID) ([]uuid.UUID, error) {
	return l.svc.ListAllSiteIDs(ctx, tenantID)
}

var _ backup.SiteLookup = (*backupSiteLookup)(nil)

// autologinSiteAdapter adapts the site service to the autologin package's
// SiteLookup interface (returns the site URL the operator's browser will
// redirect to, with RLS-scoped tenant verification).
type autologinSiteAdapter struct {
	svc *site.Service
}

func newAutologinSiteAdapter(svc *site.Service) *autologinSiteAdapter {
	return &autologinSiteAdapter{svc: svc}
}

func (a *autologinSiteAdapter) GetSiteForAutologin(ctx context.Context, tenantID, siteID uuid.UUID) (string, bool, error) {
	s, err := a.svc.Get(ctx, tenantID, siteID)
	if err != nil {
		if de, ok := domain.AsDomain(err); ok && de.Kind == domain.KindNotFound {
			return "", false, nil
		}
		return "", false, err
	}
	return s.URL, true, nil
}

var _ autologin.SiteLookup = (*autologinSiteAdapter)(nil)

// diagnosticsSiteAdapter resolves a site's agent URL for the diagnostics
// refresh enqueuer (ADR-037 Sprint 2). The diagnostics service interface is
// (tenantID, siteID)-only by design; the enqueuer needs the URL to mint the
// signed `diagnostics` command, so the adapter looks it up here. A not-found
// (or RLS-hidden) site surfaces as a domain.NotFound error the handler maps
// to 404 — that path is unreachable in practice because /diagnostics/refresh
// is RouterGroup-bound to /sites/:siteId, which the same RLS already gates.
type diagnosticsSiteAdapter struct {
	svc *site.Service
}

func newDiagnosticsSiteAdapter(svc *site.Service) *diagnosticsSiteAdapter {
	return &diagnosticsSiteAdapter{svc: svc}
}

func (a *diagnosticsSiteAdapter) GetSiteURL(ctx context.Context, tenantID, siteID uuid.UUID) (string, error) {
	s, err := a.svc.Get(ctx, tenantID, siteID)
	if err != nil {
		return "", err
	}
	return s.URL, nil
}

var _ diagnostics.SiteLookup = (*diagnosticsSiteAdapter)(nil)

// activitySiteAdapter resolves a site's URL + name for an activity-log security
// alert subject line. It keeps the activity package free of a site import.
type activitySiteAdapter struct {
	svc *site.Service
}

func newActivitySiteAdapter(svc *site.Service) *activitySiteAdapter {
	return &activitySiteAdapter{svc: svc}
}

func (a *activitySiteAdapter) URLAndName(ctx context.Context, tenantID, siteID uuid.UUID) (string, string) {
	s, err := a.svc.Get(ctx, tenantID, siteID)
	if err != nil {
		return "", ""
	}
	return s.URL, s.Name
}

var _ activity.SiteLookup = (*activitySiteAdapter)(nil)

// securitySiteAdapter resolves a site's agent URL for the security service
// (ADR-037 S2). Keeps the security package free of a site import.
type securitySiteAdapter struct {
	svc *site.Service
}

func newSecuritySiteAdapter(svc *site.Service) *securitySiteAdapter {
	return &securitySiteAdapter{svc: svc}
}

func (a *securitySiteAdapter) GetSiteURL(ctx context.Context, tenantID, siteID uuid.UUID) (string, error) {
	s, err := a.svc.Get(ctx, tenantID, siteID)
	if err != nil {
		return "", err
	}
	return s.URL, nil
}

var _ security.SiteLookup = (*securitySiteAdapter)(nil)

// loginBrandSiteAdapter resolves a site's agent URL for the loginbrand service
// (M14 Login Whitelabel). Keeps the loginbrand package free of a site import.
type loginBrandSiteAdapter struct {
	svc *site.Service
}

func newLoginBrandSiteAdapter(svc *site.Service) *loginBrandSiteAdapter {
	return &loginBrandSiteAdapter{svc: svc}
}

func (a *loginBrandSiteAdapter) GetSiteURL(ctx context.Context, tenantID, siteID uuid.UUID) (string, error) {
	s, err := a.svc.Get(ctx, tenantID, siteID)
	if err != nil {
		return "", err
	}
	return s.URL, nil
}

var _ loginbrand.SiteLookup = (*loginBrandSiteAdapter)(nil)

// scanSiteAdapter resolves site info for the scan domain (agent URL +
// wp_version). Keeps the scan package free of a site import.
type scanSiteAdapter struct {
	svc *site.Service
}

func newScanSiteAdapter(svc *site.Service) *scanSiteAdapter {
	return &scanSiteAdapter{svc: svc}
}

func (a *scanSiteAdapter) GetScanSiteInfo(ctx context.Context, tenantID, siteID uuid.UUID) (scan.ScanSiteInfo, error) {
	s, err := a.svc.Get(ctx, tenantID, siteID)
	if err != nil {
		return scan.ScanSiteInfo{}, err
	}
	return scan.ScanSiteInfo{
		URL:       s.URL,
		WPVersion: s.WPVersion,
		Enrolled:  s.EnrolledAt != nil,
	}, nil
}

var _ scan.SiteLookup = (*scanSiteAdapter)(nil)

// mediaSiteAdapter resolves site info for the Media Optimizer service (agent URL
// + enrollment). Keeps the media package free of a site import.
type mediaSiteAdapter struct {
	svc *site.Service
}

func newMediaSiteAdapter(svc *site.Service) *mediaSiteAdapter {
	return &mediaSiteAdapter{svc: svc}
}

func (a *mediaSiteAdapter) GetMediaSiteInfo(ctx context.Context, tenantID, siteID uuid.UUID) (mediasvc.MediaSiteInfo, error) {
	s, err := a.svc.Get(ctx, tenantID, siteID)
	if err != nil {
		return mediasvc.MediaSiteInfo{}, err
	}
	return mediasvc.MediaSiteInfo{
		URL:      s.URL,
		Enrolled: s.EnrolledAt != nil,
	}, nil
}

var _ mediasvc.SiteLookup = (*mediaSiteAdapter)(nil)

// perfSiteAdapter resolves a site's agent URL for the Performance Suite service
// (ADR-046). Keeps the perf package free of a site import.
type perfSiteAdapter struct {
	svc *site.Service
}

func newPerfSiteAdapter(svc *site.Service) *perfSiteAdapter {
	return &perfSiteAdapter{svc: svc}
}

func (a *perfSiteAdapter) GetSiteURL(ctx context.Context, tenantID, siteID uuid.UUID) (string, error) {
	s, err := a.svc.Get(ctx, tenantID, siteID)
	if err != nil {
		return "", err
	}
	return s.URL, nil
}

// ListSiteIDs returns all non-archived site IDs for the tenant (used by fleet
// RUM aggregate). Uses ListAllSiteIDs to avoid the 500-row cap in site.List.
func (a *perfSiteAdapter) ListSiteIDs(ctx context.Context, tenantID uuid.UUID) ([]uuid.UUID, error) {
	return a.svc.ListAllSiteIDs(ctx, tenantID)
}

// ListSitesMeta returns the name+url for every non-archived site in the
// tenant (used by the fleet RUM worst-offenders enrichment, GH #202). Pages
// through site.Service.List (which clamps Limit to 200 per call) so large
// fleets are not silently truncated.
func (a *perfSiteAdapter) ListSitesMeta(ctx context.Context, tenantID uuid.UUID) ([]perf.SiteMeta, error) {
	const pageSize = 200
	var metas []perf.SiteMeta
	offset := int32(0)
	for {
		sites, err := a.svc.List(ctx, site.ListInput{TenantID: tenantID, Limit: pageSize, Offset: offset})
		if err != nil {
			return nil, err
		}
		for _, s := range sites {
			metas = append(metas, perf.SiteMeta{ID: s.ID, Name: s.Name, URL: s.URL})
		}
		if len(sites) < pageSize {
			break
		}
		offset += pageSize
	}
	return metas, nil
}

var _ perf.SiteLookup = (*perfSiteAdapter)(nil)

// backupCheckerAdapter adapts the backup service to the perf.BackupChecker
// interface (narrow: only HasRecentBackup). Keeps the perf package free of a
// backup import.
type backupCheckerAdapter struct {
	svc *backup.Service
}

func newBackupCheckerAdapter(svc *backup.Service) *backupCheckerAdapter {
	return &backupCheckerAdapter{svc: svc}
}

// HasRecentBackup returns true when the site has at least one completed snapshot
// created within the lookback window. It uses ListSnapshots with limit=1 and
// checks whether the single most-recent snapshot falls within `within`.
func (a *backupCheckerAdapter) HasRecentBackup(ctx context.Context, tenantID, siteID uuid.UUID, within time.Duration) (bool, error) {
	snaps, err := a.svc.ListSnapshots(ctx, tenantID, siteID, 1, 0)
	if err != nil {
		return false, err
	}
	if len(snaps) == 0 {
		return false, nil
	}
	latest := snaps[0]
	if latest.Status != backup.StatusCompleted {
		return false, nil
	}
	if latest.FinishedAt == nil {
		return false, nil
	}
	return time.Since(*latest.FinishedAt) <= within, nil
}

var _ perf.BackupChecker = (*backupCheckerAdapter)(nil)

// activitySecurityAlerter is the seam between the activity log and the EXISTING
// uptime alert Dispatcher (ADR-037 Sprint 3): a high-severity activity event
// loads the tenant's AlertConfig, checks Enabled + NotifySecurity, and dispatches
// through the same Mailer + WebhookPoster as uptime down/recovery alerts. No
// parallel notification system. Delivery is best-effort: config-load failures
// and the (gated) decision are swallowed/logged, never surfaced to the agent.
type activitySecurityAlerter struct {
	repo       uptime.Repo
	dispatcher *uptime.Dispatcher
	clock      domain.Clock
	logger     *slog.Logger
}

func newActivitySecurityAlerter(repo uptime.Repo, dispatcher *uptime.Dispatcher, clock domain.Clock, logger *slog.Logger) *activitySecurityAlerter {
	if logger == nil {
		logger = slog.Default()
	}
	return &activitySecurityAlerter{repo: repo, dispatcher: dispatcher, clock: clock, logger: logger}
}

func (a *activitySecurityAlerter) NotifySecurity(ctx context.Context, tenantID, siteID uuid.UUID, summary, eventType, severity, siteURL, siteName string) {
	cfg, found, err := a.repo.GetAlertConfig(ctx, tenantID)
	if err != nil {
		a.logger.Warn("security alert config load failed",
			slog.String("site_id", siteID.String()), slog.Any("error", err))
		return
	}
	// Gate: only fire when the tenant has a config, it is enabled, and the
	// operator opted into security notifications.
	if !found || !cfg.Enabled || !cfg.NotifySecurity {
		return
	}
	a.dispatcher.FireSecurityEvent(ctx, cfg, uptime.SecurityEvent{
		TenantID:  tenantID,
		SiteID:    siteID,
		SiteURL:   siteURL,
		SiteName:  siteName,
		Summary:   summary,
		EventType: eventType,
		Severity:  severity,
		FiredAt:   a.clock.Now(),
	})
}

var _ activity.SecurityAlerter = (*activitySecurityAlerter)(nil)

// vulnSiteAdapter adapts the site service to the vuln package's SiteLoader
// interface. It keeps the vuln package free of a site import.
type vulnSiteAdapter struct {
	svc *site.Service
}

func newVulnSiteAdapter(svc *site.Service) *vulnSiteAdapter {
	return &vulnSiteAdapter{svc: svc}
}

// GetSiteForVuln returns the site's WP version and installed component
// inventory in the shape the vuln matcher expects.
func (a *vulnSiteAdapter) GetSiteForVuln(ctx context.Context, tenantID, siteID uuid.UUID) (vuln.SiteSnapshot, error) {
	s, err := a.svc.Get(ctx, tenantID, siteID)
	if err != nil {
		return vuln.SiteSnapshot{}, err
	}
	plugins, themes := s.ParsedComponents()
	snap := vuln.SiteSnapshot{
		ID:        s.ID,
		TenantID:  s.TenantID,
		Name:      s.Name,
		URL:       s.URL,
		WPVersion: s.WPVersion,
	}
	for _, p := range plugins {
		snap.Plugins = append(snap.Plugins, vuln.ComponentSnapshot{
			Slug:    p.Slug,
			Name:    p.Name,
			Version: p.Version,
		})
	}
	for _, t := range themes {
		snap.Themes = append(snap.Themes, vuln.ComponentSnapshot{
			Slug:    t.Slug,
			Name:    t.Name,
			Version: t.Version,
		})
	}
	return snap, nil
}

// ListAllSiteIDs returns all non-archived site IDs for the tenant.
func (a *vulnSiteAdapter) ListAllSiteIDs(ctx context.Context, tenantID uuid.UUID) ([]uuid.UUID, error) {
	return a.svc.ListAllSiteIDs(ctx, tenantID)
}

var _ vuln.SiteLoader = (*vulnSiteAdapter)(nil)

// toSiteInfo translates site.Site (with its JSONB component inventory,
// including each item's own AvailableUpdate advisory and the optional core
// update advisory) into update.SiteInfo. Carrying the per-site pending-update
// signal through is what lets update.Service.planTasks intersect a run's
// requested items against what THIS site actually has pending, instead of the
// raw cross-product of the whole run's site/item selection (#126).
func toSiteInfo(s site.Site) update.SiteInfo {
	plugins, themes := s.ParsedComponents()
	comps := make([]update.Component, 0, len(plugins)+len(themes))
	for _, p := range plugins {
		comps = append(comps, toUpdateComponent(update.TargetPlugin, p))
	}
	for _, t := range themes {
		comps = append(comps, toUpdateComponent(update.TargetTheme, t))
	}
	info := update.SiteInfo{
		ID:       s.ID,
		URL:      s.URL,
		Name:     s.Name,
		Enrolled: s.EnrolledAt != nil,
		// Carried straight from the site row, NOT from the component
		// inventory: the agent self-update planner compares this against the
		// published release version and must not depend on the site's own
		// (deliberately suppressed) self-update advisory.
		AgentVersion: s.AgentVersion,
		Components:   comps,
	}
	// GH #211: drop a same-version phantom core-update advisory here too
	// (defense-in-depth; the update package's planTasks authority,
	// indexPending, re-checks this same condition independently).
	if core := s.ParsedCoreUpdate(); core != nil && core.NewVersion != "" &&
		!wpversion.SameVersion(core.CurrentVersion, core.NewVersion) {
		info.CoreUpdateAvailable = true
		info.CoreCurrentVersion = core.CurrentVersion
		info.CoreNewVersion = core.NewVersion
	}
	return info
}

func toUpdateComponent(typ string, c site.Component) update.Component {
	out := update.Component{Type: typ, Slug: c.Slug, Name: c.Name, Version: c.Version}
	// GH #211: a same-version advisory (new_version == the component's own
	// installed Version) must never surface as UpdateAvailable, or planTasks
	// would plan a doomed/no-op update task for it. The agent's own plugin is
	// excluded for the same reason and a stronger one: it is never an
	// actionable update target (see agentplugin), matched on its plugin-header
	// name as well as its slug so a renamed plugin directory cannot slip past.
	// The component itself still carries its installed version through, so
	// from_version seeding is unaffected; only the pending signal is withheld.
	if c.AvailableUpdate != nil && c.AvailableUpdate.NewVersion != "" &&
		!wpversion.SameVersion(c.Version, c.AvailableUpdate.NewVersion) &&
		!agentplugin.IsComponent(c.Slug, c.Name) {
		out.UpdateAvailable = true
		out.NewVersion = c.AvailableUpdate.NewVersion
	}
	return out
}
