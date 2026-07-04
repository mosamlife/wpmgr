package tests

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// TestAuditConcurrentAppendChainIntact fires many concurrent Record calls for
// the SAME tenant and asserts the hash chain is still intact afterward.
//
// Before the per-tenant pg_advisory_xact_lock serialization in Record, two
// concurrent appends for the same tenant could both run their
// GetLastAuditHash read in overlapping READ COMMITTED transactions, both see
// the same "previous" hash, and both chain their new row to it. Verify would
// then report the second row as a broken link — a false "chain break"/
// "tampering" signal on the web Audit page, even though nothing was actually
// tampered with; it was a lost-update race between two legitimate appends.
// This test fails (Verify reports ok=false) without the advisory-lock fix.
func TestAuditConcurrentAppendChainIntact(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	rec := audit.NewRecorder(pool, domain.SystemClock{})

	tenant := seedTenant(t, pool, "audit-concurrency")

	const n = 20
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all goroutines together to maximize overlap
			_, err := rec.Record(ctx, audit.Event{
				TenantID:  tenant,
				ActorType: audit.ActorUser,
				ActorID:   uuid.New().String(),
				Action:    "test.concurrent",
				Metadata:  map[string]any{"i": i},
			})
			errs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}

	ok, brokenAt, err := rec.Verify(ctx, tenant)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Fatalf("concurrent appends broke the hash chain at %s (lost-update race not serialized per tenant)", brokenAt)
	}

	// Sanity: all n entries actually landed (no silent drops), and List
	// returns them newest-first without error under the same load.
	entries, err := rec.List(ctx, tenant, int32(n), 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != n {
		t.Fatalf("expected %d entries, got %d", n, len(entries))
	}
}
