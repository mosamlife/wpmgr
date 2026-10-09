package aireadiness

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// RefreshTarget is what a refresh needs to know about a site.
type RefreshTarget struct {
	SiteID uuid.UUID
	URL    string
	// Enrolled is true once the site has a connected agent identity.
	Enrolled        bool
	LastSeenAt      *time.Time
	AgentVersion    string
	ConnectionState string
}

// Repo reads the readiness facts. Every call runs inside the caller's tenant
// transaction, so the tenant and site-scope policies apply.
type Repo interface {
	// LoadFacts returns one Facts per enrolled, non-archived site the caller
	// can see, or only siteID's when it is set. A site that is archived, never
	// enrolled, in another tenant or outside the caller's scope is absent.
	LoadFacts(ctx context.Context, p domain.Principal, siteID *uuid.UUID) ([]Facts, error)
	// RefreshTarget returns the site's refresh facts, or a not-found error.
	RefreshTarget(ctx context.Context, p domain.Principal, siteID uuid.UUID) (RefreshTarget, error)
}

// PGRepo is the Postgres Repo.
type PGRepo struct{ pool *db.Pool }

// NewRepo builds the Postgres Repo.
func NewRepo(pool *db.Pool) *PGRepo { return &PGRepo{pool: pool} }

var _ Repo = (*PGRepo)(nil)

// LoadFacts reads both readiness queries in one RunTenantTx, so a
// site-constrained principal runs with the site-scope settings the policies
// key on. It never opens a transaction by any other route.
func (r *PGRepo) LoadFacts(ctx context.Context, p domain.Principal, siteID *uuid.UUID) ([]Facts, error) {
	var site pgtype.UUID
	if siteID != nil {
		site = pgtype.UUID{Bytes: *siteID, Valid: true}
	}
	var (
		rows   []sqlc.ListAIReadinessSiteFactsRow
		counts []sqlc.CountAIReadinessAbilityOwnersRow
	)
	err := r.pool.RunTenantTx(ctx, p, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		var err error
		rows, err = q.ListAIReadinessSiteFacts(ctx, sqlc.ListAIReadinessSiteFactsParams{TenantID: p.TenantID, SiteID: site})
		if err != nil {
			return err
		}
		counts, err = q.CountAIReadinessAbilityOwners(ctx, sqlc.CountAIReadinessAbilityOwnersParams{TenantID: p.TenantID, SiteID: site})
		return err
	})
	if err != nil {
		return nil, err
	}
	return assembleFacts(rows, counts, siteID), nil
}

// RefreshTarget reads the site row the refresh gating needs.
func (r *PGRepo) RefreshTarget(ctx context.Context, p domain.Principal, siteID uuid.UUID) (RefreshTarget, error) {
	var row sqlc.GetSiteRow
	err := r.pool.RunTenantTx(ctx, p, func(tx pgx.Tx) error {
		var err error
		row, err = sqlc.New(tx).GetSite(ctx, sqlc.GetSiteParams{ID: siteID, TenantID: p.TenantID})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return RefreshTarget{}, domain.NotFound("site_not_found", "site not found")
	}
	if err != nil {
		return RefreshTarget{}, err
	}
	return RefreshTarget{
		SiteID:          row.ID,
		URL:             row.Url,
		Enrolled:        row.EnrolledAt.Valid,
		LastSeenAt:      tsPtr(row.LastSeenAt),
		AgentVersion:    row.AgentVersion,
		ConnectionState: row.ConnectionState,
	}, nil
}
