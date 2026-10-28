package pdf

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"testing"

	fpdflib "codeberg.org/go-pdf/fpdf"
)

// A logo the PDF library cannot draw must not fail the report. The fetch checks
// every logo it hands over, but the library has limits of its own that a
// well-formed image can still hit (it does not take interlaced PNGs), and a
// panic or an error recorded on the document while it reads the image would
// otherwise fail every PDF for that client until the agency changed the logo.

// logRecorder collects what the package logs while a test runs.
type logRecorder struct {
	mu      sync.Mutex
	records []slog.Record
}

func (l *logRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (l *logRecorder) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, r)
	return nil
}
func (l *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logRecorder) WithGroup(string) slog.Handler      { return l }

// count returns how many records at level or above were logged.
func (l *logRecorder) count(level slog.Level) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, r := range l.records {
		if r.Level >= level {
			n++
		}
	}
	return n
}

// recordLogs routes the default logger to a recorder until the test ends.
func recordLogs(t *testing.T) *logRecorder {
	t.Helper()
	rec := &logRecorder{}
	prev := slog.Default()
	slog.SetDefault(slog.New(rec))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return rec
}

// imageObject is what the library writes for an image it embeds.
var imageObject = []byte("/Subtype /Image")

func TestPDFDrawsALogoItCanRead(t *testing.T) {
	fpdflib.SetDefaultCompression(false)
	t.Cleanup(func() { fpdflib.SetDefaultCompression(true) })

	logos := map[string][]byte{
		"png":       encodePNG(t, gradientImage(40, 40, false)),
		"alpha png": encodePNG(t, gradientImage(40, 40, true)),
		"jpeg":      encodeJPEG(t, gradientImage(40, 40, false)),
	}
	for name, logo := range logos {
		t.Run(name, func(t *testing.T) {
			logs := recordLogs(t)
			out, err := NewFpdfRenderer().Render(pdfFullFixture(), logo)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if !bytes.Contains(out, imageObject) {
				t.Error("rendered PDF does not carry the logo")
			}
			if n := logs.count(slog.LevelWarn); n != 0 {
				t.Errorf("a logo that draws logged %d warnings", n)
			}
		})
	}
}

func TestPDFRendersWithoutALogoItCannotDraw(t *testing.T) {
	fpdflib.SetDefaultCompression(false)
	t.Cleanup(func() { fpdflib.SetDefaultCompression(true) })

	logos := map[string][]byte{
		// The library reads past the end of the data and panics.
		"png cut short": truncatedPNG(t),
		// The library inflates the data and records an error on the document.
		"alpha png cut short": truncatedAlphaPNG(t),
		// A whole PNG the library does not take: it records an error.
		"interlaced png": interlacedPNG(t),
		// Bytes that are not an image are read as a JPEG.
		"not an image": []byte("<html>not a picture</html>"),
	}
	for name, logo := range logos {
		t.Run(name, func(t *testing.T) {
			logs := recordLogs(t)
			out, err, p := renderCatching(pdfFullFixture(), logo)
			if p != nil {
				t.Fatalf("Render panicked: %v", p)
			}
			if err != nil {
				t.Fatalf("a logo that cannot be drawn failed the report: %v", err)
			}
			if !bytes.HasPrefix(out, []byte("%PDF")) {
				t.Fatalf("output is not a PDF: %.16q", out)
			}
			if bytes.Contains(out, imageObject) {
				t.Error("rendered PDF carries an image although the logo could not be read")
			}
			// The rest of the report is all there.
			for _, want := range []string{"Acme", "Overview", "site-a", "Akismet"} {
				if !bytes.Contains(out, asciiUTF16BE(want)) {
					t.Errorf("rendered PDF lost %q along with the logo", want)
				}
			}
			if n := logs.count(slog.LevelWarn); n != 1 {
				t.Errorf("logged %d warnings for the logo, want exactly 1", n)
			}
		})
	}
}
