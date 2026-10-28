package pdf

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	fpdflib "codeberg.org/go-pdf/fpdf"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/report/reportdata"
)

// The PDF library sizes each font's width table at 65,536 entries and indexes
// it by code point, so any character above U+FFFF used to panic while the
// document was written. Names and versions reported by a client's sites reach
// the PDF, so one emoji in one plugin name took the client's whole report
// down. These tests pin that no string can reach the library with such a
// character, and that a panic anywhere in a render comes back as an error.

// planeMarker carries a character the library cannot take (U+1F680) and the
// first one past its table (U+10000, which also trips its string-width
// helper), between plain letters.
const planeMarker = "x\U0001F680y\U00010000z"

// pdfFullFixture fills every section a PDF can draw: the header, the intro and
// closing text, a paused site, uptime with a TLS date and a daily series,
// backups, updates with a version table and a "more" line, performance and
// email.
func pdfFullFixture() reportdata.ReportData {
	day := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	paused := day
	return reportdata.ReportData{
		SchemaVersion: 1,
		GeneratedAt:   day,
		PeriodStart:   time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		PeriodEnd:     time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		PeriodLabel:   "1 May 2026 - 31 May 2026",
		ClientID:      uuid.New(),
		ClientName:    "Acme",
		Company:       "Acme Co",
		AgencyName:    "Agency",
		AccentColor:   "#3b82f6",
		IntroText:     "Hello",
		ClosingText:   "Thanks",
		ShowPoweredBy: true,
		Sections:      reportdata.DefaultSectionFlags(),
		Totals: reportdata.ReportTotals{
			SiteCount: 1, AvgUptimePct: 99.2, UptimeSiteCount: 1, PausedSiteCount: 1,
			Incidents: 1, BackupsCount: 2, UpdatesApplied: 3, EmailsSent: 4,
		},
		Sites: []reportdata.SiteReport{{
			SiteID:             uuid.New(),
			Name:               "site-a",
			URL:                "https://a.example",
			MonitoringPaused:   true,
			MonitoringPausedAt: &paused,
			Uptime: &reportdata.UptimeSection{
				UptimePct: 99.2, AvgLatencyMs: 210, Checks: 1000, DownChecks: 8, Incidents: 2,
				TLSExpiry: &paused, PartialCoverage: true, UnmonitoredHours: 30,
				Daily: []reportdata.UptimeDay{{Day: day, UptimePct: 99.2}, {Day: day.AddDate(0, 0, 1), UptimePct: 100}},
			},
			Backups: &reportdata.BackupSection{CompletedInPeriod: 2, TotalBytes: 4096, LastCompletedAt: &paused},
			Updates: &reportdata.UpdateSection{
				Plugins: 2, Themes: 1, Core: 1, Failed: 1, Total: 4,
				Items: []reportdata.UpdateItem{
					{Type: "plugin", Slug: "akismet", Name: "Akismet", FromVersion: "5.0", ToVersion: "5.3", Status: "succeeded", Date: day},
					{Type: "theme", Slug: "twenty", Name: "Twenty", FromVersion: "1.0", ToVersion: "1.1", Status: "failed", Date: day},
					{Type: "core", Slug: "core", Name: "WordPress", FromVersion: "6.5", ToVersion: "6.5", Status: "rolled_back", Date: day},
				},
				MoreItems: 3, MoreItemsAtLeast: true,
			},
			Performance: &reportdata.PerfSection{Metrics: []reportdata.PerfMetric{
				{Metric: "lcp", P75: 2100, Rating: "good", SampleCount: 90},
				{Metric: "cls", P75: 120, Rating: "needs_improvement", SampleCount: 90},
			}},
			Email: &reportdata.EmailSection{Total: 9, Sent: 7, Failed: 1, Bounced: 1},
		}},
	}
}

// markStrings sets every string reachable from v to s, except the struct fields
// named in skip.
func markStrings(v reflect.Value, s string, skip map[string]bool) {
	switch v.Kind() {
	case reflect.String:
		if v.CanSet() {
			v.SetString(s)
		}
	case reflect.Pointer:
		if !v.IsNil() {
			markStrings(v.Elem(), s, skip)
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if skip[v.Type().Field(i).Name] {
				continue
			}
			markStrings(v.Field(i), s, skip)
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			markStrings(v.Index(i), s, skip)
		}
	}
}

// renderCatching runs Render and reports a panic instead of crashing the test
// binary, so a regression names the input that caused it.
func renderCatching(data reportdata.ReportData, logo []byte) (out []byte, err error, panicked any) {
	defer func() { panicked = recover() }()
	out, err = NewFpdfRenderer().Render(data, logo)
	return out, err, nil
}

// The field that carries the character decides which fpdf call receives it,
// so each places it alone.
func TestPDFRendersACharacterAboveU10000InEveryTextField(t *testing.T) {
	fields := map[string]func(*reportdata.ReportData){
		"client name":          func(d *reportdata.ReportData) { d.ClientName = planeMarker },
		"agency name":          func(d *reportdata.ReportData) { d.AgencyName = planeMarker },
		"period label":         func(d *reportdata.ReportData) { d.PeriodLabel = planeMarker },
		"intro text":           func(d *reportdata.ReportData) { d.IntroText = planeMarker },
		"closing text":         func(d *reportdata.ReportData) { d.ClosingText = planeMarker },
		"site name":            func(d *reportdata.ReportData) { d.Sites[0].Name = planeMarker },
		"site url":             func(d *reportdata.ReportData) { d.Sites[0].URL = planeMarker },
		"update name":          func(d *reportdata.ReportData) { d.Sites[0].Updates.Items[0].Name = planeMarker },
		"update from version":  func(d *reportdata.ReportData) { d.Sites[0].Updates.Items[0].FromVersion = planeMarker },
		"update to version":    func(d *reportdata.ReportData) { d.Sites[0].Updates.Items[0].ToVersion = planeMarker },
		"update type":          func(d *reportdata.ReportData) { d.Sites[0].Updates.Items[1].Type = planeMarker },
		"update status":        func(d *reportdata.ReportData) { d.Sites[0].Updates.Items[1].Status = planeMarker },
		"performance metric":   func(d *reportdata.ReportData) { d.Sites[0].Performance.Metrics[0].Metric = planeMarker },
		"performance rating":   func(d *reportdata.ReportData) { d.Sites[0].Performance.Metrics[0].Rating = planeMarker },
		"long update name":     func(d *reportdata.ReportData) { d.Sites[0].Updates.Items[0].Name = strings.Repeat(planeMarker, 40) },
		"long intro text":      func(d *reportdata.ReportData) { d.IntroText = strings.Repeat(planeMarker+" ", 200) },
		"only the one past it": func(d *reportdata.ReportData) { d.Sites[0].Updates.Items[0].Name = "\U00010000" },
	}
	for name, set := range fields {
		t.Run(name, func(t *testing.T) {
			data := pdfFullFixture()
			set(&data)
			out, err, p := renderCatching(data, nil)
			if p != nil {
				t.Fatalf("Render panicked: %v", p)
			}
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if !bytes.HasPrefix(out, []byte("%PDF")) {
				t.Fatalf("output is not a PDF: %q", out[:min(len(out), 8)])
			}
		})
	}
}

// Every string in the data at once, so a text field added later that reaches
// the PDF cannot reintroduce the crash without this test seeing it. That
// includes the pause reason: marking it gives the paused site a Reason line,
// which is drawn in the oblique face.
func TestPDFRendersACharacterAboveU10000InEveryString(t *testing.T) {
	data := pdfFullFixture()
	markStrings(reflect.ValueOf(&data).Elem(), planeMarker, nil)
	out, err, p := renderCatching(data, nil)
	if p != nil {
		t.Fatalf("Render panicked: %v", p)
	}
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !bytes.HasPrefix(out, []byte("%PDF")) {
		t.Fatalf("output is not a PDF")
	}
}

// The other side: text the font can take is untouched, so the fix does not
// blank accents, CJK or right-to-left text, or the last code point of the
// plane the library can hold.
func TestPDFKeepsEveryCharacterBelowU10000(t *testing.T) {
	fpdflib.SetDefaultCompression(false)
	t.Cleanup(func() { fpdflib.SetDefaultCompression(true) })

	names := []string{"Café Plugin", "日本語のプラグイン", "إضافة ووردبريس", "edge￿"}
	data := pdfFullFixture()
	day := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	data.Sites[0].Updates.Items = nil
	for _, n := range names {
		data.Sites[0].Updates.Items = append(data.Sites[0].Updates.Items,
			reportdata.UpdateItem{Type: "plugin", Slug: n, Name: n, FromVersion: "1", ToVersion: "2", Status: "succeeded", Date: day})
	}
	out, err, p := renderCatching(data, nil)
	if p != nil || err != nil {
		t.Fatalf("Render: err=%v panic=%v", err, p)
	}
	// Plain ASCII in the same row still reads as itself.
	if !bytes.Contains(out, asciiUTF16BE("Updated")) {
		t.Error("rendered PDF lost the row's plain text")
	}
	// A character past the plane is replaced, not dropped silently into the
	// neighbouring text.
	data.Sites[0].Updates.Items[0].Name = "Rocket \U0001F680 Launcher"
	out, err, p = renderCatching(data, nil)
	if p != nil || err != nil {
		t.Fatalf("Render with an emoji: err=%v panic=%v", err, p)
	}
	if !bytes.Contains(out, asciiUTF16BE("Rocket ")) || !bytes.Contains(out, asciiUTF16BE(" Launcher")) {
		t.Error("the text around the replaced character was lost")
	}
}

func TestPDFTextReplacesOnlyCharactersAboveU10000(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"", ""},
		{"Rocket \U0001F680 Launcher", "Rocket � Launcher"},
		{"\U00010000", "�"},
		{"\U0010FFFF", "�"},
		{"a\U0001F600\U0001F600b", "a��b"},
		{"￿", "￿"},
		{"�", "�"},
		{"日本語 Café إضافة", "日本語 Café إضافة"},
		{"\x00\x1b\r\n", "\x00\x1b\r\n"},
	}
	for _, c := range cases {
		if got := pdfText(c.in); got != c.want {
			t.Errorf("pdfText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := pdfText("bad\xff\xfe"); !utf8.ValidString(got) {
		t.Errorf("pdfText left invalid UTF-8 in %q", got)
	}
}

// truncatedPNG is a logo whose header decodes, which is all the fetch-time
// validation reads, and whose body is cut short.
func truncatedPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 40, 40))
	for x := 0; x < 40; x++ {
		for y := 0; y < 40; y++ {
			img.Set(x, y, color.RGBA{uint8(x * 6), uint8(y * 6), 90, 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	full := b.Bytes()
	cut := full[:len(full)/2]
	if _, err := png.DecodeConfig(bytes.NewReader(cut)); err != nil {
		t.Fatalf("test setup: the truncated logo should still decode its header: %v", err)
	}
	return cut
}

// A panic inside the library is an error to the caller, not a crash of the
// worker: the worker can then fail the report row.
func TestPDFRenderTurnsAPanicIntoAnError(t *testing.T) {
	out, err, p := renderCatching(pdfFullFixture(), truncatedPNG(t))
	if p != nil {
		t.Fatalf("Render panicked instead of returning an error: %v", p)
	}
	if err == nil {
		t.Fatalf("Render accepted a truncated logo and returned %d bytes", len(out))
	}
	if out != nil {
		t.Errorf("Render returned bytes alongside an error")
	}
	if !strings.Contains(err.Error(), "pdf render") {
		t.Errorf("error %q does not say it came from the PDF render", err)
	}
}
