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
