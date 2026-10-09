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
		"create_content_invalid": {"parentheses", "square brackets", "HTML", "shortcodes", "invisible", "200", "5000", "60000",
			"alt text", "captions", "citations", "button text", "table cells"},
		"bad_input":                     {"site_ability_describe"},
		"sanitiser_changed_new_content": {"markup", "plain"},
		// Outline grammar v2 (GUT).
		"layout_invalid":            {"group", "column", "2 to 4", "10 to 90", "100", "400", "20 images", "12 buttons", "10 tables"},
		"link_invalid":              {"https://", "user name", "/", "2048"},
		"image_not_available":       {"media library", "wp-v2-media-list"},
		"image_url_unusable":        {"wp-v2-media-list"},
		"layout_needs_block_editor": {"classic editor", "wordpress_classic", "captions"},
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
