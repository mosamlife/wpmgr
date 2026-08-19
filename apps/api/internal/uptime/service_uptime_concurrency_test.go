package uptime

// service_uptime_concurrency_test.go: 0.61.125. Service.Uptime used to issue
// its three metrics reads (aggregate, latest, series) STRICTLY IN SEQUENCE,
// each opening its own transaction in the Postgres backend, so the endpoint
// paid three full round-trip stacks where /uptime/summary pays one. These
// tests pin the two properties of the fan-out that matter:
//
//   - the three reads actually overlap in time (a barrier none of them can
//     pass alone, so a sequential implementation deadlocks itself and fails
//     with a specific message rather than merely being slow), and
//   - the error the caller sees is still the deterministic one the sequential
//     version would have produced, with each read's own wrapping intact.
//
// Neither test needs a database: the metrics.Store is a stub.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/metrics"
)

// barrierWait is how long a stubbed store call waits at the barrier before
// declaring the reads sequential. Generous enough to never flake on a loaded
// CI box, short enough that a genuine regression fails fast.
const barrierWait = 5 * time.Second

// barrierStore is a metrics.Store whose three per-site read methods each
// announce their arrival and then block until the TEST releases them. All
// three can only ever complete if all three are in flight at the same time,
// which is exactly the property under test.
type barrierStore struct {
	arrived chan string
	release chan struct{}

	// Canned results, so the test can also assert the report is assembled
	// correctly from three concurrently-produced values.
	agg    metrics.Aggregate
	latest metrics.Latest
	series []metrics.Point

	// Optional per-method failures for the error-priority test.
	aggErr, latestErr, seriesErr error
}

func newBarrierStore() *barrierStore {
	return &barrierStore{
		arrived: make(chan string, 3),
		release: make(chan struct{}),
	}
}

// wait records that name entered, then blocks until released or the barrier
// times out. A timeout means the other reads had not started yet, i.e. the
// service is running them one after another.
func (s *barrierStore) wait(name string) error {
	s.arrived <- name
	select {
	case <-s.release:
		return nil
	case <-time.After(barrierWait):
		return fmt.Errorf("%s blocked at the barrier for %s: the other two reads never started, so Service.Uptime is issuing them SEQUENTIALLY", name, barrierWait)
	}
}

func (s *barrierStore) Enabled() bool { return true }
func (s *barrierStore) Close() error  { return nil }
func (s *barrierStore) InsertChecks(_ context.Context, _ []metrics.Check) error {
	panic("not called")
}
func (s *barrierStore) QueryFleetUptime(_ context.Context, _ uuid.UUID, _ []uuid.UUID, _ time.Duration) (map[uuid.UUID]metrics.FleetUptimeRow, error) {
	panic("not called")
}
func (s *barrierStore) QueryProbeWindow(_ context.Context, _, _ uuid.UUID, _, _ time.Time, _ int) ([]metrics.ProbeSample, error) {
	panic("not called")
}
func (s *barrierStore) QueryFleetDailySeries(_ context.Context, _ uuid.UUID, _ []uuid.UUID, _ time.Duration) (map[uuid.UUID][]metrics.Point, error) {
	panic("not called")
}

func (s *barrierStore) QueryAggregate(_ context.Context, _, _ uuid.UUID, _ time.Duration) (metrics.Aggregate, error) {
	if err := s.wait("QueryAggregate"); err != nil {
		return metrics.Aggregate{}, err
	}
	return s.agg, s.aggErr
}

func (s *barrierStore) QueryLatest(_ context.Context, _, _ uuid.UUID) (metrics.Latest, error) {
	if err := s.wait("QueryLatest"); err != nil {
		return metrics.Latest{}, err
	}
	return s.latest, s.latestErr
}

func (s *barrierStore) QuerySeries(_ context.Context, _, _ uuid.UUID, _ time.Duration, _ int) ([]metrics.Point, error) {
	if err := s.wait("QuerySeries"); err != nil {
		return nil, err
	}
	return s.series, s.seriesErr
}

// countingVerifier is a SiteVerifier that always confirms ownership and counts
// how many times it was asked, so the test can prove the authorisation check
// still happens exactly once and BEFORE any store read.
type countingVerifier struct {
	calls int
}

func (v *countingVerifier) VerifySite(_ context.Context, _, _ uuid.UUID) (string, bool, error) {
	v.calls++
	return "example", true, nil
}
func (v *countingVerifier) ListSiteIDs(_ context.Context, _ uuid.UUID) ([]uuid.UUID, error) {
	panic("not called")
}

// TestUptime_ThreeStoreReadsRunConcurrently is the ordering proof: each of the
// three reads blocks at a shared barrier that only the test can lift, and the
// test only lifts it once it has seen all three arrive. Against the previous
// sequential implementation the first read waits alone for barrierWait, the
// other two never start, and the test fails naming the read that was stuck.
func TestUptime_ThreeStoreReadsRunConcurrently(t *testing.T) {
	store := newBarrierStore()
	lastCheck := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	tlsExpiry := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	store.agg = metrics.Aggregate{Checks: 43200, UpChecks: 43100, UptimePct: 99.768, AvgLatencyMs: 187.5}
	store.latest = metrics.Latest{
		CheckedAt: lastCheck, Up: true, TLSExpiry: tlsExpiry,
		TLSIssuer: "Test CA", TLSSubject: "example.com", Found: true,
	}
	store.series = []metrics.Point{
		{Bucket: lastCheck.Add(-24 * time.Hour), Checks: 1440, UpChecks: 1440, AvgLatencyMs: 190},
		{Bucket: lastCheck, Checks: 720, UpChecks: 700, AvgLatencyMs: 185},
	}

	verifier := &countingVerifier{}
	svc := NewService(&stubRepo{}, store, verifier)

	type result struct {
		rep UptimeReport
		err error
	}
	done := make(chan result, 1)
	go func() {
		rep, err := svc.Uptime(context.Background(), uuid.New(), uuid.New(), 30*24*time.Hour, 100)
		done <- result{rep, err}
	}()

	// All three reads must be in flight before ANY of them is allowed to
	// finish. Collect the three arrivals; a missing one is the regression.
	seen := make(map[string]bool, 3)
	for i := 0; i < 3; i++ {
		select {
		case name := <-store.arrived:
			seen[name] = true
		case <-time.After(barrierWait):
			t.Fatalf("only %d of 3 store reads had started after %s (saw %v): Service.Uptime is running them sequentially, not concurrently", len(seen), barrierWait, seen)
		}
	}
	for _, want := range []string{"QueryAggregate", "QueryLatest", "QuerySeries"} {
		if !seen[want] {
			t.Fatalf("%s never started; saw %v", want, seen)
		}
	}
	close(store.release)

	var got result
	select {
	case got = <-done:
	case <-time.After(barrierWait):
		t.Fatal("Service.Uptime did not return after the barrier was released")
	}
	if got.err != nil {
		t.Fatalf("Uptime returned an error: %v", got.err)
	}

	// The report must still be assembled correctly from the three concurrent
	// results (the refactor must not have crossed any wires).
	if got.rep.Checks != 43200 || got.rep.UptimePct != 99.768 || got.rep.AvgLatencyMs != 187.5 {
		t.Fatalf("aggregate fields not wired through: %+v", got.rep)
	}
	if !got.rep.Up {
		t.Fatal("latest.Up not wired through")
	}
	if got.rep.LastCheck == nil || !got.rep.LastCheck.Equal(lastCheck) {
		t.Fatalf("LastCheck = %v, want %v", got.rep.LastCheck, lastCheck)
	}
	if got.rep.TLSExpiry == nil || !got.rep.TLSExpiry.Equal(tlsExpiry) {
		t.Fatalf("TLSExpiry = %v, want %v", got.rep.TLSExpiry, tlsExpiry)
	}
	if got.rep.TLSIssuer != "Test CA" || got.rep.TLSSubject != "example.com" {
		t.Fatalf("TLS identity not wired through: issuer=%q subject=%q", got.rep.TLSIssuer, got.rep.TLSSubject)
	}
	if len(got.rep.Series) != 2 || got.rep.Series[1].Checks != 720 {
		t.Fatalf("series not wired through: %+v", got.rep.Series)
	}

	// The authorisation check ran exactly once, and (by construction) before
	// the fan-out: the goroutine could not have reached the barrier at all if
	// VerifySite had not already returned ok.
	if verifier.calls != 1 {
		t.Fatalf("VerifySite called %d times, want exactly 1", verifier.calls)
	}
}

// TestUptime_ForeignSiteIsRefusedWithoutAnyStoreRead pins the authorisation
// ordering explicitly: a site the tenant does not own must 404 without a
// single metrics read being issued. The barrier store panics on nothing here,
// but its reads would block forever, so a regression that moved VerifySite
// after the fan-out would hang this test rather than pass it.
func TestUptime_ForeignSiteIsRefusedWithoutAnyStoreRead(t *testing.T) {
	store := newBarrierStore()
	svc := NewService(&stubRepo{}, store, &denyingVerifier{})

	done := make(chan error, 1)
	go func() {
		_, err := svc.Uptime(context.Background(), uuid.New(), uuid.New(), 30*24*time.Hour, 100)
		done <- err
	}()

	select {
	case err := <-done:
		de, ok := domain.AsDomain(err)
		if !ok || de.Kind != domain.KindNotFound || de.Code != "site_not_found" {
			t.Fatalf("err = %v, want a domain NotFound site_not_found", err)
		}
	case <-time.After(barrierWait):
		t.Fatal("Uptime blocked: a store read was issued for a site the tenant does not own")
	}
	if len(store.arrived) != 0 {
		t.Fatalf("%d store reads were issued for an unowned site, want 0", len(store.arrived))
	}
}

type denyingVerifier struct{}

func (denyingVerifier) VerifySite(_ context.Context, _, _ uuid.UUID) (string, bool, error) {
	return "", false, nil
}
func (denyingVerifier) ListSiteIDs(_ context.Context, _ uuid.UUID) ([]uuid.UUID, error) {
	panic("not called")
}

// TestUptime_ErrorPriorityIsDeterministic proves the fan-out did not make the
// returned error depend on which goroutine happened to lose the race: with all
// three reads failing at once, the caller still gets the aggregate read's
// error, exactly as the old sequential code (which returned on the first
// failure and never issued the other two) would have.
func TestUptime_ErrorPriorityIsDeterministic(t *testing.T) {
	cases := []struct {
		name                         string
		aggErr, latestErr, seriesErr error
		wantMessage                  string
	}{
		{"all three fail", errors.New("agg boom"), errors.New("latest boom"), errors.New("series boom"), "failed to query uptime metrics"},
		{"latest and series fail", nil, errors.New("latest boom"), errors.New("series boom"), "failed to query latest uptime"},
		{"series only", nil, nil, errors.New("series boom"), "failed to query uptime series"},
		{"aggregate only", errors.New("agg boom"), nil, nil, "failed to query uptime metrics"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newBarrierStore()
			// No barrier in this test: release immediately so all three run
			// to their canned outcome.
			close(store.release)
			store.aggErr, store.latestErr, store.seriesErr = tc.aggErr, tc.latestErr, tc.seriesErr

			svc := NewService(&stubRepo{}, store, &countingVerifier{})
			_, err := svc.Uptime(context.Background(), uuid.New(), uuid.New(), 30*24*time.Hour, 100)
			de, ok := domain.AsDomain(err)
			if !ok {
				t.Fatalf("err = %v, want a domain error", err)
			}
			if de.Kind != domain.KindInternal || de.Code != "uptime_query_failed" {
				t.Fatalf("err kind/code = %v/%q, want internal/uptime_query_failed", de.Kind, de.Code)
			}
			if de.Message != tc.wantMessage {
				t.Fatalf("err message = %q, want %q (each read must keep its own wrapping, and the winner must be deterministic)", de.Message, tc.wantMessage)
			}
		})
	}
}
