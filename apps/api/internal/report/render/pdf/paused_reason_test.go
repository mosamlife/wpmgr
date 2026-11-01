package pdf

import (
	"bytes"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	fpdflib "codeberg.org/go-pdf/fpdf"
)

// utf16BE is how the library writes text in an embedded UTF-8 font: each UTF-16
// code unit big-endian, no byte-order mark. asciiUTF16BE does the same for
// ASCII only; this one also takes accents.
func utf16BE(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 0, len(units)*2)
	for _, u := range units {
		out = append(out, byte(u>>8), byte(u))
	}
	return out
}

// A paused site whose pause carries a reason prints that reason on its own
// line, in the oblique face. Only the regular and bold faces were registered,
// so the library failed the whole document with "undefined font: dejavu I":
// every report for a client with one such site failed to render.
//
// The fixture is the one the other paused-site tests use (paused_window_test.go);
// the only difference is MonitoringPausedReason.
func TestPDFPausedSiteWithReasonRenders(t *testing.T) {
	fpdflib.SetDefaultCompression(false)
	t.Cleanup(func() { fpdflib.SetDefaultCompression(true) })

	reasons := []struct {
		name   string
		reason string
		// want is text the PDF must contain for this reason. Empty means the
		// reason is checked only for rendering without an error.
		want string
	}{
		{name: "plain", reason: "Moving to a new host", want: "Reason: Moving to a new host"},
		{name: "accented", reason: "Migración al nuevo alojamiento", want: "Reason: Migración al nuevo alojamiento"},
		// Characters above U+FFFF are replaced before they reach the library
		// (see pdfText); the oblique face must take that path too.
		{name: "above the basic plane", reason: "Launch freeze \U0001F680"},
		{name: "wraps over several lines", reason: strings.Repeat("Waiting on the client to approve the redesign. ", 12)},
	}
	for _, tc := range reasons {
		t.Run(tc.name, func(t *testing.T) {
			pausedAt := time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC)
			data := pausedWindowFixture(true, &pausedAt, nil)
			data.Sites[0].MonitoringPausedReason = tc.reason

			out, err := NewFpdfRenderer().Render(data, nil)
			if err != nil {
				t.Fatalf("Render failed for a paused site whose pause has a reason: %v", err)
			}
			if !bytes.HasPrefix(out, []byte("%PDF-")) {
				t.Fatalf("Render returned something that is not a PDF: %.16q", out)
			}
			if !bytes.Contains(out, asciiUTF16BE("Reason:")) {
				t.Error("rendered PDF drops the Reason line for a pause that has a reason")
			}
			if tc.want != "" && !bytes.Contains(out, utf16BE(tc.want)) {
				t.Errorf("rendered PDF does not print %q", tc.want)
			}
			// The Reason line is drawn in the oblique face, which the document
			// carries as its own embedded font.
			if !bytes.Contains(out, []byte("utf8dejavuI")) {
				t.Error("rendered PDF does not embed the oblique face the Reason line is drawn in")
			}
		})
	}
}

// TestPDFPausedSiteWithoutReasonHasNoReasonLine is the control: a pause with no
// reason prints no Reason line and does not carry the oblique face, and the
// paused note itself is still there.
func TestPDFPausedSiteWithoutReasonHasNoReasonLine(t *testing.T) {
	fpdflib.SetDefaultCompression(false)
	t.Cleanup(func() { fpdflib.SetDefaultCompression(true) })

	pausedAt := time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC)
	data := pausedWindowFixture(true, &pausedAt, nil)

	out, err := NewFpdfRenderer().Render(data, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if bytes.Contains(out, asciiUTF16BE("Reason:")) {
		t.Error("rendered PDF prints a Reason line for a pause that has none")
	}
	if !bytes.Contains(out, asciiUTF16BE("Monitoring paused")) {
		t.Error("rendered PDF drops the paused title")
	}
}
