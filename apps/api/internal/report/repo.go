package report

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Repo is the persistence interface for report schedules and generated reports.
type Repo interface {
	// Schedule management — run under InTenantTx.
	GetSchedule(ctx context.Context, tenantID, clientID uuid.UUID) (Schedule, bool, error)
	UpsertSchedule(ctx context.Context, in UpsertScheduleInput) (Schedule, error)

	// Due-scan — run under InAgentTx (cross-tenant).
	ListDueSchedules(ctx context.Context, limit int32) ([]DueSchedule, error)
	ClaimAdvanceSchedule(ctx context.Context, tenantID, scheduleID uuid.UUID, newNextAt time.Time) (Schedule, error)

	// Report lifecycle — run under InTenantTx for operator paths, InAgentTx for worker.
	CreateReport(ctx context.Context, in CreateReportInput) (GeneratedReport, error)
	// CreateReportAgent creates a pending report row under InAgentTx (used by
	// the ScheduleScanWorker which runs cross-tenant).
	CreateReportAgent(ctx context.Context, in CreateReportInput) (GeneratedReport, error)
	MarkGenerating(ctx context.Context, tenantID, id uuid.UUID) (GeneratedReport, error)
	CompleteReport(ctx context.Context, tenantID, id uuid.UUID, htmlKey, pdfKey string, snapshot []byte) (GeneratedReport, error)
	FailReport(ctx context.Context, tenantID, id uuid.UUID, errMsg string) (GeneratedReport, error)
	GetReport(ctx context.Context, tenantID, clientID, id uuid.UUID) (GeneratedReport, error)
	ListReports(ctx context.Context, in ListReportsInput) (ListReportsResult, error)
	DeleteReport(ctx context.Context, tenantID, clientID, id uuid.UUID) error
	// GetActiveReport returns the most recent pending or generating report for
	// a client, or (GeneratedReport{}, false, nil) when none exists. Used by
	// GenerateNow to detect and return an already-in-flight report (abuse guard).
	GetActiveReport(ctx context.Context, tenantID, clientID uuid.UUID) (GeneratedReport, bool, error)

	// Agency metadata (InAgentTx or InTenantTx).
	GetTenantName(ctx context.Context, tenantID uuid.UUID) (string, error)

	// GetClientInfo fetches the ClientInfo fields needed for the report header
	// (name, company, logo_url, accent color, timezone) under InAgentTx.
	// Returns ErrNotFound when no client row exists for the given tenant.
	GetClientInfo(ctx context.Context, tenantID, clientID uuid.UUID) (ClientInfo, error)

	// GetClientTimezone returns the IANA timezone on the client's record,
	// which the schedule's send hour is in, or "" when the client has no row.
	// Operator path, under InTenantTx.
	GetClientTimezone(ctx context.Context, tenantID, clientID uuid.UUID) (string, error)
}

type pgRepo struct {
	pool *db.Pool
}

// NewRepo builds a Repo backed by the pgx pool.
func NewRepo(pool *db.Pool) Repo {
	return &pgRepo{pool: pool}
}

// ---------------------------------------------------------------------------
// Schedule management
// ---------------------------------------------------------------------------

func (r *pgRepo) GetSchedule(ctx context.Context, tenantID, clientID uuid.UUID) (Schedule, bool, error) {
	var out Schedule
	var found bool
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).GetReportSchedule(ctx, sqlc.GetReportScheduleParams{
			ClientID: clientID,
			TenantID: tenantID,
		})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				found = false
				return nil
			}
			return domain.Internal("report_schedule_get_failed", "failed to load report schedule").WithCause(qerr)
		}
		found = true
		var parseErr error
		out, parseErr = rowToSchedule(row)
		return parseErr
	})
	return out, found, err
}

func (r *pgRepo) UpsertSchedule(ctx context.Context, in UpsertScheduleInput) (Schedule, error) {
	var out Schedule
	err := r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		recipJSON, err := json.Marshal(in.Recipients)
		if err != nil {
			return domain.Internal("report_schedule_marshal_failed", "failed to marshal recipients").WithCause(err)
		}
		sectJSON, err := json.Marshal(in.Sections)
		if err != nil {
			return domain.Internal("report_schedule_marshal_failed", "failed to marshal sections").WithCause(err)
		}
		var nextRunAt pgtype.Timestamptz
		if in.NextRunAt != nil {
			nextRunAt = pgtype.Timestamptz{Time: *in.NextRunAt, Valid: true}
		}
		row, qerr := sqlc.New(tx).UpsertReportSchedule(ctx, sqlc.UpsertReportScheduleParams{
			TenantID:         in.TenantID,
			ClientID:         in.ClientID,
			Enabled:          in.Enabled,
			Cadence:          in.Cadence,
			SendDay:          int32(in.SendDay),
			SendHour:         int32(in.SendHour),
			Recipients:       recipJSON,
			Sections:         sectJSON,
			IntroText:        in.IntroText,
			ClosingText:      in.ClosingText,
			PoweredByRemoved: in.PoweredByRemoved,
			NextRunAt:        nextRunAt,
		})
		if qerr != nil {
			return domain.Internal("report_schedule_upsert_failed", "failed to upsert report schedule").WithCause(qerr)
		}
		var parseErr error
		out, parseErr = rowToSchedule(row)
		return parseErr
	})
	return out, err
}

// ---------------------------------------------------------------------------
// Due-scan (InAgentTx — cross-tenant)
// ---------------------------------------------------------------------------

func (r *pgRepo) ListDueSchedules(ctx context.Context, limit int32) ([]DueSchedule, error) {
	var out []DueSchedule
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).ListDueReportSchedules(ctx, limit)
		if qerr != nil {
			return qerr
		}
		out = make([]DueSchedule, 0, len(rows))
		for _, row := range rows {
			sched, err := rowToSchedule(sqlc.ReportSchedule{
				ID:               row.ID,
				TenantID:         row.TenantID,
				ClientID:         row.ClientID,
				Enabled:          row.Enabled,
				Cadence:          row.Cadence,
				SendDay:          row.SendDay,
				SendHour:         row.SendHour,
				Recipients:       row.Recipients,
				Sections:         row.Sections,
				IntroText:        row.IntroText,
				ClosingText:      row.ClosingText,
				PoweredByRemoved: row.PoweredByRemoved,
				NextRunAt:        row.NextRunAt,
				LastRunAt:        row.LastRunAt,
				CreatedAt:        row.CreatedAt,
				UpdatedAt:        row.UpdatedAt,
			})
			if err != nil {
				continue
			}
			d := DueSchedule{
				Schedule:       sched,
				ClientName:     row.ClientName,
				ClientTimezone: row.ClientTimezone,
			}
			if row.ClientContactEmail != nil {
				d.ClientContactEmail = row.ClientContactEmail
			}
			out = append(out, d)
		}
		return nil
	})
	return out, err
}

func (r *pgRepo) ClaimAdvanceSchedule(ctx context.Context, tenantID, scheduleID uuid.UUID, newNextAt time.Time) (Schedule, error) {
	var out Schedule
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).ClaimAdvanceReportSchedule(ctx, sqlc.ClaimAdvanceReportScheduleParams{
			ID:           scheduleID,
			TenantID:     tenantID,
			NewNextRunAt: pgtype.Timestamptz{Time: newNextAt, Valid: true},
		})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return qerr
		}
		var parseErr error
		out, parseErr = rowToSchedule(row)
		return parseErr
	})
	return out, err
}

// ---------------------------------------------------------------------------
// Report lifecycle
// ---------------------------------------------------------------------------

func (r *pgRepo) CreateReport(ctx context.Context, in CreateReportInput) (GeneratedReport, error) {
	var out GeneratedReport
	err := r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		var schedID pgtype.UUID
		if in.ScheduleID != nil {
			schedID = pgtype.UUID{Bytes: [16]byte(*in.ScheduleID), Valid: true}
		}
		row, qerr := sqlc.New(tx).CreateReport(ctx, sqlc.CreateReportParams{
			TenantID:    in.TenantID,
			ClientID:    in.ClientID,
			ScheduleID:  schedID,
			PeriodStart: in.PeriodStart,
			PeriodEnd:   in.PeriodEnd,
		})
		if qerr != nil {
			return domain.Internal("report_create_failed", "failed to create report").WithCause(qerr)
		}
		out = rowToReport(row)
		return nil
	})
	return out, err
}

// CreateReportAgent creates a pending report row under InAgentTx (used by the
// ScheduleScanWorker, which runs cross-tenant).
func (r *pgRepo) CreateReportAgent(ctx context.Context, in CreateReportInput) (GeneratedReport, error) {
	var out GeneratedReport
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		var schedID pgtype.UUID
		if in.ScheduleID != nil {
			schedID = pgtype.UUID{Bytes: [16]byte(*in.ScheduleID), Valid: true}
		}
		row, qerr := sqlc.New(tx).CreateReport(ctx, sqlc.CreateReportParams{
			TenantID:    in.TenantID,
			ClientID:    in.ClientID,
			ScheduleID:  schedID,
			PeriodStart: in.PeriodStart,
			PeriodEnd:   in.PeriodEnd,
		})
		if qerr != nil {
			return domain.Internal("report_create_failed", "failed to create report").WithCause(qerr)
		}
		out = rowToReport(row)
		return nil
	})
	return out, err
}

func (r *pgRepo) MarkGenerating(ctx context.Context, tenantID, id uuid.UUID) (GeneratedReport, error) {
	var out GeneratedReport
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).MarkReportGenerating(ctx, sqlc.MarkReportGeneratingParams{
			ID:       id,
			TenantID: tenantID,
		})
		if qerr != nil {
			return domain.Internal("report_mark_generating_failed", "failed to mark report generating").WithCause(qerr)
		}
		out = rowToReport(row)
		return nil
	})
	return out, err
}

func (r *pgRepo) CompleteReport(ctx context.Context, tenantID, id uuid.UUID, htmlKey, pdfKey string, snapshot []byte) (GeneratedReport, error) {
	var out GeneratedReport
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).CompleteReport(ctx, sqlc.CompleteReportParams{
			ID:           id,
			TenantID:     tenantID,
			HtmlBlobKey:  htmlKey,
			PdfBlobKey:   pdfKey,
			DataSnapshot: snapshot,
		})
		if qerr != nil {
			return domain.Internal("report_complete_failed", "failed to complete report").WithCause(qerr)
		}
		out = rowToReport(row)
		return nil
	})
	return out, err
}

func (r *pgRepo) FailReport(ctx context.Context, tenantID, id uuid.UUID, errMsg string) (GeneratedReport, error) {
	var out GeneratedReport
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).FailReport(ctx, sqlc.FailReportParams{
			ID:       id,
			TenantID: tenantID,
			Error:    errMsg,
		})
		if qerr != nil {
			return domain.Internal("report_fail_failed", "failed to mark report failed").WithCause(qerr)
		}
		out = rowToReport(row)
		return nil
	})
	return out, err
}

func (r *pgRepo) GetReport(ctx context.Context, tenantID, clientID, id uuid.UUID) (GeneratedReport, error) {
	var out GeneratedReport
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).GetReport(ctx, sqlc.GetReportParams{
			ID:       id,
			TenantID: tenantID,
			ClientID: clientID,
		})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return domain.NotFound("report_not_found", "report not found")
			}
			return domain.Internal("report_get_failed", "failed to load report").WithCause(qerr)
		}
		out = rowToReport(row)
		return nil
	})
	return out, err
}

func (r *pgRepo) ListReports(ctx context.Context, in ListReportsInput) (ListReportsResult, error) {
	var rows []sqlc.GeneratedReport
	err := r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		var cursorAt pgtype.Timestamptz
		var cursorID uuid.UUID
		if in.CursorCreatedAt != nil && in.CursorID != nil {
			cursorAt = pgtype.Timestamptz{Time: *in.CursorCreatedAt, Valid: true}
			cursorID = *in.CursorID
		}
		var qerr error
		rows, qerr = sqlc.New(tx).ListReports(ctx, sqlc.ListReportsParams{
			TenantID:        in.TenantID,
			ClientID:        in.ClientID,
			CursorCreatedAt: cursorAt,
			CursorID:        cursorID,
			RowLimit:        in.Limit + 1, // fetch one extra to detect next page
		})
		return qerr
	})
	if err != nil {
		return ListReportsResult{}, domain.Internal("report_list_failed", "failed to list reports").WithCause(err)
	}

	limit := int(in.Limit)
	var nextCursor string
	if len(rows) > limit {
		last := rows[limit-1]
		nextCursor = encodeCursor(last.CreatedAt, last.ID)
		rows = rows[:limit]
	}

	items := make([]GeneratedReport, 0, len(rows))
	for _, row := range rows {
		items = append(items, rowToReport(row))
	}
	return ListReportsResult{Items: items, NextCursor: nextCursor}, nil
}

func (r *pgRepo) DeleteReport(ctx context.Context, tenantID, clientID, id uuid.UUID) error {
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		n, qerr := sqlc.New(tx).DeleteReport(ctx, sqlc.DeleteReportParams{
			ID:       id,
			TenantID: tenantID,
			ClientID: clientID,
		})
		if qerr != nil {
			return domain.Internal("report_delete_failed", "failed to delete report").WithCause(qerr)
		}
		if n == 0 {
			return domain.NotFound("report_not_found", "report not found")
		}
		return nil
	})
}

func (r *pgRepo) GetTenantName(ctx context.Context, tenantID uuid.UUID) (string, error) {
	var name string
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		var qerr error
		name, qerr = sqlc.New(tx).GetTenantName(ctx, tenantID)
		return qerr
	})
	return name, err
}

func (r *pgRepo) GetClientInfo(ctx context.Context, tenantID, clientID uuid.UUID) (ClientInfo, error) {
	var out ClientInfo
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).GetClient(ctx, sqlc.GetClientParams{
			ID:       clientID,
			TenantID: tenantID,
		})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return domain.Internal("client_get_failed", "failed to load client info").WithCause(qerr)
		}
		out = ClientInfo{
			Name: row.Name,
		}
		if row.Company != nil {
			out.Company = *row.Company
		}
		if row.LogoUrl != nil {
			out.LogoURL = *row.LogoUrl
		}
		if row.Color != nil {
			out.Color = *row.Color
		}
		return nil
	})
	return out, err
}

func (r *pgRepo) GetClientTimezone(ctx context.Context, tenantID, clientID uuid.UUID) (string, error) {
	var tz string
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).GetClientWithTimezone(ctx, sqlc.GetClientWithTimezoneParams{
			ID:       clientID,
			TenantID: tenantID,
		})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return nil
			}
			return domain.Internal("report_client_timezone_failed", "failed to load the client's timezone").WithCause(qerr)
		}
		tz = row.Timezone
		return nil
	})
	return tz, err
}

// getActiveReportQuery selects the most recent pending or generating report
// for a client. No sqlc wrapper — avoids a regen cycle for this single extra
// query whose filter (status IN (...)) doesn't fit the existing ListReports
// keyset shape.
const getActiveReportQuery = `
SELECT id, tenant_id, client_id, schedule_id, period_start, period_end,
       status, html_blob_key, pdf_blob_key, error, created_at, updated_at,
       completed_at
FROM generated_reports
WHERE tenant_id = $1
  AND client_id = $2
  AND status IN ('pending', 'generating')
ORDER BY created_at DESC, id DESC
LIMIT 1
`

func (r *pgRepo) GetActiveReport(ctx context.Context, tenantID, clientID uuid.UUID) (GeneratedReport, bool, error) {
	var out GeneratedReport
	var found bool
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, qerr := tx.Query(ctx, getActiveReportQuery, tenantID, clientID)
		if qerr != nil {
			return domain.Internal("report_active_get_failed", "failed to query active report").WithCause(qerr)
		}
		defer rows.Close()
		for rows.Next() {
			found = true
			var schedID pgtype.UUID
			var htmlKey, pdfKey, errMsg string
			var completedAt pgtype.Timestamptz
			if scanErr := rows.Scan(
				&out.ID,
				&out.TenantID,
				&out.ClientID,
				&schedID,
				&out.PeriodStart,
				&out.PeriodEnd,
				&out.Status,
				&htmlKey,
				&pdfKey,
				&errMsg,
				&out.CreatedAt,
				&out.UpdatedAt,
				&completedAt,
			); scanErr != nil {
				return domain.Internal("report_active_scan_failed", "failed to scan active report").WithCause(scanErr)
			}
			if schedID.Valid {
				id := uuid.UUID(schedID.Bytes)
				out.ScheduleID = &id
			}
			out.HTMLBlobKey = htmlKey
			out.PDFBlobKey = pdfKey
			out.Error = errMsg
			if completedAt.Valid {
				t := completedAt.Time
				out.CompletedAt = &t
			}
		}
		return rows.Err()
	})
	return out, found, err
}

// ---------------------------------------------------------------------------
// Cursor helpers
// ---------------------------------------------------------------------------

// encodeCursor encodes a (createdAt, id) keyset cursor as base64("RFC3339Nano|uuid").
func encodeCursor(createdAt time.Time, id uuid.UUID) string {
	raw := createdAt.UTC().Format(time.RFC3339Nano) + "|" + id.String()
	return base64.URLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor decodes a cursor produced by encodeCursor.
func DecodeCursor(cursor string) (time.Time, uuid.UUID, error) {
	b, err := base64.URLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("invalid cursor encoding: %w", err)
	}
	parts := strings.SplitN(string(b), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, uuid.Nil, fmt.Errorf("invalid cursor format")
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("invalid cursor timestamp: %w", err)
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("invalid cursor id: %w", err)
	}
	return t, id, nil
}

// ---------------------------------------------------------------------------
// Row mapping helpers
// ---------------------------------------------------------------------------

func rowToSchedule(row sqlc.ReportSchedule) (Schedule, error) {
	var recipients []string
	if len(row.Recipients) > 0 {
		if err := json.Unmarshal(row.Recipients, &recipients); err != nil {
			recipients = nil
		}
	}
	if recipients == nil {
		recipients = []string{}
	}

	sections := DefaultSectionFlags()
	if len(row.Sections) > 0 {
		_ = json.Unmarshal(row.Sections, &sections)
	}

	s := Schedule{
		ID:               row.ID,
		TenantID:         row.TenantID,
		ClientID:         row.ClientID,
		Enabled:          row.Enabled,
		Cadence:          row.Cadence,
		SendDay:          int(row.SendDay),
		SendHour:         int(row.SendHour),
		Recipients:       recipients,
		Sections:         sections,
		IntroText:        row.IntroText,
		ClosingText:      row.ClosingText,
		PoweredByRemoved: row.PoweredByRemoved,
		CreatedAt:        row.CreatedAt,
		UpdatedAt:        row.UpdatedAt,
	}
	if row.NextRunAt.Valid {
		t := row.NextRunAt.Time
		s.NextRunAt = &t
	}
	if row.LastRunAt.Valid {
		t := row.LastRunAt.Time
		s.LastRunAt = &t
	}
	return s, nil
}

func rowToReport(row sqlc.GeneratedReport) GeneratedReport {
	r := GeneratedReport{
		ID:          row.ID,
		TenantID:    row.TenantID,
		ClientID:    row.ClientID,
		PeriodStart: row.PeriodStart,
		PeriodEnd:   row.PeriodEnd,
		Status:      row.Status,
		HTMLBlobKey: row.HtmlBlobKey,
		PDFBlobKey:  row.PdfBlobKey,
		Error:       row.Error,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
	}
	if row.ScheduleID.Valid {
		id := uuid.UUID(row.ScheduleID.Bytes)
		r.ScheduleID = &id
	}
	if row.CompletedAt.Valid {
		t := row.CompletedAt.Time
		r.CompletedAt = &t
	}
	return r
}

// ---------------------------------------------------------------------------
// Aggregator update sources (wired in cmd/wpmgr/main.go)
// ---------------------------------------------------------------------------

// UpdateReportStatsSource is the production Sources.GetUpdateReportStats. It
// runs under InAgentTx, where RLS admits every tenant's update_tasks rows, so
// the query's tenant_id predicate is the tenant boundary.
func UpdateReportStatsSource(pool *db.Pool) func(ctx context.Context, tenantID, siteID uuid.UUID, from, to time.Time) ([]sqlc.GetUpdateReportStatsRow, error) {
	return func(ctx context.Context, tenantID, siteID uuid.UUID, from, to time.Time) ([]sqlc.GetUpdateReportStatsRow, error) {
		var rows []sqlc.GetUpdateReportStatsRow
		err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
			var qerr error
			rows, qerr = sqlc.New(tx).GetUpdateReportStats(ctx, sqlc.GetUpdateReportStatsParams{
				TenantID: tenantID,
				SiteID:   siteID,
				FromTime: pgtype.Timestamptz{Time: from, Valid: true},
				ToTime:   pgtype.Timestamptz{Time: to, Valid: true},
			})
			return qerr
		})
		return rows, err
	}
}

// ListReportUpdateItemsSource is the production Sources.ListReportUpdateItems
// (GH #969 version table). It is called once per site with that one site's
// id, which ListClientSites returned for this client: under InAgentTx the
// tenant_id predicate is the tenant boundary and the site id list is the
// client boundary.
func ListReportUpdateItemsSource(pool *db.Pool) func(ctx context.Context, tenantID, siteID uuid.UUID, from, to time.Time, limit int32) ([]sqlc.ListReportUpdateItemsRow, error) {
	return func(ctx context.Context, tenantID, siteID uuid.UUID, from, to time.Time, limit int32) ([]sqlc.ListReportUpdateItemsRow, error) {
		var rows []sqlc.ListReportUpdateItemsRow
		err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
			var qerr error
			rows, qerr = sqlc.New(tx).ListReportUpdateItems(ctx, sqlc.ListReportUpdateItemsParams{
				TenantID: tenantID,
				SiteIds:  []uuid.UUID{siteID},
				FromTime: pgtype.Timestamptz{Time: from, Valid: true},
				ToTime:   pgtype.Timestamptz{Time: to, Valid: true},
				RowLimit: limit,
			})
			return qerr
		})
		return rows, err
	}
}
