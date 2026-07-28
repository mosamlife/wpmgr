package agentrelease

import (
	"context"

	"github.com/google/uuid"
)

// SiteRollup is one site's classified row for the fleet agent-version
// dashboard.
type SiteRollup struct {
	SiteID       uuid.UUID
	SiteName     string
	AgentVersion string
	Status       Status
}

// Counts tallies SiteRollup.Status across the fleet.
type Counts struct {
	Current    int
	Outdated   int
	Unknown    int
	Ineligible int
}

// FleetSummary is the GET /api/v1/fleet/agents payload, at the service layer
// (DTO-agnostic: the handler maps this to the wire shape).
type FleetSummary struct {
	LatestVersion string
	Counts        Counts
	Sites         []SiteRollup
}

// SiteLister is the narrow repo surface Service needs. Satisfied by *Repo.
type SiteLister interface {
	ListSiteAgentVersions(ctx context.Context, tenantID uuid.UUID) ([]SiteAgentVersion, error)
}

// VersionReader is the narrow reader surface Service needs. Satisfied by
// *Reader.
type VersionReader interface {
	LatestVersion(ctx context.Context) string
}

// Service composes the tenant-scoped site rollup with the cached published
// version.
type Service struct {
	repo   SiteLister
	reader VersionReader
}

// NewService builds a Service. reader may be nil (object storage not
// configured); LatestVersion and FleetRollup then always treat the published
// version as unknown.
func NewService(repo SiteLister, reader VersionReader) *Service {
	return &Service{repo: repo, reader: reader}
}

// LatestVersion returns the currently published agent version, or "" when
// unknown (see Reader.LatestVersion).
func (s *Service) LatestVersion(ctx context.Context) string {
	if s.reader == nil {
		return ""
	}
	return s.reader.LatestVersion(ctx)
}

// FleetRollup returns the tenant-scoped per-site agent-version rollup plus
// counts, classified against the currently published version.
func (s *Service) FleetRollup(ctx context.Context, tenantID uuid.UUID) (FleetSummary, error) {
	latest := s.LatestVersion(ctx)

	rows, err := s.repo.ListSiteAgentVersions(ctx, tenantID)
	if err != nil {
		return FleetSummary{}, err
	}

	summary := FleetSummary{
		LatestVersion: latest,
		Sites:         make([]SiteRollup, 0, len(rows)),
	}
	for _, row := range rows {
		status := Classify(row.AgentVersion, latest, row.Distribution)
		switch status {
		case StatusCurrent:
			summary.Counts.Current++
		case StatusOutdated:
			summary.Counts.Outdated++
		case StatusIneligible:
			summary.Counts.Ineligible++
		default:
			summary.Counts.Unknown++
		}
		summary.Sites = append(summary.Sites, SiteRollup{
			SiteID:       row.SiteID,
			SiteName:     row.SiteName,
			AgentVersion: row.AgentVersion,
			Status:       status,
		})
	}
	return summary, nil
}
