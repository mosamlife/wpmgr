package rum

import (
	"reflect"
	"testing"
)

// TestStoreInterface_NoDeadFoldMethods is a regression test for GH #459.
//
// FoldHourly and FoldDaily used to be part of the Store interface. Both were
// dead surface: FoldHourly discarded its query result (`_ = rows`) and
// returned nil without folding anything, and FoldDaily was a bare `return
// nil`. Neither method had any real caller — RumRollupWorker calls
// UpsertRollupHourly/UpsertRollupDaily directly on the concrete
// *StorePostgres, never through the Store interface's Fold* methods — so
// every implementation silently "succeeded" while doing nothing. That is the
// exact shape that caused GH #459 to be misfiled as a rollup-aggregation bug
// when the real per-beacon rollup path (StorePostgres.WriteEvent) was
// working correctly the whole time.
//
// This test asserts the dead methods are gone from the interface, so they
// cannot be reintroduced as a no-op stub. It fails on the pre-fix tree
// (interface has FoldHourly/FoldDaily) and passes after the fix (interface
// does not).
func TestStoreInterface_NoDeadFoldMethods(t *testing.T) {
	storeType := reflect.TypeOf((*Store)(nil)).Elem()

	for _, name := range []string{"FoldHourly", "FoldDaily"} {
		if _, ok := storeType.MethodByName(name); ok {
			t.Errorf("Store interface still declares %s; this method was dead surface "+
				"(silently returns nil without doing its documented work) and was "+
				"removed for GH #459 — do not reintroduce it as a stub", name)
		}
	}
}
