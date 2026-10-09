package govcontext

import (
	"strings"
	"testing"
)

// guidanceBreaks are the eight forms a line break inside operator guidance
// can take: Unicode's mandatory line breaks, with CRLF as one break.
var guidanceBreaks = map[string]string{
	"LF":     "\n",
	"CR":     "\r",
	"CRLF":   "\r\n",
	"VT":     "\v",
	"FF":     "\f",
	"NEL":    "\u0085",
	"U+2028": "\u2028",
	"U+2029": "\u2029",
}

// TestInstructionText_EveryLineOfGuidanceCarriesThePrefix pins the rendered
// bytes for each line-break form inside guidance. Every line of operator
// guidance carries the operator prefix: the text after the break is a line of
// its own, and it begins with "| ".
func TestInstructionText_EveryLineOfGuidanceCarriesThePrefix(t *testing.T) {
	const want = goldenPreamble +
		"| organisation default — brand voice: first\n" +
		"| second\n" +
		goldenEpilogue
	for name, sep := range guidanceBreaks {
		t.Run(name, func(t *testing.T) {
			text := ResolvedContext{Layers: []LayerContribution{{
				Name:     "organisation default",
				Guidance: GuidanceSet{BrandVoice: "first" + sep + "second"},
			}}}.InstructionText()

			if text != want {
				t.Fatalf("rendered block differs from the golden.\ngot:  %q\nwant: %q", text, want)
			}
			assertFenceHolds(t, text)
		})
	}
}

// TestInstructionText_GuidanceSplitsAtEveryLineBreakInOneValue puts all eight
// forms in one value, followed by trailing breaks. Each break starts a new
// prefixed line, and trailing breaks add no lines.
func TestInstructionText_GuidanceSplitsAtEveryLineBreakInOneValue(t *testing.T) {
	value := "a\nb\rc\r\nd\ve\ff\u0085g\u2028h\u2029i" + "\u2028\u2029\v\f\u0085\r\n"
	text := ResolvedContext{Layers: []LayerContribution{{
		Name:     "organisation default",
		Guidance: GuidanceSet{Style: value},
	}}}.InstructionText()

	want := goldenPreamble +
		"| organisation default — style: a\n" +
		"| b\n| c\n| d\n| e\n| f\n| g\n| h\n| i\n" +
		goldenEpilogue
	if text != want {
		t.Fatalf("rendered block differs from the golden.\ngot:  %q\nwant: %q", text, want)
	}
	assertFenceHolds(t, text)
}

// TestInstructionText_GuidanceKeepsEveryOtherByte is the over-fire arm: the
// split changes line structure only. Text without a line break renders byte
// for byte, including characters next to the break set (tab, NBSP, the
// zero-width space) and a blank line between two paragraphs.
func TestInstructionText_GuidanceKeepsEveryOtherByte(t *testing.T) {
	value := "tab\there, nbsp\u00a0here, zero-width\u200bhere\n\nnext paragraph"
	text := ResolvedContext{Layers: []LayerContribution{{
		Name:     "organisation default",
		Guidance: GuidanceSet{Audience: value},
	}}}.InstructionText()

	want := goldenPreamble +
		"| organisation default — audience: tab\there, nbsp\u00a0here, zero-width\u200bhere\n" +
		"| \n" +
		"| next paragraph\n" +
		goldenEpilogue
	if text != want {
		t.Fatalf("rendered block differs from the golden.\ngot:  %q\nwant: %q", text, want)
	}
	if !strings.Contains(text, "\u00a0") || !strings.Contains(text, "\u200b") {
		t.Errorf("guidance lost a character that is not a line break:\n%q", text)
	}
}
