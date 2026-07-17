package sitetag

import (
	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
)

// toAPI maps a domain Tag to the OpenAPI SiteTag wire type.
func toAPI(t Tag) gen.SiteTag {
	return gen.SiteTag{
		ID:         t.ID,
		Name:       t.Name,
		Color:      t.Color,
		UsageCount: t.UsageCount,
		CreatedAt:  t.CreatedAt,
	}
}

// bulkTagResultDTO is one per-site result in the bulk-apply response. Mirrors
// internal/perf's bulkResultDTO shape (site_id/ok/detail) so the response
// unmarshals cleanly into the shared gen.BulkResult / gen.BulkResultList
// OpenAPI types.
type bulkTagResultDTO struct {
	SiteID string `json:"site_id"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}
