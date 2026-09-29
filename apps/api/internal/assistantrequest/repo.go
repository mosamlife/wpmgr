package assistantrequest

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// Repo opens the transactions this package runs in. It never sets a GUC
// itself; every transaction comes from a db helper chosen by principal.
//
// Which helper, per path (the principal invariant):
//   - the queue, approve and decline run under the calling person's own
//     principal (runAsCaller);
//   - every read or write of site data on the approve and dispatch paths, and
//     every worker write to a request row, runs under a single-site principal
//     from mcp.SingleSitePrincipal, opened with mcp.AssertSingleSiteTx
//     (runSiteTx);
//   - the three cross-tenant scans are plain reads under the agent context
//     (runAgentScan);
//   - the only tenant transactions are closeWithoutSite, the sweeper and the
//     reconciler, which call the pool directly and touch nothing but the
//     request row and the audit log.
type Repo struct {
	pool *db.Pool
}

// NewRepo builds a Repo on the application pool.
func NewRepo(pool *db.Pool) *Repo { return &Repo{pool: pool} }

// runAsCaller runs fn under the calling person's own principal.
func (r *Repo) runAsCaller(ctx context.Context, p domain.Principal, fn func(tx pgx.Tx) error) error {
	return r.pool.RunTenantTx(ctx, p, fn)
}

// runSiteTx runs fn under a single-site principal. The first statement proves
// the transaction admits exactly siteID; fn never runs otherwise.
func (r *Repo) runSiteTx(ctx context.Context, p domain.Principal, siteID uuid.UUID, fn func(tx pgx.Tx) error) error {
	return r.pool.RunTenantTx(ctx, p, func(tx pgx.Tx) error {
		if err := mcp.AssertSingleSiteTx(ctx, tx, siteID); err != nil {
			return err
		}
		return fn(tx)
	})
}

// runAgentScan runs a plain cross-tenant read.
func (r *Repo) runAgentScan(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return r.pool.InAgentTx(ctx, fn)
}

// readScopedSite reads the one site this single-site transaction admits,
// through the same statement the assistant surface reads sites with. found is
// false when the site is archived or no longer readable.
func readScopedSite(ctx context.Context, tx pgx.Tx, tenantID, siteID uuid.UUID) (sqlc.Site, bool, error) {
	rows, err := sqlc.New(tx).ListSitesForMCPScope(ctx, sqlc.ListSitesForMCPScopeParams{
		TenantID: tenantID,
		SiteIds:  []uuid.UUID{siteID},
		RowLimit: 2,
	})
	if err != nil {
		return sqlc.Site{}, false, fmt.Errorf("read site: %w", err)
	}
	for _, s := range rows {
		if s.ID == siteID && s.TenantID == tenantID {
			return s, true, nil
		}
	}
	return sqlc.Site{}, false, nil
}

// errNoRow reports whether err is pgx's "no row".
func errNoRow(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
