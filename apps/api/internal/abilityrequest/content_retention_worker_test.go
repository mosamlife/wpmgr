package abilityrequest

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

type retentionBatchStub struct {
	counts []int32
	calls  int
	err    error
	full   bool
}

func (s *retentionBatchStub) ClearAbilityRequestContent(_ context.Context, limit int32) (int32, error) {
	s.calls++
	if limit != contentRetentionBatchSize {
		return 0, errors.New("wrong batch size")
	}
	if s.err != nil {
		return 0, s.err
	}
	if s.full {
		return limit, nil
	}
	if len(s.counts) == 0 {
		return 0, errors.New("unexpected batch")
	}
	n := s.counts[0]
	s.counts = s.counts[1:]
	return n, nil
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
	}
}

func TestContentRetentionWorkerBoundAndErrors(t *testing.T) {
	q := &retentionBatchStub{full: true}
	w := &ContentRetentionWorker{queries: q}
	var snooze *river.JobSnoozeError
	if err := w.Work(context.Background(), nil); !errors.As(err, &snooze) || q.calls != contentRetentionMaxBatches {
		t.Fatalf("unbounded work or no continuation: calls=%d err=%v", q.calls, err)
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

func TestContentRetentionJobUniqueness(t *testing.T) {
	opts := (ContentRetentionArgs{}).InsertOpts()
	if !slices.Contains(opts.UniqueOpts.ByState, rivertype.JobStateRunning) {
		t.Fatal("running sweep is not unique")
	}
}
