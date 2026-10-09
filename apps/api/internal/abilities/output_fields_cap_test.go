package abilities

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// output_fields over m159's 16 KiB cap is a validation error, measured as
// jsonb prints it.
func TestValidateEntryShape_OutputFieldsCap(t *testing.T) {
	fields := func(n int) []byte {
		var b strings.Builder
		b.WriteString(`{"fields":{`)
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `"k%05d":"string"`, i)
		}
		b.WriteString("}}")
		return []byte(b.String())
	}
	if err := validateEntryShape(sqlc.AbilityCatalogue{OutputFields: fields(10)}); err != nil {
		t.Fatalf("small shape refused: %v", err)
	}
	// 850 fields: about 15.3 KB compact (under the cap) but about 17 KB as
	// jsonb prints it (over).
	big := fields(850)
	if len(big) >= outputFieldsMaxBytes {
		t.Fatalf("fixture too large to prove the jsonb measure: %d", len(big))
	}
	err := validateEntryShape(sqlc.AbilityCatalogue{OutputFields: big})
	if de, ok := domain.AsDomain(err); !ok || de.Kind != domain.KindValidation || de.Code != "invalid_output_fields" {
		t.Fatalf("oversized shape (%d bytes): %v", len(big), err)
	}
	if n := jsonbTextLen([]byte(`{"a": [1,2],"b":"x:,y"}`)); n != len(`{"a": [1, 2], "b": "x:,y"}`) {
		t.Fatalf("jsonbTextLen = %d", n)
	}
}
