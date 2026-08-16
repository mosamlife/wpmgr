package pdf

import (
	"bytes"
	"testing"
	"time"

	fpdflib "codeberg.org/go-pdf/fpdf"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/report/reportdata"
)

// GH #414 aggregator follow-up. reportdata.UptimeSection now carries
// PartialCoverage and UnmonitoredHours (reportdata.go), computed from real
// pause/window overlap, but as reported by the agent that built it: "renderer
// prose ... doesn't yet say anything about partial coverage — the data
// contract carries it, but nothing prints it." These tests pin that the PDF
// renderer, drawPartialCoverageNote in fpdf.go, now says so next to the
// uptime figures it qualifies, and that a fully-covered period is untouched.

func pdfPartialFixture(partial bool) reportdata.ReportData {
	u := &reportdata.UptimeSection{
		UptimePct:    99.2,
		AvgLatencyMs: 210,
		Checks:       1000,
		DownChecks:   8,
		Incidents:    2,
	}
	if partial {
		u.PartialCoverage = true
		u.UnmonitoredHours = 504 // 21 days — same figure the aggregator's own
		// TestPauseResumedMidWindowIsPartialCoverage fixture produces for a
		// pause running 2026-05-05..2026-05-26.
	}
	return reportdata.ReportData{
		SchemaVersion: 1,
		GeneratedAt:   time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		PeriodStart:   time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		PeriodEnd:     time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		PeriodLabel:   "1 May 2026 – 31 May 2026",
		ClientID:      uuid.New(),
		ClientName:    "Acme",
		AgencyName:    "Agency",
		Sections:      reportdata.DefaultSectionFlags(),
		Totals: reportdata.ReportTotals{
			SiteCount: 1, AvgUptimePct: 99.2, UptimeSiteCount: 1,
		},
		Sites: []reportdata.SiteReport{
			{SiteID: uuid.New(), Name: "site-a", URL: "https://a.example", Uptime: u},
		},
	}
}

// asciiUTF16BE reproduces fpdf's utf8toutf16(s, false) for pure-ASCII input:
// each input byte becomes a big-endian 0x00,byte pair, no BOM. fpdf embeds
// the DejaVu faces as composite (Type0 UTF-8) fonts, so every text-showing
// operator in the content stream carries this encoding rather than raw
// ASCII — see codeberg.org/go-pdf/fpdf@v0.12.0 util.go's utf8toutf16 and its
// callers in fpdf.go (e.g. line 2564, `f.escape(utf8toutf16(txtStr, false))`).
func asciiUTF16BE(s string) []byte {
	out := make([]byte, 0, len(s)*2)
	for i := 0; i < len(s); i++ {
		out = append(out, 0, s[i])
	}
	return out
}

// TestPDFStatesPartialCoverage is the PDF twin of the HTML assertion: the
// rendered document says, next to the uptime figures, that coverage is
// partial and roughly how much went unmeasured, in words ("21 days"), not the
// raw float (504).
//
// Compression is disabled for this test only (fpdf zlib-compresses content
// streams by default, which would hide the literal text from a byte search)
// and restored via t.Cleanup so it never leaks into TestPDFFullCoverageUnchanged
// or any other test — SetDefaultCompression is a package-level global.
func TestPDFStatesPartialCoverage(t *testing.T) {
	fpdflib.SetDefaultCompression(false)
	t.Cleanup(func() { fpdflib.SetDefaultCompression(true) })

	r := NewFpdfRenderer()
	out, err := r.Render(pdfPartialFixture(true), nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !bytes.Contains(out, asciiUTF16BE("Partial coverage")) {
		t.Error("rendered PDF never says coverage is partial")
	}
	// The count and its unit are asserted as ONE contiguous token, "21 days".
	// Splitting them into separate "21" and "days" checks made this assertion
	// incapable of failing: the same fixture sets AvgLatencyMs: 210, which
	// renders "210 ms", so the bare "21" matched the latency no matter what
	// the note printed. Under a mutation making humanizeHours return a
	// constant — "about 3 days went unmeasured" — both halves still passed,
	// while the identical mutation on the HTML twin was caught. The two
	// renderers could be made to disagree, which is exactly what the comment
	// at fpdf.go's humanizeHours says must never happen, with only one side
	// able to notice.
	//
	// Contiguity is safe here: MultiCell wraps at word boundaries, and for
	// this fixed fixture (contentW, dejavu 9pt, this sentence) the wrap does
	// not fall between the number and its unit. If a future wording change
	// moves the wrap, this test fails loudly rather than silently passing,
	// which is the correct direction for an assertion whose whole job is to
	// notice the phrase changing.
	if !bytes.Contains(out, asciiUTF16BE("21 days")) {
		t.Error(`rendered PDF does not print the humanized duration "21 days" — the PDF and HTML renderers disagree about how much of the period went unmeasured`)
	}
	if !bytes.Contains(out, asciiUTF16BE("about 21 days went unmeasured")) {
		t.Error(`rendered PDF does not contain the partial-coverage phrase "about 21 days went unmeasured"`)
	}
	if bytes.Contains(out, asciiUTF16BE("504")) {
		t.Error("rendered PDF prints the raw UnmonitoredHours float (504) instead of a human duration")
	}
}

// TestPDFFullCoverageUnchanged proves a fully-covered site's PDF is untouched
// by this change. drawPartialCoverageNote is called only from inside
// `if u.PartialCoverage` at the end of drawUptimeRow (fpdf.go); with
// PartialCoverage=false that branch is never entered, so the new code is
// structurally unreachable for this fixture, and the assertion below is the
// direct proof: no trace of the new prose appears.
//
// This is a content check rather than a full-document byte comparison
// on purpose. codeberg.org/go-pdf/fpdf@v0.12.0 fpdf.go:4430 and :4887 both
// iterate the font-resource map (`for key = range f.fonts`) when writing
// page resource dictionaries and font objects — Go randomizes map iteration
// order per range, so fpdf's own object numbering and /Font dictionary key
// order already vary from one Render() call to the next, with or without this
// change, on fixtures that register more than one font. A raw byte-equality
// assertion across two renders is therefore flaky against the *library*, not
// a signal about this change; confirmed by running two back-to-back renders
// of the same input in a throwaway test and observing the font object
// numbering (e.g. "utf8dejavuB" vs "utf8dejavu" at object 5) swap places
// across otherwise-identical runs.
func TestPDFFullCoverageUnchanged(t *testing.T) {
	r := NewFpdfRenderer()
	data := pdfPartialFixture(false)

	out, err := r.Render(data, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if bytes.Contains(out, asciiUTF16BE("Partial coverage")) {
		t.Error("full-coverage PDF mentions partial coverage")
	}
}
