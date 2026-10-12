package mcp

import (
	"strings"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/wpversion"
)

// lastAgentReleaseWithoutDesignedPages is the newest released agent that
// does not build designed pages.
const lastAgentReleaseWithoutDesignedPages = "0.61.164"

// TestDesignedPagesFloorRefusesOlderAgent holds a designed page to
// MinAgentVersionForDesignedPages at run: on an agent below it the run path's
// per-input floor answers the update-required code naming that floor, and at
// the floor it lets the input on. A version 2 input keeps its own floor. The
// dispatch half is the test of the same name in internal/abilityrequest.
func TestDesignedPagesFloorRefusesOlderAgent(t *testing.T) {
	designed := v3HeadInput(nil)
	if _, refusal := checkPageCreateInput(designed); refusal.Code != "" {
		t.Fatalf("the designed input is not one the grammar accepts: %+v", refusal)
	}

	t.Run("floor", func(t *testing.T) {
		for name, in := range map[string][]byte{
			"version 3":            designed,
			"version 3 with frame": v3HeadInput(map[string]string{"page_frame": `"canvas"`}),
			"a design alone":       v3HeadInput(nil, "outline_version"),
			"a page frame alone":   v3HeadInput(map[string]string{"page_frame": `"theme"`}, "outline_version", "design"),
			"another version":      v3HeadInput(map[string]string{"outline_version": `4`}),
		} {
			if got := PageCreateAgentFloor(in); got != agentcmd.MinAgentVersionForDesignedPages {
				t.Errorf("%s: floor %s, want %s (%s)", name, got, agentcmd.MinAgentVersionForDesignedPages, in)
			}
		}
		for name, c := range map[string]struct{ in, floor string }{
			"builder, version 2": {elementorInput(`[{"type":"paragraph","text":"x"}]`), agentcmd.MinAgentVersionForBuilderAdapters},
			"layout":             {layoutPage, agentcmd.MinAgentVersionForPageLayout},
			"text only":          {textOnlyPage, agentcmd.MinAgentVersionForPageCreate},
		} {
			if got := PageCreateAgentFloor([]byte(c.in)); got != c.floor {
				t.Errorf("%s: floor %s, want %s", name, got, c.floor)
			}
		}
		if wpversion.Compare(agentcmd.MinAgentVersionForDesignedPages, agentcmd.MinAgentVersionForBuilderAdapters) <= 0 {
			t.Fatalf("the designed-pages floor %s is not above the builder floor %s",
				agentcmd.MinAgentVersionForDesignedPages, agentcmd.MinAgentVersionForBuilderAdapters)
		}
	})

	t.Run("run", func(t *testing.T) {
		for _, v := range []string{lastAgentReleaseWithoutDesignedPages, agentcmd.MinAgentVersionForBuilderAdapters, agentcmd.MinAgentVersionForPageCreate} {
			r := pageCreateFloorRefusal(v, designed)
			if r == nil {
				t.Fatalf("a designed page on %s: not refused", v)
			}
			if r.reason != reasonAgentOutdated || r.err.Code != ErrCodeSiteAgentOutdated ||
				r.err.Details["min_agent_version"] != agentcmd.MinAgentVersionForDesignedPages || r.err.Details["retryable"] != false {
				t.Errorf("a designed page on %s: reason %s, code %s, details %v", v, r.reason, r.err.Code, r.err.Details)
			}
			if r.err.Message != msgAbilityDesignedOutdated || !strings.Contains(r.err.Message, agentcmd.MinAgentVersionForDesignedPages) {
				t.Errorf("a designed page on %s: message %q", v, r.err.Message)
			}
		}
		for _, v := range []string{agentcmd.MinAgentVersionForDesignedPages, "0.61.170"} {
			if r := pageCreateFloorRefusal(v, designed); r != nil {
				t.Errorf("a designed page on %s: refused (%v)", v, r)
			}
		}
		// The floors of version 2 inputs did not move.
		builder := []byte(elementorInput(`[{"type":"paragraph","text":"x"}]`))
		if r := pageCreateFloorRefusal(lastAgentReleaseWithoutDesignedPages, builder); r != nil {
			t.Errorf("a version 2 builder page on %s: refused (%v)", lastAgentReleaseWithoutDesignedPages, r)
		}
		if r := pageCreateFloorRefusal(agentcmd.MinAgentVersionForPageLayout, builder); r == nil ||
			r.err.Message != msgAbilityBuilderOutdated || r.err.Details["min_agent_version"] != agentcmd.MinAgentVersionForBuilderAdapters {
			t.Errorf("a version 2 builder page below its floor: %v", r)
		}
		if r := pageCreateFloorRefusal("0.61.158", []byte(layoutPage)); r == nil || r.err.Message != msgAbilityLayoutOutdated {
			t.Errorf("a layout page below its floor: %v", r)
		}
		if r := pageCreateFloorRefusal("0.61.158", []byte(textOnlyPage)); r != nil {
			t.Errorf("a text-only page: refused (%v)", r)
		}
	})
}

// firstAgentReleaseWithDesignedPages is the agent release that builds
// designed pages, the floor the run path names.
const firstAgentReleaseWithDesignedPages = "0.61.165"

// TestPageCreateRun_DesignedPagesFloorIsPerInput holds a designed page to its
// floor through the real transport. On the newest agent that does not build
// designed pages the answer is the update-required refusal naming the floor,
// and the site receives no precheck call; at the floor the precheck is sent,
// so the refusal below it is the floor's and not another check's.
func TestPageCreateRun_DesignedPagesFloorIsPerInput(t *testing.T) {
	designed := string(v3HeadInput(nil))
	if agentcmd.MinAgentVersionForDesignedPages != firstAgentReleaseWithDesignedPages ||
		lastAgentReleaseWithoutDesignedPages != "0.61.164" {
		t.Fatalf("the floor %s and the release below it %s are not the ones this test is about",
			agentcmd.MinAgentVersionForDesignedPages, lastAgentReleaseWithoutDesignedPages)
	}

	agent := &refusingPrecheckAgent{code: "create_content_invalid"}
	r, site := pageCreateRunRouterLimits(t, lastAgentReleaseWithoutDesignedPages, agent, elementorEnabled)
	ref := runPageCreate(t, r, site, designed)
	if ref.code != codeSiteAgentOutdated || ref.data["min_agent_version"] != firstAgentReleaseWithDesignedPages ||
		ref.data["retryable"] != false || ref.msg != msgAbilityDesignedOutdated ||
		!strings.Contains(ref.msg, firstAgentReleaseWithDesignedPages) {
		t.Fatalf("a designed page on %s: %s", lastAgentReleaseWithoutDesignedPages, ref.raw)
	}
	if len(agent.calls) != 0 {
		t.Fatalf("the site was asked %d times for a page it cannot build", len(agent.calls))
	}

	// At the floor the same input reaches the site, once, as a precheck.
	agent = &refusingPrecheckAgent{code: "create_content_invalid"}
	r, site = pageCreateRunRouterLimits(t, firstAgentReleaseWithDesignedPages, agent, elementorEnabled)
	_ = runPageCreate(t, r, site, designed)
	if len(agent.calls) != 1 || agent.calls[0].Mode != agentcmd.AbilityRunModePrecheck {
		t.Fatalf("a designed page on %s: %d precheck calls, want 1", firstAgentReleaseWithDesignedPages, len(agent.calls))
	}
}
