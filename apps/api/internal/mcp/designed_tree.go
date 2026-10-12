package mcp

// The Elementor tree of a designed page (outline version 3), recomputed by the
// control plane.
//
// A designed page is built twice, on the site and here, and the precheck is
// accepted only when the site's tree is this tree, byte for byte as PHP's
// json_encode writes it (see ability_builder_tree.go for the version 2 twin and
// the comparison it feeds). Every table, order of keys and order of node ids
// below does what the agent's compile path does:
//
//	designSections    ElementorSectionCompiler  a section's container and its
//	                                            inner groups, rows and columns
//	designLeafStyle   ElementorLeafStyle        the look of headings, text and
//	                                            buttons
//	designedBuild     ElementorClassicMapper    the walk over the outline: a
//	                                            section's leaves, groups and
//	                                            columns, and the hook a pattern
//	                                            compiles through
//
// The tree is a function of the input, its design, the breakpoints the site has
// active, the images the site resolved and the request id; the Elementor
// release, the theme and the page frame never change it. The shared golden is
// apps/agent/tests/fixtures/ability-run/elementor-designed-containers.json,
// held by TestDesignedTreeGolden.
//
// Node ids come from the same elementorIDs the version 2 twin uses. A section
// takes its id after its children, a group, a columns row and a row of buttons
// take theirs before theirs, as the agent's mapper draws them.
//
// Patterns are compiled by functions registered in designedPatternBuilders,
// one file per area, each registering its names in an init. The base builds
// no pattern: a pattern node whose name is not registered is refused.

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

const (
	// designedSectionLabel is the Navigator label of a section's container:
	// the label prefix, a middle dot, then "Section".
	designedSectionLabel = "WPMgr · Section"

	designedSidePaddingPx      = 24
	designedScreenHeightVh     = 80
	designedMinColumnPercent   = 10
	designedMaxColumnPercent   = 90
	designedPhoneColumnPercent = 100
	designedButtonGapPx        = 16
	designedBorderWidth        = "1.5"

	// designedBackgroundTone is the tone a section with a background image
	// is written in whatever tone it names.
	designedBackgroundTone = "inverse"
)

// The closed sets of the grammar and the tables the compile reads.
var (
	designedWidths         = []string{"boxed", "full"}
	designedTones          = []string{"ground", "surface", "inverse", "accent"}
	designedSpacings       = []string{"md", "lg", "xl"}
	designedAligns         = []string{"start", "center"}
	designedMinHeights     = []string{"auto", "screen"}
	designedOverlays       = []string{"none", "soft", "strong"}
	designedFocals         = []string{"center", "top", "bottom"}
	designedBackgroundKeys = []string{"attachment_id", "overlay", "focal"}
	designedTextStyles     = []string{"body", "lead", "eyebrow"}
	designedButtonStyles   = []string{"fill", "outline", "text"}
	designedRowAligns      = []string{"left", "center"}

	designedFocalPositions = map[string]string{"center": "center center", "top": "top center", "bottom": "bottom center"}
	// designedSideOf is the alignment Elementor stores for a section's.
	designedSideOf        = map[string]string{"start": "left", "center": "center"}
	designedHeadingStyles = map[int64]string{1: "display", 2: "h2", 3: "h3", 4: "h3"}
	designedHeaderSizes   = map[int64]string{1: "h1", 2: "h2", 3: "h3", 4: "h4"}
)

// ---------------------------------------------------------------------------
// Settings shapes
// ---------------------------------------------------------------------------

// designedAllSides is a dimensions value with one length on every side.
func designedAllSides(length string) phpObject {
	return phpObject{{"unit", "px"}, {"top", length}, {"right", length}, {"bottom", length}, {"left", length}, {"isLinked", true}}
}

// designedSides is a section's padding: the vertical length and the side
// gutter.
func designedSides(vertical int) phpObject {
	v := strconv.Itoa(vertical)
	return phpObject{
		{"unit", "px"}, {"top", v}, {"right", strconv.Itoa(designedSidePaddingPx)},
		{"bottom", v}, {"left", strconv.Itoa(designedSidePaddingPx)}, {"isLinked", false},
	}
}

// designedGaps is a gap value, the same across and down.
func designedGaps(px int) phpObject {
	v := strconv.Itoa(px)
	return phpObject{{"column", v}, {"row", v}, {"isLinked", true}, {"unit", "px"}}
}

// designedPercent is a width in percent.
func designedPercent(size int) phpObject {
	return phpObject{{"unit", "%"}, {"size", strconv.Itoa(size)}}
}

// designedInner is an inner container: a group, a row or a column.
func designedInner(id string, settings phpObject, elements phpList) phpObject {
	return elementorLayout(id, "container", settings, elements, true)
}

// designedOption is the value of a section option: the fallback when the key
// is absent, the value when it is one of allowed, and a refusal otherwise
// (JSON null included).
func designedOption(obj map[string]json.RawMessage, key string, allowed []string, fallback string) (string, bool) {
	raw, has := obj[key]
	if !has {
		return fallback, true
	}
	v, ok := jsonStringOf(raw)
	if !ok || !slices.Contains(allowed, v) {
		return "", false
	}
	return v, true
}

// ---------------------------------------------------------------------------
// Sections
// ---------------------------------------------------------------------------

// designSections builds the containers of a designed page.
type designSections struct {
	tokens *designTokens
	active []string
	media  map[int64]pageMediaFact
}

// newDesignSections is a compiler for a design, the breakpoints the site has
// active (nil for none) and the images the site resolved.
func newDesignSections(tokens *designTokens, breakpoints []string, media []pageMediaFact) *designSections {
	byID := make(map[int64]pageMediaFact, len(media))
	for _, m := range media {
		byID[m.ID] = m
	}
	return &designSections{tokens: tokens, active: breakpoints, media: byID}
}

// isActive: the site has the breakpoint (tablet or mobile) active. A name
// other than those two is never consulted.
func (s *designSections) isActive(breakpoint string) bool {
	return slices.Contains(s.active, breakpoint)
}

// designedBackground is a section's background with its options filled in.
type designedBackground struct {
	attachmentID int64
	overlay      string
	focal        string
}

// designedBackgroundOf reads a section's background: nil when it has none.
func designedBackgroundOf(section map[string]json.RawMessage) (*designedBackground, bool) {
	raw, has := section["background"]
	if !has {
		return nil, true
	}
	obj, ok := jsonObjectOf(raw)
	if !ok {
		return nil, false
	}
	for key := range obj {
		if !slices.Contains(designedBackgroundKeys, key) {
			return nil, false
		}
	}
	id, ok := jsonIntOf(obj["attachment_id"])
	if !ok || id < 1 || id > pageCreateMaxAttachmentID {
		return nil, false
	}
	overlay, ok1 := designedOption(obj, "overlay", designedOverlays, "strong")
	focal, ok2 := designedOption(obj, "focal", designedFocals, "center")
	if !ok1 || !ok2 {
		return nil, false
	}
	return &designedBackground{attachmentID: id, overlay: overlay, focal: focal}, true
}

// designedAnchor is a section's anchor: the shape the grammar accepts, and not
// an id the theme or WordPress already uses.
func designedAnchor(section map[string]json.RawMessage) (string, bool) {
	anchor, ok := jsonStringOf(section["anchor"])
	if !ok || !pageAnchorPattern.MatchString(anchor) || slices.Contains(pageReservedAnchors, anchor) {
		return "", false
	}
	return anchor, true
}

// tone is the tone whose colours a section and its children are written in:
// the inverse tone when the section has a background image.
func (s *designSections) tone(section map[string]json.RawMessage) (string, bool) {
	tone, ok := designedOption(section, "tone", designedTones, "ground")
	if !ok {
		return "", false
	}
	bg, ok := designedBackgroundOf(section)
	if !ok {
		return "", false
	}
	if bg != nil {
		return designedBackgroundTone, true
	}
	return tone, true
}

// container is a section as a container around its compiled children.
func (s *designSections) container(id string, section map[string]json.RawMessage, elements phpList) (phpObject, bool) {
	settings, ok := s.settings(section)
	if !ok {
		return nil, false
	}
	return elementorLayout(id, "container", settings, elements, false), true
}

// settings are a section container's, in the order Elementor returns them:
// width, tag, direction, anchor, padding per active breakpoint, the tone's
// background, the background image and its overlay, the screen height, and the
// Navigator label.
func (s *designSections) settings(section map[string]json.RawMessage) (phpObject, bool) {
	width, ok1 := designedOption(section, "width", designedWidths, "boxed")
	tone, ok2 := designedOption(section, "tone", designedTones, "ground")
	spacing, ok3 := designedOption(section, "spacing", designedSpacings, "lg")
	bg, ok4 := designedBackgroundOf(section)
	anchor, ok5 := designedAnchor(section)
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 {
		return nil, false
	}
	out := phpObject{{"content_width", width}, {"html_tag", "section"}, {"flex_direction", "column"}, {"_element_id", anchor}}

	padding, ok := s.padding(spacing)
	if !ok {
		return nil, false
	}
	out = append(out, padding...)

	if bg != nil {
		tone = designedBackgroundTone
	}
	colours, err := s.tokens.tone(tone)
	if err != nil {
		return nil, false
	}
	out = append(out, phpPair{"background_background", "classic"}, phpPair{"background_color", colours.Background})

	if bg != nil {
		image, ok := s.image(*bg)
		if !ok {
			return nil, false
		}
		out = append(out, image...)
	}

	height, ok := s.minHeight(section)
	if !ok {
		return nil, false
	}
	out = append(out, height...)
	return append(out, phpPair{"_title", designedSectionLabel}), true
}

// padding is the top and bottom padding at the desktop step, then at each of
// tablet and mobile the site has active.
func (s *designSections) padding(spacing string) (phpObject, bool) {
	steps, err := s.tokens.sectionPadding(spacing)
	if err != nil {
		return nil, false
	}
	out := phpObject{{"padding", designedSides(steps.Desktop)}}
	if s.isActive("tablet") {
		out = append(out, phpPair{"padding_tablet", designedSides(steps.Tablet)})
	}
	if s.isActive("mobile") {
		out = append(out, phpPair{"padding_mobile", designedSides(steps.Mobile)})
	}
	return out, true
}

// image is the background image over the tone's colour: the image the site
// resolved, placed at the focal point, covering the section, then the ink
// overlay at the background's strength.
func (s *designSections) image(bg designedBackground) (phpObject, bool) {
	fact, ok := s.media[bg.attachmentID]
	if !ok || fact.ID != bg.attachmentID || !pageImageURLUsable(fact.URL) {
		return nil, false
	}
	if _, ok := pageImageMimes[fact.Mime]; !ok {
		return nil, false
	}
	out := phpObject{
		{"background_image", phpObject{{"id", strconv.FormatInt(bg.attachmentID, 10)}, {"url", fact.URL}}},
		{"background_position", designedFocalPositions[bg.focal]},
		{"background_size", "cover"},
		{"background_repeat", "no-repeat"},
	}
	overlay, err := s.tokens.overlay(bg.overlay)
	if err != nil {
		return nil, false
	}
	if overlay == nil {
		return out, true
	}
	opacity, err := designFractionText(overlay.Opacity)
	if err != nil {
		return nil, false
	}
	return append(out,
		phpPair{"background_overlay_background", "classic"},
		phpPair{"background_overlay_color", overlay.Color},
		phpPair{"background_overlay_opacity", phpObject{{"unit", "px"}, {"size", opacity}}},
	), true
}

// minHeight is the height of a section of screen height: 80vh from the tablet
// up, its own height on a phone when the site has that breakpoint, its content
// centred. A section of auto height writes nothing.
func (s *designSections) minHeight(section map[string]json.RawMessage) (phpObject, bool) {
	height, ok := designedOption(section, "min_height", designedMinHeights, "auto")
	if !ok {
		return nil, false
	}
	if height != "screen" {
		return phpObject{}, true
	}
	out := phpObject{{"min_height", phpObject{{"unit", "vh"}, {"size", strconv.Itoa(designedScreenHeightVh)}}}}
	if s.isActive("mobile") {
		out = append(out, phpPair{"min_height_mobile", phpObject{{"unit", "vh"}, {"size", "0"}}})
	}
	return append(out, phpPair{"flex_justify_content", "center"}), true
}

// group is a full-width column of its elements inside a section.
func (s *designSections) group(id string, elements phpList) phpObject {
	return designedInner(id, phpObject{{"content_width", "full"}, {"flex_direction", "column"}}, elements)
}

// row is a columns row: its columns side by side, stacked on a phone when the
// site has the mobile breakpoint, with the design's row gap.
func (s *designSections) row(id string, columns phpList) phpObject {
	settings := phpObject{{"flex_direction", "row"}}
	if s.isActive("mobile") {
		settings = append(settings, phpPair{"flex_direction_mobile", "column"})
	}
	return designedInner(id, append(settings, s.gapSettings()...), columns)
}

// gapSettings are the design's row gap as container settings: flex_gap at the
// desktop step, then flex_gap_tablet and flex_gap_mobile for each breakpoint
// the site has active.
func (s *designSections) gapSettings() phpObject {
	gap := s.tokens.rowGap()
	out := phpObject{{"flex_gap", designedGaps(gap.Desktop)}}
	if s.isActive("tablet") {
		out = append(out, phpPair{"flex_gap_tablet", designedGaps(gap.Tablet)})
	}
	if s.isActive("mobile") {
		out = append(out, phpPair{"flex_gap_mobile", designedGaps(gap.Mobile)})
	}
	return out
}

// column is one column of a row: full content width at its share of the row,
// and the whole row's width on a phone when the site has the mobile
// breakpoint.
func (s *designSections) column(id string, width int, elements phpList) (phpObject, bool) {
	if width < designedMinColumnPercent || width > designedMaxColumnPercent {
		return nil, false
	}
	settings := phpObject{{"content_width", "full"}, {"width", designedPercent(width)}}
	if s.isActive("mobile") {
		settings = append(settings, phpPair{"width_mobile", designedPercent(designedPhoneColumnPercent)})
	}
	return designedInner(id, settings, elements), true
}

// cardSettings are the settings that make a container a card, to follow its
// layout keys: the design's card padding per active breakpoint, a solid border
// of edgePx in edgeColour, the design's radius on every corner, and, for a
// raised card, the wide layer of the card shadow (a container holds one).
func (s *designSections) cardSettings(edgeColour string, edgePx int, raised bool) (phpObject, bool) {
	if edgePx < 1 {
		return nil, false
	}
	padding := s.tokens.cardPadding()
	out := phpObject{{"padding", designedAllSides(strconv.Itoa(padding.Desktop))}}
	if s.isActive("tablet") {
		out = append(out, phpPair{"padding_tablet", designedAllSides(strconv.Itoa(padding.Tablet))})
	}
	if s.isActive("mobile") {
		out = append(out, phpPair{"padding_mobile", designedAllSides(strconv.Itoa(padding.Mobile))})
	}
	out = append(out,
		phpPair{"border_border", "solid"},
		phpPair{"border_width", designedAllSides(strconv.Itoa(edgePx))},
		phpPair{"border_color", edgeColour},
		phpPair{"border_radius", designedAllSides(strconv.Itoa(s.tokens.radiusPx()))},
	)
	if raised {
		layers, err := s.tokens.cardShadow()
		if err != nil || len(layers) == 0 {
			return nil, false
		}
		wide := layers[len(layers)-1]
		out = append(out,
			phpPair{"box_shadow_box_shadow_type", "yes"},
			phpPair{"box_shadow_box_shadow", phpObject{
				{"horizontal", strconv.Itoa(wide.X)}, {"vertical", strconv.Itoa(wide.Y)},
				{"blur", strconv.Itoa(wide.Blur)}, {"spread", strconv.Itoa(wide.Spread)}, {"color", wide.Color},
			}},
		)
	}
	return out, true
}

// ---------------------------------------------------------------------------
// The look of leaves
// ---------------------------------------------------------------------------

// designLeafStyle is the look a designed page gives the leaves of a section:
// the settings Elementor stores after a leaf's own content keys.
type designLeafStyle struct {
	tokens *designTokens
}

// typography is a typography group as literals behind its activator.
func (l designLeafStyle) typography(style string) (phpObject, bool) {
	t, err := l.tokens.typography(style)
	if err != nil {
		return nil, false
	}
	out := phpObject{{"typography_typography", "custom"}}
	if t.Family != nil {
		out = append(out, phpPair{"typography_font_family", *t.Family})
	}
	out = append(out,
		phpPair{"typography_font_size", phpObject{{"unit", "px"}, {"size", strconv.Itoa(t.Size)}}},
		phpPair{"typography_font_weight", strconv.Itoa(t.Weight)},
	)
	if t.LineHeight != nil {
		out = append(out, phpPair{"typography_line_height", phpObject{{"unit", "em"}, {"size", designDecimalText(*t.LineHeight)}}})
	}
	if t.Transform != nil {
		out = append(out, phpPair{"typography_text_transform", *t.Transform})
	}
	if t.LetterSpacing != nil {
		out = append(out, phpPair{"typography_letter_spacing", phpObject{{"unit", "em"}, {"size", designDecimalText(*t.LetterSpacing)}}})
	}
	return out, true
}

// headingStyle is the style keys of a heading of level 1 to 4: its alignment
// and the title colour of the tone, then the type ramp's group.
func (l designLeafStyle) headingStyle(level int64, tone, align string) (phpObject, bool) {
	style, ok := designedHeadingStyles[level]
	side, ok2 := designedSideOf[align]
	colours, err := l.tokens.tone(tone)
	if !ok || !ok2 || err != nil {
		return nil, false
	}
	out := phpObject{{"align", side}, {"title_color", colours.Heading}}
	type_, ok := l.typography(style)
	if !ok {
		return nil, false
	}
	return append(out, type_...), true
}

// textStyle is the style keys of a text-editor widget: the alignment only when
// the section is centred, the colour of the tone (its accent for an eyebrow),
// then the type ramp's group.
func (l designLeafStyle) textStyle(style, tone, align string) (phpObject, bool) {
	side, ok := designedSideOf[align]
	colours, err := l.tokens.tone(tone)
	if !slices.Contains(designedTextStyles, style) || !ok || err != nil {
		return nil, false
	}
	out := phpObject{}
	if side == "center" {
		out = append(out, phpPair{"align", side})
	}
	colour := colours.Body
	if style == "eyebrow" {
		colour = colours.Accent
	}
	out = append(out, phpPair{"text_color", colour})
	type_, ok := l.typography(style)
	if !ok {
		return nil, false
	}
	return append(out, type_...), true
}

// buttonStyle is the style keys of a button, to follow its text and link: a
// fill (the accent as background), an outline (the accent as text and as a
// 1.5 px border) or text (underlined at rest), in the accent of the tone; on
// the accent tone the button is drawn in on_accent. Every button has size md,
// the design's radius and the Button typography.
func (l designLeafStyle) buttonStyle(style, tone string) (phpObject, bool) {
	if !slices.Contains(designedButtonStyles, style) {
		return nil, false
	}
	onAccent, err1 := l.tokens.tone("accent")
	colours, err2 := l.tokens.tone(tone)
	if err1 != nil || err2 != nil {
		return nil, false
	}
	main := colours.Accent
	on := onAccent.Heading
	if tone == "accent" {
		on = onAccent.Background
	}

	out := phpObject{{"size", "md"}, {"button_type", ""}, {"background_background", "classic"}}
	switch style {
	case "fill":
		hover, err := designHover(main)
		if err != nil {
			return nil, false
		}
		out = append(out,
			phpPair{"background_color", main},
			phpPair{"button_text_color", on},
			phpPair{"button_background_hover_background", "classic"},
			phpPair{"button_background_hover_color", hover},
			phpPair{"hover_color", on},
		)
	case "outline":
		out = append(out,
			phpPair{"background_color", "transparent"},
			phpPair{"button_text_color", main},
			phpPair{"border_border", "solid"},
			phpPair{"border_width", designedAllSides(designedBorderWidth)},
			phpPair{"border_color", main},
			phpPair{"button_background_hover_background", "classic"},
			phpPair{"button_background_hover_color", main},
			phpPair{"hover_color", on},
		)
	default:
		hover, err := designHover(main)
		if err != nil {
			return nil, false
		}
		out = append(out,
			phpPair{"background_color", "transparent"},
			phpPair{"button_text_color", main},
			phpPair{"button_background_hover_background", "classic"},
			phpPair{"button_background_hover_color", "transparent"},
			phpPair{"hover_color", hover},
		)
	}
	out = append(out, phpPair{"border_radius", designedAllSides(strconv.Itoa(l.tokens.radiusPx()))})
	type_, ok := l.typography("button")
	if !ok {
		return nil, false
	}
	out = append(out, type_...)
	if style == "text" {
		out = append(out, phpPair{"typography_text_decoration", "underline"})
	}
	return out, true
}

// buttonRow is a row of buttons: an inner container that holds them side by
// side and wraps them on a narrow screen.
func (l designLeafStyle) buttonRow(id string, buttons phpList, align string) (phpObject, bool) {
	if !slices.Contains(designedRowAligns, align) {
		return nil, false
	}
	gap := strconv.Itoa(designedButtonGapPx)
	settings := phpObject{
		{"content_width", "full"}, {"flex_direction", "row"}, {"flex_wrap", "wrap"},
		{"flex_gap", phpObject{{"column", gap}, {"row", gap}, {"isLinked", true}, {"unit", "px"}}},
	}
	if align == "center" {
		settings = append(settings, phpPair{"flex_justify_content", "center"})
	}
	return designedInner(id, settings, buttons), true
}

// ---------------------------------------------------------------------------
// The walk over the outline
// ---------------------------------------------------------------------------

// designedPatternFunc compiles one pattern node into its container. path is
// the node's outline path, tone the tone of the section it stands in. A
// function draws its ids from b.ids and builds blocks it holds with b.blocks.
type designedPatternFunc func(b *designedBuild, n map[string]json.RawMessage, path, tone string) (phpObject, bool)

// designedPatternBuilders are the patterns the twin compiles, by name.
var designedPatternBuilders = map[string]designedPatternFunc{}

// designedBuild compiles the sections of one designed page.
type designedBuild struct {
	ids      *elementorIDs
	sections *designSections
	look     designLeafStyle
	media    map[int64]pageMediaFact
	patterns map[string]designedPatternFunc

	// The tone and alignment of the section whose leaves are being built, and
	// whether the next paragraph is the lead: no paragraph has followed a
	// level 1 heading yet.
	tone     string
	align    string
	leadNext bool
}

// designedClassicTree is the Elementor tree the agent builds for a valid
// designed page-create input: node ids from requestID, the breakpoints the site
// has active (nil for none) and each image's address from media. ok is false
// for an input the compile refuses.
func designedClassicTree(input []byte, requestID string, breakpoints []string, media []pageMediaFact) (phpList, bool) {
	return designedTreeWith(input, requestID, breakpoints, media, designedPatternBuilders)
}

// designedTreeWith is designedClassicTree with the patterns given.
func designedTreeWith(input []byte, requestID string, breakpoints []string, media []pageMediaFact, patterns map[string]designedPatternFunc) (phpList, bool) {
	top, ok := jsonObjectOf(input)
	if !ok {
		return nil, false
	}
	design, problem := validatePageDesign(top["design"])
	if problem != nil {
		return nil, false
	}
	tokens, err := newDesignTokens(design)
	if err != nil {
		return nil, false
	}
	outline, ok := jsonArrayOf(top["outline"])
	if !ok || len(outline) == 0 {
		return nil, false
	}
	sections := newDesignSections(tokens, breakpoints, media)
	b := &designedBuild{
		ids:      &elementorIDs{requestID: requestID, taken: map[string]bool{}},
		sections: sections,
		look:     designLeafStyle{tokens: tokens},
		media:    sections.media,
		patterns: patterns,
	}
	return b.page(outline, "outline")
}

// page maps the top level of a designed page: every item is a section.
func (b *designedBuild) page(outline []json.RawMessage, listPath string) (phpList, bool) {
	tree := phpList{}
	for i, raw := range outline {
		section, ok := jsonObjectOf(raw)
		if !ok {
			return nil, false
		}
		if kind, _ := jsonStringOf(section["type"]); kind != "section" {
			return nil, false
		}
		built, ok := b.section(section, fmt.Sprintf("%s[%d]", listPath, i))
		if !ok {
			return nil, false
		}
		tree = append(tree, built)
	}
	return tree, true
}

// section maps one section: its children first, then its own container.
func (b *designedBuild) section(section map[string]json.RawMessage, path string) (phpObject, bool) {
	children, ok := jsonArrayOf(section["children"])
	if !ok || len(children) == 0 {
		return nil, false
	}
	align := "start"
	if raw, has := present(section, "align"); has {
		if align, ok = jsonStringOf(raw); !ok || !slices.Contains(designedAligns, align) {
			return nil, false
		}
	}
	tone, ok := b.sections.tone(section)
	if !ok {
		return nil, false
	}
	elements, ok := b.children(children, path+".children", tone, align)
	if !ok {
		return nil, false
	}
	id, ok := b.ids.next(path)
	if !ok {
		return nil, false
	}
	return b.sections.container(id, section, elements)
}

// children maps the children of a section: leaves as widgets in the design's
// look and the section's tone and alignment, groups and columns as the
// design's containers, patterns as the registered compilers'.
func (b *designedBuild) children(children []json.RawMessage, listPath, tone, align string) (phpList, bool) {
	b.tone, b.align, b.leadNext = tone, align, false
	elements := phpList{}
	var run []elementorLeafAt
	flush := func() bool {
		if len(run) == 0 {
			return true
		}
		widgets, ok := b.leafList(run)
		run = nil
		if !ok {
			return false
		}
		elements = append(elements, widgets...)
		return true
	}
	for j, raw := range children {
		at := fmt.Sprintf("%s[%d]", listPath, j)
		n, ok := jsonObjectOf(raw)
		if !ok {
			return nil, false
		}
		kind, _ := jsonStringOf(n["type"])
		if kind != "group" && kind != "columns" && kind != "pattern" {
			run = append(run, elementorLeafAt{at, n})
			continue
		}
		if !flush() {
			return nil, false
		}
		b.leadNext = false
		var built phpObject
		switch kind {
		case "group":
			built, ok = b.group(n, at)
		case "columns":
			built, ok = b.columns(n, at)
		default:
			built, ok = b.pattern(n, at, tone)
		}
		if !ok {
			return nil, false
		}
		elements = append(elements, built)
	}
	if !flush() {
		return nil, false
	}
	return elements, true
}

// blocks maps the blocks a pattern holds (a hero's or a call to action's) as a
// section's leaves are built, in the tone and alignment the pattern asks for.
// It starts from a fresh state, so no paragraph is the lead until a level 1
// heading has come, and it leaves the section's state as it was.
func (b *designedBuild) blocks(leaves []json.RawMessage, tone, align, listPath string) (phpList, bool) {
	if len(leaves) == 0 || !slices.Contains(designedTones, tone) || !slices.Contains(designedAligns, align) {
		return nil, false
	}
	sub := *b
	sub.tone, sub.align, sub.leadNext = tone, align, false
	run := make([]elementorLeafAt, 0, len(leaves))
	for i, raw := range leaves {
		n, ok := jsonObjectOf(raw)
		if !ok {
			return nil, false
		}
		run = append(run, elementorLeafAt{fmt.Sprintf("%s[%d]", listPath, i), n})
	}
	return sub.leafList(run)
}

// pattern maps one pattern node with the compiler registered for its name.
func (b *designedBuild) pattern(n map[string]json.RawMessage, path, tone string) (phpObject, bool) {
	name, ok := jsonStringOf(n["pattern"])
	if !ok {
		return nil, false
	}
	build, known := b.patterns[name]
	if !known {
		return nil, false
	}
	return build(b, n, path, tone)
}

// group maps a group of a section: a full-width column of its leaves and
// rows. The group's id comes before its children's.
func (b *designedBuild) group(n map[string]json.RawMessage, path string) (phpObject, bool) {
	children, ok := jsonArrayOf(n["children"])
	if !ok || len(children) == 0 {
		return nil, false
	}
	id, ok := b.ids.next(path)
	if !ok {
		return nil, false
	}
	listPath := path + ".children"
	elements := phpList{}
	for j, raw := range children {
		at := fmt.Sprintf("%s[%d]", listPath, j)
		child, ok := jsonObjectOf(raw)
		if !ok {
			return nil, false
		}
		switch kind, _ := jsonStringOf(child["type"]); kind {
		case "group":
			return nil, false
		case "columns":
			built, ok := b.columns(child, at)
			if !ok {
				return nil, false
			}
			elements = append(elements, built)
		default:
			widgets, ok := b.leaf(child, at)
			if !ok {
				return nil, false
			}
			elements = append(elements, widgets...)
		}
	}
	return b.sections.group(id, elements), true
}

// columns maps a columns node of a section: a row of 2 to 4 columns, each at
// its share of the row. The row's id comes before its columns', and a column's
// before its leaves'.
func (b *designedBuild) columns(n map[string]json.RawMessage, path string) (phpObject, bool) {
	cols, ok := jsonArrayOf(n["columns"])
	if !ok || len(cols) < 2 || len(cols) > 4 {
		return nil, false
	}
	count := len(cols)
	var widths []int64
	if raw, has := present(n, "widths"); has {
		list, ok := jsonArrayOf(raw)
		if !ok || len(list) != count {
			return nil, false
		}
		for _, item := range list {
			w, ok := jsonIntOf(item)
			if !ok || w < designedMinColumnPercent || w > designedMaxColumnPercent {
				return nil, false
			}
			widths = append(widths, w)
		}
	}
	id, ok := b.ids.next(path)
	if !ok {
		return nil, false
	}
	built := phpList{}
	for k, rawCol := range cols {
		at := fmt.Sprintf("%s.columns[%d]", path, k)
		col, ok := jsonObjectOf(rawCol)
		if !ok {
			return nil, false
		}
		children, ok := jsonArrayOf(col["children"])
		if !ok || len(children) == 0 {
			return nil, false
		}
		columnID, ok := b.ids.next(at)
		if !ok {
			return nil, false
		}
		run := make([]elementorLeafAt, 0, len(children))
		for m, raw := range children {
			child, ok := jsonObjectOf(raw)
			if !ok {
				return nil, false
			}
			run = append(run, elementorLeafAt{fmt.Sprintf("%s.children[%d]", at, m), child})
		}
		elements, ok := b.leafList(run)
		if !ok {
			return nil, false
		}
		share := 100 / count
		if widths != nil {
			share = int(widths[k])
		}
		column, ok := b.sections.column(columnID, share, elements)
		if !ok {
			return nil, false
		}
		built = append(built, column)
	}
	return b.sections.row(id, built), true
}

func (b *designedBuild) leafList(run []elementorLeafAt) (phpList, bool) {
	elements := phpList{}
	for _, l := range run {
		widgets, ok := b.leaf(l.node, l.path)
		if !ok {
			return nil, false
		}
		elements = append(elements, widgets...)
	}
	return elements, true
}

// widget is one widget with the id of its path.
func (b *designedBuild) widget(path, kind string, settings phpObject) (phpList, bool) {
	id, ok := b.ids.next(path)
	if !ok {
		return nil, false
	}
	return phpList{phpObject{{"id", id}, {"elType", "widget"}, {"settings", settings}, {"elements", phpList{}}, {"widgetType", kind}}}, true
}

// textEditor is a text-editor widget holding html, in the text style given.
func (b *designedBuild) textEditor(path, html, style string) (phpList, bool) {
	look, ok := b.look.textStyle(style, b.tone, b.align)
	if !ok {
		return nil, false
	}
	return b.widget(path, "text-editor", append(phpObject{{"editor", html}}, look...))
}

// leaf maps one outline leaf of a designed section to its widgets (a buttons
// node is one row container).
func (b *designedBuild) leaf(n map[string]json.RawMessage, path string) (phpList, bool) {
	switch kind, _ := jsonStringOf(n["type"]); kind {
	case "heading":
		level, ok1 := jsonIntOf(n["level"])
		text, ok2 := jsonStringOf(n["text"])
		size, ok3 := designedHeaderSizes[level]
		if !ok1 || !ok2 || !ok3 {
			return nil, false
		}
		look, ok := b.look.headingStyle(level, b.tone, b.align)
		if !ok {
			return nil, false
		}
		b.leadNext = b.leadNext || level == 1
		return b.widget(path, "heading", append(phpObject{{"title", elementorHTMLText.Replace(text)}, {"header_size", size}}, look...))
	case "paragraph":
		text, ok := jsonStringOf(n["text"])
		if !ok || elementorOwnAddress.MatchString(text) {
			return nil, false
		}
		style := "body"
		if b.leadNext {
			style = "lead"
		}
		b.leadNext = false
		return b.textEditor(path, "<p>"+elementorHTMLText.Replace(text)+"</p>", style)
	case "list":
		ordered, ok1 := jsonBoolOf(n["ordered"])
		items, ok2 := elementorTextList(n["items"], false)
		if !ok1 || !ok2 {
			return nil, false
		}
		tag := "ul"
		if ordered {
			tag = "ol"
		}
		var h strings.Builder
		h.WriteString("<" + tag + ">")
		for _, item := range items {
			h.WriteString("<li>" + elementorHTMLText.Replace(item) + "</li>")
		}
		return b.textEditor(path, h.String()+"</"+tag+">", "body")
	case "quote":
		html, ok := designedQuoteHTML(n)
		if !ok {
			return nil, false
		}
		return b.textEditor(path, html, "body")
	case "table":
		html, ok := designedTableHTML(n)
		if !ok {
			return nil, false
		}
		return b.textEditor(path, html, "body")
	case "separator":
		return b.widget(path, "divider", phpObject{})
	case "spacer":
		size, _ := jsonStringOf(n["size"])
		px, ok := elementorSpacerPx[size]
		if !ok {
			return nil, false
		}
		return b.widget(path, "spacer", phpObject{{"space", phpObject{{"unit", "px"}, {"size", px}, {"sizes", phpList{}}}}})
	case "image":
		return b.image(n, path)
	case "buttons":
		return b.buttons(n, path)
	}
	return nil, false
}

// designedQuoteHTML is the blockquote template of a quote's paragraphs and
// optional citation.
func designedQuoteHTML(n map[string]json.RawMessage) (string, bool) {
	paras, ok := elementorTextList(n["paragraphs"], false)
	if !ok {
		return "", false
	}
	var h strings.Builder
	h.WriteString("<blockquote>")
	for _, p := range paras {
		if elementorOwnAddress.MatchString(p) {
			return "", false
		}
		h.WriteString("<p>" + elementorHTMLText.Replace(p) + "</p>")
	}
	if raw, ok := present(n, "citation"); ok {
		citation, ok := jsonStringOf(raw)
		if !ok {
			return "", false
		}
		h.WriteString("<cite>" + elementorHTMLText.Replace(citation) + "</cite>")
	}
	return h.String() + "</blockquote>", true
}

// designedTableHTML is the table template of a table's optional header and its
// rows.
func designedTableHTML(n map[string]json.RawMessage) (string, bool) {
	var h strings.Builder
	h.WriteString("<table>")
	if raw, ok := present(n, "header"); ok {
		header, ok := elementorTextList(raw, true)
		if !ok {
			return "", false
		}
		h.WriteString("<thead><tr>")
		for _, cell := range header {
			h.WriteString("<th>" + elementorHTMLText.Replace(cell) + "</th>")
		}
		h.WriteString("</tr></thead>")
	}
	rows, ok := jsonArrayOf(n["rows"])
	if !ok || len(rows) == 0 {
		return "", false
	}
	h.WriteString("<tbody>")
	for _, raw := range rows {
		cells, ok := elementorTextList(raw, true)
		if !ok {
			return "", false
		}
		h.WriteString("<tr>")
		for _, cell := range cells {
			h.WriteString("<td>" + elementorHTMLText.Replace(cell) + "</td>")
		}
		h.WriteString("</tr>")
	}
	return h.String() + "</tbody></table>", true
}

// image maps an image leaf: the library address the site resolved, size
// "large", the alignment when it is not none, and the caption.
func (b *designedBuild) image(n map[string]json.RawMessage, path string) (phpList, bool) {
	id, ok := jsonIntOf(n["attachment_id"])
	if !ok || id < 1 || id > pageCreateMaxAttachmentID {
		return nil, false
	}
	if _, ok := jsonStringOf(n["alt"]); !ok {
		return nil, false
	}
	align := "none"
	if raw, has := present(n, "align"); has {
		if align, ok = jsonStringOf(raw); !ok {
			return nil, false
		}
	}
	if !stringIn(align, elementorImageAligns) {
		return nil, false
	}
	fact, known := b.media[id]
	if !known || !pageImageURLUsable(fact.URL) {
		return nil, false
	}
	settings := phpObject{
		{"image", phpObject{{"url", fact.URL}, {"id", strconv.FormatInt(id, 10)}, {"source", "library"}}},
		{"image_size", "large"},
	}
	if align != "none" {
		settings = append(settings, phpPair{"align", align})
	}
	if raw, ok := present(n, "caption"); ok {
		caption, ok := jsonStringOf(raw)
		if !ok {
			return nil, false
		}
		settings = append(settings, phpPair{"caption_source", "custom"}, phpPair{"caption", elementorHTMLText.Replace(caption)})
	}
	return b.widget(path, "image", settings)
}

// buttons maps a buttons node of a designed section: one button per button, in
// a row container whose id is drawn before the buttons'.
func (b *designedBuild) buttons(n map[string]json.RawMessage, path string) (phpList, bool) {
	align := "left"
	if raw, has := present(n, "align"); has {
		var ok bool
		if align, ok = jsonStringOf(raw); !ok || !slices.Contains(designedRowAligns, align) {
			return nil, false
		}
	}
	list, ok := jsonArrayOf(n["buttons"])
	if !ok || len(list) == 0 {
		return nil, false
	}
	rowID, ok := b.ids.next(path)
	if !ok {
		return nil, false
	}
	widgets := phpList{}
	for j, raw := range list {
		button, ok := jsonObjectOf(raw)
		if !ok {
			return nil, false
		}
		text, ok1 := jsonStringOf(button["text"])
		url, ok2 := jsonStringOf(button["url"])
		style, ok3 := "fill", true
		if raw, has := present(button, "style"); has {
			style, ok3 = jsonStringOf(raw)
		}
		if !ok1 || !ok2 || !ok3 || !slices.Contains(designedButtonStyles, style) || strings.Contains(url, "&") {
			return nil, false
		}
		look, ok := b.look.buttonStyle(style, b.tone)
		if !ok {
			return nil, false
		}
		settings := append(phpObject{
			{"text", elementorHTMLText.Replace(text)},
			{"link", phpObject{{"url", url}, {"is_external", ""}, {"nofollow", ""}}},
		}, look...)
		w, ok := b.widget(fmt.Sprintf("%s.buttons[%d]", path, j), "button", settings)
		if !ok {
			return nil, false
		}
		widgets = append(widgets, w...)
	}
	row, ok := b.look.buttonRow(rowID, widgets, align)
	if !ok {
		return nil, false
	}
	return phpList{row}, true
}
