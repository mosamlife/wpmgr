package backup

// gh408_tenant_drain_roots_test.go: the Lane A tenant drain and the Lane B
// grace-window purge must agree, root for root, about what an organisation owns.
//
// This is the test internal/org/purge_worker.go's doc comment names. Until this
// file existed it did not, and the property it claims ("adding a root anywhere is
// a single edit here") was asserted nowhere.
//
// The drain calls org.ObjectStoragePrefixes today, so the equality below is
// currently structural rather than coincidental, and that is the point: this
// test is what goes red the day somebody copies the seven roots into
// internal/backup, or filters the shared list down to "the ones that matter",
// or reorders it. The direction that drift takes is a silent leak. A root added
// for Lane B and missing from Lane A is storage nothing ever frees again once
// the tenant row is gone, and no test that exercises either lane on its own can
// see it.

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/org"
)

func TestGH408_TenantDrainRootsAreLaneBsRoots(t *testing.T) {
	tenantID := uuid.New()

	laneB := org.ObjectStoragePrefixes(tenantID)
	laneA, err := TenantObjectPrefixes(TenantReclaimKindStorage, tenantID)
	if err != nil {
		t.Fatalf("the Lane A drain cannot derive its roots: %v", err)
	}

	if len(laneA) != len(laneB) {
		t.Fatalf("the Lane A drain frees %d roots and the Lane B purge frees %d.\n  Lane A: %v\n"+
			"  Lane B: %v\nA root in Lane B alone is storage nothing frees once the tenant row is gone",
			len(laneA), len(laneB), laneA, laneB)
	}
	for i := range laneB {
		if laneA[i] != laneB[i] {
			t.Errorf("root %d differs: Lane A has %q, Lane B has %q", i, laneA[i], laneB[i])
		}
	}

	// Every root must be scoped to this tenant and to nothing else. The drain
	// checks this itself one line before an irreversible delete; asserted here
	// too because it is the property that makes a corrupt or empty root
	// impossible rather than unlikely.
	for _, r := range laneA {
		if !strings.HasSuffix(r, "/"+tenantID.String()+"/") {
			t.Errorf("root %q is not scoped to tenant %s, so it could name storage belonging to "+
				"somebody else", r, tenantID)
		}
	}

	// The one-character adjacency the m116 header calls out: "tenant/" holds
	// backup manifests and "tenants/" holds white-label client report PDFs with
	// client PII. Both are legitimately in the set, and a prefix match of the
	// singular against the plural would delete the wrong one, so this asserts
	// they are present as two distinct, separately named roots.
	var singular, plural bool
	for _, r := range laneA {
		switch {
		case strings.HasPrefix(r, "tenant/"):
			singular = true
		case strings.HasPrefix(r, "tenants/"):
			plural = true
		}
	}
	if !singular || !plural {
		t.Errorf("expected both the singular tenant/ root and the plural tenants/ root in %v "+
			"(singular=%v plural=%v)", laneA, singular, plural)
	}
}
