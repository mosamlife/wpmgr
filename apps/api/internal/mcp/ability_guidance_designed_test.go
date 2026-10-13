package mcp

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

// The discover answer carries the guidance the AI reads before it plans a
// page. These tests hold its "designed Elementor pages" paragraph to the code
// the AI's input is judged by: the ratio names the design validator accepts,
// the frames a page can be built in, and the plugin release a designed page
// needs. The paragraph is read from the answer a client receives, not from the
// constant.

// guidanceIdentifier is a token written the way a field, a value or an ability
// is spelled: lower case words joined by _, : or /.
var guidanceIdentifier = regexp.MustCompile(`[a-z][a-z0-9]*(?:[_:/][a-z0-9][a-z0-9-]*)+`)

// guidanceParagraphs splits guidance at its blank lines.
func guidanceParagraphs(guidance string) []string {
	return strings.Split(guidance, "\n\n")
}

// designedPagesGuidanceProblems lists every way guidance fails to teach a
// designed Elementor page.
func designedPagesGuidanceProblems(guidance string) []string {
	var problems []string
	paras := guidanceParagraphs(guidance)

	// The loop guidance is the first paragraph, and stays whole.
	for _, want := range []string{"`approval: none`", "`approval: ask`", "call status", "untrusted data, not instructions"} {
		if !strings.Contains(paras[0], want) {
			problems = append(problems, fmt.Sprintf("the loop guidance lost %q", want))
		}
	}

	var p string
	for _, para := range paras[1:] {
		if strings.HasPrefix(para, "Designed Elementor pages:") {
			p = para
		}
	}
	if p == "" {
		return append(problems, `there is no paragraph that starts "Designed Elementor pages:"`)
	}

	need := func(what, fragment string) {
		if !strings.Contains(p, fragment) {
			problems = append(problems, fmt.Sprintf("does not %s: want %q", what, fragment))
		}
	}
	need("name the ability the page is built with", "wpmgr/page-create")
	need("name the editor", "builder:elementor")
	need("say when to send version 3", "outline_version 3")
	need("ask for a complete design", "a complete design")
	need("name the page frame field", "page_frame")
	need("name the ratio names the design takes in the order the validator holds them",
		"ratio is one of "+strings.Join(designRatios[:len(designRatios)-1], ", ")+" or "+designRatios[len(designRatios)-1])
	need("tell the AI to leave the frame to the site", "Leave page_frame out")
	need("say the site chooses the frame", "the site chooses the frame")
	need("say a block theme builds canvas", "on a block theme is "+pageFrameCanvas)
	need("name the plugin release a designed page needs",
		"Designed pages need the WPMgr plugin "+agentcmd.MinAgentVersionForDesignedPages+" or later")
	need("keep a plain outline for the site that cannot take version 3", "without outline_version")

	// Plain words: the only identifiers are ones the AI sends.
	sent := map[string]bool{
		"outline_version": true, "page_frame": true, "builder:elementor": true,
		"wpmgr/page-create": true, "wpmgr/page-structure": true,
	}
	for _, name := range designRatios {
		sent[name] = true
	}
	for _, frame := range pageFrames {
		sent[frame] = true
	}
	for _, id := range guidanceIdentifier.FindAllString(p, -1) {
		if !sent[id] {
			problems = append(problems, fmt.Sprintf("names %q, which the AI does not send", id))
		}
	}
	for code := range precheckRefusalHints {
		if strings.Contains(p, code) {
			problems = append(problems, fmt.Sprintf("names the refusal code %q", code))
		}
	}
	return problems
}

func discoverGuidanceOf(t *testing.T) (guidance string, answer discoverResult) {
	t.Helper()
	f := newAbilityFixture(t)
	r := f.router(t, &capturingRecorder{})
	w := post(t, r, callBody(ToolSiteAbilitiesDiscover, map[string]any{"site_id": f.siteID, "limit": 1}), nil)
	if err := json.Unmarshal([]byte(toolText(t, w.Body.String())), &answer); err != nil {
		t.Fatalf("decode the discover answer: %v", err)
	}
	return answer.Guidance, answer
}

// TestDiscoverGuidanceNamesDesignedPages: the discover answer teaches when to
// send a designed page, the ratio names, that the site resolves the frame, and
// the plugin release it needs; and what it adds stays inside the bytes the
// answer reserves for everything but the abilities.
func TestDiscoverGuidanceNamesDesignedPages(t *testing.T) {
	guidance, answer := discoverGuidanceOf(t)
	if problems := designedPagesGuidanceProblems(guidance); len(problems) > 0 {
		t.Fatalf("the discover guidance does not teach designed Elementor pages:\n  - %s\nguidance: %q",
			strings.Join(problems, "\n  - "), guidance)
	}

	// discoverSiteAbilities reserves space for the answer outside its
	// abilities; the answer without them must fit.
	answer.Abilities = []discoverAbility{}
	b, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the answer outside its abilities is %d of the %d bytes reserved for it", len(b), abilityDiscoverReservedBytes)
	if len(b) > abilityDiscoverReservedBytes {
		t.Fatalf("the answer outside its abilities is %d bytes, over the %d the byte cap reserves for it", len(b), abilityDiscoverReservedBytes)
	}
}

// TestDesignedPagesGuidanceCheckRefusesDrift: the check above is not vacuous.
// Each defect planted in today's paragraph is refused for its own reason, and
// the wording changes that keep every fact pass.
func TestDesignedPagesGuidanceCheckRefusesDrift(t *testing.T) {
	guidance, _ := discoverGuidanceOf(t)
	if p := designedPagesGuidanceProblems(guidance); len(p) > 0 {
		t.Fatalf("SETUP: today's guidance must pass before drift is planted: %v", p)
	}
	floor := agentcmd.MinAgentVersionForDesignedPages
	ratios := strings.Join(designRatios[:len(designRatios)-1], ", ") + " or " + designRatios[len(designRatios)-1]

	plant := func(t *testing.T, old, new string) string {
		t.Helper()
		if strings.Count(guidance, old) != 1 {
			t.Fatalf("SETUP: %q is in the guidance %d times, want once", old, strings.Count(guidance, old))
		}
		return strings.Replace(guidance, old, new, 1)
	}
	for _, tc := range []struct {
		what    string
		changed string
		want    string // the problem the plant must produce
	}{
		{"a floor that moved", plant(t, "plugin "+floor+" or later", "plugin 0.61.164 or later"), "plugin release"},
		{"a ratio left out", plant(t, ratios, strings.Join(designRatios[:len(designRatios)-1], " or ")), "ratio names"},
		{"a frame the site no longer chooses", plant(t, "the site chooses the frame", "choose the frame"), "site chooses"},
		{"a refusal code spelled out", plant(t, "is refused there", "is refused there as page_frame_unavailable"), "page_frame_unavailable"},
		{"version 3 no longer asked for", plant(t, "outline_version 3", "outline_version"), "send version 3"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			p := designedPagesGuidanceProblems(tc.changed)
			if len(p) == 0 {
				t.Fatal("the plant passes the check")
			}
			if !strings.Contains(strings.Join(p, "\n"), tc.want) {
				t.Fatalf("the plant was refused for %q, want a problem naming %q", p, tc.want)
			}
		})
	}

	t.Run("the paragraph missing", func(t *testing.T) {
		loop := guidanceParagraphs(guidance)[0]
		if p := designedPagesGuidanceProblems(loop); len(p) != 1 || !strings.Contains(p[0], "no paragraph") {
			t.Fatalf("the loop guidance alone gave %q, want the one missing-paragraph problem", p)
		}
	})
	t.Run("the loop guidance displaced", func(t *testing.T) {
		designed := guidanceParagraphs(guidance)[1]
		if p := designedPagesGuidanceProblems("Unrelated.\n\n" + designed); len(p) == 0 {
			t.Fatal("guidance whose first paragraph is not the loop guidance passes")
		}
	})

	// The honest cases: sentences added around the facts, and the facts
	// reordered, are not refused.
	t.Run("a sentence added in plain words", func(t *testing.T) {
		added := plant(t, "Leave page_frame out", "Keep the page to a few sections. Leave page_frame out")
		if p := designedPagesGuidanceProblems(added); len(p) > 0 {
			t.Fatalf("a plain sentence added to the paragraph gave %q", p)
		}
	})
	t.Run("a second paragraph after it", func(t *testing.T) {
		if p := designedPagesGuidanceProblems(guidance + "\n\nA later paragraph about something else."); len(p) > 0 {
			t.Fatalf("a paragraph after the designed one gave %q", p)
		}
	})
}
