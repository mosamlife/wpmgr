package mcp

import (
	"strings"
	"testing"
)

// GH #823: a precheck refused over its input carries a fixed hint of ours,
// never the site's text.
func TestPrecheckRefusalHint(t *testing.T) {
	cases := map[string][]string{
		// The page-create builder's text rules: brackets, markup and
		// template syntax, control and invisible characters, length.
		"create_content_invalid":        {"parentheses", "square brackets", "HTML", "shortcodes", "invisible", "200", "5000", "60000"},
		"bad_input":                     {"site_ability_describe"},
		"sanitiser_changed_new_content": {"markup", "plain"},
	}
	for code, wants := range cases {
		hint := precheckRefusalHint(code)
		if hint == "" {
			t.Fatalf("%s: no hint", code)
		}
		for _, w := range wants {
			if !strings.Contains(hint, w) {
				t.Errorf("%s: hint does not mention %q: %s", code, w, hint)
			}
		}
	}
	for _, code := range []string{"", "conflict", "editor_unavailable", "content_editing_not_enabled", "unknown", "internal", "ability_denied"} {
		if hint := precheckRefusalHint(code); hint != "" {
			t.Errorf("%s: not an input refusal, but hinted %q", code, hint)
		}
	}
}
