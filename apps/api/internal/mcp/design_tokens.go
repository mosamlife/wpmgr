package mcp

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The literal values a designed page is built from, worked out from the
// request's design object.
//
// This is the control plane's copy of the agent's DesignTokens class
// (apps/agent/includes/abilities/builders/class-design-tokens.php). A designed
// page is built twice, here and on the site, and the two trees have to be equal
// byte for byte, so every table, every integer division and every number
// written as text below does what the class does. The shared golden
// apps/agent/tests/fixtures/ability-run/design-tokens-cases.json pins both
// halves: TestDesignTokensGolden here, DesignTokensGoldenTest there.
//
// Every value is a literal. A colour is an upper-case #RRGGBB, or
// rgba(r,g,b,a) with no spaces and the alpha written without trailing zeros.
// Pure black is never a colour of this file: it is refused on the way in, so no
// value is #000000 and no tint is rgba(0,0,0,...). Sizes and spacing are whole
// pixels. Line height and letter spacing are em values that the caller writes
// with designDecimalText, and an overlay opacity with designFractionText.

// designTokenStyles are the text styles of a designed page, largest first.
var designTokenStyles = []string{"display", "h2", "h3", "lead", "body", "eyebrow", "button"}

// designTokenRatios is the number each type-scale ratio name stands for. The
// input holds the name; this is the one place that holds the number.
var designTokenRatios = map[string]float64{
	"major_second":   1.125,
	"minor_third":    1.2,
	"major_third":    1.25,
	"perfect_fourth": 1.333,
}

// designOverlayOpacity is the opacity of the ink overlay on a background
// image, by strength.
var designOverlayOpacity = map[string]float64{"soft": 0.45, "strong": 0.65}

const (
	// designInverseBodyAlpha is the alpha, in percent, of the ground colour
	// the inverse tone writes its body text in.
	designInverseBodyAlpha = 80

	// Lightness is counted in 2550ths, which keeps the hover maths in whole
	// numbers: the lightness of a colour is 5 * (its brightest channel + its
	// darkest channel) of 2550. A hover colour is 8 points darker (204 of
	// 2550) and does not go below 4 points (102), so a dark colour never
	// turns pure black.
	designLightnessFull = 2550
	designHoverStep     = 204
	designHoverFloor    = 102

	// A hairline is this many pixels wide, in the muted colour at this alpha.
	designHairlineWidth = 1
	designHairlineAlpha = 20
)

// designCardShadow is the tight and the wide layer of a card shadow, each at
// an alpha, in percent, of the ink colour.
var designCardShadow = []struct{ x, y, blur, spread, alpha int }{
	{0, 1, 2, 0, 6},
	{0, 8, 24, 0, 8},
}

// designTypeRule is what a text style is. The size is the base size times the
// ratio to the power step, or, when percent is not 0, that percent of the base
// size. A nil line height or letter spacing, and an empty transform, mean the
// style sets none.
type designTypeRule struct {
	heading       bool // the heading font, else the body font
	weight        int
	step          int
	percent       int
	lineHeight    *float64
	letterSpacing *float64
	transform     string
}

func designEm(v float64) *float64 { return &v }

var designTypeRules = map[string]designTypeRule{
	"display": {heading: true, weight: 700, step: 5, lineHeight: designEm(1.05), letterSpacing: designEm(-0.02)},
	"h2":      {heading: true, weight: 700, step: 3, lineHeight: designEm(1.1)},
	"h3":      {heading: true, weight: 600, step: 2, lineHeight: designEm(1.15)},
	"lead":    {weight: 400, step: 1, lineHeight: designEm(1.5)},
	"body":    {weight: 400, step: 0, lineHeight: designEm(1.6)},
	"eyebrow": {weight: 600, percent: 80, letterSpacing: designEm(0.08), transform: "uppercase"},
	"button":  {weight: 600, percent: 95, lineHeight: designEm(1.0)},
}

// Spacing at the density normal, in pixels: desktop, tablet, mobile. Every
// value is a multiple of 4, so the 75% and 125% densities give whole pixels.
var (
	designSectionPadding = map[string][3]int{
		"md": {64, 56, 40},
		"lg": {96, 72, 56},
		"xl": {128, 96, 72},
	}
	designRowGap      = [3]int{32, 24, 20}
	designCardPadding = [3]int{32, 28, 24}
)

// designDensityPercent is the density as a percent of the normal spacing.
var designDensityPercent = map[string]int{"compact": 75, "normal": 100, "airy": 125}

// designRadiusPx is the corner radius in pixels, by name.
var designRadiusPx = map[string]int{"none": 0, "sm": 6, "md": 12, "lg": 20}

// designFractionPattern is the shape of an overlay opacity written as text: a
// fraction above 0 and below 1 with at most two decimals.
var designFractionPattern = regexp.MustCompile(`^0\.[0-9]?[1-9]$`)

// designTone is the colours a section of one tone is drawn in.
type designTone struct {
	Background, Heading, Body, Accent string
}

// designOverlay is the overlay on a background image: the ink colour at an
// opacity.
type designOverlay struct {
	Color   string
	Opacity float64
}

// designShadowLayer is one layer of a box shadow in pixels, its colour
// rgba(...).
type designShadowLayer struct {
	X, Y, Blur, Spread int
	Color              string
}

// designHairline is a thin border: its width in pixels and its colour.
type designHairline struct {
	Width int
	Color string
}

// designSpacing is a length in pixels at each breakpoint.
type designSpacing struct {
	Desktop, Tablet, Mobile int
}

// designTypeStyle is one text style as literals. Family is nil for the design
// font system, which means set no family. LineHeight and LetterSpacing are em
// values, nil when the style sets none; Transform is nil likewise.
type designTypeStyle struct {
	Family        *string
	Size          int
	Weight        int
	LineHeight    *float64
	LetterSpacing *float64
	Transform     *string
}

// designTokens is the tokens of one design.
type designTokens struct {
	colours designColours
	heading *string // the heading family, nil for system
	body    *string // the body family, nil for system
	basePx  int
	ratio   float64
	radius  string
	density string
}

// newDesignTokens works out the tokens of a design. It refuses what the
// agent's class refuses: a colour that is not an upper-case #RRGGBB, pure
// black, a font name outside 1 to 60 letters, digits and spaces, a base size
// outside 15 to 20, and a ratio, radius or density that is not one of the
// closed names.
func newDesignTokens(d pageDesign) (*designTokens, error) {
	t := &designTokens{colours: d.Colors}
	for _, role := range designColourRoles {
		if _, err := designChannels(*t.colours.slot(role)); err != nil {
			return nil, err
		}
	}

	var err error
	if t.heading, err = designFamilyOf(d.Fonts.Heading); err != nil {
		return nil, err
	}
	if t.body, err = designFamilyOf(d.Fonts.Body); err != nil {
		return nil, err
	}

	if d.Scale.BasePx < designBasePxMin || d.Scale.BasePx > designBasePxMax {
		return nil, errors.New("a design base size is a whole number from 15 to 20")
	}
	t.basePx = int(d.Scale.BasePx)
	ratio, ok := designTokenRatios[d.Scale.Ratio]
	if !ok {
		return nil, errors.New("a design ratio is major_second, minor_third, major_third or perfect_fourth")
	}
	t.ratio = ratio

	if !slices.Contains(designRadii, d.Radius) {
		return nil, errors.New("a design radius is none, sm, md or lg")
	}
	if !slices.Contains(designDensities, d.Density) {
		return nil, errors.New("a design density is compact, normal or airy")
	}
	t.radius, t.density = d.Radius, d.Density
	return t, nil
}

// tone is the colours a section of one tone is drawn in:
//
//	ground   background ground,  headings ink,       body muted,         accent accent
//	surface  background surface, headings ink,       body muted,         accent accent
//	inverse  background ink,     headings ground,    body ground at 80%, accent accent
//	accent   background accent,  headings on_accent, body on_accent,     accent on_accent
//
// "accent" is the colour of what stands out in the section: a link, an
// eyebrow, the border of an outline button.
func (t *designTokens) tone(name string) (designTone, error) {
	c := t.colours
	switch name {
	case "ground":
		return designTone{Background: c.Ground, Heading: c.Ink, Body: c.Muted, Accent: c.Accent}, nil
	case "surface":
		return designTone{Background: c.Surface, Heading: c.Ink, Body: c.Muted, Accent: c.Accent}, nil
	case "inverse":
		body, err := designAlpha(c.Ground, designInverseBodyAlpha)
		if err != nil {
			return designTone{}, err
		}
		return designTone{Background: c.Ink, Heading: c.Ground, Body: body, Accent: c.Accent}, nil
	case "accent":
		return designTone{Background: c.Accent, Heading: c.OnAccent, Body: c.OnAccent, Accent: c.OnAccent}, nil
	}
	return designTone{}, errors.New("a tone is ground, surface, inverse or accent")
}

// overlay is the overlay on a background image: the ink colour at 0.45 (soft)
// or 0.65 (strong) opacity. None gives nil, no overlay.
func (t *designTokens) overlay(strength string) (*designOverlay, error) {
	if strength == "none" {
		return nil, nil
	}
	opacity, ok := designOverlayOpacity[strength]
	if !ok {
		return nil, errors.New("an overlay is none, soft or strong")
	}
	return &designOverlay{Color: t.colours.Ink, Opacity: opacity}, nil
}

// cardShadow is the two layers of a card shadow, tight then wide, in the ink
// colour at 6% and 8%. A shadow is tinted with ink, never pure black.
func (t *designTokens) cardShadow() ([]designShadowLayer, error) {
	layers := make([]designShadowLayer, 0, len(designCardShadow))
	for _, l := range designCardShadow {
		color, err := designAlpha(t.colours.Ink, l.alpha)
		if err != nil {
			return nil, err
		}
		layers = append(layers, designShadowLayer{X: l.x, Y: l.y, Blur: l.blur, Spread: l.spread, Color: color})
	}
	return layers, nil
}

// hairline is one pixel in the muted colour at 20%.
func (t *designTokens) hairline() (designHairline, error) {
	color, err := designAlpha(t.colours.Muted, designHairlineAlpha)
	if err != nil {
		return designHairline{}, err
	}
	return designHairline{Width: designHairlineWidth, Color: color}, nil
}

// typography is one text style as literals:
//
//	display  heading font, 700, base x ratio^5, line height 1.05, letter spacing -0.02
//	h2       heading font, 700, base x ratio^3, line height 1.1
//	h3       heading font, 600, base x ratio^2, line height 1.15
//	lead     body font,    400, base x ratio,   line height 1.5
//	body     body font,    400, base,           line height 1.6
//	eyebrow  body font,    600, base x 0.8,     letter spacing 0.08, uppercase
//	button   body font,    600, base x 0.95,    line height 1
//
// The size is in whole pixels, a half rounded up.
func (t *designTokens) typography(style string) (designTypeStyle, error) {
	rule, ok := designTypeRules[style]
	if !ok {
		return designTypeStyle{}, errors.New("a text style is display, h2, h3, lead, body, eyebrow or button")
	}

	var size int
	if rule.percent != 0 {
		size = (t.basePx*rule.percent + 50) / 100
	} else {
		size = designRung(t.basePx, t.ratio, rule.step)
	}

	out := designTypeStyle{Family: t.body, Size: size, Weight: rule.weight}
	if rule.heading {
		out.Family = t.heading
	}
	if rule.lineHeight != nil {
		v := *rule.lineHeight
		out.LineHeight = &v
	}
	if rule.letterSpacing != nil {
		v := *rule.letterSpacing
		out.LetterSpacing = &v
	}
	if rule.transform != "" {
		v := rule.transform
		out.Transform = &v
	}
	return out, nil
}

// sectionPadding is the top and bottom padding of a section, in pixels per
// breakpoint, for the design's density: md 64/56/40, lg 96/72/56 and xl
// 128/96/72 at normal, 75% at compact, 125% at airy.
func (t *designTokens) sectionPadding(spacing string) (designSpacing, error) {
	normal, ok := designSectionPadding[spacing]
	if !ok {
		return designSpacing{}, errors.New("a spacing is md, lg or xl")
	}
	return t.perBreakpoint(normal), nil
}

// rowGap is the gap between the children of a row, in pixels per breakpoint:
// 32/24/20 at normal, scaled by the density.
func (t *designTokens) rowGap() designSpacing { return t.perBreakpoint(designRowGap) }

// cardPadding is the padding inside a card, in pixels per breakpoint: 32/28/24
// at normal, scaled by the density.
func (t *designTokens) cardPadding() designSpacing { return t.perBreakpoint(designCardPadding) }

// radiusPx is the corner radius in pixels: none 0, sm 6, md 12, lg 20.
func (t *designTokens) radiusPx() int { return designRadiusPx[t.radius] }

// perBreakpoint is a spacing at the design's density, per breakpoint.
func (t *designTokens) perBreakpoint(normal [3]int) designSpacing {
	percent := designDensityPercent[t.density]
	return designSpacing{
		Desktop: normal[0] * percent / 100,
		Tablet:  normal[1] * percent / 100,
		Mobile:  normal[2] * percent / 100,
	}
}

// designHover is the colour of a button on hover: the HSL lightness lowered by
// 8 percentage points, hue and saturation kept, each channel rounded half up.
// Lightness stops at 4 points (about #0A0A0A), so the result is never
// #000000; a colour already darker than that is returned unchanged.
//
// Whole-number maths. With S the sum of the brightest and the darkest channel,
// the new lightness L (in 2550ths), K = min(L, 2550 - L) and D = S when S is at
// most 255, else 510 - S, a channel v of the colour becomes L + K * (2v - S) / D
// in 2550ths, which is v / 10 on the 0 to 255 scale.
func designHover(hex string) (string, error) {
	ch, err := designChannels(hex)
	if err != nil {
		return "", err
	}
	hi, lo := max(ch[0], ch[1], ch[2]), min(ch[0], ch[1], ch[2])
	sum := hi + lo
	lightness := 5 * sum
	target := max(lightness-designHoverStep, min(lightness, designHoverFloor))

	if hi == lo {
		grey := (2*target + 10) / 20
		return fmt.Sprintf("#%02X%02X%02X", grey, grey, grey), nil
	}

	span := sum
	if sum > 255 {
		span = 510 - sum
	}
	reach := min(target, designLightnessFull-target)
	var out [3]int
	for i, v := range ch {
		scaled := target*span + reach*(2*v-sum)
		out[i] = (2*scaled + 10*span) / (20 * span)
	}
	return fmt.Sprintf("#%02X%02X%02X", out[0], out[1], out[2]), nil
}

// designAlpha is a colour at an alpha, as rgba(r,g,b,a). The alpha is written
// from the integer percent, so the text does not depend on a float: 80 is 0.8,
// 6 is 0.06, 100 is 1, 0 is 0.
func designAlpha(hex string, percent int) (string, error) {
	if percent < 0 || percent > 100 {
		return "", errors.New("an alpha is 0 to 100 percent")
	}
	ch, err := designChannels(hex)
	if err != nil {
		return "", err
	}
	return "rgba(" + strconv.Itoa(ch[0]) + "," + strconv.Itoa(ch[1]) + "," + strconv.Itoa(ch[2]) + "," + designPercentText(percent) + ")", nil
}

// designRung is the base size times the ratio, steps times, in whole pixels (a
// half rounds up). Repeated multiplication, each product rounded to a double
// before the next step, so the result does not depend on the maths library or
// on a fused multiply-add.
func designRung(base int, ratio float64, steps int) int {
	size := float64(base)
	for range steps {
		size = float64(size * ratio)
	}
	return int(math.Floor(size + 0.5))
}

// designFamilyOf is the family a design font name sets: the name, or nil for
// system.
func designFamilyOf(name string) (*string, error) {
	if !designFontPattern.MatchString(name) {
		return nil, errors.New("a design font is 1 to 60 letters, digits and spaces")
	}
	if name == designFontSystem {
		return nil, nil
	}
	return &name, nil
}

// designPercentText is the decimal text of a percent: 80 is "0.8", 6 is "0.06",
// 100 is "1".
func designPercentText(percent int) string {
	switch percent {
	case 0:
		return "0"
	case 100:
		return "1"
	}
	return strings.TrimRight("0."+fmt.Sprintf("%02d", percent), "0")
}

// designChannels is the red, green and blue of a colour.
func designChannels(hex string) ([3]int, error) {
	if !designColourPattern.MatchString(hex) || hex == designPureBlack {
		return [3]int{}, errors.New("a colour is an upper-case #RRGGBB other than #000000")
	}
	var ch [3]int
	for i := range ch {
		v, err := strconv.ParseUint(hex[1+2*i:3+2*i], 16, 8)
		if err != nil {
			return [3]int{}, err
		}
		ch[i] = int(v)
	}
	return ch, nil
}

// designDecimalText is a line height, a letter spacing or a ratio as the
// compile path writes it: the number to four decimals with a point and no
// thousands mark, then the trailing zeros and a trailing point cut off (PHP
// number_format, then rtrim of "0", then rtrim of "."). A point for the
// decimal mark, at most four decimals, no trailing zero.
func designDecimalText(v float64) string {
	return strings.TrimRight(strings.TrimRight(designRoundedText(v, 4), "0"), ".")
}

// designFractionText is an overlay opacity as the compile path writes it:
// rtrim(rtrim(sprintf('%.2F', $v), '0'), '.'). It refuses a value that is not
// a fraction above 0 and below 1.
func designFractionText(v float64) (string, error) {
	text := strings.TrimRight(strings.TrimRight(strconv.FormatFloat(v, 'f', 2, 64), "0"), ".")
	if !designFractionPattern.MatchString(text) {
		return "", errors.New("an overlay opacity is a fraction above 0 and below 1")
	}
	return text, nil
}

// designRoundedText is PHP number_format with a point and no thousands mark: v
// rounded to places decimals, a tie going away from zero, written with exactly
// that many decimals. The rounding reads the shortest decimal form of v, so a
// value written with few decimals rounds the way its digits read. A negative
// that rounds to zero has no sign.
func designRoundedText(v float64, places int) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return strconv.FormatFloat(v, 'f', places, 64)
	}
	whole, frac, _ := strings.Cut(strconv.FormatFloat(math.Abs(v), 'f', -1, 64), ".")
	up := len(frac) > places && frac[places] >= '5'
	if len(frac) > places {
		frac = frac[:places]
	} else {
		frac += strings.Repeat("0", places-len(frac))
	}

	digits := []byte(whole + frac)
	if up {
		i := len(digits) - 1
		for ; i >= 0; i-- {
			if digits[i] != '9' {
				digits[i]++
				break
			}
			digits[i] = '0'
		}
		if i < 0 {
			digits = append([]byte{'1'}, digits...)
		}
	}

	split := len(digits) - places
	out := string(digits[:split])
	if places > 0 {
		out += "." + string(digits[split:])
	}
	if v < 0 && strings.Trim(string(digits), "0") != "" {
		out = "-" + out
	}
	return out
}
