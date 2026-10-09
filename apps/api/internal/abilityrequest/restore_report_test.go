package abilityrequest

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

func boolp(b bool) *bool { return &b }

// A failed rest-write that left other columns changed: recorded as failed,
// restored false, the closed columns kept for the audit row.
func TestClassifyWrite_PartialRestore(t *testing.T) {
	now := time.Now()
	oc := classifyWrite(agentcmd.AbilityRunResponse{}, &agentcmd.AbilityRunRefusal{
		Code: "side_effect_detected", Restored: boolp(false),
		ColumnsStillChanged: []string{"post_status", "unknown"},
	}, now)
	if oc.outcome != OutcomeRefused || oc.code == nil || *oc.code != "side_effect_detected" {
		t.Fatalf("outcome %q code %v", oc.outcome, oc.code)
	}
	if oc.restored == nil || *oc.restored {
		t.Fatalf("restored: %v", oc.restored)
	}
	if !reflect.DeepEqual(oc.columnsStillChanged, []string{"post_status", "unknown"}) {
		t.Fatalf("columns: %v", oc.columnsStillChanged)
	}
	if oc.undoUntil.Valid {
		t.Fatal("a failed write opened the done-row undo")
	}
}

func TestClassifyWrite_CleanRestoreUnchanged(t *testing.T) {
	oc := classifyWrite(agentcmd.AbilityRunResponse{}, &agentcmd.AbilityRunRefusal{
		Code: "side_effect_detected", Restored: boolp(true),
	}, time.Now())
	if oc.outcome != OutcomeRefused || oc.restored == nil || !*oc.restored || oc.columnsStillChanged != nil {
		t.Fatalf("clean restore: %+v", oc)
	}
	plain := classifyWrite(agentcmd.AbilityRunResponse{}, &agentcmd.AbilityRunRefusal{Code: "conflict"}, time.Now())
	if plain.restored != nil || plain.columnsStillChanged != nil {
		t.Fatalf("a refusal with no report grew one: %+v", plain)
	}
}

// The ledger path reads the same report from the stored result.
func TestOutcomeFromStored_PartialRestore(t *testing.T) {
	oc, ok := outcomeFromStored([]byte(`{"ok":false,"code":"side_effect_detected","restored":false,"exact":false,
		"columns_still_changed":["post_name","wp_injected"]}`), time.Now())
	if !ok || oc.outcome != OutcomeRefused {
		t.Fatalf("ok=%v outcome=%q", ok, oc.outcome)
	}
	if oc.restored == nil || *oc.restored {
		t.Fatalf("restored: %v", oc.restored)
	}
	if !reflect.DeepEqual(oc.columnsStillChanged, []string{"post_name", "unknown"}) {
		t.Fatalf("columns: %v", oc.columnsStillChanged)
	}
	none, _ := outcomeFromStored([]byte(`{"ok":false,"code":"rest_error","restored":false,"changed":false}`), time.Now())
	if none.restored != nil {
		t.Fatal("a write that changed nothing was recorded as not restored")
	}
}

// A success never carries a restore report, whatever the agent sent.
func TestWithRestoreReport_OnlyOnFailures(t *testing.T) {
	oc := appliedOutcome(time.Now()).withRestoreReport(boolp(false), []string{"post_name"})
	if oc.restored != nil || oc.columnsStillChanged != nil {
		t.Fatalf("applied outcome took a restore report: %+v", oc)
	}
}

// A partial revert is recorded as undone (what WPMgr changed is back), and
// its report names what remains.
func TestRevertReport_Partial(t *testing.T) {
	resp := agentcmd.AbilityRunResponse{Outcome: "reverted", Restored: boolp(false),
		ColumnsStillChanged: json.RawMessage(`["post_status","bogus"]`)}
	if got := undoResultFor(resp, nil); got != UndoDone {
		t.Fatalf("partial revert result %q", got)
	}
	md := revertReport(resp, nil)
	if md["restored"] != false || !reflect.DeepEqual(md["columns_still_changed"], []string{"post_status", "unknown"}) {
		t.Fatalf("report: %v", md)
	}
	if revertReport(agentcmd.AbilityRunResponse{Outcome: "reverted"}, nil) != nil {
		t.Fatal("a revert with no report grew one")
	}
	if revertReport(resp, &agentcmd.AbilityRunRefusal{Code: "conflict"}) != nil {
		t.Fatal("a refused revert carried a report")
	}
}

func TestToDTO_Restored(t *testing.T) {
	r := sqlc.AssistantAbilityRequest{Restored: boolp(false)}
	b, _ := json.Marshal(toDTO(r, false, ""))
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if v, ok := m["restored"]; !ok || v != false {
		t.Fatalf("restored on the wire: %s", b)
	}
	b, _ = json.Marshal(toDTO(sqlc.AssistantAbilityRequest{}, false, ""))
	_ = json.Unmarshal(b, &m)
	if v, ok := m["restored"]; !ok || v != nil {
		t.Fatalf("restored null on the wire: %s", b)
	}
}
