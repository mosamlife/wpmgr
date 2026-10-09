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
			"alt text", "captions", "citations", "button text", "table cells",
			"Alt text may not contain a character reference such as &amp; or &#47;"},
		"bad_input":                     {"site_ability_describe"},
		"sanitiser_changed_new_content": {"markup", "plain"},
		// Outline grammar v2 (GUT).
		"layout_invalid": {"group", "column", "2 to 4", "10 to 90", "100", "400", "20 images", "12 buttons", "10 tables"},
		"link_invalid": {"https://", "user name", "/", "2048",
			"a link or an image address may not contain a character reference such as &amp; or &#47;",
			"a path on this site may not contain : or &#", "%3A"},
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
	// The link hint's list of characters a link may hold names neither ':'
	// nor '&': each is allowed only where the sentences after it say.
	link := precheckRefusalHint("link_invalid")
	_, list, ok := strings.Cut(link, "Use only ASCII letters, digits and ")
	if ok {
		list, _, ok = strings.Cut(list, ", write every %")
	}
	if !ok || list == "" {
		t.Fatalf("link_invalid: no character list to check: %s", link)
	}
	if strings.ContainsAny(list, ":&") {
		t.Errorf("link_invalid: the character list allows ':' or '&' everywhere: %q", list)
	}
	for _, code := range []string{"", "conflict", "editor_unavailable", "content_editing_not_enabled", "unknown", "internal", "ability_denied"} {
		if hint := precheckRefusalHint(code); hint != "" {
			t.Errorf("%s: not an input refusal, but hinted %q", code, hint)
		}
	}
}
