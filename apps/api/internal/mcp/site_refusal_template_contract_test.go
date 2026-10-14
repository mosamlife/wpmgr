package mcp

import (
	"os"
	"regexp"
	"testing"
)

func TestTemplateLiveListRefusalMatchesAgent(t *testing.T) {
	source, err := os.ReadFile("../../../agent/includes/abilities/builders/class-theme-template-create.php")
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`const DETAIL_LIVE_LIST_KEPT = '([^']+)'`).FindSubmatch(source)
	if len(match) != 2 {
		t.Fatal("agent live-list detail constant not found")
	}
	want := "Elementor Pro tried to change which header or footer shows on the live site. WPMgr kept it as it was, and nothing was changed."
	if got := ThemeTemplateRefusalMessage("side_effect_detected", string(match[1]), "site"); got != want {
		t.Fatalf("agent detail maps to %q, want %q", got, want)
	}
}
