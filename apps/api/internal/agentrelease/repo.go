package agentrelease

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentplugin"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// SiteAgentVersion is one tenant-scoped site row for the fleet
// agent-version rollup: the site's id, display name, and last-reported
// agent_version (empty when the agent has never reported one).
type SiteAgentVersion struct {
	SiteID       uuid.UUID
	SiteName     string
	AgentVersion string
	// Distribution is the build of the agent the site's own plugin inventory
	// identifies. agentplugin.DistributionNone when the inventory names no
	// agent (never reported, or unrecognizable), which Classify treats exactly
	// as it did before this signal existed.
	Distribution agentplugin.Distribution
}

// pluginIdentity is one entry of ListSitesAgentVersions' plugin_identities
// projection: the inventory key and the plugin-header name, which are the two
// facts agentplugin needs to recognize the agent and its build.
type pluginIdentity struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// agentDistribution finds the agent in a site's plugin-identity projection and
// reports which build it is. An empty or malformed projection yields
// agentplugin.DistributionNone: an unreadable inventory must never be able to
// promote a site to a definite status, so the caller falls back to comparing
// versions rather than guessing.
func agentDistribution(raw []byte) agentplugin.Distribution {
	if len(raw) == 0 {
		return agentplugin.DistributionNone
	}
	var ids []pluginIdentity
	if err := json.Unmarshal(raw, &ids); err != nil {
		return agentplugin.DistributionNone
	}
	for _, id := range ids {
		if d := agentplugin.DistributionOf(id.Slug, id.Name); d != agentplugin.DistributionNone {
			return d
		}
	}
	return agentplugin.DistributionNone
}

// Repo is the agentrelease package's tenant-scoped data access.
type Repo struct {
	pool *db.Pool
}

// NewRepo builds a Repo.
func NewRepo(pool *db.Pool) *Repo {
	return &Repo{pool: pool}
}

// ListSiteAgentVersions returns every non-archived site under tenantID with
// its reported agent_version, for the fleet agent-version rollup.
//
// Runs under InTenantTx: the sites_tenant_isolation RLS policy plus the
// explicit tenant_id filter in ListSitesAgentVersions both scope the read:
// a tenant can never see another tenant's sites through this call. This
// mirrors the vulnerability-scanner fleet rollup (internal/vuln.Repo.
// FleetOpenFindings), the precedent for a tenant-scoped cross-site read.
func (r *Repo) ListSiteAgentVersions(ctx context.Context, tenantID uuid.UUID) ([]SiteAgentVersion, error) {
	var out []SiteAgentVersion
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListSitesAgentVersions(ctx, tenantID)
		if err != nil {
			return err
		}
		out = make([]SiteAgentVersion, 0, len(rows))
		for _, row := range rows {
			out = append(out, SiteAgentVersion{
				SiteID:       row.ID,
				SiteName:     row.Name,
				AgentVersion: row.AgentVersion,
				Distribution: agentDistribution(row.PluginIdentities),
			})
		}
		return nil
	})
	return out, err
}
