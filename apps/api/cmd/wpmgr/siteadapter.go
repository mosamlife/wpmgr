package main

import (
	"context"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	"github.com/mosamlife/wpmgr/apps/api/internal/update"
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
