package agentcmd

import "testing"

func TestThemeTemplateRefusalDecode(t *testing.T) {
	for _, code := range []string{"theme_builder_unavailable", "shows_on_place_unavailable", "shows_on_target_missing"} {
		if got := abilityRunRefusalOf(AbilityRunResponse{Code: code}); got.Code != code {
			t.Errorf("%s decoded as %s", code, got.Code)
		}
	}
}
