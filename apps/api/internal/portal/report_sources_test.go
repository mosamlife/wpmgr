package portal

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/email"
	"github.com/mosamlife/wpmgr/apps/api/internal/report"
)

// The portal summary reads only update counts, so SetReportSources drops the
// version-table source (GH #969) along with the email source it always drops.
func TestSetReportSourcesDropsUnusedSources(t *testing.T) {
	h := &Handler{}
	h.SetReportSources(report.Sources{
		GetFleetStatsBySite: func(context.Context, uuid.UUID, time.Time, time.Time, int32) ([]email.SiteStatsRow, error) {
			return nil, nil
		},
		ListReportUpdateItems: func(context.Context, uuid.UUID, uuid.UUID, time.Time, time.Time, int32) ([]sqlc.ListReportUpdateItemsRow, error) {
			return nil, nil
		},
		GetUpdateReportStats: func(context.Context, uuid.UUID, uuid.UUID, time.Time, time.Time) ([]sqlc.GetUpdateReportStatsRow, error) {
			return nil, nil
		},
	})
	if h.reportSources.GetFleetStatsBySite != nil {
		t.Error("the email source reached the portal")
	}
	if h.reportSources.ListReportUpdateItems != nil {
		t.Error("the portal summary still runs the per-site version-table query")
	}
	if h.reportSources.GetUpdateReportStats == nil {
		t.Error("the update counts the summary shows were dropped too")
	}
}
