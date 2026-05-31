package worker

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
)

// DBSiteLookup resolves a site's agent URL + enrollment directly from the sites
// table under the agent GUC. The media-encoder process is intentionally lean
// (no full site service); this is the only site fact it needs (for media_apply
// dispatch). It satisfies SiteLookup.
type DBSiteLookup struct {
	pool *db.Pool
}

// NewDBSiteLookup wires a DBSiteLookup.
func NewDBSiteLookup(pool *db.Pool) *DBSiteLookup {
	return &DBSiteLookup{pool: pool}
}

// GetMediaSiteURL returns (url, enrolled, error) for a site. tenantID scopes the
// row (cross-tenant under the agent GUC; the worker already trusts the job args).
func (l *DBSiteLookup) GetMediaSiteURL(ctx context.Context, tenantID, siteID uuid.UUID) (string, bool, error) {
	var siteURL string
	var enrolled bool
	err := l.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT url, (enrolled_at IS NOT NULL)
			 FROM sites WHERE id = $1 AND tenant_id = $2`,
			siteID, tenantID)
		if err := row.Scan(&siteURL, &enrolled); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errors.New("media: site not found")
			}
			return err
		}
		return nil
	})
	return siteURL, enrolled, err
}

var _ SiteLookup = (*DBSiteLookup)(nil)
