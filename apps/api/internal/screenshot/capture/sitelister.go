package capture

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
)

// DBSiteIDLister implements SiteIDLister by querying connected sites cross-tenant
// under the app.agent GUC. Only sites in the 'connected' state (not degraded or
// pending) are included — degraded sites are reachable but excluded to avoid
// burning encoder capacity on known-troubled sites.
type DBSiteIDLister struct {
	pool *db.Pool
}

// NewDBSiteIDLister builds a DBSiteIDLister.
func NewDBSiteIDLister(pool *db.Pool) *DBSiteIDLister {
	return &DBSiteIDLister{pool: pool}
}

// listConnectedForScheduledScreenshotSQL mirrors db/query/sites.sql's
// ListConnectedSiteIDsForScreenshot column-for-column and adds ONE predicate:
// monitoring_paused_at IS NULL (m117, GH #414).
//
// This is the SCHEDULED weekly fanout's enumeration only. It deliberately does
// not replace the sqlc query, and it is deliberately not shared with any
// operator-initiated path: a person clicking "Refresh screenshot" on a paused
// site still gets a screenshot (screenshot.ReasonManual and ReasonEnroll never
// reach this list, and Worker.Work only re-checks the pause for
// ReasonScheduled).
//
// Same plan note as uptime's listEnrolledForMonitoringProbeSQL: m117 shipped no
// index on monitoring_paused_at on purpose. The predicate matches nearly every
// row and this enumeration is an uncapped scan already, so the added IS NULL is
// a filter on a scan that has to happen anyway. Do not add an index for it.
const listConnectedForScheduledScreenshotSQL = `
SELECT id, tenant_id, url FROM sites
 WHERE connection_state = 'connected'
   AND enrolled_at IS NOT NULL
   AND monitoring_paused_at IS NULL
 ORDER BY created_at DESC, id DESC`

// ListConnectedSiteIDs returns the ID, tenant ID, and URL of every enrolled site
// in the 'connected' state whose monitoring is not paused, across all tenants,
// under the app.agent GUC.
func (l *DBSiteIDLister) ListConnectedSiteIDs(ctx context.Context) ([]SiteIDWithTenantAndURL, error) {
	var out []SiteIDWithTenantAndURL
	err := l.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, listConnectedForScheduledScreenshotSQL)
		if err != nil {
			return err
		}
		defer rows.Close()
		out = out[:0]
		for rows.Next() {
			var r SiteIDWithTenantAndURL
			if err := rows.Scan(&r.SiteID, &r.TenantID, &r.URL); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// IsMonitoringPaused reports whether the site's monitoring is currently paused.
// Used as the point-of-action re-check by the capture worker: nothing drains
// already-queued capture jobs, so a job enqueued before a pause must find out
// at the moment it runs.
//
// A missing row reports NOT paused, mirroring uptime's IsMonitoringPaused: the
// site was deleted between the fanout and the capture, and the capture's own
// MarkReady/MarkFailed will drop harmlessly. Inventing a pause for a deleted
// site would be a worse failure.
func (l *DBSiteIDLister) IsMonitoringPaused(ctx context.Context, siteID uuid.UUID) (bool, error) {
	var paused bool
	err := l.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`SELECT monitoring_paused_at IS NOT NULL FROM sites WHERE id = $1`, siteID).Scan(&paused)
		if errors.Is(err, pgx.ErrNoRows) {
			paused = false
			return nil
		}
		return err
	})
	return paused, err
}

// Ensure DBSiteIDLister satisfies both interfaces at compile time.
var (
	_ SiteIDLister           = (*DBSiteIDLister)(nil)
	_ MonitoringPauseChecker = (*DBSiteIDLister)(nil)
)
