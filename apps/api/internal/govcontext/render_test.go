package govcontext

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// TestCheckDeliverable_RefusesOneByteOverAndAcceptsExactlyAtTheLimit is the
// write-time ceiling proved at its boundary, in both directions.
//
// BOTH DIRECTIONS IS THE POINT. A guard proven only to fire is half-proven: a
// ceiling that also refused a context sitting exactly on the limit would be a
// guard that reddens correct work, and the first operator it blocked would ask
// for it to be turned off. The two cases below are one byte apart.
func TestCheckDeliverable_RefusesOneByteOverAndAcceptsExactlyAtTheLimit(t *testing.T) {
	// Measure the fixed overhead the render adds (preamble, layer name,
	// epilogue) rather than hard-coding it, so this test keeps working when
	// the wording changes and keeps testing the BOUNDARY rather than a
	// number someone typed.
	overhead := len(ResolvedContext{
		Layers: []LayerContribution{{Name: "organisation default", Guidance: GuidanceSet{BrandVoice: "x"}}},
	}.InstructionText()) - 1

	atLimit := Snapshot{Guidance: GuidanceSet{
		BrandVoice: strings.Repeat("a", MaxDeliverableInstructionBytes-overhead),
	}}
	if err := checkDeliverable("organisation default", atLimit); err != nil {
		t.Fatalf("a context sitting exactly on the limit was refused: %v", err)
	}

	overLimit := Snapshot{Guidance: GuidanceSet{
		BrandVoice: strings.Repeat("a", MaxDeliverableInstructionBytes-overhead+1),
	}}
	err := checkDeliverable("organisation default", overLimit)
	if err == nil {
		t.Fatal("a context one byte over the limit was accepted; the editor would then have written " +
			"instructions the assistant refuses to deliver")
	}
	if err.Code != ErrCodeContextTooLarge {
		t.Errorf("refusal code = %q, want %q", err.Code, ErrCodeContextTooLarge)
	}
	// THE MESSAGE MUST NAME THE LIMIT. The owner's ruling is that being
	// stopped while typing beats discovering later that half the text was
	// never read — which only holds if the operator is told what the limit is.
	if !strings.Contains(err.Message, "2048") {
		t.Errorf("the refusal does not name the limit an operator has to write under: %q", err.Message)
	}
	if domain.HTTPStatus(err) != 413 {
		t.Errorf("refusal HTTP status = %d, want 413", domain.HTTPStatus(err))
	}
}

// TestInstructionText_EmptyContextRendersNothing is the not-over-fire case for
// the render itself: an organisation that has authored nothing must add no
// bytes at all to a model's input, not an empty fence announcing its own
// emptiness.
func TestInstructionText_EmptyContextRendersNothing(t *testing.T) {
	r := &Resolver{Store: &fakeStore{}}
	rc, err := r.Resolve(context.Background(), uuid.New(), uuid.Nil, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := rc.InstructionText(); got != "" {
		t.Errorf("an unauthored organisation rendered %q; it must render nothing", got)
	}
}

// TestResolve_OrganisationScopeReadsNoSiteRow is Ruling 1 expressed as a test:
// a fleet-wide resolution touches the org layer and nothing site-shaped.
func TestResolve_OrganisationScopeReadsNoSiteRow(t *testing.T) {
	store := &fakeStore{
		orgOK: true,
		org:   Snapshot{Guidance: GuidanceSet{BrandVoice: "org voice"}},
		// A site snapshot that WOULD be visible if the resolver read one. If
		// this text ever appears in the render, the fleet-wide path is
		// resolving against a site.
		siteOK: true,
		site:   Snapshot{Guidance: GuidanceSet{BrandVoice: "site voice"}},
	}
	rc, err := (&Resolver{Store: store}).Resolve(context.Background(), uuid.New(), uuid.Nil, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	text := rc.InstructionText()
	if strings.Contains(text, "site voice") {
		t.Errorf("an organisation-scope resolution rendered a SITE's context:\n%s", text)
	}
	if !strings.Contains(text, "org voice") {
		t.Errorf("an organisation-scope resolution dropped the organisation's own context:\n%s", text)
	}
	// The layer is present and says why it is empty, so nothing downstream can
	// read this as the fact "this site overrides nothing".
	if !strings.Contains(rc.Layers[2].Name, "not applicable") {
		t.Errorf("layer 3 name at organisation scope = %q; it must state that it does not apply",
			rc.Layers[2].Name)
	}
}
