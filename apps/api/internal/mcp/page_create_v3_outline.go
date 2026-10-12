package mcp

import (
	"encoding/json"
	"slices"
)

// The outline grammar of a designed page (outline_version 3). It is the
// control plane's twin of the agent's OutlineV3Schema and shares one case
// table with it (page-create-v3-cases.json).
//
// A designed outline is a list of sections. A section holds the blocks of the
// version 2 grammar, columns and group included, and pattern nodes. Every
// object is closed, and per-node style is token references only, so a node
// never carries a raw value. What needs the site (the attachment of a
// background is an image, containers are on) and the text rules are the
// agent's: this side checks shapes, choices and counts.

// Limits of the version 3 grammar: the agent's OutlineV3Schema constants.
const (
	pageCreateMaxSections = 20
	pageCreateMaxHeroes   = 1
)

// pageNodeTypes are the node types of the grammar. At the top of a designed
// outline a node of one of these types that is not a section is in the wrong
// place; a node of any other type is no node of the grammar.
var pageNodeTypes = []string{
	"heading", "paragraph", "list", "image", "buttons", "quote", "separator", "spacer", "table",
	"group", "columns", "pattern",
}

// sectionChoices are the section members that take one of a closed set of
// words, and backgroundChoices the same for the members of a background.
var (
	sectionChoices = []struct {
		key    string
		values []string
	}{
		{"width", []string{"boxed", "full"}},
		{"tone", []string{"ground", "surface", "inverse", "accent"}},
		{"spacing", []string{"md", "lg", "xl"}},
		{"align", []string{"start", "center"}},
		{"min_height", []string{"auto", "screen"}},
	}
	backgroundChoices = []struct {
		key    string
		values []string
	}{
		{"overlay", []string{"none", "soft", "strong"}},
		{"focal", []string{"center", "top", "bottom"}},
	}
)

// designedNode judges a node of a designed outline by where it stands, before
// the blocks of version 2 are read. handled is false for a node that the
// version 2 rules judge as they always did.
func (w *pageWalk) designedNode(n map[string]json.RawMessage, kind string, at pagePlace) (code string, handled bool) {
	if at == pageAtTop {
		if kind == "section" {
			return w.section(n), true
		}
		if slices.Contains(pageNodeTypes, kind) {
			return pageCreateLayoutInvalid, true
		}
		return pageCreateBadInput, true
	}
	if kind == "section" {
		return pageCreateLayoutInvalid, true
	}
	return "", false
}

// section is one section: at most pageCreateMaxSections on a page, a closed
// set of members, and 1..pageCreateMaxChildren children.
func (w *pageWalk) section(n map[string]json.RawMessage) string {
	if w.sections++; w.sections > pageCreateMaxSections {
		return pageCreateLayoutInvalid
	}
	w.usesLayout = true
	if !keysExactly(n, []string{"type", "anchor", "width", "tone", "background", "spacing", "align", "min_height", "children"},
		"type", "anchor", "children") {
		return pageCreateBadInput
	}
	if !isJSONString(n["anchor"]) {
		return pageCreateBadInput
	}
	for _, c := range sectionChoices {
		if raw, has := n[c.key]; has {
			if v, ok := jsonStringOf(raw); !ok || !slices.Contains(c.values, v) {
				return pageCreateBadInput
			}
		}
	}
	if raw, has := n["background"]; has {
		if code := w.background(raw); code != "" {
			return code
		}
	}
	return w.children(n["children"], pageAtSection)
}

// background is a section's image: an attachment, which counts toward the
// images of the page, and the choices of its overlay and focal point.
func (w *pageWalk) background(raw json.RawMessage) string {
	b, ok := jsonObjectOf(raw)
	if !ok || !keysExactly(b, []string{"attachment_id", "overlay", "focal"}, "attachment_id") {
		return pageCreateBadInput
	}
	id, ok := jsonIntOf(b["attachment_id"])
	if !ok || id < 1 || id > pageCreateMaxAttachmentID {
		return pageCreateBadInput
	}
	for _, c := range backgroundChoices {
		if raw, has := b[c.key]; has {
			if v, ok := jsonStringOf(raw); !ok || !slices.Contains(c.values, v) {
				return pageCreateBadInput
			}
		}
	}
	return w.media(id)
}

// media counts one image of the page toward pageCreateMaxImages and lists its
// attachment once, in document order. A section's background comes before its
// children.
func (w *pageWalk) media(id int64) string {
	if w.images++; w.images > pageCreateMaxImages {
		return pageCreateLayoutInvalid
	}
	if _, dup := w.seen[id]; !dup {
		w.seen[id] = struct{}{}
		w.ids = append(w.ids, id)
	}
	return ""
}
