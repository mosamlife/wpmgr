package agentcmd

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/wpversion"
)

// TestMinAgentVersionForBuilderAdapters_Pinned pins the literal: the first
// agent release that builds a page-create draft with a page builder. It is
// re-pinned to the number that release ships as, in the same commit as the
// constant (internal/mcp holds it to the agent tree).
func TestMinAgentVersionForBuilderAdapters_Pinned(t *testing.T) {
	const firstVersionWithBuilderAdapters = "0.61.161"
	if MinAgentVersionForBuilderAdapters != firstVersionWithBuilderAdapters {
		t.Fatalf("MinAgentVersionForBuilderAdapters = %q, want %q; if the floor is re-gated, update this pin in the same commit",
			MinAgentVersionForBuilderAdapters, firstVersionWithBuilderAdapters)
	}
	if wpversion.Compare(MinAgentVersionForBuilderAdapters, MinAgentVersionForPageLayout) <= 0 {
		t.Fatalf("the builder floor %q must be above the layout floor %q: the builder path needs outline grammar v2",
			MinAgentVersionForBuilderAdapters, MinAgentVersionForPageLayout)
	}
}

// builderRefusalCodes are the codes the agent's wpmgr/page-create answers
// for a page built with a page builder (MinAgentVersionForBuilderAdapters).
var builderRefusalCodes = []string{
	"builder_not_enabled", "builder_not_available", "node_not_supported_by_builder", "image_alt_from_library",
	"leaf_unsafe", "adapter_key_not_allowed", "page_has_unknown_elements", "builder_would_change_layout",
	"builder_save_refused", "builder_crashed",
}

// TestAbilityRun_BuilderRefusalsKeepTheirCode sends the agent's own refusal
// JSON through the real transport and decoder: each builder code arrives as
// itself, never unknown.
func TestAbilityRun_BuilderRefusalsKeepTheirCode(t *testing.T) {
	entry := []byte(`{"name":"wpmgr/page-create"}`)
	for _, code := range builderRefusalCodes {
		srv := fakeAbilityAgent(t, func(map[string]string) any {
			return map[string]any{"ok": false, "outcome": "refused", "code": code, "detail": "outline[0].buttons[0].style", "retryable": false}
		})
		_, err := realCommandClient(t).AbilityRun(context.Background(), uuid.New(), srv.URL, AbilityRunCall{
			Mode: AbilityRunModePrecheck, RequestID: uuid.New(), Entry: entry, EntrySHA256: SHA256Hex(entry),
			Input: []byte(`{"post_type":"page"}`),
		})
		srv.Close()
		var ref *AbilityRunRefusal
		if !errors.As(err, &ref) {
			t.Errorf("code %q: err = %v, want a refusal", code, err)
			continue
		}
		if ref.Code != code {
			t.Errorf("code %q arrived as %q", code, ref.Code)
		}
	}
}
