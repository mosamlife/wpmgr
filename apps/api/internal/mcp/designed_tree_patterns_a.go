package mcp

// The patterns hero, cta, stats and logos of a designed page, as Elementor
// containers: the twin of the agent's ElementorPatternCompiler for those four.
//
// A pattern is a child of a section, compiled to one labelled inner container:
// the Navigator label (LeafPolicy::LABEL_KEY, "_title") is the label prefix, the
// pattern's name and its variant, after the container's last key.
//
//	hero background   a group of the hero's blocks, centred
//	hero centered     the same
//	hero split        a columns row: the copy in the 55 percent column on the
//	                  left, the pictures in the 45 percent column on the right;
//	                  a hero with no picture, or no copy, is the stacked group
//	cta band          a group of the call to action's blocks, centred
//	stats counters    a columns row of equal columns, one counter in each
//	logos row         one row that wraps, centred, holding the logos' image
//	                  widgets
//
// The blocks of a hero or a call to action are built first, as a section's
// leaves are, in the tone of the section and the alignment of the variant (the
// pattern lays its blocks out, so the section's own alignment does not reach
// them); a centred pattern also centres each row of buttons that does not say
// where it stands. Node ids are drawn in the agent's order: a composed pattern's
// blocks, then its container, then the two columns of a split; a stats row, then
// for each counter its column and its counter; a logos row, then its logos.

import (
	"encoding/json"
	"fmt"
	"strconv"
)

const (
	// designedLogoWidthPx is the width of a logo, in px.
	designedLogoWidthPx = 140

	// designedSplitCopyPercent and designedSplitMediaPercent are the widths of the
	// copy column and of the picture column of a split hero, in percent.
	designedSplitCopyPercent  = 55
	designedSplitMediaPercent = 45
)

func init() {
	designedPatternBuilders["hero"] = func(b *designedBuild, n map[string]json.RawMessage, path, tone string) (phpObject, bool) {
		return designedPatternsA{b: b}.hero(n, path, tone)
	}
	designedPatternBuilders["cta"] = func(b *designedBuild, n map[string]json.RawMessage, path, tone string) (phpObject, bool) {
		return designedPatternsA{b: b}.cta(n, path, tone)
	}
	designedPatternBuilders["stats"] = func(b *designedBuild, n map[string]json.RawMessage, path, tone string) (phpObject, bool) {
		return designedPatternsA{b: b}.stats(n, path, tone)
	}
	designedPatternBuilders["logos"] = func(b *designedBuild, n map[string]json.RawMessage, path, tone string) (phpObject, bool) {
		return designedPatternsA{b: b}.logos(n, path)
	}
}

// designedPatternsA compiles the patterns of this file for one page build.
type designedPatternsA struct {
	b *designedBuild
}

// variant is the variant of a pattern node of the given name: the node names
// the pattern and one of that pattern's variants.
func (designedPatternsA) variant(n map[string]json.RawMessage, name string) (string, bool) {
	pattern, _ := jsonStringOf(n["pattern"])
	variant, ok := jsonStringOf(n["variant"])
	if pattern != name || !ok || !stringIn(variant, patternSpecs[name].variants) {
		return "", false
	}
	return variant, true
}

// label is the Navigator label of a pattern's container.
func (designedPatternsA) label(name, variant string) string {
	const prefix = "WPMgr · "
	titles := map[string]string{"hero": "Hero", "cta": "CTA", "stats": "Stats", "logos": "Logos"}
	return prefix + titles[name] + " " + variant
}

// align is the alignment the blocks of a composed pattern are built in: centre
// for a hero over a picture, a centred hero and a call to action, start for a
// split hero.
func (designedPatternsA) align(name, variant string) (string, bool) {
	switch name + " " + variant {
	case "hero background", "hero centered", "cta band":
		return "center", true
	case "hero split":
		return "start", true
	}
	return "", false
}

// member is the value of a key of a built node, nil when it has none.
func (designedPatternsA) member(o phpObject, key string) any {
	for _, pair := range o {
		if pair.key == key {
			return pair.value
		}
	}
	return nil
}

// withSetting is a container with a setting written the way PHP assigns an array
// key: replaced in its place when it is there, after the last key when it is not.
func (designedPatternsA) withSetting(container phpObject, key string, value any) phpObject {
	out := make(phpObject, len(container))
	copy(out, container)
	for i, pair := range out {
		if pair.key != "settings" {
			continue
		}
		settings, _ := pair.value.(phpObject)
		next := make(phpObject, len(settings), len(settings)+1)
		copy(next, settings)
		replaced := false
		for j := range next {
			if next[j].key == key {
				next[j].value = value
				replaced = true
			}
		}
		if !replaced {
			next = append(next, phpPair{key, value})
		}
		out[i].value = next
	}
	return out
}

// labelled is a container with its Navigator label after the last key.
func (p designedPatternsA) labelled(container phpObject, label string) phpObject {
	return p.withSetting(container, "_title", label)
}

// stack is the blocks in one full-width column, labelled.
func (p designedPatternsA) stack(id, label string, elements phpList) phpObject {
	return p.labelled(p.b.sections.group(id, elements), label)
}

// centred is the elements with each row of buttons that has no justification
// of its own centred: the key follows the row keys the leaf compiler wrote.
func (p designedPatternsA) centred(elements phpList) phpList {
	out := make(phpList, len(elements))
	copy(out, elements)
	for i, element := range elements {
		container, ok := element.(phpObject)
		if !ok || p.member(container, "elType") != "container" {
			continue
		}
		children, _ := p.member(container, "elements").(phpList)
		settings, _ := p.member(container, "settings").(phpObject)
		if len(children) == 0 || p.member(settings, "flex_justify_content") != nil || !p.allButtons(children) {
			continue
		}
		out[i] = p.withSetting(container, "flex_justify_content", "center")
	}
	return out
}

// allButtons: every child is a button widget.
func (p designedPatternsA) allButtons(children phpList) bool {
	for _, child := range children {
		widget, ok := child.(phpObject)
		if !ok || p.member(widget, "widgetType") != "button" {
			return false
		}
	}
	return true
}

// composed are the blocks of a hero or a call to action, built in the
// alignment of the variant.
func (p designedPatternsA) composed(n map[string]json.RawMessage, name, variant, path, tone string) (phpList, bool) {
	align, ok := p.align(name, variant)
	leaves, isList := jsonArrayOf(n["children"])
	if !ok || !isList {
		return nil, false
	}
	return p.b.blocks(leaves, tone, align, path+".children")
}

// hero is a hero around its built blocks.
func (p designedPatternsA) hero(n map[string]json.RawMessage, path, tone string) (phpObject, bool) {
	variant, ok := p.variant(n, "hero")
	if !ok {
		return nil, false
	}
	elements, ok := p.composed(n, "hero", variant, path, tone)
	if !ok {
		return nil, false
	}
	id, ok := p.b.ids.next(path)
	if !ok {
		return nil, false
	}
	label := p.label("hero", variant)
	if variant == "split" {
		return p.split(path, id, label, elements)
	}
	return p.stack(id, label, p.centred(elements)), true
}

// split is a split hero: the copy on the left and the pictures on the right, in
// a labelled row. Without a picture, or without copy, it is the stacked group.
func (p designedPatternsA) split(path, id, label string, elements phpList) (phpObject, bool) {
	var copyElements, mediaElements phpList
	for _, element := range elements {
		widget, _ := element.(phpObject)
		if p.member(widget, "widgetType") == "image" {
			mediaElements = append(mediaElements, element)
		} else {
			copyElements = append(copyElements, element)
		}
	}
	if len(copyElements) == 0 || len(mediaElements) == 0 {
		return p.stack(id, label, elements), true
	}
	copyID, ok := p.b.ids.next(path + "#copy")
	if !ok {
		return nil, false
	}
	copyColumn, ok := p.b.sections.column(copyID, designedSplitCopyPercent, copyElements)
	if !ok {
		return nil, false
	}
	mediaID, ok := p.b.ids.next(path + "#media")
	if !ok {
		return nil, false
	}
	mediaColumn, ok := p.b.sections.column(mediaID, designedSplitMediaPercent, mediaElements)
	if !ok {
		return nil, false
	}
	return p.labelled(p.b.sections.row(id, phpList{copyColumn, mediaColumn}), label), true
}

// cta is a call to action around its built blocks.
func (p designedPatternsA) cta(n map[string]json.RawMessage, path, tone string) (phpObject, bool) {
	variant, ok := p.variant(n, "cta")
	if !ok {
		return nil, false
	}
	elements, ok := p.composed(n, "cta", variant, path, tone)
	if !ok {
		return nil, false
	}
	id, ok := p.b.ids.next(path)
	if !ok {
		return nil, false
	}
	return p.stack(id, p.label("cta", variant), p.centred(elements)), true
}

// items are the items of an item pattern, counted by the grammar's caps.
func (designedPatternsA) items(n map[string]json.RawMessage, name string) ([]json.RawMessage, bool) {
	spec := patternSpecs[name]
	items, ok := jsonArrayOf(n[spec.array])
	if !ok || len(items) < spec.min || len(items) > spec.max {
		return nil, false
	}
	return items, true
}

// stats is a row of equal columns, one counter in each. A counter counts from 0
// to its value, then shows the suffix, with its label under it; the number is
// in the tone's heading colour and the label in its accent colour.
func (p designedPatternsA) stats(n map[string]json.RawMessage, path, tone string) (phpObject, bool) {
	variant, ok := p.variant(n, "stats")
	if !ok {
		return nil, false
	}
	items, ok := p.items(n, "stats")
	if !ok {
		return nil, false
	}
	colours, err := p.b.sections.tokens.tone(tone)
	if err != nil {
		return nil, false
	}
	rowID, ok := p.b.ids.next(path)
	if !ok {
		return nil, false
	}
	width := 100 / len(items)
	columns := make(phpList, 0, len(items))
	for i, item := range items {
		at := fmt.Sprintf("%s.stats[%d]", path, i)
		columnID, ok := p.b.ids.next(at + "#column")
		if !ok {
			return nil, false
		}
		counterID, ok := p.b.ids.next(at)
		if !ok {
			return nil, false
		}
		counter, ok := p.counter(counterID, item, colours)
		if !ok {
			return nil, false
		}
		column, ok := p.b.sections.column(columnID, width, phpList{counter})
		if !ok {
			return nil, false
		}
		columns = append(columns, column)
	}
	return p.labelled(p.b.sections.row(rowID, columns), p.label("stats", variant)), true
}

// counter is one counter widget, in the key order Elementor returns.
func (designedPatternsA) counter(id string, raw json.RawMessage, colours designTone) (phpObject, bool) {
	item, ok := jsonObjectOf(raw)
	if !ok {
		return nil, false
	}
	value, okValue := jsonIntOf(item["value"])
	label, okLabel := jsonStringOf(item["label"])
	if !okValue || value < 0 || value > pageCreateMaxStatValue || !okLabel || label == "" {
		return nil, false
	}
	settings := phpObject{{"starting_number", "0"}, {"ending_number", strconv.FormatInt(value, 10)}}
	if rawSuffix, has := present(item, "suffix"); has {
		suffix, ok := jsonStringOf(rawSuffix)
		if !ok || suffix == "" {
			return nil, false
		}
		settings = append(settings, phpPair{"suffix", elementorHTMLText.Replace(suffix)})
	}
	settings = append(settings,
		phpPair{"title", elementorHTMLText.Replace(label)},
		phpPair{"number_color", colours.Heading},
		phpPair{"title_color", colours.Accent},
	)
	return phpObject{{"id", id}, {"elType", "widget"}, {"settings", settings}, {"elements", phpList{}}, {"widgetType", "counter"}}, true
}

// logos is the library images in one row that wraps, centred, never stacked,
// with the design's row gap between logos at each step. A logo's alt text is the
// library's, which the page-create check holds the outline to before the page
// is compiled.
func (p designedPatternsA) logos(n map[string]json.RawMessage, path string) (phpObject, bool) {
	variant, ok := p.variant(n, "logos")
	if !ok {
		return nil, false
	}
	items, ok := p.items(n, "logos")
	if !ok {
		return nil, false
	}
	id, ok := p.b.ids.next(path)
	if !ok {
		return nil, false
	}
	settings := phpObject{{"content_width", "full"}, {"flex_direction", "row"}, {"flex_wrap", "wrap"}}
	settings = append(settings, p.b.sections.gapSettings()...)
	settings = append(settings,
		phpPair{"flex_justify_content", "center"},
		phpPair{"flex_align_items", "center"},
		phpPair{"_title", p.label("logos", variant)},
	)
	logos := make(phpList, 0, len(items))
	for k, item := range items {
		logoID, ok := p.b.ids.next(fmt.Sprintf("%s.logos[%d]", path, k))
		if !ok {
			return nil, false
		}
		logo, ok := p.logo(logoID, item)
		if !ok {
			return nil, false
		}
		logos = append(logos, logo)
	}
	return designedInner(id, settings, logos), true
}

// logo is one logo: the library image, medium size, centred, designedLogoWidthPx
// wide.
func (p designedPatternsA) logo(id string, raw json.RawMessage) (phpObject, bool) {
	item, _ := jsonObjectOf(raw)
	image, ok := p.libraryImage(item["attachment_id"])
	if !ok {
		return nil, false
	}
	settings := phpObject{
		{"image", image},
		{"image_size", "medium"},
		{"align", "center"},
		{"width", phpObject{{"unit", "px"}, {"size", strconv.Itoa(designedLogoWidthPx)}}},
	}
	return phpObject{{"id", id}, {"elType", "widget"}, {"settings", settings}, {"elements", phpList{}}, {"widgetType", "image"}}, true
}

// libraryImage is an image of the media library as a media control stores it:
// one the site resolved before the page is compiled.
func (p designedPatternsA) libraryImage(raw json.RawMessage) (phpObject, bool) {
	id, ok := jsonIntOf(raw)
	if !ok || id < 1 || id > pageCreateMaxAttachmentID {
		return nil, false
	}
	fact, known := p.b.media[id]
	if !known || fact.ID != id || !pageImageURLUsable(fact.URL) {
		return nil, false
	}
	if _, ok := pageImageMimes[fact.Mime]; !ok {
		return nil, false
	}
	return phpObject{{"url", fact.URL}, {"id", strconv.FormatInt(id, 10)}, {"source", "library"}}, true
}
