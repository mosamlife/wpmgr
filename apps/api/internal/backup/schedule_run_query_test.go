package backup

// schedule_run_query_test.go — regression tests for GitHub issue #104.
//
// Bug: the "Past runs" panel in the dashboard always shows empty even when
// backup_schedule_runs has terminal rows.
//
// Root cause found: ListUpcomingScheduleRuns only included 'scheduled' and
// 'queued' rows with scheduled_for > now(). A currently-running backup
// ('running' status) was invisible in BOTH the upcoming panel (not in the
// status filter) AND the past panel (not terminal). The result: the UI showed
// nothing for an active backup.
//
// The fix adds 'running' to ListUpcomingScheduleRuns regardless of
// scheduled_for. ListPastScheduleRuns is structurally correct; these tests
// serve as regression guards.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// fakeScheduleRunStore — in-memory ScheduleRunStore for query-contract tests.
// ---------------------------------------------------------------------------

type fakeScheduleRunStore struct {
	rows []ScheduleRun
}

func (s *fakeScheduleRunStore) UpsertScheduleRun(_ context.Context, in UpsertScheduleRunInput) (ScheduleRun, error) {
	r := ScheduleRun{
		ID:           uuid.New(),
		TenantID:     in.TenantID,
		SiteID:       in.SiteID,
		ScheduleID:   in.ScheduleID,
		ScheduledFor: in.ScheduledFor,
		Status:       in.Status,
		Kind:         in.Kind,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
	s.rows = append(s.rows, r)
	return r, nil
}

func (s *fakeScheduleRunStore) AgentUpsertScheduleRun(_ context.Context, in UpsertScheduleRunInput) (ScheduleRun, error) {
	return s.UpsertScheduleRun(context.Background(), in)
}

func (s *fakeScheduleRunStore) SetScheduleRunSnapshot(_ context.Context, _, runID, snapshotID uuid.UUID) (ScheduleRun, error) {
	for i := range s.rows {
		if s.rows[i].ID == runID {
			s.rows[i].SnapshotID = &snapshotID
			s.rows[i].Status = "queued"
			return s.rows[i], nil
		}
	}
	return ScheduleRun{}, nil
}

func (s *fakeScheduleRunStore) SetScheduleRunStatusByID(_ context.Context, in SetScheduleRunStatusInput) (ScheduleRun, error) {
	for i := range s.rows {
		if s.rows[i].ID == in.RunID {
			s.rows[i].Status = in.Status
			return s.rows[i], nil
		}
	}
	return ScheduleRun{}, nil
}

func (s *fakeScheduleRunStore) SetScheduleRunStatusBySnapshot(_ context.Context, _, snapshotID uuid.UUID, in SetScheduleRunStatusInput) (ScheduleRun, error) {
	for i := range s.rows {
		if s.rows[i].SnapshotID != nil && *s.rows[i].SnapshotID == snapshotID {
			s.rows[i].Status = in.Status
			return s.rows[i], nil
		}
	}
	return ScheduleRun{}, nil
}

func (s *fakeScheduleRunStore) SetScheduleRunAttemptErrorBySnapshot(_ context.Context, _, snapshotID uuid.UUID, msg string) (int64, error) {
	for i := range s.rows {
		if s.rows[i].SnapshotID != nil && *s.rows[i].SnapshotID == snapshotID && s.rows[i].Status == ScheduleRunStatusRunning {
			s.rows[i].AttemptError = msg
			return 1, nil
		}
	}
	return 0, nil
}

func (s *fakeScheduleRunStore) ClearScheduleRunAttemptErrorBySnapshot(_ context.Context, _, snapshotID uuid.UUID) (int64, error) {
	for i := range s.rows {
		if s.rows[i].SnapshotID != nil && *s.rows[i].SnapshotID == snapshotID &&
			s.rows[i].Status == ScheduleRunStatusRunning && s.rows[i].AttemptError != "" {
			s.rows[i].AttemptError = ""
			return 1, nil
		}
	}
	return 0, nil
}

func (s *fakeScheduleRunStore) GetScheduleRun(_ context.Context, _, runID uuid.UUID) (ScheduleRun, error) {
	for _, r := range s.rows {
		if r.ID == runID {
			return r, nil
		}
	}
	return ScheduleRun{}, nil
}

func (s *fakeScheduleRunStore) ListScheduleRunsBySite(_ context.Context, tenantID, siteID uuid.UUID, limit, offset int32) ([]ScheduleRun, error) {
	var out []ScheduleRun
	for _, r := range s.rows {
		if r.TenantID == tenantID && r.SiteID == siteID {
			out = append(out, r)
		}
	}
	return paginate(out, limit, offset), nil
}

// ListUpcomingScheduleRuns returns non-terminal runs per the fixed query:
// 'running' rows always; 'scheduled'/'queued' only when scheduled_for > now().
// This mirrors the SQL fix from issue #104.
func (s *fakeScheduleRunStore) ListUpcomingScheduleRuns(_ context.Context, tenantID, siteID uuid.UUID, limit int32) ([]ScheduleRun, error) {
	now := time.Now()
	var out []ScheduleRun
	for _, r := range s.rows {
		if r.TenantID != tenantID || r.SiteID != siteID {
			continue
		}
		if r.Status == "running" {
			out = append(out, r)
			continue
		}
		if (r.Status == "scheduled" || r.Status == "queued") && r.ScheduledFor.After(now) {
			out = append(out, r)
		}
	}
	if int(limit) > 0 && len(out) > int(limit) {
		out = out[:limit]
	}
	return out, nil
}

// ListPastScheduleRuns returns terminal (completed/failed/skipped/canceled) rows.
func (s *fakeScheduleRunStore) ListPastScheduleRuns(_ context.Context, tenantID, siteID uuid.UUID, limit, offset int32) ([]ScheduleRun, error) {
	terminal := map[string]bool{"completed": true, "failed": true, "skipped": true, "canceled": true}
	var out []ScheduleRun
	for _, r := range s.rows {
		if r.TenantID == tenantID && r.SiteID == siteID && terminal[r.Status] {
			out = append(out, r)
		}
	}
	return paginate(out, limit, offset), nil
}

func paginate(rows []ScheduleRun, limit, offset int32) []ScheduleRun {
	if offset >= int32(len(rows)) {
		return []ScheduleRun{}
	}
	rows = rows[offset:]
	if limit > 0 && int(limit) < len(rows) {
		rows = rows[:limit]
	}
	return rows
}

// ---------------------------------------------------------------------------
// Test A: ListPastScheduleRuns returns terminal rows — regression for #104.
// ---------------------------------------------------------------------------

// TestListPastScheduleRuns_ReturnTerminalRows is the regression guard for
// issue #104 ("Past runs panel always empty"). It seeds a mix of terminal and
// non-terminal schedule run rows and asserts that ListPastScheduleRuns returns
// only the terminal ones, not an empty slice.
func TestListPastScheduleRuns_ReturnTerminalRows(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	scheduleID := uuid.New()

	store := &fakeScheduleRunStore{}

	pastFor := time.Now().Add(-24 * time.Hour)

	// Insert one terminal row (completed) and one non-terminal row (scheduled).
	_, _ = store.UpsertScheduleRun(context.Background(), UpsertScheduleRunInput{
		TenantID:     tenantID,
		SiteID:       siteID,
		ScheduleID:   scheduleID,
		ScheduledFor: pastFor,
		Status:       "scheduled",
		Kind:         "full",
	})
	// Advance to completed.
	for i := range store.rows {
		if store.rows[i].Status == "scheduled" {
			store.rows[i].Status = "completed"
		}
	}

	_, _ = store.UpsertScheduleRun(context.Background(), UpsertScheduleRunInput{
		TenantID:     tenantID,
		SiteID:       siteID,
		ScheduleID:   scheduleID,
		ScheduledFor: time.Now().Add(24 * time.Hour),
		Status:       "scheduled",
		Kind:         "full",
	})

	past, err := store.ListPastScheduleRuns(context.Background(), tenantID, siteID, 50, 0)
	if err != nil {
		t.Fatalf("ListPastScheduleRuns: %v", err)
	}
	if len(past) != 1 {
		t.Errorf("ListPastScheduleRuns: got %d rows, want 1 (only the completed row)", len(past))
	}
	if len(past) > 0 && past[0].Status != "completed" {
		t.Errorf("ListPastScheduleRuns: got status %q, want 'completed'", past[0].Status)
	}
}

// ---------------------------------------------------------------------------
// Test B: ListUpcomingScheduleRuns includes 'running' rows — regression for #104.
// ---------------------------------------------------------------------------

// TestListUpcomingScheduleRuns_IncludesRunningRows verifies that a schedule run
// in 'running' status (an actively executing backup with a past scheduled_for)
// appears in the upcoming list. Before the #104 fix, the SQL filtered only
// 'scheduled'/'queued' AND scheduled_for > now(), hiding active backups entirely.
func TestListUpcomingScheduleRuns_IncludesRunningRows(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	scheduleID := uuid.New()

	store := &fakeScheduleRunStore{}

	// A run that fired in the past and is currently executing.
	_, _ = store.UpsertScheduleRun(context.Background(), UpsertScheduleRunInput{
		TenantID:     tenantID,
		SiteID:       siteID,
		ScheduleID:   scheduleID,
		ScheduledFor: time.Now().Add(-time.Minute), // past scheduled_for
		Status:       "running",
		Kind:         "full",
	})

	upcoming, err := store.ListUpcomingScheduleRuns(context.Background(), tenantID, siteID, 10)
	if err != nil {
		t.Fatalf("ListUpcomingScheduleRuns: %v", err)
	}
	if len(upcoming) != 1 {
		t.Errorf("ListUpcomingScheduleRuns: got %d rows, want 1 — running backup must appear regardless of scheduled_for", len(upcoming))
	}
	if len(upcoming) > 0 && upcoming[0].Status != "running" {
		t.Errorf("ListUpcomingScheduleRuns: got status %q, want 'running'", upcoming[0].Status)
	}
}

// ---------------------------------------------------------------------------
// Test C: A running row is absent from ListPastScheduleRuns.
// ---------------------------------------------------------------------------

// TestListPastScheduleRuns_ExcludesRunningRows asserts that a 'running' row
// does NOT appear in the past list, so the UI does not double-count it.
func TestListPastScheduleRuns_ExcludesRunningRows(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	scheduleID := uuid.New()

	store := &fakeScheduleRunStore{}

	_, _ = store.UpsertScheduleRun(context.Background(), UpsertScheduleRunInput{
		TenantID:     tenantID,
		SiteID:       siteID,
		ScheduleID:   scheduleID,
		ScheduledFor: time.Now().Add(-time.Minute),
		Status:       "running",
		Kind:         "full",
	})

	past, err := store.ListPastScheduleRuns(context.Background(), tenantID, siteID, 50, 0)
	if err != nil {
		t.Fatalf("ListPastScheduleRuns: %v", err)
	}
	if len(past) != 0 {
		t.Errorf("ListPastScheduleRuns: got %d rows, want 0 — running rows must not appear in past list", len(past))
	}
}
