package mcp

import (
	"strings"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

// v3GrammarCodes are the refusal codes that outline version 3 added to the
// ones of version 2.
var v3GrammarCodes = []string{
	pageCreateAnchorInvalid, pageCreateAnchorDuplicate, pageCreateAnchorReserved, pageCreateLinkAnchorMissing,
	pageCreateHeroMisplaced, pageCreateH1OutsideHero, pageCreateItemsOutOfRange, pageCreateIconUnknown,
}

// Every code the control plane answers over the shared table, and every code
// the version 3 grammar added, has a fixed hint and is in the closed set of
// codes the agent may answer; a code outside that set arrives as unknown and
// its hint could never be given.
func TestEveryV3CodeHasHint(t *testing.T) {
	codes := map[string]string{}
	for _, code := range v3GrammarCodes {
		codes[code] = "the list of version 3 codes"
	}
	answered := map[string]bool{}
	for _, c := range readV3Cases(t) {
		if code := c.want(); code != "" {
			answered[code] = true
			if codes[code] == "" {
				codes[code] = "row " + c.Name
			}
		}
	}
	// A table that lost its refused rows would pass over every code below.
	if len(answered) < 8 {
		t.Fatalf("the shared table is answered with %d codes; a table that lost its refused rows proves nothing", len(answered))
	}
	for _, code := range v3GrammarCodes {
		if !answered[code] {
			t.Errorf("no refused row of the shared table answers %s", code)
		}
	}
	for code, from := range codes {
		if hint := precheckRefusalHint(code); len(hint) < 40 || strings.Contains(hint, "\n") {
			t.Errorf("%s (%s): hint %q", code, from, hint)
		}
		if _, ok := agentcmd.AbilityRunRefusalCodes[code]; !ok {
			t.Errorf("%s (%s) is not in agentcmd.AbilityRunRefusalCodes", code, from)
		}
	}
}

// siteOnlyCodes are the refusals that only the site can make: a font family
// its Elementor does not have, a page frame whose template its theme does not
// offer, and a page frame its theme cannot show a designed page in. The
// control plane never answers them itself, so the codes of the control
// plane's own answers do not reach them. They are written out here as the
// agent spells them.
var siteOnlyCodes = []string{"font_unknown", "page_template_unavailable", "page_frame_unavailable"}

// A site-only code is in the closed set of agent refusal codes, so that an
// answer carrying it is read as itself and not as unknown, and has a fixed
// hint that says what to change in the input.
func TestSiteOnlyCodesHaveHint(t *testing.T) {
	for _, code := range siteOnlyCodes {
		if hint := precheckRefusalHint(code); len(hint) < 40 || strings.Contains(hint, "\n") {
			t.Errorf("%s: hint %q", code, hint)
		}
		if _, ok := agentcmd.AbilityRunRefusalCodes[code]; !ok {
			t.Errorf("%s is not in agentcmd.AbilityRunRefusalCodes", code)
		}
	}
}

// awaitingAgentCodes are answers of the shared table that the agent does not
// give yet. Such a code has neither a hint nor a place in the closed set until
// the job that makes the agent give it adds both.
var awaitingAgentCodes = map[string]bool{"background_not_image": true, "containers_required": true}

// Every answer the shared table gives for the agent is read as itself and has
// a fixed hint, whether or not the control plane makes the same refusal. A
// code that has only one of the two is half added, and is a failure.
func TestEveryAgentAnswerOfTheV3TableHasHint(t *testing.T) {
	answers := map[string]string{}
	for _, c := range readV3Cases(t) {
		if _, seen := answers[c.Agent]; !seen && c.Agent != "ok" {
			answers[c.Agent] = c.Name
		}
	}
	// A table that lost its refused rows, or the rows only the site answers,
	// would pass over every code below.
	if len(answers) < 10 {
		t.Fatalf("the shared table is answered with %d refusal codes; a table that lost its refused rows proves nothing", len(answers))
	}
	for _, code := range siteOnlyCodes[:1] {
		if answers[code] == "" {
			t.Fatalf("no row of the shared table is answered with %s; the site-only rows are gone", code)
		}
	}
	for code, row := range answers {
		hint := precheckRefusalHint(code)
		_, inSet := agentcmd.AbilityRunRefusalCodes[code]
		if awaitingAgentCodes[code] {
			if inSet != (hint != "") {
				t.Errorf("%s (row %s) is awaiting the agent but is half added: in the closed set %t, hint %q", code, row, inSet, hint)
			}
			continue
		}
		if len(hint) < 40 || strings.Contains(hint, "\n") {
			t.Errorf("%s (row %s): hint %q", code, row, hint)
		}
		if !inSet {
			t.Errorf("%s (row %s) is not in agentcmd.AbilityRunRefusalCodes", code, row)
		}
	}
}
