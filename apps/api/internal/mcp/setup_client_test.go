package mcp

import (
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// These run in CI. The integration proofs in
// apps/api/tests/mcp_m128_setup_client_integration_test.go do not -- the
// integration package is excluded by name -- so the shape rule, which is the
// half a reviewer is most likely to "simplify", is pinned here where a PR
// actually runs it.

// TestValidateSetupClientAcceptsNilAndRefusesEmpty pins the distinction the
// *string exists for. nil is "the caller never asked" and is always valid; ""
// is a caller that sent the key and put nothing in it, which is a malformed
// claim. Rewriting "" to NULL would report success for a step-2 choice that was
// never stored.
func TestValidateSetupClientAcceptsNilAndRefusesEmpty(t *testing.T) {
	if err := validateSetupClient(nil); err != nil {
		t.Fatalf("nil setup_client refused (%v); the consent path and every "+
			"non-wizard caller pass nil and are entitled to", err)
	}
	empty := ""
	if err := validateSetupClient(&empty); err == nil {
		t.Fatal("an empty setup_client was accepted. It would store as \"\", " +
			"which is neither a choice nor NULL, and S31's filter would have a " +
			"third state nobody designed for.")
	}
}

// TestValidateSetupClientPinsShapeNotVocabulary is the decision this file
// exists to defend. Both halves are asserted together because each is the
// other's control: a server that refused every unknown slug would pass the
// first half alone and break the wizard at step 2 for a client its own UI
// offers, and a server that validated nothing would pass the second half alone
// and let 'Windsurf' and 'windsurf ' both into a column S31 compares by
// equality.
func TestValidateSetupClientPinsShapeNotVocabulary(t *testing.T) {
	// Every id in apps/web/src/features/ai-connections/client-table.ts, plus
	// slugs no vocabulary knows. ALL must pass: the server pins spelling only.
	valid := []string{
		"claude-code", "claude-desktop", "chatgpt", "codex-cli", "cursor",
		"vscode", "windsurf", "gemini-cli", "generic",
		// Not in the table today, and deliberately accepted. A client added
		// to that table must not require a control-plane migration or release.
		"some-future-client", "a", "x9", "a-b-c-d",
	}
	for _, v := range valid {
		v := v
		if err := validateSetupClient(&v); err != nil {
			t.Errorf("validateSetupClient(%q) = %v, want accepted. An "+
				"unrecognised but well-formed slug is a legitimate stored "+
				"state that renders as the generic panel, not an error.", v, err)
		}
	}

	// Refused for SHAPE, so that equality on the stored column is trustworthy.
	invalid := []string{
		"Windsurf",       // upper case
		"windsurf ",      // trailing space
		" windsurf",      // leading space
		"windsurf_beta",  // underscore
		"windsurf--beta", // doubled hyphen
		"-windsurf",      // leading hyphen
		"windsurf-",      // trailing hyphen
		"wind surf",      // inner space
		"Windsurf (Devin Desktop)",
		"windsurf\n",
	}
	for _, v := range invalid {
		v := v
		if err := validateSetupClient(&v); err == nil {
			t.Errorf("validateSetupClient(%q) was ACCEPTED. Free spelling in "+
				"this column makes S31 render \"None of them was set up for "+
				"Windsurf\" while a matching connection sits in the list.", v)
		}
	}

	// The length half of the CHECK, asserted at the boundary rather than near
	// it: 64 passes, 65 does not.
	at := make([]byte, setupClientShapeMaxLen)
	for i := range at {
		at[i] = 'a'
	}
	ok := string(at)
	if err := validateSetupClient(&ok); err != nil {
		t.Errorf("a %d-character slug was refused (%v); the CHECK allows it",
			setupClientShapeMaxLen, err)
	}
	over := ok + "a"
	if err := validateSetupClient(&over); err == nil {
		t.Errorf("a %d-character slug was accepted; the CHECK would take 23514 "+
			"at the INSERT and surface as a 5xx instead of a field-level 400",
			len(over))
	}
}

// TestSetupClientSurvivesTheDTOMappingUnchanged pins the read half. The mapper
// must pass nil through as nil and must never substitute ReportedClientName:
// answering "what was this set up for" with the client's own self-report is the
// exact substitution m128 DECISION 1 refuses, and it is invisible in any test
// that sets both fields to the same string.
func TestSetupClientSurvivesTheDTOMappingUnchanged(t *testing.T) {
	reported := "Cursor"
	chosen := "claude-desktop"

	got := toConnectionDTO(connectionFromGrant(sqlc.McpGrant{
		Name:        "disagreeing facts",
		Status:      "active",
		ClientName:  &reported,
		SetupClient: &chosen,
	}))
	if got.SetupClient == nil {
		t.Fatal("setup_client was dropped by the DTO mapping")
	}
	if *got.SetupClient != chosen {
		t.Fatalf("setup_client = %q, want %q; the mapper is substituting the "+
			"client's self-report for the operator's choice", *got.SetupClient, chosen)
	}
	if got.ReportedClientName == nil || *got.ReportedClientName != reported {
		t.Fatal("reported_client_name was dropped or overwritten; the two fields " +
			"must travel independently")
	}

	// A grant with NO operator choice must serialise the field as null, never
	// as the reported name and never as "generic".
	none := toConnectionDTO(connectionFromGrant(sqlc.McpGrant{
		Name:       "never asked, has connected",
		Status:     "active",
		ClientName: &reported,
	}))
	if none.SetupClient != nil {
		t.Fatalf("a grant with no operator choice reported setup_client=%q. "+
			"NULL means nobody asked; anything else invents a choice.",
			*none.SetupClient)
	}
}
