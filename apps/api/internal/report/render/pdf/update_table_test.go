package pdf

import (
	"bytes"
	"testing"
	"time"

	fpdflib "codeberg.org/go-pdf/fpdf"

	"github.com/mosamlife/wpmgr/apps/api/internal/report/reportdata"
)

// GH #969. The PDF twin of the HTML version table.
func TestPDFRendersUpdateVersionTable(t *testing.T) {
	fpdflib.SetDefaultCompression(false)
	t.Cleanup(func() { fpdflib.SetDefaultCompression(true) })

	data := pdfPartialFixture(false)
	day := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	data.Sites[0].Updates = &reportdata.UpdateSection{
		Plugins: 4, Themes: 1, Core: 7, Failed: 1, Total: 12,
		Items: []reportdata.UpdateItem{
			{Type: "plugin", Slug: "akismet", Name: "Akismet Anti-spam", FromVersion: "5.0", ToVersion: "5.3", Status: "succeeded", Date: day},
			{Type: "plugin", Slug: "woo", Name: "woo", FromVersion: "", ToVersion: "8.2", Status: "failed", Date: day},
			{Type: "core", Slug: "core", Name: "WordPress", FromVersion: "6.5", ToVersion: "6.5", Status: "rolled_back", Date: day},
		},
		MoreItems: 3,
	}
	out, err := NewFpdfRenderer().Render(data, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Akismet Anti-spam", "5.0", "5.3", "unknown", "Failed", "Rolled back", "WordPress",
		"20 May 2026", "and 3 more changes", "From", "Result",
	} {
		if !bytes.Contains(out, asciiUTF16BE(want)) {
			t.Errorf("rendered PDF Updates section is missing %q", want)
		}
	}
	if !bytes.Contains(out, asciiUTF16BE("7 core")) {
		t.Error("the PDF Updates row no longer shows the Core count")
	}
	if bytes.Contains(out, asciiUTF16BE("WPMgr")) {
		t.Error("the PDF names WPMgr although ShowPoweredBy is false in this fixture")
	}
}
