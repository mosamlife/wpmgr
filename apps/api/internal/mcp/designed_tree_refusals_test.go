package mcp

import (
	"testing"
)

// designedPageInput is a designed page-create input holding the sections
// given (a JSON list body).
func designedPageInput(sections string) []byte {
	return []byte(`{"post_type":"page","editor":"builder:elementor","elementor_format":"classic","title":"T","outline_version":3,` +
		`"design":` + designedVectorDesign + `,"outline":[` + sections + `]}`)
}

// TestDesignedTreeRefusesWhatTheAgentRefuses: the twin builds no tree for an
// outline the agent's compile refuses, and builds one for the honest outline
// next to each refusal, so a refusal is the outline's own and not the
// harness's.
func TestDesignedTreeRefusesWhatTheAgentRefuses(t *testing.T) {
	image := func(mime string) []pageMediaFact {
		return []pageMediaFact{{ID: 42, URL: "https://example.com/wp-content/uploads/img-42.jpg", Filename: "img-42.jpg", Mime: mime, Width: 1600, Height: 900}}
	}
	section := func(rest string) string {
		return `{"type":"section","anchor":"top"` + rest + `}`
	}
	para := `{"type":"paragraph","text":"Hi"}`
	children := func(items ...string) string {
		out := `,"children":[`
		for i, it := range items {
			if i > 0 {
				out += ","
			}
			out += it
		}
		return out + `]`
	}
	cols := func(widths string, columns ...string) string {
		out := `{"type":"columns",` + widths + `"columns":[`
		for i, c := range columns {
			if i > 0 {
				out += ","
			}
			out += `{"children":[` + c + `]}`
		}
		return out + `]}`
	}

	cases := []struct {
		name     string
		outline  string
		media    []pageMediaFact
		accepted bool
	}{
		{"the honest section", section(children(para)), nil, true},
		{"the honest section with a background image", section(`,"background":{"attachment_id":42}` + children(para)), image("image/jpeg"), true},
		{"the honest two columns", section(children(cols(``, para, para))), nil, true},
		{"the honest buttons", section(children(`{"type":"buttons","buttons":[{"text":"Go","url":"/go?a=1","style":"text"}]}`)), nil, true},

		{"an anchor the theme uses", `{"type":"section","anchor":"main","children":[` + para + `]}`, nil, false},
		{"an anchor of the wrong shape", `{"type":"section","anchor":"A","children":[` + para + `]}`, nil, false},
		{"a section with no anchor", `{"type":"section","children":[` + para + `]}`, nil, false},
		{"a tone that is not one", section(`,"tone":"plum"` + children(para)), nil, false},
		{"a tone of null", section(`,"tone":null` + children(para)), nil, false},
		{"an alignment that is not one", section(`,"align":"right"` + children(para)), nil, false},
		{"a spacing that is not one", section(`,"spacing":"xxl"` + children(para)), nil, false},
		{"a width that is not one", section(`,"width":"wide"` + children(para)), nil, false},
		{"a height that is not one", section(`,"min_height":"tall"` + children(para)), nil, false},
		{"a section with no children", section(`,"children":[]`), nil, false},
		{"a background the site did not resolve", section(`,"background":{"attachment_id":42}` + children(para)), nil, false},
		{"a background that is not an image", section(`,"background":{"attachment_id":42}` + children(para)), image("application/pdf"), false},
		{"a background with a member of its own", section(`,"background":{"attachment_id":42,"blur":3}` + children(para)), image("image/jpeg"), false},
		{"a background overlay that is not one", section(`,"background":{"attachment_id":42,"overlay":"dark"}` + children(para)), image("image/jpeg"), false},
		{"a link holding an ampersand", section(children(`{"type":"buttons","buttons":[{"text":"Go","url":"/go?a=1&b=2"}]}`)), nil, false},
		{"a button style that is not one", section(children(`{"type":"buttons","buttons":[{"text":"Go","url":"/go","style":"ghost"}]}`)), nil, false},
		{"a paragraph that is only an address", section(children(`{"type":"paragraph","text":"https://example.com/watch"}`)), nil, false},
		{"a group inside a group", section(children(`{"type":"group","children":[{"type":"group","children":[` + para + `]}]}`)), nil, false},
		{"columns of one", section(children(`{"type":"columns","columns":[{"children":[` + para + `]}]}`)), nil, false},
		{"column widths outside 10 to 90", section(children(cols(`"widths":[5,95],`, para, para))), nil, false},
		{"column widths of the wrong count", section(children(cols(`"widths":[50,25,25],`, para, para))), nil, false},
		{"an image the site did not resolve", section(children(`{"type":"image","attachment_id":42,"alt":"x"}`)), nil, false},
		{"a pattern the twin does not compile", section(children(`{"type":"pattern","pattern":"hero","variant":"centered","children":[` + para + `]}`)), nil, false},
		{"an item that is not a section", para, nil, false},
	}
	for _, c := range cases {
		tree, ok := designedTreeWith(designedPageInput(c.outline), designedVectorRequestID, []string{"mobile", "tablet"}, c.media, map[string]designedPatternFunc{})
		if ok != c.accepted {
			t.Errorf("%s: built %v, want %v", c.name, ok, c.accepted)
		}
		if ok && len(tree) != 1 {
			t.Errorf("%s: %d sections built for one", c.name, len(tree))
		}
	}

	if _, ok := designedTreeWith([]byte(`{"outline":[`+section(children(para))+`]}`), designedVectorRequestID, nil, nil, nil); ok {
		t.Error("a page with no design was built")
	}
	if _, ok := designedTreeWith(designedPageInput(section(children(para))), "", nil, nil, nil); ok {
		t.Error("a page with no request id was built")
	}
}
