package wpversion_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/wpversion"
)

// sharedCompareFixture is also replayed against the SQL wpmgr_version_cmp
// (m159) by tests/ability_catalogue_c1_integration_test.go, so the database
// and Go order plugin versions identically.
const sharedCompareFixture = "../../db/testdata/wpmgr_version_cmp_cases.json"

func TestCompareSharedFixture(t *testing.T) {
	raw, err := os.ReadFile(sharedCompareFixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var f struct {
		Cases [][3]json.RawMessage `json:"cases"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(f.Cases) == 0 {
		t.Fatal("fixture has no cases")
	}
	for _, c := range f.Cases {
		var a, b string
		var want int
		if json.Unmarshal(c[0], &a) != nil || json.Unmarshal(c[1], &b) != nil || json.Unmarshal(c[2], &want) != nil {
			t.Fatalf("bad case %s", c)
		}
		if got := wpversion.Compare(a, b); got != want {
			t.Errorf("Compare(%q, %q) = %d, want %d", a, b, got, want)
		}
	}
}
