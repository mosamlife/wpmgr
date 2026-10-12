package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// Operations of the page-edit floor table: the ones that need a designed
// page, and the ones every agent that edits pages runs.
const (
	opsInsertPattern = `[{"op":"insert","after":"3c4d5e6","outline":[{"type":"pattern","pattern":"faq",` +
		`"variant":"accordion","faq":[{"question":"Q?","answer":"A."}]}]}]`
	opsReplaceSection = `[{"op":"replace","ref":"3c4d5e6","outline":[{"type":"section","anchor":"press",` +
		`"children":[{"type":"paragraph","text":"Press"}]}]}]`
	opsSetTextPath = `[{"op":"set_text","ref":"3c4d5e6","field":"plans.1.price","text":"49"}]`
	opsRemoveNode  = `[{"op":"remove","ref":"3c4d5e6"}]`
	opsMoveNode    = `[{"op":"move","ref":"3c4d5e6","after":"4d5e6f7"}]`
	// opsWordsOnly adds a paragraph whose text names a section, a pattern and
	// a design: words in a text are not a node.
	opsWordsOnly = `[{"op":"insert","after":"3c4d5e6","outline":[{"type":"paragraph",` +
		`"text":"type section, type pattern and design are only words here"}]}]`
	// opsDeepSection holds a section under a group, deeper than the top of
	// the outline.
	opsDeepSection = `[{"op":"insert","after":"3c4d5e6","outline":[{"type":"group","children":[` +
		`{"type":"section","anchor":"deep","children":[{"type":"paragraph","text":"x"}]}]}]}]`
)

// TestPageEditAgentFloor: an input that carries a design, adds a section or a
// pattern, or sets text by a pattern path needs the agent that builds designed
// pages; every other edit keeps the floor of the entry. An input that cannot
// be read counts as designed, so the floor fails closed.
func TestPageEditAgentFloor(t *testing.T) {
	designed, plain := agentcmd.MinAgentVersionForDesignedPages, agentcmd.MinAgentVersionForBuilderEdit
	if designed == plain {
		t.Fatalf("the two floors are both %s; the table below proves nothing", plain)
	}
	cases := []struct {
		name  string
		input []byte
		want  string
	}{
		{"a paragraph insert", editOps(opsInsertParagraph, ""), plain},
		{"a plain set_text", editOps(opsSetText, ""), plain},
		{"a remove", editOps(opsRemoveNode, ""), plain},
		{"a move", editOps(opsMoveNode, ""), plain},
		{"words that name a section, a pattern and a design", editOps(opsWordsOnly, ""), plain},
		{"the stub of a request with no operations", []byte(`{"post_id":418}`), plain},
		{"a design on a paragraph insert", editOps(opsInsertParagraph, `"design":`+specDesignJSON), designed},
		{"a design that is null", editOps(opsInsertParagraph, `"design":null`), designed},
		{"a section insert", editOps(opsInsertSection, ""), designed},
		{"a section replace", editOps(opsReplaceSection, ""), designed},
		{"a pattern insert", editOps(opsInsertPattern, ""), designed},
		{"a section under a group", editOps(opsDeepSection, ""), designed},
		{"a set_text by a pattern path", editOps(opsSetTextPath, ""), designed},
		{"operations that are not a list", []byte(`{"post_id":418,"operations":"x"}`), designed},
		{"an operation that is not an object", []byte(`{"post_id":418,"operations":[1]}`), designed},
		{"an outline that is not JSON", []byte(`{"post_id":418,"operations":[{"op":"insert","outline":x}]}`), designed},
		{"an input that is not an object", []byte(`["design"]`), designed},
		{"no input", nil, designed},
	}
	for _, c := range cases {
		if got := PageEditAgentFloor(c.input); got != c.want {
			t.Errorf("%s: floor %s, want %s (%s)", c.name, got, c.want, c.input)
		}
	}
}

// TestPageEditFloorRefusesOlderAgent: on an agent below the floor of its
// input, the per-input floor answers the update-required refusal naming that
// floor; at the floor it lets the input on, and an edit with no designed
// content keeps the floor of the entry.
func TestPageEditFloorRefusesOlderAgent(t *testing.T) {
	designed := editOps(opsInsertSection, `"design":`+specDesignJSON)
	for _, v := range []string{lastAgentReleaseWithoutDesignedPages, agentcmd.MinAgentVersionForBuilderEdit, ""} {
		r := pageEditFloorRefusal(v, designed)
		if r == nil {
			t.Errorf("a designed edit on %q: not refused", v)
			continue
		}
		if r.reason != reasonAgentOutdated || r.err.Code != ErrCodeSiteAgentOutdated ||
			r.err.Details["min_agent_version"] != agentcmd.MinAgentVersionForDesignedPages || r.err.Details["retryable"] != false {
			t.Errorf("a designed edit on %q: reason %s, code %s, details %v", v, r.reason, r.err.Code, r.err.Details)
		}
		if r.err.Message != msgPageEditDesignedOutdated || !strings.Contains(r.err.Message, agentcmd.MinAgentVersionForDesignedPages) {
			t.Errorf("a designed edit on %q: message %q", v, r.err.Message)
		}
	}
	for _, v := range []string{agentcmd.MinAgentVersionForDesignedPages, "0.61.170"} {
		if r := pageEditFloorRefusal(v, designed); r != nil {
			t.Errorf("a designed edit on %s: refused (%v)", v, r.err)
		}
	}
	// The floor of an edit with no designed content did not move.
	plain := editOps(opsInsertParagraph, "")
	for _, v := range []string{agentcmd.MinAgentVersionForBuilderEdit, lastAgentReleaseWithoutDesignedPages} {
		if r := pageEditFloorRefusal(v, plain); r != nil {
			t.Errorf("a plain edit on %s: refused (%v)", v, r.err)
		}
	}
}

// TestPageEditRun_DesignedFloorIsPerInput holds a designed edit to its floor
// through runPageEdit. On the newest agent that does not build designed pages
// the answer is the update-required refusal naming the floor and the site
// receives no call; at the floor the precheck is sent. A design the call
// inherits from its draft counts as much as one the AI sent.
func TestPageEditRun_DesignedFloorIsPerInput(t *testing.T) {
	run := func(t *testing.T, agentVersion string, input []byte, drafts *fakeDraftCreateInputs) (*toolRefusal, int) {
		t.Helper()
		agent := newAnsweringAgent(t, map[string]any{
			"ok": false, "outcome": "refused", "code": pageEditNodeNotFound, "detail": "x", "retryable": false,
		})
		e := catalogueRow(AbilityPageEdit, "wpmgr", "write")
		e.ApprovalMode, e.Snapshot = "per_call", "builder_document"
		e.Limits = []byte(`{"builders_enabled":["elementor"]}`)
		_, sum, _ := testEntryEncoder(e)
		e.EntrySha256 = &sum
		eng := &abilityEngine{agent: agent, entry: testEntryEncoder, readLimit: newAbilityReadLimiter(), drafts: inputDraftStore{}}
		if drafts != nil {
			eng.drafts = drafts
		}
		site := abilitySite{row: sqlc.Site{ID: uuid.New(), Url: agent.srv.URL, AgentVersion: agentVersion}}
		_, err := (&Service{}).runPageEdit(context.Background(), AuthorizedRequest{GrantID: uuid.New()}, eng, site, &e, "example.com", input)
		var r *toolRefusal
		if !errors.As(err, &r) {
			t.Fatalf("got %v, want a refusal", err)
		}
		return r, len(agent.calls)
	}
	wantOutdated := func(t *testing.T, r *toolRefusal, calls int) {
		t.Helper()
		if r.reason != reasonAgentOutdated || r.err.Code != ErrCodeSiteAgentOutdated ||
			r.err.Details["min_agent_version"] != agentcmd.MinAgentVersionForDesignedPages ||
			r.err.Message != msgPageEditDesignedOutdated {
			t.Fatalf("reason %s, code %s, message %q, details %v; want the designed-pages floor", r.reason, r.err.Code, r.err.Message, r.err.Details)
		}
		if calls != 0 {
			t.Fatalf("the site was asked %d times for an edit it cannot run", calls)
		}
	}

	withDesign := editOps(opsInsertSection, `"design":`+specDesignJSON)
	t.Run("a design the AI sent", func(t *testing.T) {
		r, calls := run(t, lastAgentReleaseWithoutDesignedPages, withDesign, nil)
		wantOutdated(t, r, calls)
	})
	t.Run("a pattern path", func(t *testing.T) {
		r, calls := run(t, lastAgentReleaseWithoutDesignedPages, editOps(opsSetTextPath, ""), nil)
		wantOutdated(t, r, calls)
	})
	t.Run("the design of the draft", func(t *testing.T) {
		drafts := &fakeDraftCreateInputs{input: []byte(designedCreateInput)}
		r, calls := run(t, lastAgentReleaseWithoutDesignedPages, editOps(opsInsertParagraph, ""), drafts)
		wantOutdated(t, r, calls)
	})
	t.Run("at the floor the site is asked once", func(t *testing.T) {
		r, calls := run(t, agentcmd.MinAgentVersionForDesignedPages, withDesign, nil)
		if calls != 1 || r.err.Code == ErrCodeSiteAgentOutdated {
			t.Fatalf("%d precheck calls, refusal %s; want one call and the site's own refusal", calls, r.err.Code)
		}
	})
	t.Run("a plain edit keeps the floor of the entry", func(t *testing.T) {
		drafts := &fakeDraftCreateInputs{input: []byte(plainCreateInput)}
		r, calls := run(t, agentcmd.MinAgentVersionForBuilderEdit, editOps(opsInsertParagraph, ""), drafts)
		if calls != 1 || r.err.Code == ErrCodeSiteAgentOutdated {
			t.Fatalf("%d precheck calls, refusal %s; want one call and the site's own refusal", calls, r.err.Code)
		}
	})
}
