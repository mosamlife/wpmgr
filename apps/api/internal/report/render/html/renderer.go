// Package html renders ReportData as a self-contained HTML document.
// The document is print-optimized (inline CSS @media print block) and doubles
// as the email-linked web view. html/template auto-escapes all values, so
// hostile client names (e.g. <script>) are safe.
package html

import (
	"bytes"
	"embed"
	"encoding/base64"
	"fmt"
	"html/template"
	"math"
	"strings"

	"github.com/mosamlife/wpmgr/apps/api/internal/report/reportdata"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

// Renderer renders ReportData to a self-contained HTML document.
type Renderer struct {
	tmpl *template.Template
}

// NewRenderer parses the embedded templates. Fails fast if a template is invalid.
func NewRenderer() (*Renderer, error) {
	tmpl, err := template.New("").Funcs(funcMap()).ParseFS(templateFS, "templates/*.tmpl")
	if err != nil {
		return nil, fmt.Errorf("parse report html templates: %w", err)
	}
	return &Renderer{tmpl: tmpl}, nil
}

// renderData is the template-local data struct. It wraps reportdata.ReportData
// but replaces the string LogoURL with a template.URL so that a data: URI is
// not sanitised to #ZgotmplZ by html/template's security policy.
//
// FIX-1: html/template treats src="data:..." as an unsafe URL and replaces it
// with "#ZgotmplZ" unless the value is typed as template.URL. We NEVER pass a
// remote http(s) URL here — only a validated data: URI or empty string.
type renderData struct {
	reportdata.ReportData
	// LogoURL shadows the embedded string field with a template.URL.
	// When empty, the template's {{if .LogoURL}} guard omits the logo block.
	LogoURL template.URL
}

// Render renders the report to HTML bytes.
// logoBytes, when non-nil, is the validated logo image embedded as a data URI
// so the stored HTML blob has no external dependencies and never hot-links a
// remote URL.
//
// FIX-1: when logoBytes is nil, LogoURL is left empty so the template never
// emits an <img> element. Callers should already clear LogoURL before calling
// (the worker does this), but this renderer provides the guarantee.
func (r *Renderer) Render(data reportdata.ReportData, logoBytes []byte) ([]byte, error) {
	td := renderData{ReportData: data}
	// Clear LogoURL on the embedded struct so it cannot leak through if the
	// renderData.LogoURL field is not consulted (belt-and-braces).
	td.ReportData.LogoURL = ""

	if len(logoBytes) > 0 {
		// Detect content-type from first bytes (PNG magic: 0x89 0x50).
		ct := "image/jpeg"
		if len(logoBytes) >= 4 && logoBytes[0] == 0x89 && logoBytes[1] == 0x50 {
			ct = "image/png"
		}
		encoded := encodeBase64(logoBytes)
		// Type as template.URL so html/template accepts the data: URI in src=.
		// We control this value entirely — it is never derived from user input.
		td.LogoURL = template.URL("data:" + ct + ";base64," + encoded) //nolint:gosec // data URI, not user-supplied URL
	}
	// When logoBytes is nil, td.LogoURL is "" — the template guard omits the img.

	var buf bytes.Buffer
	if err := r.tmpl.ExecuteTemplate(&buf, "report.html.tmpl", td); err != nil {
		return nil, fmt.Errorf("render html report: %w", err)
	}
	return buf.Bytes(), nil
}

// ---------------------------------------------------------------------------
// Template function map
// ---------------------------------------------------------------------------

func funcMap() template.FuncMap {
	return template.FuncMap{
		"upper":         strings.ToUpper,
		"uptimeColor":   uptimeColor,
		"sparkHeight":   sparkHeight,
		"bytesHuman":    bytesHuman,
		"ratingClass":   ratingClass,
		"cwvDisplay":    cwvDisplay,
		"humanizeHours": humanizeHours,
	}
}

// humanizeHours formats a duration given in hours (reportdata.UptimeSection's
// UnmonitoredHours) the way a person would say it, not as a raw float: "about
// 21 days", "about 5 hours", "under an hour". GH #414 phase 5 follow-up — the
// aggregator computes real pause/window overlap in hours; this is prose, not
// a measurement, so it rounds to the unit a reader actually thinks in.
func humanizeHours(hours float64) string {
	if hours < 1 {
		return "under an hour"
	}
	if hours < 24 {
		h := int(math.Round(hours))
		if h == 1 {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", h)
	}
	d := int(math.Round(hours / 24))
	if d == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", d)
}

// uptimeColor returns a CSS color for a given uptime percentage.
func uptimeColor(pct float64) string {
	if pct >= 99.9 {
		return "#16a34a"
	} else if pct >= 95 {
		return "#ca8a04"
	}
	return "#dc2626"
}

// sparkHeight maps a 0-100 uptime pct to a bar height in px (4-32).
func sparkHeight(pct float64) int {
	h := int(math.Round(pct / 100 * 28))
	if h < 4 {
		h = 4
	}
	if h > 32 {
		h = 32
	}
	return h
}

// bytesHuman formats bytes into a human-readable size string.
func bytesHuman(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// ratingClass returns a CSS class name for a CWV rating string.
func ratingClass(rating string) string {
	switch rating {
	case "good":
		return "rating-good"
	case "needs_improvement":
		return "rating-needs"
	case "poor":
		return "rating-poor"
	default:
		return ""
	}
}

// cwvDisplay formats a CWV p75 value for display. CLS is stored as
// milli-units (actual * 1000) so divide by 1000 for display.
func cwvDisplay(metric string, p75 float64) string {
	if metric == "cls" {
		return fmt.Sprintf("%.3f", p75/1000)
	}
	return fmt.Sprintf("%.0fms", p75)
}

// encodeBase64 encodes bytes to a standard base64 string.
func encodeBase64(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}
