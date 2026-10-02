package agentcmd

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestPostScheduledIsAKnownRefusal(t *testing.T) {
	r := abilityRunRefusalOf(AbilityRunResponse{Code: "post_scheduled"})
	if r.Code != "post_scheduled" {
		t.Fatalf("post_scheduled decoded as %q", r.Code)
	}
}

// A failed rest-write that could not put the whole post back: restored and
// exact false, and the columns still changed, closed to the post column set.
func TestRefusalCarriesRestoreReport(t *testing.T) {
	var out AbilityRunResponse
	if err := json.Unmarshal([]byte(`{"ok":false,"outcome":"refused","code":"side_effect_detected",
		"detail":"x","restored":false,"exact":false,
		"columns_still_changed":["post_status","post_name","wp_evil","post_status"]}`), &out); err != nil {
		t.Fatal(err)
	}
	r := abilityRunRefusalOf(out)
	if r.Restored == nil || *r.Restored {
		t.Fatalf("restored: %v", r.Restored)
	}
	if r.Exact == nil || *r.Exact {
		t.Fatalf("exact: %v", r.Exact)
	}
	if want := []string{"post_status", "post_name", "unknown"}; !reflect.DeepEqual(r.ColumnsStillChanged, want) {
		t.Fatalf("columns_still_changed: %v, want %v", r.ColumnsStillChanged, want)
	}
}

func TestRefusalRestoreReportClean(t *testing.T) {
	var out AbilityRunResponse
	_ = json.Unmarshal([]byte(`{"ok":false,"code":"side_effect_detected","restored":true,"exact":true}`), &out)
	r := abilityRunRefusalOf(out)
	if r.Restored == nil || !*r.Restored || len(r.ColumnsStillChanged) != 0 {
		t.Fatalf("clean restore: restored=%v columns=%v", r.Restored, r.ColumnsStillChanged)
	}
}

// changed:false means the write changed nothing: restored:false there is not
// a failed put-back, so it is not recorded as one.
func TestRestoreReportNothingChanged(t *testing.T) {
	var out AbilityRunResponse
	_ = json.Unmarshal([]byte(`{"ok":false,"code":"rest_error","restored":false,"changed":false}`), &out)
	if r := abilityRunRefusalOf(out); r.Restored != nil {
		t.Fatalf("nothing changed, yet restored=%v", *r.Restored)
	}
	_ = json.Unmarshal([]byte(`{"ok":false,"code":"conflict"}`), &out)
	if r := abilityRunRefusalOf(out); r.Restored != nil || r.ColumnsStillChanged != nil {
		t.Fatal("a refusal with no report grew one")
	}
}

func TestDecodePostColumnsMalformed(t *testing.T) {
	if got := DecodePostColumns(json.RawMessage(`"post_title"`)); !reflect.DeepEqual(got, []string{"unknown"}) {
		t.Fatalf("non-list: %v", got)
	}
}
