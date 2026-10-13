package abilityrequest

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/riverqueue/river/rivertype"
)

type retentionBatchStub struct {
	batches []sqlc.ClearAbilityRequestContentRow
	inputs  []sqlc.ClearAbilityRequestContentParams
	limit   int32
	counts  []int32
	calls   int
	err     error
	full    bool
	failed  int32
}

func (s *retentionBatchStub) ClearAbilityRequestContent(_ context.Context, params sqlc.ClearAbilityRequestContentParams) (sqlc.ClearAbilityRequestContentRow, error) {
	s.calls++
	s.inputs = append(s.inputs, params)
	limit := s.limit
	if limit == 0 {
		limit = contentRetentionBatchSize
	}
	if params.BatchSize != limit {
		return sqlc.ClearAbilityRequestContentRow{}, errors.New("wrong batch size")
	}
	if s.err != nil {
		return sqlc.ClearAbilityRequestContentRow{}, s.err
	}
	if s.full {
		return s.withCursor(sqlc.ClearAbilityRequestContentRow{Cleared: limit, Examined: limit}), nil
	}
	if len(s.batches) > 0 {
		batch := s.batches[0]
		s.batches = s.batches[1:]
		return s.withCursor(batch), nil
	}
	if len(s.counts) == 0 {
		return sqlc.ClearAbilityRequestContentRow{}, errors.New("unexpected batch")
	}
	n := s.counts[0]
	s.counts = s.counts[1:]
	return s.withCursor(sqlc.ClearAbilityRequestContentRow{Cleared: n, Failed: s.failed}), nil
}

func (s *retentionBatchStub) withCursor(batch sqlc.ClearAbilityRequestContentRow) sqlc.ClearAbilityRequestContentRow {
	if batch.Examined == 0 {
		batch.Examined = batch.Cleared + batch.Failed
	}
	if batch.Examined > 0 && batch.AfterFinished.IsZero() {
		batch.AfterFinished = time.Unix(int64(s.calls), 0)
		batch.AfterID = uuid.MustParse("00000000-0000-0000-0000-000000000001")
	}
	return batch
}

func TestContentRetentionWorkerContinuesPastFailures(t *testing.T) {
	var log bytes.Buffer
	q := &retentionBatchStub{limit: 2, batches: []sqlc.ClearAbilityRequestContentRow{
		{Failed: 2},
		{Failed: 1, Cleared: 1},
		{Cleared: 1},
	}}
	w := &ContentRetentionWorker{queries: q, BatchSize: 2, logger: slog.New(slog.NewJSONHandler(&log, nil))}
	if err := w.Work(context.Background(), nil); err == nil || q.calls != 3 {
		t.Fatalf("sweep stopped before later rows: calls=%d err=%v", q.calls, err)
	}
	if !strings.Contains(log.String(), `"failed":3`) || !strings.Contains(log.String(), `"cleared":2`) {
		t.Fatalf("sweep counts: %s", log.String())
	}
	for i, params := range q.inputs {
		if i == 0 {
			if params.AfterFinished.Valid || params.AfterID.Valid {
				t.Fatal("new sweep did not start with a null cursor")
			}
		} else if !params.AfterFinished.Valid || !params.AfterID.Valid || !params.AfterFinished.Time.Equal(time.Unix(int64(i), 0)) {
			t.Fatalf("batch %d did not continue from the previous cursor: %+v", i, params)
		}
	}
	q.batches = []sqlc.ClearAbilityRequestContentRow{{Failed: 1}}
	if err := w.Work(context.Background(), nil); err == nil {
		t.Fatal("next run did not report the row failure")
	}
	if params := q.inputs[3]; params.AfterFinished.Valid || params.AfterID.Valid {
		t.Fatal("next run did not retry from the beginning")
	}
}

func TestContentRetentionWorkerReportsRowFailures(t *testing.T) {
	for _, cleared := range []int32{0, 3} {
		var log bytes.Buffer
		q := &retentionBatchStub{counts: []int32{cleared}, failed: 2}
		w := &ContentRetentionWorker{queries: q, logger: slog.New(slog.NewJSONHandler(&log, nil))}
		if err := w.Work(context.Background(), nil); err == nil || q.calls != 1 {
			t.Fatalf("row failure did not fail the job: calls=%d err=%v", q.calls, err)
		}
		if !strings.Contains(log.String(), `"cleared":`+strconv.Itoa(int(cleared))) || !strings.Contains(log.String(), `"failed":2`) {
			t.Fatalf("missing batch counts: %s", log.String())
		}
	}
}

func TestContentRetentionWorkerBatches(t *testing.T) {
	for _, counts := range [][]int32{{0}, {contentRetentionBatchSize, contentRetentionBatchSize, 7}} {
		var log bytes.Buffer
		q := &retentionBatchStub{counts: append([]int32(nil), counts...)}
		w := &ContentRetentionWorker{queries: q, logger: slog.New(slog.NewJSONHandler(&log, nil))}
		if err := w.Work(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		if q.calls != len(counts) {
			t.Fatalf("calls=%d want=%d", q.calls, len(counts))
		}
		var want int32
		for _, n := range counts {
			want += n
		}
		if !strings.Contains(log.String(), `"cleared":`+strconv.Itoa(int(want))) {
			t.Fatalf("count log = %s", log.String())
		}
		if !strings.Contains(log.String(), `"failed":0`) {
			t.Fatalf("missing failed count in log = %s", log.String())
		}
	}
}

func TestContentRetentionWorkerBoundAndErrors(t *testing.T) {
	// A sweep visits every full batch before its first partial batch.
	q := &retentionBatchStub{counts: append(make([]int32, 101), 0)}
	for i := range q.counts[:len(q.counts)-1] {
		q.counts[i] = contentRetentionBatchSize
	}
	w := &ContentRetentionWorker{queries: q}
	if err := w.Work(context.Background(), nil); err != nil || q.calls != len(q.inputs) || q.calls != 102 {
		t.Fatalf("sweep did not finish its batches: calls=%d err=%v", q.calls, err)
	}
	q = &retentionBatchStub{err: errors.New("database row contains private request words")}
	w.queries = q
	err := w.Work(context.Background(), nil)
	if err == nil || strings.Contains(err.Error(), "private") || q.calls != 1 {
		t.Fatalf("failed batch: calls=%d err=%v", q.calls, err)
	}
	q = &retentionBatchStub{counts: []int32{-1}}
	w.queries = q
	if err := w.Work(context.Background(), nil); err == nil {
		t.Fatal("invalid count passed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	q = &retentionBatchStub{full: true}
	w.queries = q
	if err := w.Work(ctx, nil); !errors.Is(err, context.Canceled) || q.calls != 0 {
		t.Fatalf("cancellation: calls=%d err=%v", q.calls, err)
	}
}

func TestContentRetentionWorkerContinuesPastSkippedRows(t *testing.T) {
	q := &retentionBatchStub{limit: 2, batches: []sqlc.ClearAbilityRequestContentRow{
		{Examined: 2}, {Cleared: 1},
	}}
	w := &ContentRetentionWorker{queries: q, BatchSize: 2}
	if err := w.Work(context.Background(), nil); err != nil || q.calls != 2 {
		t.Fatalf("skipped rows stopped the sweep: calls=%d err=%v", q.calls, err)
	}
}

func TestContentRetentionWorkerCursorProgress(t *testing.T) {
	finished := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	firstID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	laterID := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	for _, nextID := range []uuid.UUID{firstID, laterID} {
		q := &retentionBatchStub{limit: 2, batches: []sqlc.ClearAbilityRequestContentRow{
			{Cleared: 2, Examined: 2, AfterFinished: finished, AfterID: firstID},
			{Cleared: 1, Examined: 1, AfterFinished: finished, AfterID: nextID},
		}}
		w := &ContentRetentionWorker{queries: q, BatchSize: 2}
		err := w.Work(context.Background(), nil)
		if (err == nil) != (nextID == laterID) {
			t.Fatalf("cursor progress id=%s: %v", nextID, err)
		}
		if !q.inputs[1].AfterFinished.Time.Equal(finished) || q.inputs[1].AfterID.Bytes != [16]byte(firstID) {
			t.Fatal("cursor did not carry both the timestamp and id")
		}
	}
}

func TestContentRetentionJobUniqueness(t *testing.T) {
	opts := (ContentRetentionArgs{}).InsertOpts()
	if !slices.Contains(opts.UniqueOpts.ByState, rivertype.JobStateRunning) {
		t.Fatal("running sweep is not unique")
	}
}

func TestContentRetentionPeriodicSchedule(t *testing.T) {
	job := reflect.ValueOf(NewContentRetentionPeriodicJob()).Elem()
	// River's configuration fields are private; read the constructed options
	// and schedule so the test checks what the scheduler actually receives.
	t.Run("runs when the scheduler starts", func(t *testing.T) {
		if !job.FieldByName("opts").Elem().FieldByName("RunOnStart").Bool() {
			t.Fatal("retention periodic job has RunOnStart false")
		}
	})
	t.Run("runs hourly", func(t *testing.T) {
		interval := time.Duration(job.FieldByName("scheduleFunc").Elem().Elem().FieldByName("interval").Int())
		if interval != time.Hour {
			t.Fatalf("retention periodic interval=%v, want %v", interval, time.Hour)
		}
	})
}
