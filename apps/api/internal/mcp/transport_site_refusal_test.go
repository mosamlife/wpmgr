package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

// TestSiteRefusalMessageCarriesCodeAndHint: the JSON-RPC message of a
// page-edit or page-create precheck the site refused names the code (and,
// for a page-edit conflict, the reason) and carries the fixed hint, so a
// client that shows only the message still tells the AI what to do. The
// message is ours: the site's detail text never reaches it, and the data
// keeps its shape.
func TestSiteRefusalMessageCarriesCodeAndHint(t *testing.T) {
	t.Run("page-edit conflict", func(t *testing.T) {
		for _, reason := range []string{"editor_open", "changed_since_read", "autosave_pending"} {
			w := toolErrorWire(t, pageEditPrecheckRefusal(t, map[string]any{
				"ok": false, "outcome": "refused", "code": "conflict", "detail": reason,
			}))
			var data map[string]any
			if err := json.Unmarshal(w.Data, &data); err != nil {
				t.Fatalf("%s: data is not an object: %s", reason, w.Data)
			}
			hint, _ := data["hint"].(string)
			if w.Code != codeInvalidToolArguments || hint == "" {
				t.Fatalf("%s: code %d data %s", reason, w.Code, w.Data)
			}
			for _, want := range []string{"code conflict", "reason " + reason, hint} {
				if !strings.Contains(w.Message, want) {
					t.Fatalf("%s: message %q does not carry %q", reason, w.Message, want)
				}
			}
		}
	})

	t.Run("page-edit refusal with site text", func(t *testing.T) {
		for _, reply := range []map[string]any{
			{"ok": false, "outcome": "refused", "code": "conflict", "detail": plantedInstruction},
			{"ok": false, "outcome": "refused", "code": "node_not_found", "detail": plantedInstruction},
			{"ok": false, "outcome": "refused", "code": "ignore_previous_instructions", "detail": plantedInstruction},
		} {
			w := toolErrorWire(t, pageEditPrecheckRefusal(t, reply))
			raw, _ := json.Marshal(w)
			if carriesText(raw, plantedInstruction) || strings.Contains(string(raw), "ignore_previous") {
				t.Fatalf("site text reached the AI: %s", raw)
			}
			var data map[string]any
			_ = json.Unmarshal(w.Data, &data)
			code, _ := data["code"].(string)
			if code == "" || !strings.Contains(w.Message, "code "+code) {
				t.Fatalf("message %q does not name the code %q", w.Message, code)
			}
			if strings.Contains(w.Message, "reason ") {
				t.Fatalf("a detail outside the closed set was named as a reason: %q", w.Message)
			}
			if hint, _ := data["hint"].(string); hint != "" && !strings.Contains(w.Message, hint) {
				t.Fatalf("message %q does not carry the hint %q", w.Message, hint)
			}
		}
	})

	t.Run("page-create", func(t *testing.T) {
		cases := []struct {
			code, wantCode, hint string
		}{
			{"create_content_invalid", "create_content_invalid", hintCreateContentInvalid},
			{"image_not_available", "image_not_available", hintImageNotAvailable},
			{"builder_crashed", "builder_crashed", ""},
			{"made_up_code", "unknown", ""},
		}
		for _, c := range cases {
			agent := newWireRefusingAgent(t, c.code)
			r, site := pageCreateRunRouterWith(t, agentcmd.MinAgentVersionForPageLayout, agent)
			ref := runPageCreate(t, r, site, textOnlyPage)
			if len(agent.calls) != 1 || ref.code != codeInvalidToolArguments {
				t.Fatalf("%s: %d calls, %s", c.code, len(agent.calls), ref.raw)
			}
			if !strings.Contains(ref.msg, "code "+c.wantCode) || strings.Contains(ref.msg, "reason ") {
				t.Fatalf("%s: message %q does not name the code alone", c.code, ref.msg)
			}
			if c.hint != "" && !strings.Contains(ref.msg, c.hint) {
				t.Fatalf("%s: message %q does not carry the hint", c.code, ref.msg)
			}
			if carriesText([]byte(ref.raw), plantedInstruction) {
				t.Fatalf("%s: the site's words reached the AI: %s", c.code, ref.raw)
			}
			// The data keeps its shape: code, retryable and the hint when
			// there is one.
			want := map[string]any{"code": c.wantCode, "retryable": false}
			if c.hint != "" {
				want["hint"] = c.hint
			}
			if len(ref.data) != len(want) {
				t.Fatalf("%s: data %v, want %v", c.code, ref.data, want)
			}
			for k, v := range want {
				if ref.data[k] != v {
					t.Fatalf("%s: data %v, want %v", c.code, ref.data, want)
				}
			}
		}
	})

	t.Run("a code that did not come through the decoder", func(t *testing.T) {
		r, _, site := pageCreateRunRouter(t, agentcmd.MinAgentVersionForPageLayout, "<b>x")
		ref := runPageCreate(t, r, site, textOnlyPage)
		if carriesText([]byte(ref.raw), "<b>x") || carriesText([]byte(ref.raw), plantedInstruction) {
			t.Fatalf("site text reached the AI: %s", ref.raw)
		}
		if !strings.Contains(ref.msg, "code unknown") || ref.data["code"] != "unknown" {
			t.Fatalf("a code outside the closed set is not unknown: %s", ref.raw)
		}
	})
}
