package tests

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/admin"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
)

// The system audit log is read by paging, and it is written to while it is
// being read: every authentication event belonging to no organisation lands
// here as it happens. That combination is what makes the paging scheme a
// correctness question rather than a style one, so these run against the real
// table with real co-timestamped rows.

func seedSystemAuditEvent(t *testing.T, pool *db.Pool, occurredAt time.Time, action string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO system_audit_log (occurred_at, actor_type, actor_id, action, tenant_id, tenant_name, metadata)
		 VALUES ($1, 'user', $2, $3, $4, '', '{}'::jsonb)`,
		occurredAt, uuid.New(), action, uuid.Nil,
	); err != nil {
		t.Fatalf("seed system audit event: %v", err)
	}
}

// walkSystemAudit pages the whole log with the given page size and returns the
// ids in the order a reader would see them.
func walkSystemAudit(t *testing.T, repo *admin.Repo, limit int32) []uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var seen []uuid.UUID
	cursor := ""
	for range 100 { // a loop bound, not a page count: a broken cursor must fail loudly
		page, err := repo.ListSystemAuditEvents(ctx, limit, cursor)
		if err != nil {
			t.Fatalf("list system audit: %v", err)
		}
		for _, e := range page.Events {
			seen = append(seen, e.ID)
		}
		if page.NextCursor == "" {
			return seen
		}
		cursor = page.NextCursor
	}
	t.Fatal("paging did not terminate; a cursor that never advances walks the same window forever")
	return nil
}

// Rows written by one action share an occurred_at to the microsecond. Ordering
// on the timestamp alone leaves their relative order undefined, so a page
// boundary that lands inside such a group steps over the rest of it. The id
// half of the composite cursor is what stops that, and this is the shape of
// data that proves it: every row shares one timestamp.
func TestSystemAuditPagingDoesNotSkipCoTimestampedRows(t *testing.T) {
	pool := startPostgres(t)
	repo := admin.NewRepo(pool)

	shared := time.Now().UTC().Truncate(time.Microsecond)
	const n = 7
	for range n {
		seedSystemAuditEvent(t, pool, shared, "auth.social.link")
	}

	// A page size that deliberately does not divide the group evenly, so a
	// boundary lands inside it.
	seen := walkSystemAudit(t, repo, 2)
	if len(seen) != n {
		t.Fatalf("walked %d of %d co-timestamped rows; a bare timestamp cursor loses the rest of each group", len(seen), n)
	}
	distinct := map[uuid.UUID]bool{}
	for _, id := range seen {
		if distinct[id] {
			t.Fatalf("row %s appeared on two pages", id)
		}
		distinct[id] = true
	}
}

// The bug an offset has and a cursor does not. Between one request and the
// next, new rows arrive at the head of the list. An offset counts from the
// head, so everything the reader already saw shifts down past the boundary and
// comes back a second time; a cursor names the last row actually seen, so it
// does not move.
func TestSystemAuditPagingIsStableWhenRowsArriveMidWalk(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	repo := admin.NewRepo(pool)

	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	for i := range 6 {
		seedSystemAuditEvent(t, pool, base.Add(time.Duration(i)*time.Second), "auth.social.register")
	}

	first, err := repo.ListSystemAuditEvents(ctx, 3, "")
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if first.NextCursor == "" {
		t.Fatal("six rows and a page size of three produced no next cursor")
	}

	// Three more events happen while the reader is looking at page one.
	for i := range 3 {
		seedSystemAuditEvent(t, pool, time.Now().UTC().Add(time.Duration(i)*time.Millisecond), "auth.social.link")
	}

	second, err := repo.ListSystemAuditEvents(ctx, 3, first.NextCursor)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	onPageOne := map[uuid.UUID]bool{}
	for _, e := range first.Events {
		onPageOne[e.ID] = true
	}
	for _, e := range second.Events {
		if onPageOne[e.ID] {
			t.Fatalf("row %s was shown on page one and again on page two; the reader cannot tell which of these they have already read", e.ID)
		}
	}
}

// A reader must be able to reach the end of the log, however long it is. The
// page-size cap bounds one request; capping the position instead would answer
// "page fifty" with page one forever, while the total says there is more.
func TestSystemAuditPagingReachesTheEndOfALongLog(t *testing.T) {
	pool := startPostgres(t)
	repo := admin.NewRepo(pool)

	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	const n = 250 // deliberately past the 200 page-size ceiling
	for i := range n {
		seedSystemAuditEvent(t, pool, base.Add(time.Duration(i)*time.Millisecond), "auth.social.register")
	}

	seen := walkSystemAudit(t, repo, 50)
	if len(seen) != n {
		t.Fatalf("reached %d of %d rows; the oldest %d are unreachable to the reader", len(seen), n, n-len(seen))
	}
}
