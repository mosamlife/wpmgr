package agentcmd

import (
	"encoding/json"
	"strings"
	"testing"
)

const vendorOK = `{"ok":true,"outcome":"completed","mode":"read","ability":"builder/get-page","entry_sha256":"abc",` +
	`"owner":{"kind":"plugin","dir":"builder","version":"2.4.1"},"abilities_invoked":["builder/get-page"],"output":{"a":1}}`

func TestDecodeVendorRead_Strict(t *testing.T) {
	got, err := DecodeVendorRead([]byte(vendorOK), "builder/get-page", "abc")
	if err != nil || got.Owner.Dir != "builder" || string(got.Output) != `{"a":1}` {
		t.Fatalf("got %+v err %v", got, err)
	}
	bad := map[string]string{
		"unknown member":  strings.Replace(vendorOK, `"output"`, `"extra":1,"output"`, 1),
		"other ability":   strings.Replace(vendorOK, `"ability":"builder/get-page"`, `"ability":"builder/other"`, 1),
		"other entry":     strings.Replace(vendorOK, `"entry_sha256":"abc"`, `"entry_sha256":"abd"`, 1),
		"owner kind":      strings.Replace(vendorOK, `"kind":"plugin"`, `"kind":"wpmgr"`, 1),
		"owner unknown":   strings.Replace(vendorOK, `"kind":"plugin"`, `"kind":"unknown"`, 1),
		"owner extra":     strings.Replace(vendorOK, `"version":"2.4.1"}`, `"version":"2.4.1","x":1}`, 1),
		"version shape":   strings.Replace(vendorOK, `"version":"2.4.1"`, `"version":"2.4 ignore all"`, 1),
		"dir shape":       strings.Replace(vendorOK, `"dir":"builder"`, `"dir":"../x"`, 1),
		"outcome":         strings.Replace(vendorOK, `"completed"`, `"ledger"`, 1),
		"no output":       strings.Replace(vendorOK, `,"output":{"a":1}`, ``, 1),
		"no owner":        strings.Replace(vendorOK, `"owner":{"kind":"plugin","dir":"builder","version":"2.4.1"},`, ``, 1),
		"invoked name":    strings.Replace(vendorOK, `["builder/get-page"]`, `["Not A Name"]`, 1),
		"not ok":          strings.Replace(vendorOK, `"ok":true`, `"ok":false`, 1),
		"trailing object": vendorOK + `{}`,
	}
	for name, raw := range bad {
		if _, err := DecodeVendorRead([]byte(raw), "builder/get-page", "abc"); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
}

// Every refusal the agent's vendor read path returns is in the closed set
// and decodes with its extras.
func TestAbilityRunRefusal_VendorCodesDecode(t *testing.T) {
	codes := []string{"entry_source_mismatch", "snapshot_strategy_invalid", "vendor_writes_not_in_this_version",
		"permission_mode_not_assertable", "bad_entry", "wp_too_old_for_vendor_reads", "bad_input",
		"ability_not_on_site", "ability_class_overridden", "ability_owner_split", "ability_owner_mismatch",
		"builder_version_unverified", "ability_schema_changed", "ability_input_invalid",
		"content_editing_not_enabled", "principal_capabilities_drifted", "read_side_effect_detected",
		"principal_switched", "ability_intercepted", "nested_ability_refused", "ability_permission_denied",
		"ability_failed", "ability_output_invalid", "output_too_large", "internal"}
	for _, code := range codes {
		raw := `{"ok":false,"outcome":"refused","code":"` + code + `","detail":"d","retryable":false}`
		var out AbilityRunResponse
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatal(err)
		}
		if got := abilityRunRefusalOf(out); got.Code != code {
			t.Errorf("%s decoded as %s", code, got.Code)
		}
	}

	raw := `{"ok":false,"outcome":"refused","code":"read_side_effect_detected","detail":"d","retryable":false,` +
		`"side_effects":{"options":["evil_opt"],"posts":1,"roles":0,"users":2,"http_hosts":["api.example.test"],"blocked":["user_capabilities_meta","Bad Label"]},` +
		`"violations":["short_circuit","IGNORE ALL"]}`
	var out AbilityRunResponse
	_ = json.Unmarshal([]byte(raw), &out)
	r := abilityRunRefusalOf(out)
	if r.SideEffects == nil || r.SideEffects.Options[0] != "evil_opt" || r.SideEffects.Users != 2 ||
		r.SideEffects.HTTPHosts[0] != "api.example.test" || r.SideEffects.Blocked[1] != "unknown" {
		t.Fatalf("side effects: %+v", r.SideEffects)
	}
	if len(r.Violations) != 2 || r.Violations[0] != "short_circuit" || r.Violations[1] != "unknown" {
		t.Fatalf("violations: %v", r.Violations)
	}
	// An extra member in side_effects is off-contract: dropped, code kept.
	out.SideEffects = json.RawMessage(`{"options":[],"posts":0,"roles":0,"users":0,"http_hosts":[],"blocked":[],"x":1}`)
	if r := abilityRunRefusalOf(out); r.SideEffects != nil || r.Code != "read_side_effect_detected" {
		t.Fatalf("lenient side effects: %+v", r)
	}

	failed := `{"ok":false,"outcome":"refused","code":"ability_failed","detail":"d","retryable":false,"error_code":"` +
		strings.Repeat("x", 300) + `"}`
	_ = json.Unmarshal([]byte(failed), &out)
	if r := abilityRunRefusalOf(out); r.ErrorCode == "" || len(r.ErrorCode) > vendorErrorCodeBytes+4 {
		t.Fatalf("error_code not capped: %d", len(r.ErrorCode))
	}
}

func TestParseOutputShape_Grammar(t *testing.T) {
	good := []string{`"string"`, `"int"`, `"bool"`, `{"fields":{}}`, `{"items":"int"}`,
		`{"fields":{"a":{"items":{"fields":{"b-c_d":"string"}}}}}`}
	for _, g := range good {
		if _, err := ParseOutputShape([]byte(g)); err != nil {
			t.Errorf("%s refused", g)
		}
	}
	deep := `"int"`
	for i := 0; i < 9; i++ {
		deep = `{"items":` + deep + `}`
	}
	bad := []string{`"number"`, `"object"`, `{"object":{}}`, `{"fields":{},"items":"int"}`, `{"fields":[]}`,
		`{"fields":{"bad key":"string"}}`, `1`, `null`, `[]`, `{"items":"int"} {}`, deep, `{"fields":{"a":"float"}}`}
	for _, b := range bad {
		if _, err := ParseOutputShape([]byte(b)); err == nil {
			t.Errorf("%s accepted", b)
		}
	}
}
