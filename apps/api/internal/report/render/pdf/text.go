package pdf

import (
	"strings"
	"unicode/utf8"

	fpdflib "codeberg.org/go-pdf/fpdf"
)

// maxPDFRune is the highest code point the PDF library can take. It sizes each
// font's width table at 65,536 entries and indexes it by code point, so a
// character above U+FFFF panics while the document is written (or, from a
// multi-line cell, fails the render).
const maxPDFRune = 0xFFFF

// pdfText returns s with every character above U+FFFF replaced by U+FFFD, the
// replacement character, and any invalid UTF-8 repaired the same way. Every
// other character, including accents, CJK and right-to-left text, is kept.
//
// Emoji, mathematical letters and the rarer CJK ideographs live above that
// line. They arrive in site-reported plugin and theme names and versions, and
// in the names and texts the agency types, none of which the report controls.
func pdfText(s string) string {
	return strings.Map(func(r rune) rune {
		if r > maxPDFRune {
			return utf8.RuneError
		}
		return r
	}, s)
}

// pdfDoc is the document every drawing helper writes to. It redefines the
// three fpdf methods that take text, so no string reaches the library without
// going through pdfText, whatever field it came from. A helper that took the
// library's own type could skip that, which is why they take pdfDoc.
type pdfDoc struct{ *fpdflib.Fpdf }

// CellFormat draws one cell of text.
func (d pdfDoc) CellFormat(w, h float64, txt, border string, ln int, align string, fill bool, link int, linkStr string) {
	d.Fpdf.CellFormat(w, h, pdfText(txt), border, ln, align, fill, link, linkStr)
}

// MultiCell draws wrapped text.
func (d pdfDoc) MultiCell(w, h float64, txt, border, align string, fill bool) {
	d.Fpdf.MultiCell(w, h, pdfText(txt), border, align, fill)
}

// GetStringWidth measures text in the current font.
func (d pdfDoc) GetStringWidth(s string) float64 {
	return d.Fpdf.GetStringWidth(pdfText(s))
}
