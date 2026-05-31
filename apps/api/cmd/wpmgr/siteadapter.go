package main

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/activity"
	"github.com/mosamlife/wpmgr/apps/api/internal/autologin"
	"github.com/mosamlife/wpmgr/apps/api/internal/backup"
	"github.com/mosamlife/wpmgr/apps/api/internal/diagnostics"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/loginbrand"
	mediasvc "github.com/mosamlife/wpmgr/apps/api/internal/media/service"
	"github.com/mosamlife/wpmgr/apps/api/internal/scan"
	"github.com/mosamlife/wpmgr/apps/api/internal/security"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	"github.com/mosamlife/wpmgr/apps/api/internal/update"
	"github.com/mosamlife/wpmgr/apps/api/internal/uptime"
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

// ListSiteIDs returns all site IDs in the tenant (for the uptime summary).
func (a *uptimeSiteAdapter) ListSiteIDs(ctx context.Context, tenantID uuid.UUID) ([]uuid.UUID, error) {
	sites, err := a.svc.List(ctx, site.ListInput{TenantID: tenantID, Limit: 500, Offset: 0})
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(sites))
	for _, s := range sites {
		ids = append(ids, s.ID)
	}
	return ids, nil
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

func (l *siteLookup) ListSiteInfoByTag(ctx context.Context, tenantID uuid.UUID, tag string) ([]update.SiteInfo, error) {
	sites, err := l.svc.List(ctx, site.ListInput{TenantID: tenantID, Tag: tag, Limit: 200, Offset: 0})
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
		ID:           s.ID,
		URL:          s.URL,
		Enrolled:     s.EnrolledAt != nil,
		AgeRecipient: s.AgeRecipient,
		WpTimezone:   s.WpTimezone,
		WpGmtOffset:  s.WpGmtOffset,
	}, nil
}

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

func toSiteInfo(s site.Site) update.SiteInfo {
	plugins, themes := s.ParsedComponents()
	comps := make([]update.Component, 0, len(plugins)+len(themes))
	for _, p := range plugins {
		comps = append(comps, update.Component{Type: update.TargetPlugin, Slug: p.Slug, Version: p.Version})
	}
	for _, t := range themes {
		comps = append(comps, update.Component{Type: update.TargetTheme, Slug: t.Slug, Version: t.Version})
	}
	return update.SiteInfo{
		ID:         s.ID,
		URL:        s.URL,
		Name:       s.Name,
		Enrolled:   s.EnrolledAt != nil,
		Components: comps,
	}
}
