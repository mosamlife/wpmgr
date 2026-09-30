package abilities

import (
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// A number jsonb would re-spell (1e3 -> 1000) is refused at input, so the
// hash stamped from the request always equals the hash reproduced from the
// stored row.
func TestRequireCanonicalJSONNumbers(t *testing.T) {
	for _, bad := range []string{`{"max":1e3}`, `{"max":1.0}`, `{"max":01}`, `{"a":[1,2.5]}`, `{"a":{"b":-0.0}}`} {
		if err := requireCanonicalJSONNumbers(sqlc.AbilityCatalogue{Limits: []byte(bad)}); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
	for _, good := range []string{`{"max":1000}`, `{"max":0,"min":-5}`, `{}`, `{"a":["1e3"]}`} {
		if err := requireCanonicalJSONNumbers(sqlc.AbilityCatalogue{Limits: []byte(good), Admission: []byte(good)}); err != nil {
			t.Errorf("%s was refused: %v", good, err)
		}
	}
}
