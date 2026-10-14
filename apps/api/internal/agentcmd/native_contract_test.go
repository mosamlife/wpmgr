package agentcmd

import "testing"

func TestNativeRefusalDecode(t *testing.T) {
	for _, code := range []string{"widget_registry_unreadable", "widget_not_found", "atomic_not_supported", "widget_not_usable", "widget_schema_unreadable", "control_not_found", "native_settings_invalid", "native_designed_node", "native_node_shape", "native_duplicate_refused", "native_preview_too_large", "preview_unreadable"} {
		got := abilityRunRefusalOf(AbilityRunResponse{Code: code})
		if got.Code != code {
			t.Errorf("%s decoded as %s", code, got.Code)
		}
	}
	if got := abilityRunRefusalOf(AbilityRunResponse{Code: "widget_unknown_code"}); got.Code != "unknown" {
		t.Errorf("unknown code decoded as %s", got.Code)
	}
}

func TestNativeControlsUnavailableRefusalDecode(t *testing.T) {
	got := abilityRunRefusalOf(AbilityRunResponse{Code: "elementor_controls_unavailable"})
	if got.Code != "elementor_controls_unavailable" {
		t.Fatalf("controls unavailable decoded as %s", got.Code)
	}
}
