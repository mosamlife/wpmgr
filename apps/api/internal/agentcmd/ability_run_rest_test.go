package agentcmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func restCall(mode string) AbilityRunCall {
	entry := []byte(`{"name":"wpmgr/rest-write"}`)
	route := []byte(`{"route_id":"wp-v2-pages-update-fields"}`)
	return AbilityRunCall{
		Mode: mode, RequestID: uuid.New(), Entry: entry, EntrySHA256: SHA256Hex(entry),
		Input: []byte(`{"route_id":"wp-v2-pages-update-fields"}`), Route: route, RouteSHA256: SHA256Hex(route),
	}
}

func TestBuildAbilityRunParamsRoute(t *testing.T) {
	c := restCall(AbilityRunModePrecheck)
	p, _, err := BuildAbilityRunParams(c)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(p, &got)
	if got["route"] != string(c.Route) || got["route_sha256"] != c.RouteSHA256 {
		t.Fatalf("route not carried verbatim: %s", p)
	}

	bad := restCall(AbilityRunModePrecheck)
	bad.RouteSHA256 = SHA256Hex([]byte("other"))
	if _, _, err := BuildAbilityRunParams(bad); err == nil {
		t.Fatal("a route hash over other bytes was accepted")
	}

	w := restCall(AbilityRunModeWrite)
	w.Expected = &AbilityRunExpected{PrecheckDigest: strings.Repeat("a", 64), BaseFingerprint: strings.Repeat("b", 64)}
	p, _, err = BuildAbilityRunParams(w)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(p), `"expected":{"precheck_digest":"`+strings.Repeat("a", 64)+`","base_fingerprint":"`) ||
		strings.Contains(string(p), "preview_digest") {
		t.Fatalf("route write expected: %s", p)
	}
	w.Expected.BaseFingerprint = ""
	if _, _, err := BuildAbilityRunParams(w); err == nil {
		t.Fatal("a route write without a base fingerprint was accepted")
	}

	rv := restCall(AbilityRunModeRevert)
	rv.Input = nil
	if _, _, err := BuildAbilityRunParams(rv); err == nil {
		t.Fatal("a revert carrying a route was accepted")
	}
}

func TestRestRefusalLabelsAreClosed(t *testing.T) {
	r := abilityRunRefusalOf(AbilityRunResponse{
		Code: "rest_intercepted", Violations: json.RawMessage(`["pre_dispatch","after_callbacks","<b>evil</b>","hook_order"]`),
	})
	want := []string{"pre_dispatch", "after_callbacks", "unknown"}
	if strings.Join(r.Violations, ",") != strings.Join(want, ",") {
		t.Fatalf("violations = %v, want %v", r.Violations, want)
	}
	r = abilityRunRefusalOf(AbilityRunResponse{Code: "side_effect_detected", Columns: json.RawMessage(`["post_content","wp_options.siteurl"]`)})
	if strings.Join(r.Columns, ",") != "post_content,unknown" {
		t.Fatalf("columns = %v", r.Columns)
	}
	for _, code := range []string{"route_disabled", "rest_error", "post_content_would_change", "post_touched", "sanitiser_changed_value"} {
		if got := abilityRunRefusalOf(AbilityRunResponse{Code: code}).Code; got != code {
			t.Fatalf("code %s mapped to %s", code, got)
		}
	}
}
