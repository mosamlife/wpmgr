package diagnostics

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// DBSizeHistorySink appends a site_db_size_history data point sourced from a
// diagnostics push (GH #196). *perf.Repo satisfies it via
// RecordDBSizeHistoryFromDiagnostics; wired from main once the perf repo is
// constructed.
//
// Declared here (rather than importing perf directly) to keep diagnostics
// free of a cross-domain package import — the concrete wiring lives in
// cmd/wpmgr. When never wired (nil), ingestDBSizeHistory is a no-op; the
// pre-existing manual "Scan database" writer (perf.Repo.UpsertDBScanResult)
// is unaffected either way.
type DBSizeHistorySink interface {
	RecordDBSizeHistoryFromDiagnostics(ctx context.Context, tenantID, siteID uuid.UUID, dbSizeBytes int64, scannedAt time.Time) error
}

// SetDBSizeHistorySink wires the DB-size trend writer (GH #196). Optional:
// when never called, ingestDBSizeHistory silently no-ops.
func (s *Service) SetDBSizeHistorySink(sink DBSizeHistorySink) {
	s.dbSizeHistory = sink
}

// ingestDBSizeHistory taps the wp_native category's
// wp-paths-sizes.fields.database_size.debug field — the byte count the
// agent's SizeProbe already computes for the Directory Sizes card — and
// appends a site_db_size_history data point on every successful diagnostics
// push (GH #196).
//
// Before this tap, perf.Repo.UpsertDBScanResult (reached only via the
// operator-triggered "Scan database" button) was the ONLY writer to
// site_db_size_history, so the 90-day DB-size trend chart and the fleet
// "90-day growth" read never populated for a site whose operator never
// clicked that button. Every enrolled site already pushes diagnostics daily
// and that push carries the current database size, so this tap is a second,
// automatic writer for the same append-only, idempotent table.
//
// Best-effort and silently non-fatal, mirroring ingestSiteTimezone just
// above: the 15-category upsert loop in IngestDiagnostics has already
// succeeded by the time this runs, and neither a missing/zero/unparseable
// size nor a downstream insert failure may fail the diagnostics push — the
// agent's daily cron push must always see a 2xx.
//
// scanned_at is the diagnostics push's own collected_at, not time.Now():
// site_db_size_history's uniqueness is (site_id, scanned_at), so reusing the
// push's own timestamp naturally dedups to one point per push — a retried or
// double-delivered push for the same collected_at is a no-op (ON CONFLICT DO
// NOTHING), never a duplicate point.
func (s *Service) ingestDBSizeHistory(ctx context.Context, tenantID, siteID uuid.UUID, payload json.RawMessage, collectedAt time.Time) {
	if s.dbSizeHistory == nil {
		return
	}
	dbSizeBytes, ok := dbSizeFromWPNative(payload)
	if !ok {
		return
	}
	_ = s.dbSizeHistory.RecordDBSizeHistoryFromDiagnostics(ctx, tenantID, siteID, dbSizeBytes, collectedAt)
}

// dbSizeFromWPNative extracts the database size in bytes from the wp_native
// category payload's wp-paths-sizes.fields.database_size.debug field.
//
// Shape (see apps/agent SizeProbe + class-diagnostics-command.php):
//
//	{
//	  "wp-paths-sizes": {
//	    "fields": {
//	      "database_size": { "value": "1.2 GB", "debug": 1288490188 }
//	    }
//	  }
//	}
//
// `debug` carries the WP_Debug_Data placeholder string "Loading&hellip;"
// (HTML-entity encoded) when the agent's SizeProbe has not yet resolved the
// size (pending/timeout status) — decoding that as flexInt64 fails, so this
// function correctly reports ok=false rather than recording a junk 0-byte
// point. Returns ok=false when the field is absent, non-numeric, or <= 0.
func dbSizeFromWPNative(payload json.RawMessage) (bytes int64, ok bool) {
	var v struct {
		WPPathsSizes struct {
			Fields struct {
				DatabaseSize struct {
					Debug flexInt64 `json:"debug"`
				} `json:"database_size"`
			} `json:"fields"`
		} `json:"wp-paths-sizes"`
	}
	if err := json.Unmarshal(payload, &v); err != nil {
		return 0, false
	}
	b := int64(v.WPPathsSizes.Fields.DatabaseSize.Debug)
	if b <= 0 {
		return 0, false
	}
	return b, true
}
