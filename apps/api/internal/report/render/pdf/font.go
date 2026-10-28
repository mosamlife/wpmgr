package pdf

import (
	"fmt"

	fpdflib "codeberg.org/go-pdf/fpdf"
	"github.com/go-fonts/dejavu/dejavusans"
	"github.com/go-fonts/dejavu/dejavusansbold"
	"github.com/go-fonts/dejavu/dejavusansoblique"
)

// registerFonts embeds DejaVu Sans (regular, bold and oblique) into an fpdf
// document. Every style a drawing helper passes to SetFont must be registered
// here: the library fails the whole document with "undefined font" otherwise.
// github.com/go-fonts/dejavu exports var TTF []byte from each sub-package;
// fpdf subsets the font per-document so only glyphs actually used are included.
// AddUTF8FontFromBytes returns void; errors are captured via f.Err().
func registerFonts(f *fpdflib.Fpdf) error {
	f.AddUTF8FontFromBytes("dejavu", "", dejavusans.TTF)
	f.AddUTF8FontFromBytes("dejavu", "B", dejavusansbold.TTF)
	f.AddUTF8FontFromBytes("dejavu", "I", dejavusansoblique.TTF)
	if f.Err() {
		return fmt.Errorf("pdf: register DejaVu fonts: %s", f.Error().Error())
	}
	return nil
}
