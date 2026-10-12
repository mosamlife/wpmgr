package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

// pageEditMapping compiles new nodes with the same seed and design as mapFragment.
// Image addresses and host colours are site facts read from the answer; all
// other settings and the order of keys and node ids come from the input.
type pageEditMapping struct {
	designed   *designedBuild
	isDesigned bool
}

func newPageEditMapping(resp agentcmd.AbilityRunResponse, f pageEditFacts, elements map[string]*treeElement, changes []pageEditChange, proj *projection) (*pageEditMapping, bool) {
	m := &pageEditMapping{}
	designed := f.design != nil
	for _, n := range proj.nodes {
		designed = designed || n.kind == "pattern" || n.kind == "section" && n.anchor != ""
	}
	if !designed {
		return m, true
	}
	m.isDesigned = true
	breakpoints, ok := precheckActiveBreakpoints(resp.Raw)
	if !ok {
		return nil, false
	}
	if f.design == nil || !f.addsNodes() {
		return m, true
	}
	tokens, err := newDesignTokens(*f.design)
	if err != nil {
		return nil, false
	}
	newIDs := map[string]bool{}
	for _, c := range changes {
		for _, id := range c.NewRefs {
			newIDs[id] = true
		}
	}
	ids := &elementorIDs{requestID: resp.RequestID, taken: map[string]bool{}}
	for id := range elements {
		if !newIDs[id] {
			ids.taken[id] = true
		}
	}
	for _, op := range f.ops {
		for _, id := range op.refs() {
			ids.taken[id] = true
		}
	}
	media, ok := pageEditTreeMedia(elements, newIDs)
	if !ok {
		return nil, false
	}
	sections := newDesignSections(tokens, breakpoints, media)
	m.designed = &designedBuild{ids: ids, sections: sections, look: designLeafStyle{tokens: tokens}, media: sections.media, patterns: designedPatternBuilders}
	return m, true
}

// page-edit's preview has no media ledger. The address paired with an
// attachment id in the tree is the site's resolved address for that image.
func pageEditTreeMedia(elements map[string]*treeElement, newIDs map[string]bool) ([]pageMediaFact, bool) {
	urls := map[int64]string{}
	for ref := range newIDs {
		e := elements[ref]
		if e == nil {
			continue
		}
		settings, _ := phpGet(e.node, "settings")
		for _, key := range []string{"image", "background_image", "testimonial_image"} {
			control, has := phpGet(settings, key)
			if !has {
				continue
			}
			idRaw, _ := phpGet(control, "id")
			if url, has := phpStringAt(control, "url"); has {
				var id string
				switch raw := idRaw.(type) {
				case string:
					id = raw
				case json.Number:
					id = string(raw)
				default:
					return nil, false
				}
				n, err := strconv.ParseInt(id, 10, 64)
				if err != nil || n < 1 {
					return nil, false
				}
				if old, seen := urls[n]; seen && old != url {
					return nil, false
				}
				urls[n] = url
			}
		}
	}
	media := make([]pageMediaFact, 0, len(urls))
	for id, url := range urls {
		// MIME is not encoded in the tree. The site prechecks image type;
		// the twin needs an image MIME to compile, and writes no MIME setting.
		media = append(media, pageMediaFact{ID: id, URL: url, Mime: "image/png"})
	}
	return media, true
}

func (m *pageEditMapping) matches(op pageEditOp, i int, context string, containers bool, got phpList, tree any, path []int) bool {
	b := m.designed
	if !containers {
		return false
	}
	tone := "ground"
	if len(path) > 1 {
		host := phpValues(tree)[path[0]]
		settings, _ := phpGet(host, "settings")
		colour, _ := phpStringAt(settings, "background_color")
		for _, name := range designedTones {
			colours, err := b.sections.tokens.tone(name)
			if err == nil && colour == colours.Background {
				tone = name
				break
			}
		}
	}
	listPath := fmt.Sprintf("operations[%d].outline", i)
	var want phpList
	var ok bool
	switch context {
	case fragmentTop:
		hasSection := false
		for _, raw := range op.Outline {
			n, _ := jsonObjectOf(raw)
			kind, _ := jsonStringOf(n["type"])
			hasSection = hasSection || kind == "section"
		}
		if hasSection {
			want, ok = b.page(op.Outline, listPath)
		} else {
			want, ok = m.plain().top(op.Outline, listPath)
		}
	case fragmentGroup:
		want, ok = b.children(op.Outline, listPath, tone, "start")
	case fragmentInner:
		// mapFragment's inner context uses unstyled leaves, even with a design.
		plain := m.plain()
		run := make([]elementorLeafAt, 0, len(op.Outline))
		for j, raw := range op.Outline {
			n, valid := jsonObjectOf(raw)
			if !valid {
				return false
			}
			run = append(run, elementorLeafAt{path: fmt.Sprintf("%s[%d]", listPath, j), node: n})
		}
		want, ok = plain.leafList(run)
	}
	if !ok {
		return false
	}
	ours, ok1 := phpEncodeBytes(want)
	theirs, ok2 := phpEncodeBytes(got)
	return ok1 && ok2 && bytes.Equal(ours, theirs)
}

func (m *pageEditMapping) plain() *elementorBuild {
	b := m.designed
	plain := &elementorBuild{ids: b.ids, urls: map[int64]string{}, containers: true, sections: b.sections}
	for id, fact := range b.media {
		plain.urls[id] = fact.URL
	}
	return plain
}

// setText and setPatternText store the escaped value at its exact setting path.
func pageEditSetTextBytes(op pageEditOp, elements map[string]*treeElement, designed bool) bool {
	if elements[op.Ref] == nil {
		return true
	}
	node := elements[op.Ref].node
	settings, _ := phpGet(node, "settings")
	if !pageEditPathPattern.MatchString(op.Field) {
		if !designed {
			return true
		}
		typ, _ := phpStringAt(node, "widgetType")
		var stored any
		value := elementorHTMLText.Replace(op.Text)
		switch {
		case typ == "heading" && op.Field == "text":
			stored, _ = phpGet(settings, "title")
		case typ == "text-editor" && op.Field == "text":
			stored, _ = phpGet(settings, "editor")
			value = "<p>" + value + "</p>"
		case typ == "button" && op.Field == "text":
			stored, _ = phpGet(settings, "text")
		case typ == "button" && op.Field == "url":
			link, _ := phpGet(settings, "link")
			stored, _ = phpGet(link, "url")
			value = op.Text
		case typ == "image" && op.Field == "caption":
			stored, _ = phpGet(settings, "caption")
		default:
			return false
		}
		return stored == value
	}
	name, variant := elementorPatternOf(settings)
	fields, ok := elementorPatternFields(node, name, variant)
	if !ok {
		return false
	}
	for _, field := range fields {
		if field.field != op.Field {
			continue
		}
		value := op.Text
		switch field.form {
		case "number":
			if !elementorPatternNumber.MatchString(value) {
				return false
			}
		case "url":
		default:
			value = elementorHTMLText.Replace(value)
			if field.form == "paragraph" {
				value = "<p>" + value + "</p>"
			}
		}
		stored, _ := phpGet(field.widget, "settings")
		for _, step := range field.at {
			stored, _ = phpGet(stored, step)
		}
		return stored == value
	}
	return false
}

// LayoutOps puts an insert or move directly beside its anchor, or at the
// first or last child slot. New refs are listed in depth-first tree order.
func pageEditPlacementMatches(c pageEditChange, op pageEditOp, elements map[string]*treeElement) bool {
	if op.Op != pageEditOpInsert && op.Op != pageEditOpMove {
		return true
	}
	anchor := elements[op.AnchorRef]
	if anchor == nil {
		return true
	} // A later operation took its parent off.
	var tops []*treeElement
	if op.Op == pageEditOpMove {
		if elements[op.Ref] == nil {
			return false
		}
		tops = []*treeElement{elements[op.Ref]}
	} else {
		set := map[string]bool{}
		for _, id := range c.NewRefs {
			set[id] = true
		}
		for _, id := range c.NewRefs {
			e := elements[id]
			if e == nil {
				return false
			}
			if !set[e.parent] {
				tops = append(tops, e)
			}
		}
		sortTops(tops)
		var ordered []string
		var walk func(any)
		walk = func(node any) {
			id, _ := phpStringAt(node, "id")
			ordered = append(ordered, id)
			children, _ := phpGet(node, "elements")
			for _, child := range phpValues(children) {
				walk(child)
			}
		}
		for _, top := range tops {
			walk(top.node)
		}
		if !reflect.DeepEqual(ordered, c.NewRefs) {
			return false
		}
	}
	if len(tops) == 0 {
		return false
	}
	first, last := tops[0], tops[len(tops)-1]
	index := func(e *treeElement) int { return e.path[len(e.path)-1] }
	switch op.Anchor {
	case pageEditAnchorInto:
		if first.parent != op.AnchorRef || last.parent != op.AnchorRef {
			return false
		}
		position := op.Position
		if position == "" {
			position = pageEditPositionLast
		}
		if position == pageEditPositionFirst {
			return index(first) == 0
		}
		children, _ := phpGet(anchor.node, "elements")
		return index(last) == len(phpValues(children))-1
	case "before":
		return first.parent == anchor.parent && last.parent == anchor.parent && index(last)+1 == index(anchor)
	case "after":
		return first.parent == anchor.parent && last.parent == anchor.parent && index(first) == index(anchor)+1
	}
	return false
}
