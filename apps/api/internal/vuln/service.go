package vuln

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/update"
	"github.com/mosamlife/wpmgr/apps/api/internal/wpversion"
)

// SiteLoader is the narrow interface the service uses to load a site's
// component inventory.  Implemented in main by a site adapter.
type SiteLoader interface {
	// GetSiteForVuln returns the site's WP version and installed plugins/themes.
	// Returns a NotFound domain error when the site does not exist or the tenant
	// does not own it.
	GetSiteForVuln(ctx context.Context, tenantID, siteID uuid.UUID) (SiteSnapshot, error)

	// ListAllSiteIDs returns every site ID under a tenant (for RescanAll fan-out).
	ListAllSiteIDs(ctx context.Context, tenantID uuid.UUID) ([]uuid.UUID, error)
}

// UpdateCreator is the narrow interface the remediation endpoint uses to
// trigger a plugin/theme/core update run via the existing update domain.
// *update.Service satisfies it.
type UpdateCreator interface {
	CreateRun(ctx context.Context, in update.CreateRunInput) (update.Run, []update.Task, error)
}

// RescanEnqueuer enqueues a per-site rescan River job.
type RescanEnqueuer interface {
	EnqueueRescanSite(ctx context.Context, args RescanSiteArgs) error
}

// Service is the vulnerability scanning service.
type Service struct {
	repo    *Repo
	pool    *db.Pool
	sites   SiteLoader
	updates UpdateCreator
	enqueue RescanEnqueuer
	logger  *slog.Logger

	// m103 (GH #247) — vulnerability alerting. All four are wired post-boot
	// via their Set* methods (mirrors email.Service's SetMailer/SetPublicBase
	// pattern); a nil alertCfg makes DispatchVulnAlerts a clean no-op.
	mailer     AlertMailer
	webhook    WebhookPoster
	alertCfg   AlertConfigReader
	publicBase string
}

// NewService builds a Service.
func NewService(
	repo *Repo,
	pool *db.Pool,
	sites SiteLoader,
	updates UpdateCreator,
	enqueue RescanEnqueuer,
	logger *slog.Logger,
) *Service {
	return &Service{
		repo:    repo,
		pool:    pool,
		sites:   sites,
		updates: updates,
		enqueue: enqueue,
		logger:  logger,
	}
}

// ---------------------------------------------------------------------------
// Matching
// ---------------------------------------------------------------------------

// RescanSite loads the site's installed inventory, joins it against the feed,
// upserts matched findings, and resolves stale ones.  It is idempotent and
// safe to call concurrently (finding rows are upserted under InTenantTx).
func (s *Service) RescanSite(ctx context.Context, tenantID, siteID uuid.UUID) error {
	snap, err := s.sites.GetSiteForVuln(ctx, tenantID, siteID)
	if err != nil {
		return err
	}

	meta, err := s.repo.GetFeedMeta(ctx)
	if err != nil {
		s.logger.Warn("vuln: could not get feed meta; skipping rescan",
			slog.String("site_id", siteID.String()), slog.Any("error", err))
		return nil
	}
	if !meta.OK {
		// Feed not yet ingested or degraded — skip silently.
		return nil
	}

	// Build the component list: plugins + themes + core (if version known).
	type item struct {
		kind string
		slug string
		name string
		ver  string
	}
	var items []item
	for _, p := range snap.Plugins {
		if p.Slug == "" || p.Version == "" || p.Version == "unknown" {
			continue
		}
		items = append(items, item{KindPlugin, p.Slug, p.Name, p.Version})
	}
	for _, t := range snap.Themes {
		if t.Slug == "" || t.Version == "" || t.Version == "unknown" {
			continue
		}
		items = append(items, item{KindTheme, t.Slug, t.Name, t.Version})
	}
	if snap.WPVersion != "" && snap.WPVersion != "unknown" {
		items = append(items, item{KindCore, "wordpress", "WordPress", snap.WPVersion})
	}

	// Match each item against the feed.
	var upserts []FindingUpsert
	for _, it := range items {
		rows, err := s.repo.LookupSoftware(ctx, it.kind, it.slug)
		if err != nil {
			s.logger.Warn("vuln: software lookup failed",
				slog.String("kind", it.kind), slog.String("slug", it.slug),
				slog.Any("error", err))
			continue
		}
		for _, row := range rows {
			ranges, err := parseAffectedVersions(row.AffectedVersions)
			if err != nil {
				continue
			}
			if !wpversion.IsVulnerable(it.ver, ranges) {
				continue
			}
			patched, err := parsePatchedVersions(row.PatchedVersions)
			if err != nil {
				patched = nil
			}
			fixedVersion := wpversion.BestFixedVersion(it.ver, patched)
			severity := SeverityFromRating(row.CVSSRating, row.CVSSScore)

			upserts = append(upserts, FindingUpsert{
				TenantID: tenantID,
				SiteID:   siteID,
				VulnID:   row.VulnID,
				Kind:     it.kind,
				// Store the RAW agent-inventory slug (it.slug), NOT the
				// canonical wordfence_vuln_software slug (row.Slug). For a
				// plugin, it.slug is the get_plugins() file path (e.g.
				// "woocommerce/woocommerce.php"): that is exactly the value
				// the update domain indexes a site's installed components by
				// (update.Component.Slug is a straight passthrough of
				// site.Component.Slug; see cmd/wpmgr/siteadapter.go
				// toUpdateComponent), so Remediate can hand it straight to
				// update.Service.CreateRun with no resolution step. Storing
				// the raw form also keeps the finding's UNIQUE(site_id,
				// vuln_id, kind, slug) key stable across rescans: the
				// canonical form is lower-cased (normSlug), so a mixed-case
				// raw slug such as a theme's stylesheet directory "Divi"
				// would otherwise collide with a second "divi" row on the
				// very next rescan, leaving the original row unreachable by
				// ResolveStaleFindings and permanently open. normSlug is used
				// ONLY to derive the LookupSoftware query key above, never to
				// decide what gets stored here.
				Slug:             it.slug,
				Name:             it.name,
				InstalledVersion: it.ver,
				FixedVersion:     fixedVersion,
				Severity:         severity,
				CVSSScore:        row.CVSSScore,
				CVE:              row.CVE,
				Title:            row.Title,
			})
		}
	}

	// Persist findings in a single tenant tx.
	err = s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		for _, u := range upserts {
			if err := s.repo.UpsertFinding(ctx, tx, u); err != nil {
				return err
			}
		}
		// Collect matched vuln IDs so we can resolve stale ones.
		matchedIDs := make([]string, 0, len(upserts))
		seen := make(map[string]bool)
		for _, u := range upserts {
			if !seen[u.VulnID] {
				seen[u.VulnID] = true
				matchedIDs = append(matchedIDs, u.VulnID)
			}
		}
		return s.repo.ResolveStaleFindings(ctx, tx, tenantID, siteID, matchedIDs)
	})
	if err != nil {
		return fmt.Errorf("vuln rescan site %s: %w", siteID, err)
	}

	s.logger.Info("vuln: site rescan complete",
		slog.String("site_id", siteID.String()),
		slog.Int("matched", len(upserts)))
	return nil
}

// RescanAll fans RescanSite out across every site under the given tenant, or
// across ALL tenants when tenantID is uuid.Nil (used after a feed refresh).
// Enqueues individual per-site River jobs rather than running inline to bound
// memory and support partial failure.
func (s *Service) RescanAll(ctx context.Context, tenantID uuid.UUID) error {
	if s.enqueue == nil {
		return nil
	}
	if tenantID != uuid.Nil {
		// Enqueue per-site jobs for one tenant.
		ids, err := s.sites.ListAllSiteIDs(ctx, tenantID)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := s.enqueue.EnqueueRescanSite(ctx, RescanSiteArgs{
				TenantID: tenantID,
				SiteID:   id,
			}); err != nil {
				s.logger.Warn("vuln: enqueue rescan failed",
					slog.String("site_id", id.String()), slog.Any("error", err))
			}
		}
		return nil
	}
	// Cross-tenant rescan after feed refresh: use InAgentTx to enumerate all
	// tenant IDs then fan out.
	var tenantIDs []uuid.UUID
	err := s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM tenants`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var tid uuid.UUID
			if err := rows.Scan(&tid); err != nil {
				return err
			}
			tenantIDs = append(tenantIDs, tid)
		}
		return rows.Err()
	})
	if err != nil {
		return fmt.Errorf("vuln: list tenants for rescan-all: %w", err)
	}
	for _, tid := range tenantIDs {
		ids, err := s.sites.ListAllSiteIDs(ctx, tid)
		if err != nil {
			s.logger.Warn("vuln: list site IDs failed", slog.String("tenant_id", tid.String()), slog.Any("error", err))
			continue
		}
		for _, sid := range ids {
			if err := s.enqueue.EnqueueRescanSite(ctx, RescanSiteArgs{
				TenantID: tid,
				SiteID:   sid,
			}); err != nil {
				s.logger.Warn("vuln: enqueue rescan failed",
					slog.String("site_id", sid.String()), slog.Any("error", err))
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Finding lifecycle
// ---------------------------------------------------------------------------

// GetSiteFindings returns open findings for a site.
func (s *Service) GetSiteFindings(ctx context.Context, tenantID, siteID uuid.UUID) ([]Finding, FeedMeta, error) {
	findings, err := s.repo.ListOpenFindings(ctx, tenantID, siteID)
	if err != nil {
		return nil, FeedMeta{}, domain.Internal("list_findings_failed", "failed to list vulnerability findings").WithCause(err)
	}
	meta, _ := s.repo.GetFeedMeta(ctx) // attribution; ignore error
	return findings, meta, nil
}

// Dismiss sets a finding to dismissed status.
func (s *Service) Dismiss(ctx context.Context, tenantID, siteID, findingID, userID uuid.UUID) error {
	return s.repo.DismissFinding(ctx, tenantID, siteID, findingID, userID)
}

// Restore re-opens a dismissed finding.
func (s *Service) Restore(ctx context.Context, tenantID, siteID, findingID uuid.UUID) error {
	return s.repo.RestoreFinding(ctx, tenantID, siteID, findingID)
}

// Remediate maps a finding to an update run via the update domain.  It resolves
// the finding's fixed_version and maps Kind → update.Item.Type, then calls
// update.Service.CreateRun for the single target site.
func (s *Service) Remediate(ctx context.Context, tenantID, siteID, findingID, userID uuid.UUID) (update.Run, []update.Task, error) {
	if s.updates == nil {
		return update.Run{}, nil, domain.ServiceUnavailable("updates_not_wired", "the update service is not available")
	}

	f, err := s.repo.GetFinding(ctx, tenantID, siteID, findingID)
	if err != nil {
		return update.Run{}, nil, err
	}
	if f.Status == StatusResolved {
		return update.Run{}, nil, domain.Validation("already_resolved", "this vulnerability is already resolved")
	}

	version := f.FixedVersion
	if version == "" {
		version = "latest"
	}
	// Map vuln kind to update item type.
	itemType := f.Kind
	if itemType == KindCore {
		itemType = "core"
	}

	// f.Slug is the RAW agent-inventory slug (RescanSite stores it.slug, not
	// the canonical wordfence_vuln_software form; see the Slug comment in
	// RescanSite). That is exactly the value the update domain indexes a
	// site's installed components by (update.Component.Slug is a straight
	// passthrough of site.Component.Slug; see cmd/wpmgr/siteadapter.go
	// toUpdateComponent, which for a plugin is the get_plugins() file path,
	// e.g. "woocommerce/woocommerce.php"), so it is handed straight through
	// with no resolution step.
	return s.updates.CreateRun(ctx, update.CreateRunInput{
		TenantID:  tenantID,
		CreatedBy: userID,
		SiteIDs:   []uuid.UUID{siteID},
		Items: []update.Item{{
			Type:    itemType,
			Slug:    f.Slug,
			Version: version,
		}},
	})
}

// GetFleetSummary returns the cross-site vulnerability counts + prioritized list.
func (s *Service) GetFleetSummary(ctx context.Context, tenantID uuid.UUID, limit int) (FleetSummary, FeedMeta, error) {
	critical, high, medium, low, unknown, err := s.repo.FleetOpenCounts(ctx, tenantID)
	if err != nil {
		return FleetSummary{}, FeedMeta{}, domain.Internal("fleet_counts_failed", "failed to aggregate fleet vulnerability counts").WithCause(err)
	}
	rows, err := s.repo.FleetOpenFindings(ctx, tenantID, limit)
	if err != nil {
		return FleetSummary{}, FeedMeta{}, domain.Internal("fleet_findings_failed", "failed to list fleet vulnerability findings").WithCause(err)
	}
	meta, _ := s.repo.GetFeedMeta(ctx)

	fleet := FleetSummary{
		// Unknown MUST be included in TotalOpen — an unknown-severity finding
		// (no CVSS data yet) is still an open finding; dropping it from the
		// total would make it vanish from the fleet count entirely (GH #245).
		TotalOpen: critical + high + medium + low + unknown,
		Critical:  critical,
		High:      high,
		Medium:    medium,
		Low:       low,
		Unknown:   unknown,
	}
	for _, row := range rows {
		fleet.Findings = append(fleet.Findings, FleetFinding{
			SiteID:   row.Finding.SiteID,
			SiteName: row.SiteName,
			SiteURL:  row.SiteURL,
			Finding:  row.Finding,
		})
	}
	return fleet, meta, nil
}

// ---------------------------------------------------------------------------
// JSON parsing helpers
// ---------------------------------------------------------------------------

// parseAffectedVersions decodes the affected_versions JSON stored in the DB.
//
// The real Wordfence v3 feed sends an OBJECT keyed by range-string:
//
//	{"* - 2.0.0": {"from_version":"*","from_inclusive":true,"to_version":"2.0.0","to_inclusive":false}}
//
// For forward-tolerance, a JSON array of the same range objects is also
// accepted (the shape used by older feed revisions and the existing service
// tests). A null/empty value returns a nil slice (not vulnerable).
func parseAffectedVersions(raw []byte) ([]wpversion.AffectedVersionRange, error) {
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "{}" || string(raw) == "[]" {
		return nil, nil
	}
	// Try array first (forward-compat + existing test fixtures).
	if len(raw) > 0 && raw[0] == '[' {
		var ranges []wpversion.AffectedVersionRange
		if err := json.Unmarshal(raw, &ranges); err != nil {
			return nil, err
		}
		return ranges, nil
	}
	// Object shape: the keys are human-readable range labels (not meaningful for
	// matching); decode the values into a slice.
	if len(raw) > 0 && raw[0] == '{' {
		var obj map[string]wpversion.AffectedVersionRange
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, err
		}
		ranges := make([]wpversion.AffectedVersionRange, 0, len(obj))
		for _, r := range obj {
			ranges = append(ranges, r)
		}
		return ranges, nil
	}
	return nil, nil
}

func parsePatchedVersions(raw []byte) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var versions []string
	if err := json.Unmarshal(raw, &versions); err != nil {
		return nil, err
	}
	return versions, nil
}
