package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
)

// designTokensGoldenFixture is written by the agent
// (apps/agent/tests/Builders/DesignTokensGoldenTest.php) and never edited by
// hand. It holds designs and every value the agent's DesignTokens class answers
// for each of them. The Go twin has to answer the same bytes.
const designTokensGoldenFixture = agentAbilityFixtures + "design-tokens-cases.json"

type designTokensGolden struct {
	Note      string                   `json:"note"`
	Ratios    map[string]string        `json:"ratios"`
	Cases     []designTokensGoldenCase `json:"cases"`
	TypeScale []struct {
		Ratio  string         `json:"ratio"`
		BasePx int            `json:"base_px"`
		Sizes  map[string]int `json:"sizes"`
	} `json:"type_scale"`
	Hover []struct {
		Hex    string `json:"hex"`
		Result string `json:"result"`
	} `json:"hover"`
	Alpha []struct {
		Hex     string `json:"hex"`
		Percent int    `json:"percent"`
		Result  string `json:"result"`
	} `json:"alpha"`
	Refused []struct {
		Call    string `json:"call"`
		Hex     string `json:"hex"`
		Percent int    `json:"percent"`
	} `json:"refused"`
}

type designTokensGoldenCase struct {
	Name   string          `json:"name"`
	Design pageDesign      `json:"design"`
	Tokens json.RawMessage `json:"tokens"`
}

// The snapshot types give the golden's shape and key order. A number the
// compile path writes as text is a string here, as it is in the file.
type goldenBreakpoints struct {
	Desktop int `json:"desktop"`
	Tablet  int `json:"tablet"`
	Mobile  int `json:"mobile"`
}

type goldenShadow struct {
	X      int    `json:"x"`
	Y      int    `json:"y"`
	Blur   int    `json:"blur"`
	Spread int    `json:"spread"`
	Color  string `json:"color"`
}

type goldenHairline struct {
	Width int    `json:"width"`
	Color string `json:"color"`
}

type goldenOverlayLayer struct {
	Color   string `json:"color"`
	Opacity string `json:"opacity"`
}

type goldenTone struct {
	Background string `json:"background"`
	Heading    string `json:"heading"`
	Body       string `json:"body"`
	Accent     string `json:"accent"`
}

type goldenType struct {
	Family        *string `json:"family"`
	Size          int     `json:"size"`
	Weight        int     `json:"weight"`
	LineHeight    *string `json:"line_height"`
	LetterSpacing *string `json:"letter_spacing"`
	Transform     *string `json:"transform"`
}

type goldenTokens struct {
	Radius         int               `json:"radius"`
	RowGap         goldenBreakpoints `json:"row_gap"`
	CardPadding    goldenBreakpoints `json:"card_padding"`
	SectionPadding struct {
		Md goldenBreakpoints `json:"md"`
		Lg goldenBreakpoints `json:"lg"`
		Xl goldenBreakpoints `json:"xl"`
	} `json:"section_padding"`
	CardShadow []goldenShadow `json:"card_shadow"`
	Hairline   goldenHairline `json:"hairline"`
	Overlay    struct {
		None   *goldenOverlayLayer `json:"none"`
		Soft   *goldenOverlayLayer `json:"soft"`
		Strong *goldenOverlayLayer `json:"strong"`
	} `json:"overlay"`
	Tones struct {
		Ground  goldenTone `json:"ground"`
		Surface goldenTone `json:"surface"`
		Inverse goldenTone `json:"inverse"`
		Accent  goldenTone `json:"accent"`
	} `json:"tones"`
	Typography struct {
		Display goldenType `json:"display"`
		H2      goldenType `json:"h2"`
		H3      goldenType `json:"h3"`
		Lead    goldenType `json:"lead"`
		Body    goldenType `json:"body"`
		Eyebrow goldenType `json:"eyebrow"`
		Button  goldenType `json:"button"`
	} `json:"typography"`
	Hover struct {
		Ground   string `json:"ground"`
		Surface  string `json:"surface"`
		Ink      string `json:"ink"`
		Muted    string `json:"muted"`
		Accent   string `json:"accent"`
		OnAccent string `json:"on_accent"`
	} `json:"hover"`
}

func readDesignTokensGolden(t *testing.T) (designTokensGolden, []byte) {
	t.Helper()
	raw, err := os.ReadFile(designTokensGoldenFixture)
	if err != nil {
		t.Fatalf("read %s: %v", designTokensGoldenFixture, err)
	}
	var doc designTokensGolden
	dec := json.NewDecoder(bytes.NewReader(raw))
	// A section the file gains and the twin does not read would pass unseen.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode %s: %v", designTokensGoldenFixture, err)
	}
	return doc, raw
}

// snapshotDesignTokens asks the twin for every value the golden holds and
// writes them as the golden's JSON, compact and in the golden's key order.
func snapshotDesignTokens(t *testing.T, design pageDesign) string {
	t.Helper()
	tok, err := newDesignTokens(design)
	if err != nil {
		t.Fatalf("newDesignTokens: %v", err)
	}
	spacing := func(b designSpacing) goldenBreakpoints {
		return goldenBreakpoints{Desktop: b.Desktop, Tablet: b.Tablet, Mobile: b.Mobile}
	}
	pad := func(name string) goldenBreakpoints {
		p, err := tok.sectionPadding(name)
		if err != nil {
			t.Fatalf("sectionPadding(%s): %v", name, err)
		}
		return spacing(p)
	}
	tone := func(name string) goldenTone {
		v, err := tok.tone(name)
		if err != nil {
			t.Fatalf("tone(%s): %v", name, err)
		}
		return goldenTone{Background: v.Background, Heading: v.Heading, Body: v.Body, Accent: v.Accent}
	}
	overlay := func(strength string) *goldenOverlayLayer {
		v, err := tok.overlay(strength)
		if err != nil {
			t.Fatalf("overlay(%s): %v", strength, err)
		}
		if v == nil {
			return nil
		}
		opacity, err := designFractionText(v.Opacity)
		if err != nil {
			t.Fatalf("designFractionText(%v): %v", v.Opacity, err)
		}
		return &goldenOverlayLayer{Color: v.Color, Opacity: opacity}
	}
	typ := func(style string) goldenType {
		v, err := tok.typography(style)
		if err != nil {
			t.Fatalf("typography(%s): %v", style, err)
		}
		text := func(f *float64) *string {
			if f == nil {
				return nil
			}
			s := designDecimalText(*f)
			return &s
		}
		return goldenType{
			Family: v.Family, Size: v.Size, Weight: v.Weight,
			LineHeight: text(v.LineHeight), LetterSpacing: text(v.LetterSpacing), Transform: v.Transform,
		}
	}
	hover := func(hex string) string {
		v, err := designHover(hex)
		if err != nil {
			t.Fatalf("designHover(%s): %v", hex, err)
		}
		return v
	}

	snap := goldenTokens{Radius: tok.radiusPx(), RowGap: spacing(tok.rowGap()), CardPadding: spacing(tok.cardPadding())}
	snap.SectionPadding.Md, snap.SectionPadding.Lg, snap.SectionPadding.Xl = pad("md"), pad("lg"), pad("xl")

	layers, err := tok.cardShadow()
	if err != nil {
		t.Fatalf("cardShadow: %v", err)
	}
	snap.CardShadow = []goldenShadow{}
	for _, l := range layers {
		snap.CardShadow = append(snap.CardShadow, goldenShadow{X: l.X, Y: l.Y, Blur: l.Blur, Spread: l.Spread, Color: l.Color})
	}
	hair, err := tok.hairline()
	if err != nil {
		t.Fatalf("hairline: %v", err)
	}
	snap.Hairline = goldenHairline{Width: hair.Width, Color: hair.Color}

	snap.Overlay.None, snap.Overlay.Soft, snap.Overlay.Strong = overlay("none"), overlay("soft"), overlay("strong")
	snap.Tones.Ground, snap.Tones.Surface, snap.Tones.Inverse, snap.Tones.Accent = tone("ground"), tone("surface"), tone("inverse"), tone("accent")
	snap.Typography.Display, snap.Typography.H2, snap.Typography.H3, snap.Typography.Lead =
		typ("display"), typ("h2"), typ("h3"), typ("lead")
	snap.Typography.Body, snap.Typography.Eyebrow, snap.Typography.Button = typ("body"), typ("eyebrow"), typ("button")

	c := design.Colors
	snap.Hover.Ground, snap.Hover.Surface, snap.Hover.Ink = hover(c.Ground), hover(c.Surface), hover(c.Ink)
	snap.Hover.Muted, snap.Hover.Accent, snap.Hover.OnAccent = hover(c.Muted), hover(c.Accent), hover(c.OnAccent)

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(snap); err != nil {
		t.Fatalf("encode snapshot: %v", err)
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

func compactDesignJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("compact golden tokens: %v", err)
	}
	return buf.String()
}

// firstDifference points at where two texts part, with the text around it.
func firstDifference(got, want string) string {
	n := min(len(got), len(want))
	i := 0
	for i < n && got[i] == want[i] {
		i++
	}
	clip := func(s string) string {
		from := min(max(i-50, 0), len(s))
		return s[from:min(i+50, len(s))]
	}
	return fmt.Sprintf("differs at byte %d\n got: ...%s\nwant: ...%s", i, clip(got), clip(want))
}

// TestDesignTokensGolden replays the agent's golden through the Go twin and
// compares bytes: a case holds the whole token set as the agent wrote it, and
// the twin's answers written in the same shape must be the same text.
func TestDesignTokensGolden(t *testing.T) {
	doc, _ := readDesignTokensGolden(t)
	if len(doc.Cases) == 0 {
		t.Fatal("the golden holds no case")
	}

	t.Run("cases", func(t *testing.T) {
		for _, c := range doc.Cases {
			t.Run(c.Name, func(t *testing.T) {
				got := snapshotDesignTokens(t, c.Design)
				want := compactDesignJSON(t, c.Tokens)
				if got != want {
					t.Fatal(firstDifference(got, want))
				}
			})
		}
	})

	t.Run("ratios", func(t *testing.T) {
		if len(doc.Ratios) != len(designRatios) || len(designTokenRatios) != len(designRatios) {
			t.Fatalf("ratio names: golden %d, twin %d, input %d", len(doc.Ratios), len(designTokenRatios), len(designRatios))
		}
		for _, name := range designRatios {
			ratio, ok := designTokenRatios[name]
			if !ok {
				t.Fatalf("the twin has no number for ratio %q", name)
			}
			if got := designDecimalText(ratio); got != doc.Ratios[name] {
				t.Errorf("ratio %s: got %q, golden %q", name, got, doc.Ratios[name])
			}
		}
	})

	t.Run("type_scale", func(t *testing.T) {
		seen := map[string]bool{}
		for _, row := range doc.TypeScale {
			design := doc.Cases[0].Design
			design.Scale = designScale{BasePx: int64(row.BasePx), Ratio: row.Ratio}
			tok, err := newDesignTokens(design)
			if err != nil {
				t.Fatalf("%s/%d: %v", row.Ratio, row.BasePx, err)
			}
			if len(row.Sizes) != len(designTokenStyles) {
				t.Fatalf("%s/%d: golden holds %d sizes, the twin has %d styles", row.Ratio, row.BasePx, len(row.Sizes), len(designTokenStyles))
			}
			for _, style := range designTokenStyles {
				ty, err := tok.typography(style)
				if err != nil {
					t.Fatalf("typography(%s): %v", style, err)
				}
				if ty.Size != row.Sizes[style] {
					t.Errorf("%s at %dpx, %s: got %d, golden %d", row.Ratio, row.BasePx, style, ty.Size, row.Sizes[style])
				}
			}
			seen[fmt.Sprintf("%s/%d", row.Ratio, row.BasePx)] = true
		}
		if want := len(designRatios) * (designBasePxMax - designBasePxMin + 1); len(seen) != want {
			t.Fatalf("type_scale holds %d distinct ratio and size pairs, want %d", len(seen), want)
		}
	})

	t.Run("hover", func(t *testing.T) {
		if len(doc.Hover) == 0 {
			t.Fatal("the golden holds no hover row")
		}
		for _, row := range doc.Hover {
			got, err := designHover(row.Hex)
			if err != nil {
				t.Errorf("hover %s: %v", row.Hex, err)
				continue
			}
			if got != row.Result {
				t.Errorf("hover %s: got %s, golden %s", row.Hex, got, row.Result)
			}
		}
	})

	t.Run("alpha", func(t *testing.T) {
		if len(doc.Alpha) == 0 {
			t.Fatal("the golden holds no alpha row")
		}
		for _, row := range doc.Alpha {
			got, err := designAlpha(row.Hex, row.Percent)
			if err != nil {
				t.Errorf("alpha %s at %d: %v", row.Hex, row.Percent, err)
				continue
			}
			if got != row.Result {
				t.Errorf("alpha %s at %d: got %s, golden %s", row.Hex, row.Percent, got, row.Result)
			}
		}
	})

	t.Run("refused", func(t *testing.T) {
		if len(doc.Refused) == 0 {
			t.Fatal("the golden holds no refused call")
		}
		for _, row := range doc.Refused {
			var got string
			var err error
			switch row.Call {
			case "hover":
				got, err = designHover(row.Hex)
			case "alpha":
				got, err = designAlpha(row.Hex, row.Percent)
			default:
				t.Fatalf("the golden refuses a call the twin does not know: %q", row.Call)
			}
			if err == nil {
				t.Errorf("%s(%q, %d) answered %q, the golden refuses it", row.Call, row.Hex, row.Percent, got)
			}
		}
	})
}

// TestDesignTokensGoldenCoversEveryOption keeps the replay from passing over
// a file that has stopped exercising a choice the twin has to get right.
func TestDesignTokensGoldenCoversEveryOption(t *testing.T) {
	doc, _ := readDesignTokensGolden(t)
	ratios, densities, radii, bases := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[int64]bool{}
	for _, c := range doc.Cases {
		ratios[c.Design.Scale.Ratio] = true
		densities[c.Design.Density] = true
		radii[c.Design.Radius] = true
		bases[c.Design.Scale.BasePx] = true
	}
	for _, name := range designRatios {
		if !ratios[name] {
			t.Errorf("no case uses ratio %s", name)
		}
	}
	for _, name := range designDensities {
		if !densities[name] {
			t.Errorf("no case uses density %s", name)
		}
	}
	for _, name := range designRadii {
		if !radii[name] {
			t.Errorf("no case uses radius %s", name)
		}
	}
	if len(bases) < 3 {
		t.Errorf("cases use %d base sizes, want a spread", len(bases))
	}
}

// TestDesignTokensGoldenHoldsNoFloat: a number written as a float depends on
// the host's float settings, so the golden holds integers and strings only,
// and no answer is pure black.
func TestDesignTokensGoldenHoldsNoFloat(t *testing.T) {
	_, raw := readDesignTokensGolden(t)

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case json.Number:
			if strings.ContainsAny(x.String(), ".eE") {
				t.Errorf("%s holds the number %s, not an integer", path, x)
			}
		case map[string]any:
			for k, item := range x {
				walk(path+"/"+k, item)
			}
		case []any:
			for i, item := range x {
				walk(fmt.Sprintf("%s/%d", path, i), item)
			}
		}
	}
	walk("", tree)

	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("decode top level: %v", err)
	}
	for key, section := range top {
		if key == "refused" {
			continue // the refused rows name pure black as the input they refuse
		}
		if bytes.Contains(section, []byte("#000000")) || bytes.Contains(section, []byte("rgba(0,0,0")) {
			t.Errorf("section %q holds pure black", key)
		}
	}
}

// TestDesignTokensAnchors holds values worked out by hand from the documented
// rule, so a change to the twin cannot be matched by regenerating a file.
func TestDesignTokensAnchors(t *testing.T) {
	hover := map[string]string{
		"#FFFFFF": "#EBEBEB", // 100% less 8 points is 92%: 0.92 * 255 = 234.6, a half up 235
		"#FF0000": "#D60000", // 50% becomes 42%: the red channel is 0.84 * 255 = 214.2
		"#111111": "#0A0A0A", // the lightness stops at 4 points: 0.04 * 255 = 10.2
		"#050505": "#050505", // a grey darker than the floor stays as it is
		"#0B0F1A": "#06080E", // near black with chroma: each channel scaled by 4 / 7.25
	}
	for in, want := range hover {
		got, err := designHover(in)
		if err != nil || got != want {
			t.Errorf("designHover(%s) = %q, %v; want %s", in, got, err, want)
		}
	}
	alpha := map[int]string{
		0:   "rgba(13,122,99,0)",
		5:   "rgba(13,122,99,0.05)",
		6:   "rgba(13,122,99,0.06)",
		10:  "rgba(13,122,99,0.1)",
		45:  "rgba(13,122,99,0.45)",
		80:  "rgba(13,122,99,0.8)",
		100: "rgba(13,122,99,1)",
	}
	for percent, want := range alpha {
		got, err := designAlpha("#0D7A63", percent)
		if err != nil || got != want {
			t.Errorf("designAlpha(#0D7A63, %d) = %q, %v; want %s", percent, got, err, want)
		}
	}

	doc, _ := readDesignTokensGolden(t)
	byName := map[string]goldenTokens{}
	for _, c := range doc.Cases {
		var g goldenTokens
		if err := json.Unmarshal(c.Tokens, &g); err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		byName[c.Name] = g
	}
	// The inverse tone: ink ground, ground headings, ground body text at 80%.
	if got, want := byName["studio-light"].Tones.Inverse, (goldenTone{Background: "#15171C", Heading: "#F6F5F2", Body: "rgba(246,245,242,0.8)", Accent: "#0D7A63"}); got != want {
		t.Errorf("inverse tone of studio-light = %+v, want %+v", got, want)
	}
	// Density compact is 75% of normal: 64/56/40 is 48/42/30; airy is 125%: 80/70/50.
	if got, want := byName["compact-slate"].SectionPadding.Md, (goldenBreakpoints{Desktop: 48, Tablet: 42, Mobile: 30}); got != want {
		t.Errorf("compact md padding = %+v, want %+v", got, want)
	}
	if got, want := byName["airy-warm"].SectionPadding.Md, (goldenBreakpoints{Desktop: 80, Tablet: 70, Mobile: 50}); got != want {
		t.Errorf("airy md padding = %+v, want %+v", got, want)
	}
}

// TestDesignTokensRefuseWhatTheInputRefuses: the constructor refuses a design
// the class refuses, and takes a font named system as no family.
func TestDesignTokensRefuseWhatTheInputRefuses(t *testing.T) {
	good := pageDesign{
		Colors:  designColours{Ground: "#F6F5F2", Surface: "#FFFFFF", Ink: "#15171C", Muted: "#5A606C", Accent: "#0D7A63", OnAccent: "#FFFFFF"},
		Fonts:   designFonts{Heading: "Manrope", Body: "system"},
		Scale:   designScale{BasePx: 17, Ratio: "major_third"},
		Radius:  "md",
		Density: "normal",
	}
	tok, err := newDesignTokens(good)
	if err != nil {
		t.Fatalf("an honest design is refused: %v", err)
	}
	heading, err := tok.typography("display")
	if err != nil || heading.Family == nil || *heading.Family != "Manrope" {
		t.Errorf("heading family = %v, %v; want Manrope", heading.Family, err)
	}
	body, err := tok.typography("body")
	if err != nil || body.Family != nil {
		t.Errorf("a body font named system gave family %v, %v; want none", body.Family, err)
	}

	refused := map[string]func(d *pageDesign){
		"pure black":           func(d *pageDesign) { d.Colors.Ink = "#000000" },
		"lower case colour":    func(d *pageDesign) { d.Colors.Accent = "#0d7a63" },
		"short colour":         func(d *pageDesign) { d.Colors.Muted = "#FFF" },
		"colour with newline":  func(d *pageDesign) { d.Colors.Ground = "#F6F5F2\n" },
		"empty colour":         func(d *pageDesign) { d.Colors.OnAccent = "" },
		"font with a symbol":   func(d *pageDesign) { d.Fonts.Heading = "Man;rope" },
		"font with a newline":  func(d *pageDesign) { d.Fonts.Body = "Inter\n" },
		"empty font":           func(d *pageDesign) { d.Fonts.Body = "" },
		"font past 60":         func(d *pageDesign) { d.Fonts.Heading = strings.Repeat("a", 61) },
		"base below 15":        func(d *pageDesign) { d.Scale.BasePx = 14 },
		"base above 20":        func(d *pageDesign) { d.Scale.BasePx = 21 },
		"unknown ratio":        func(d *pageDesign) { d.Scale.Ratio = "golden" },
		"ratio as a number":    func(d *pageDesign) { d.Scale.Ratio = "1.25" },
		"unknown radius":       func(d *pageDesign) { d.Radius = "xl" },
		"unknown density":      func(d *pageDesign) { d.Density = "dense" },
		"empty density":        func(d *pageDesign) { d.Density = "" },
		"colour of a document": func(d *pageDesign) { d.Colors.Surface = "red" },
	}
	for name, mutate := range refused {
		d := good
		mutate(&d)
		if _, err := newDesignTokens(d); err == nil {
			t.Errorf("%s: the design was taken", name)
		}
	}

	// A font of exactly 60 characters and the edges of the base size are taken.
	d := good
	d.Fonts.Heading = strings.Repeat("a", 60)
	d.Scale.BasePx = 15
	if _, err := newDesignTokens(d); err != nil {
		t.Errorf("a 60 character font at 15px is refused: %v", err)
	}
	d.Scale.BasePx = 20
	if _, err := newDesignTokens(d); err != nil {
		t.Errorf("a 20px base is refused: %v", err)
	}

	if _, err := tok.tone("sunset"); err == nil {
		t.Error("an unknown tone was answered")
	}
	if _, err := tok.typography("h1"); err == nil {
		t.Error("an unknown text style was answered")
	}
	if _, err := tok.sectionPadding("xs"); err == nil {
		t.Error("an unknown spacing was answered")
	}
	if _, err := tok.overlay("medium"); err == nil {
		t.Error("an unknown overlay strength was answered")
	}
	if none, err := tok.overlay("none"); err != nil || none != nil {
		t.Errorf("overlay none = %v, %v; want no overlay and no error", none, err)
	}
}

// TestDesignDecimalTextMatchesPHP: the number to four decimals with the
// trailing zeros and point cut off. The answers were read from the PHP the
// agent runs.
func TestDesignDecimalTextMatchesPHP(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{1.05, "1.05"}, {1.1, "1.1"}, {1.15, "1.15"}, {1.5, "1.5"}, {1.6, "1.6"}, {1.0, "1"},
		{-0.02, "-0.02"}, {0.08, "0.08"}, {0, "0"}, {10, "10"}, {100, "100"},
		{1.125, "1.125"}, {1.2, "1.2"}, {1.25, "1.25"}, {1.333, "1.333"},
		// A tie goes away from zero, and a tie written in decimal is a tie.
		{0.03125, "0.0313"}, {0.15625, "0.1563"}, {-0.03125, "-0.0313"}, {1.00005, "1.0001"}, {5e-05, "0.0001"},
		{-5e-05, "-0.0001"}, {1.23455, "1.2346"}, {7.00005, "7.0001"},
		// Either side of a tie.
		{4.999e-05, "0"}, {5.001e-05, "0.0001"}, {0.999949999, "0.9999"}, {2.5e-05, "0"},
		// A value already short stays as written.
		{0.285, "0.285"}, {1.005, "1.005"}, {123456.789, "123456.789"}, {0.30000000000000004, "0.3"},
		// A negative that rounds to zero has no sign, and a carry reaches the integer part.
		{-1e-05, "0"}, {99999.99995, "100000"}, {0.99995, "1"},
	}
	for _, c := range cases {
		if got := designDecimalText(c.in); got != c.want {
			t.Errorf("designDecimalText(%v) = %q, want %q", c.in, got, c.want)
		}
	}
	// A value without a finite text still gets some text and never panics.
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		_ = designDecimalText(v)
	}
}

// TestDesignFractionText: rtrim(rtrim(sprintf('%.2F', $v), '0'), '.') for an
// opacity above 0 and below 1.
func TestDesignFractionText(t *testing.T) {
	ok := []struct {
		in   float64
		want string
	}{
		{0.45, "0.45"}, {0.65, "0.65"}, {0.5, "0.5"}, {0.1, "0.1"}, {0.05, "0.05"}, {0.99, "0.99"},
		{0.125, "0.12"}, // a tie in binary goes to the even digit, as sprintf does
		{0.005, "0.01"},
	}
	for _, c := range ok {
		got, err := designFractionText(c.in)
		if err != nil || got != c.want {
			t.Errorf("designFractionText(%v) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	for _, v := range []float64{0, 1, -0.1, 0.0049, 0.999, 1.5, math.NaN(), math.Inf(1)} {
		if got, err := designFractionText(v); err == nil {
			t.Errorf("designFractionText(%v) = %q, want a refusal", v, got)
		}
	}
}
