package main

import (
	"context"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/autologin"
	"github.com/mosamlife/wpmgr/apps/api/internal/backup"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
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
