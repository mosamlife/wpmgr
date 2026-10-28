// report_schedule_concurrent_save_integration_test.go -- database proofs that
// two overlapping saves of one client's report settings both keep their change.
// They run on the pool startPostgres returns, which connects as wpmgr_app
// (NOSUPERUSER, NOBYPASSRLS); every row is seeded and read through InTenantTx
// and the code under test is the production service and repo.
//
// A save merges the sections the request sent over the stored ones. When the
// stored sections are read in one transaction and the merged result written in
// another, a second save that reads between them writes its own merge over the
// first, and the first save's change is gone. Saves of one client therefore
// run one at a time, including the first save, when there is no row to lock.
package tests

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/report"
)

// readGate holds the first n callers of arrive until the nth has arrived, so
// that n saves which read the stored schedule are all past the read before any
// of them writes. It makes the overlap that two slow requests produce certain
// rather than likely. A caller waits at most 5s: a save that never reaches the
// gate (a read and write done in one step) is not held up by it.
type readGate struct {
	mu      sync.Mutex
	arrived int
	want    int
	open    chan struct{}
}

func newReadGate(want int) *readGate { return &readGate{want: want, open: make(chan struct{})} }

func (g *readGate) arrive() {
	g.mu.Lock()
	g.arrived++
	if g.arrived == g.want {
		close(g.open)
	}
	g.mu.Unlock()
	select {
	case <-g.open:
	case <-time.After(5 * time.Second):
	}
}

// gatedReadRepo is the production repo with every stored-schedule read held at
// the gate once it has returned its result.
type gatedReadRepo struct {
	report.Repo
	gate *readGate
}

func (r *gatedReadRepo) GetSchedule(ctx context.Context, tenantID, clientID uuid.UUID) (report.Schedule, bool, error) {
	s, found, err := r.Repo.GetSchedule(ctx, tenantID, clientID)
	r.gate.arrive()
	return s, found, err
}

func reportSavePatching(tenant, client uuid.UUID, patch report.SectionFlagsPatch) report.UpsertScheduleInput {
	return report.UpsertScheduleInput{
		TenantID:      tenant,
		ClientID:      client,
		Enabled:       true,
		Cadence:       "weekly",
		SendDay:       1,
		SendHour:      8,
		SectionsPatch: patch,
	}
}

// saveTwoAtOnce runs two saves of the client's settings at the same moment, one
// per patch, and returns when both are done.
func saveTwoAtOnce(t *testing.T, svc *report.Service, tenant, client uuid.UUID, a, b report.SectionFlagsPatch) {
	t.Helper()
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, patch := range []report.SectionFlagsPatch{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := svc.UpsertSchedule(context.Background(), reportSavePatching(tenant, client, patch))
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a save failed: %v", err)
		}
	}
}

// advisoryWaiters counts the backends waiting for the advisory lock keyed on
// (hashtext(scope), hashtext(key)). Read through the same helper a request uses.
func advisoryWaiters(t *testing.T, pool *db.Pool, tenant uuid.UUID, scope, key string) (n int) {
	t.Helper()
	ctx := context.Background()
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
SELECT count(*) FROM pg_locks
 WHERE locktype = 'advisory' AND NOT granted AND objsubid = 2
   AND classid::bigint = (hashtext($1)::bigint & 4294967295)
   AND objid::bigint   = (hashtext($2)::bigint & 4294967295)`, scope, key).Scan(&n)
	}); err != nil {
		t.Fatalf("read pg_locks: %v", err)
	}
	return n
}

func TestReportScheduleConcurrentSaves(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "rpt-concurrent-save")
	readBack := report.NewRepo(pool)

	off := false
	uptimeOff := report.SectionFlagsPatch{Uptime: &off}
	emailOff := report.SectionFlagsPatch{Email: &off}
	want := report.DefaultSectionFlags()
	want.Uptime, want.Email = false, false

	// Two saves that change different sections both keep their change, whether
	// the client has saved before or not.
	for _, tc := range []struct {
		name     string
		hasSaved bool
	}{
		{"first save", false},
		{"save over an existing row", true},
	} {
		t.Run("overlapping saves keep both changes: "+tc.name, func(t *testing.T) {
			const rounds = 8
			for round := 0; round < rounds; round++ {
				client := gh970SeedClient(t, pool, tenant, fmt.Sprintf("%s %d", tc.name, round))
				if tc.hasSaved {
					plain := report.NewService(readBack, rptBlob{})
					if _, err := plain.UpsertSchedule(ctx, reportSavePatching(tenant, client, report.SectionFlagsPatch{})); err != nil {
						t.Fatalf("round %d: first save: %v", round, err)
					}
				}
				// The gate holds both saves past their read of the stored schedule
				// before either writes, if the service reads it as a step of its own.
				svc := report.NewService(&gatedReadRepo{Repo: readBack, gate: newReadGate(2)}, rptBlob{})

				saveTwoAtOnce(t, svc, tenant, client, uptimeOff, emailOff)

				got, found, err := readBack.GetSchedule(ctx, tenant, client)
				if err != nil || !found {
					t.Fatalf("round %d: read the saved schedule: found=%v err=%v", round, found, err)
				}
				if got.Sections != want {
					t.Fatalf("round %d: sections %+v after a save turning Uptime off and a save turning Email off, want %+v: one save undid the other", round, got.Sections, want)
				}
			}
		})
	}

	// The saves above do not prove the lock on their own: they pass if the two
	// happen not to overlap. This does. While another transaction holds the
	// client's save lock, a save for that client waits on it and finishes only
	// once it is released; a save for another client is not held up.
	t.Run("a save waits for its client's lock", func(t *testing.T) {
		locked := gh970SeedClient(t, pool, tenant, "locked client")
		other := gh970SeedClient(t, pool, tenant, "other client")
		svc := report.NewService(readBack, rptBlob{})

		held := make(chan struct{})
		release := make(chan struct{})
		holder := make(chan error, 1)
		go func() {
			holder <- pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))`,
					"report_schedule", locked.String()); err != nil {
					return err
				}
				close(held)
				<-release
				return nil
			})
		}()
		select {
		case <-held:
		case err := <-holder:
			t.Fatalf("could not take the client's lock: %v", err)
		case <-time.After(15 * time.Second):
			t.Fatal("could not take the client's lock in 15s")
		}
		releaseOnce := sync.OnceFunc(func() { close(release) })
		defer releaseOnce()

		if _, err := svc.UpsertSchedule(ctx, reportSavePatching(tenant, other, uptimeOff)); err != nil {
			t.Fatalf("a save for another client was held up by this client's lock: %v", err)
		}

		saved := make(chan error, 1)
		go func() {
			_, err := svc.UpsertSchedule(ctx, reportSavePatching(tenant, locked, emailOff))
			saved <- err
		}()
		deadline := time.Now().Add(10 * time.Second)
		for advisoryWaiters(t, pool, tenant, "report_schedule", locked.String()) == 0 {
			select {
			case err := <-saved:
				t.Fatalf("the save finished (err=%v) while another transaction held the client's lock: it does not take that lock", err)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatal("no backend ever waited for the client's lock")
			}
			time.Sleep(20 * time.Millisecond)
		}
		select {
		case err := <-saved:
			t.Fatalf("the save finished (err=%v) while another transaction held the client's lock", err)
		default:
		}

		releaseOnce()
		select {
		case err := <-saved:
			if err != nil {
				t.Fatalf("the save failed after the lock was released: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("the save did not finish after the lock was released")
		}
		if err := <-holder; err != nil {
			t.Fatalf("the lock holder: %v", err)
		}
		got, found, err := readBack.GetSchedule(ctx, tenant, locked)
		if err != nil || !found || got.Sections.Email {
			t.Fatalf("the waiting save did not write its change: found=%v email=%v err=%v", found, got.Sections.Email, err)
		}
	})
}
