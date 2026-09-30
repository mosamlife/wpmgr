package content

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

func sp(s string) *string { return &s }

func okResp(rows ...agentcmd.ContentProbeRow) agentcmd.ContentProbeListResponse {
	return agentcmd.ContentProbeListResponse{OK: true, ProbeVersion: 2, Mode: "list", Rows: rows}
}

func goodRow(id int64) agentcmd.ContentProbeRow {
	return agentcmd.ContentProbeRow{
		Post: id, Type: "page", Status: "publish", Verdict: "classic",
		Route: agentcmd.ContentProbeRoute{Number: 1, Reason: "content_column"}, Title: sp("Home"),
	}
}

var types = []string{"page", "post"}

func TestValidate_AcceptsGoodRow(t *testing.T) {
	rows, unknown, err := ValidateListResponse(okResp(goodRow(7)), 200, types, nil)
	if err != nil || unknown != 0 || len(rows) != 1 {
		t.Fatalf("rows=%d unknown=%d err=%v", len(rows), unknown, err)
	}
	if rows[0].Title != "Home" || rows[0].RouteNumber != 1 {
		t.Errorf("unexpected row %+v", rows[0])
	}
}

func TestValidate_StructuralViolationsFailTheWholeReply(t *testing.T) {
	bad := map[string]func(*agentcmd.ContentProbeListResponse){
		"not ok":         func(r *agentcmd.ContentProbeListResponse) { r.OK = false },
		"probe version":  func(r *agentcmd.ContentProbeListResponse) { r.ProbeVersion = 3 },
		"mode":           func(r *agentcmd.ContentProbeListResponse) { r.Mode = "single" },
		"post id zero":   func(r *agentcmd.ContentProbeListResponse) { r.Rows[0].Post = 0 },
		"type shape":     func(r *agentcmd.ContentProbeListResponse) { r.Rows[0].Type = "Page; DROP" },
		"status shape":   func(r *agentcmd.ContentProbeListResponse) { r.Rows[0].Status = strings.Repeat("a", 21) },
		"type not asked": func(r *agentcmd.ContentProbeListResponse) { r.Rows[0].Type = "product" },
		"route number":   func(r *agentcmd.ContentProbeListResponse) { r.Rows[0].Route.Number = 4 },
		"owner id shape": func(r *agentcmd.ContentProbeListResponse) {
			r.Rows[0].Owner = &agentcmd.ContentProbeOwner{IntegrationID: "Bad Id"}
		},
		"owner id length": func(r *agentcmd.ContentProbeListResponse) {
			r.Rows[0].Owner = &agentcmd.ContentProbeOwner{IntegrationID: strings.Repeat("a", 65)}
		},
		"duplicate id": func(r *agentcmd.ContentProbeListResponse) { r.Rows = append(r.Rows, r.Rows[0]) },
		"too many rows": func(r *agentcmd.ContentProbeListResponse) {
			for i := int64(2); i < 12; i++ {
				r.Rows = append(r.Rows, goodRow(i))
			}
		},
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			resp := okResp(goodRow(1))
			mutate(&resp)
			_, _, err := ValidateListResponse(resp, 5, types, nil)
			if !errors.Is(err, ErrInvalidProbeResponse) {
				t.Fatalf("want ErrInvalidProbeResponse, got %v", err)
			}
		})
	}
}

func TestValidate_UnknownVerdictOrReasonSkipsOnlyThatRow(t *testing.T) {
	unknownVerdict := goodRow(1)
	unknownVerdict.Verdict = "a_future_verdict"
	unknownReason := goodRow(2)
	unknownReason.Route.Reason = "a_future_reason"
	rows, unknown, err := ValidateListResponse(okResp(unknownVerdict, unknownReason, goodRow(3)), 200, types, nil)
	if err != nil {
		t.Fatal(err)
	}
	if unknown != 2 || len(rows) != 1 || rows[0].PostID != 3 {
		t.Fatalf("rows=%+v unknown=%d", rows, unknown)
	}
}

func TestValidate_EveryStoredEnumIsInTheClosedSets(t *testing.T) {
	for _, v := range []string{"classic", "empty", "block_document", "builder", "ambiguous", "unrecognised_builder", "special_page", "template_may_override"} {
		if _, ok := verdicts[v]; !ok {
			t.Errorf("verdict %q missing", v)
		}
	}
	for _, r := range []string{"content_column", "builder_not_supported", "block_editor_unsupported", "ambiguous_owner", "empty_page", "special_page", "unrecognised_builder", "template_may_override"} {
		if _, ok := routeReasons[r]; !ok {
			t.Errorf("reason %q missing", r)
		}
	}
}

func TestValidate_TitleIsCleanedCappedAndOnlyForPublished(t *testing.T) {
	r := goodRow(1)
	r.Title = sp("  Sale‮\u0000 now\n\t on  " + strings.Repeat("é", 200))
	rows, _, err := ValidateListResponse(okResp(r), 200, types, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := rows[0].Title
	if len(got) > 120 {
		t.Errorf("title %d bytes, want at most 120", len(got))
	}
	if strings.ContainsAny(got, "‮\x00\n\t") || strings.Contains(got, "  ") {
		t.Errorf("title not cleaned: %q", got)
	}
	if !strings.HasPrefix(got, "Sale now on") {
		t.Errorf("title = %q", got)
	}

	draft := goodRow(2)
	draft.Status = "draft"
	draft.Title = sp("Secret draft title")
	rows, _, err = ValidateListResponse(okResp(draft), 200, types, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Title != "" {
		t.Errorf("a draft's title was kept: %q", rows[0].Title)
	}
}

func TestValidate_OwnerNameComesFromTheAllowlistNeverTheSite(t *testing.T) {
	r := goodRow(1)
	r.Verdict = "builder"
	r.Route = agentcmd.ContentProbeRoute{Number: 3, Reason: "builder_not_supported"}
	r.Owner = &agentcmd.ContentProbeOwner{IntegrationID: "elementor", Version: sp("3.21.0")}
	rows, _, err := ValidateListResponse(okResp(r), 200, types, map[string]string{"elementor": "Elementor"})
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].OwnerName != "Elementor" || rows[0].OwnerVer != "3.21.0" || rows[0].OwnerID != "elementor" {
		t.Errorf("row = %+v", rows[0])
	}
	// An integration id the allowlist does not know gets no display name.
	rows, _, _ = ValidateListResponse(okResp(r), 200, types, nil)
	if rows[0].OwnerName != "" {
		t.Errorf("owner name invented: %q", rows[0].OwnerName)
	}
}

func TestValidate_BadOwnerVersionIsDroppedNotFatal(t *testing.T) {
	for _, v := range []string{"", "not a version", "<script>", strings.Repeat("1", 40), "1.2.3.4.5"} {
		r := goodRow(1)
		r.Owner = &agentcmd.ContentProbeOwner{IntegrationID: "divi", Version: sp(v)}
		rows, _, err := ValidateListResponse(okResp(r), 200, types, nil)
		if err != nil {
			t.Fatalf("version %q: %v", v, err)
		}
		if rows[0].OwnerVer != "" {
			t.Errorf("version %q kept as %q", v, rows[0].OwnerVer)
		}
	}
}

func TestBuildDescriptors(t *testing.T) {
	in := []Integration{
		{ID: "empty", Descriptor: []byte(`{}`)},
		{ID: "no-payload", Descriptor: []byte(`{"mode_flag":{"meta_key":"_x","on_values":["1"]}}`)},
		{ID: "good", Descriptor: []byte(`{"mode_flag":{"meta_key":"_x","on_values":["1"]},"payload_keys":["_data"],"stray_key":true,"integration_id":"spoofed","status":"admitted"}`)},
	}
	out := BuildDescriptors(in)
	if len(out) != 1 {
		t.Fatalf("got %d descriptors, want 1 (rows without detection data are left out)", len(out))
	}
	var d map[string]any
	if err := json.Unmarshal(out[0], &d); err != nil {
		t.Fatal(err)
	}
	if _, has := d["stray_key"]; has {
		t.Error("a key the agent does not accept was sent")
	}
	if d["integration_id"] != "good" || d["status"] != "detect_only" || d["enabled"] != true {
		t.Errorf("identity fields must come from the allowlist row, got %v", d)
	}
}

var hintAllowlist = []Integration{
	{ID: "elementor", Descriptor: []byte(`{"plugin_dir":"elementor"}`)},
	{ID: "bricks", Descriptor: []byte(`{"plugin_dir":"bricks-plugin","theme_slug":"bricks"}`)},
	{ID: "divi", Descriptor: []byte(`{"theme_slug":"Divi"}`)},
}

func TestBuildIndicators_OrdinarySiteSendsNoHints(t *testing.T) {
	comp := []byte(`{"plugins":[{"slug":"akismet/akismet.php","active":true},{"slug":"hello-dolly/hello.php","active":true}],"themes":[{"slug":"twentytwentyfour","active":true}]}`)
	ind := BuildIndicators(comp, hintAllowlist)
	if len(ind.PluginSlugs) != 0 || len(ind.ThemeSlugs) != 0 {
		t.Fatalf("an ordinary site sent hints: %+v", ind)
	}
	if ind.MetaKeyPrefixes == nil || ind.PluginSlugs == nil {
		t.Error("lists must be empty, not null")
	}
}

func TestBuildIndicators_AllowlistedBuilderIsSent(t *testing.T) {
	comp := []byte(`{"plugins":[{"slug":"elementor/elementor.php","active":true},{"slug":"akismet/akismet.php","active":true}],"themes":[{"slug":"bricks","active":true},{"slug":"divi","active":false}]}`)
	ind := BuildIndicators(comp, hintAllowlist)
	if len(ind.PluginSlugs) != 1 || ind.PluginSlugs[0] != "elementor" {
		t.Errorf("plugins = %v", ind.PluginSlugs)
	}
	if len(ind.ThemeSlugs) != 1 || ind.ThemeSlugs[0] != "bricks" {
		t.Errorf("themes = %v", ind.ThemeSlugs)
	}
	if got := BuildIndicators([]byte(`not json`), hintAllowlist); len(got.PluginSlugs) != 0 {
		t.Errorf("hints from garbage: %v", got)
	}
}
