package mcp

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
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
	pageCreateMaxH1       = 1
)

// Refusal codes of the version 3 grammar that no version 2 input meets.
const (
	pageCreateItemsOutOfRange = "pattern_items_out_of_range"
	pageCreateIconUnknown     = "icon_unknown"
)

// Refusal codes of the anchors, links and places of a designed page.
const (
	pageCreateAnchorInvalid     = "anchor_invalid"
	pageCreateAnchorDuplicate   = "anchor_duplicate"
	pageCreateAnchorReserved    = "anchor_reserved"
	pageCreateLinkAnchorMissing = "link_anchor_missing"
	pageCreateHeroMisplaced     = "hero_misplaced"
	pageCreateH1OutsideHero     = "h1_outside_hero"
)

// The fixed hints of those codes (precheckRefusalHints): what to change, in
// WPMgr's words, for a refusal whose failures name where.
const (
	hintAnchorInvalid = "Every section needs an anchor, the id its links point at. Write it with 2 to 40 characters: " +
		"a lower-case letter first, then lower-case letters, digits or hyphens (for example how-it-works). " +
		"The failure names the section."
	hintAnchorDuplicate = "Two sections have the same anchor, and a link to an anchor can lead to only one of them. " +
		"Give each section its own anchor: the failure names the section that repeats one."
	hintLinkAnchorMissing = "A button points at a section anchor the page does not have. Link with # and the anchor of a section " +
		"(the failure lists the anchors the page has), or add the section the button should lead to."
	hintHeroMisplaced = "The hero opens the page: it is the first block of the first section, and a page has one. " +
		"Move it there, or leave the hero out and start with another block. The failure names where it stands now."
	hintH1OutsideHero = "A level 1 heading is the title of the page, and a designed page has one, inside its hero. " +
		"Use level 2 for any heading outside the hero. The failure names the heading."
)

// hintAnchorReserved and the hints after it list what the grammar accepts,
// built from the grammar's own lists so that the hint cannot drift from them.
var (
	hintAnchorReserved = "These ids belong to a theme or to WordPress and cannot be the anchor of a section: " +
		strings.Join(pageReservedAnchors, ", ") + ". Choose another anchor for the section the failure names."
	hintItemsOutOfRange = "A pattern holds more or fewer items than it takes: " + patternItemCapsText() +
		fmt.Sprintf(", and a pricing plan lists 1 to %d features. ", pageCreateMaxPlanBullets) +
		"The failure names the list and the object that holds it."
	hintIconUnknown = "An icon must be the name of one of these: " + strings.Join(pageIcons, ", ") +
		". The failures name each icon that is not in the list."
)

// patternItemCapsText is the item counts of the item patterns, in the order
// a page usually shows them.
func patternItemCapsText() string {
	var caps []string
	for _, name := range []string{"features", "testimonials", "pricing", "faq", "logos", "stats"} {
		s := patternSpecs[name]
		caps = append(caps, fmt.Sprintf("%s %d to %d", s.array, s.min, s.max))
	}
	return strings.Join(caps, ", ")
}

// pageAnchorPattern is the shape of a section anchor, and of the slug of a
// link to one: the agent's OutlineV3Schema::ANCHOR_PATTERN. Go's $ matches at
// the end of the text only.
var pageAnchorPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,39}$`)

// pageReservedAnchors are ids a theme already owns: the agent's
// OutlineV3Schema::RESERVED_ANCHORS.
var pageReservedAnchors = []string{
	"content", "main", "primary", "page", "masthead", "colophon", "site-header", "site-footer", "wpadminbar",
}

// The styles a button may take: version 2 has two, a designed page adds text.
var (
	pageButtonStyles         = []string{"fill", "outline"}
	pageButtonStylesDesigned = []string{"fill", "outline", "text"}
)

// Limits of the pattern nodes: the agent's OutlineV3Schema constants.
const (
	pageCreateMaxHeroChildren = 6
	pageCreateMaxCTAChildren  = 4
	pageCreateMaxPlanBullets  = 8
	pageCreateMaxStatValue    = 999999999
)

// pageIcons is the closed list of icon names a feature may show; the agent's
// OutlineV3Schema::ICONS, which the shared case table repeats.
var pageIcons = []string{
	"arrow-right", "award", "bell", "bolt", "book", "briefcase", "calendar", "chart", "check", "clock",
	"cloud", "code", "cog", "comment", "compass", "credit-card", "cube", "database", "envelope", "eye",
	"flag", "gift", "globe", "heart", "home", "image", "key", "laptop", "layers", "leaf",
	"lightbulb", "lock", "map-pin", "phone", "rocket", "search", "shield", "star", "tag", "users",
}

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
	here := w.where
	if at == pageAtTop {
		if kind == "section" {
			return w.section(n), true
		}
		if slices.Contains(pageNodeTypes, kind) {
			return w.refuse(pageCreateLayoutInvalid, here, "type", "every top-level item is a section; put other blocks inside a section", "section"), true
		}
		return w.refuse(pageCreateBadInput, here, "type", "is not a block of the outline grammar", pageNodeTypes...), true
	}
	if kind == "section" {
		return w.refuse(pageCreateLayoutInvalid, here, "type", "a section is a top-level item; it cannot sit inside a section"), true
	}
	if blocks := patternBlocksAt(at); blocks != nil && slices.Contains(pageNodeTypes, kind) && !slices.Contains(blocks, kind) {
		return w.refuse(pageCreateLayoutInvalid, here, "type", "a "+patternNameAt(at)+" holds only "+strings.Join(blocks, ", "), blocks...), true
	}
	if kind == "pattern" {
		return w.pattern(n, at), true
	}
	return "", false
}

// section is one section: a closed set of members, an anchor that is its own
// on the page, and 1..pageCreateMaxChildren children. The page holds at most
// pageCreateMaxSections of them, which is judged before any is read.
func (w *pageWalk) section(n map[string]json.RawMessage) string {
	here := w.where
	w.sections++
	w.usesLayout = true
	if code := w.members(here, "a section", n,
		[]string{"type", "anchor", "width", "tone", "background", "spacing", "align", "min_height", "children"},
		"type", "anchor", "children"); code != "" {
		return code
	}
	anchor, ok := jsonStringOf(n["anchor"])
	if !ok {
		return w.refuse(pageCreateBadInput, here, "anchor", "must be a string")
	}
	if code := w.anchor(anchor, here); code != "" {
		return code
	}
	for _, c := range sectionChoices {
		if raw, has := n[c.key]; has {
			if v, ok := jsonStringOf(raw); !ok || !slices.Contains(c.values, v) {
				return w.refuse(pageCreateBadInput, here, c.key, "must be one of the allowed values", c.values...)
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
	section := w.where
	where := section + ".background"
	b, ok := jsonObjectOf(raw)
	if !ok {
		return w.refuse(pageCreateBadInput, section, "background", "must be an object with an attachment_id, and an overlay and a focal point if wanted")
	}
	if code := w.members(where, "a background", b, []string{"attachment_id", "overlay", "focal"}, "attachment_id"); code != "" {
		return code
	}
	id, ok := jsonIntOf(b["attachment_id"])
	if !ok || id < 1 || id > pageCreateMaxAttachmentID {
		return w.refuse(pageCreateBadInput, where, "attachment_id", "must be a whole number from 1 to "+strconv.Itoa(pageCreateMaxAttachmentID))
	}
	for _, c := range backgroundChoices {
		if raw, has := b[c.key]; has {
			if v, ok := jsonStringOf(raw); !ok || !slices.Contains(c.values, v) {
				return w.refuse(pageCreateBadInput, where, c.key, "must be one of the allowed values", c.values...)
			}
		}
	}
	return w.media(id, where, "attachment_id")
}

// media counts one image of the page toward pageCreateMaxImages and lists its
// attachment once, in document order. A section's background comes before its
// children. where and key are the object and the field that name the image.
func (w *pageWalk) media(id int64, where, key string) string {
	if w.images++; w.images > pageCreateMaxImages {
		return w.refuse(pageCreateLayoutInvalid, where, key,
			fmt.Sprintf("a page shows at most %d images, section backgrounds and pattern pictures included", pageCreateMaxImages))
	}
	if _, dup := w.seen[id]; !dup {
		w.seen[id] = struct{}{}
		w.ids = append(w.ids, id)
	}
	return ""
}

// anchor judges the anchor of a section and keeps it: it has the shape of an
// anchor, is not an id the theme owns, and is not one an earlier section took.
// where is the address of the section.
func (w *pageWalk) anchor(a, where string) string {
	if !pageAnchorPattern.MatchString(a) {
		return w.refuse(pageCreateAnchorInvalid, where, "anchor",
			"must be 2 to 40 characters: a lower-case letter, then lower-case letters, digits or hyphens")
	}
	if slices.Contains(pageReservedAnchors, a) {
		return w.refuse(pageCreateAnchorReserved, where, "anchor", "is an id the theme or WordPress already uses; choose another", pageReservedAnchors...)
	}
	if first, taken := w.anchors[a]; taken {
		return w.refuse(pageCreateAnchorDuplicate, where, "anchor", "is the anchor of "+first+" already; give each section its own")
	}
	if w.anchors == nil {
		w.anchors = map[string]string{}
	}
	w.anchors[a] = where
	w.anchorList = append(w.anchorList, a)
	return ""
}

// linkCode is the refusal of a link target, "" when the grammar accepts it.
// The buttons of a page and the calls to action of a plan share it; where is
// the address of the object that holds the link, whose field is url. A
// designed page may also point at one of its own sections, with "#" and the
// anchor; the target is kept and judged by resolveLinks once the whole outline
// is read, so a section further down resolves it.
func (w *pageWalk) linkCode(url, where string) string {
	if w.designed && strings.HasPrefix(url, "#") {
		slug := url[1:]
		if !pageAnchorPattern.MatchString(slug) {
			return w.refuse(pageCreateLinkInvalid, where, "url",
				"must be # and a section anchor: a lower-case letter, then 1 to 39 lower-case letters, digits or hyphens")
		}
		w.links = append(w.links, pageLink{slug: slug, where: where})
		return ""
	}
	if !pageLinkUsable(url) {
		reason := "is not a link the page can carry: use an https:// address or a path on this site that starts with /"
		if w.designed {
			reason += ", or # and the anchor of a section"
		}
		return w.refuse(pageCreateLinkInvalid, where, "url", reason)
	}
	return ""
}

// resolveLinks is the refusal of the links to an anchor the page does not
// have, judged after every section is read. Every such link is listed, in the
// order of the page, each with the anchors the page does have.
func (w *pageWalk) resolveLinks() string {
	missing := false
	for _, l := range w.links {
		if _, ok := w.anchors[l.slug]; !ok {
			missing = true
			w.fail(l.where, "url", "leads to a section anchor the page does not have; use # and one of the anchors listed, or add that section", w.anchorList...)
		}
	}
	if missing {
		return pageCreateLinkAnchorMissing
	}
	return ""
}

// h1 judges a level 1 heading, of which a designed page has one, inside its
// hero. A version 2 outline has no level 1 heading.
func (w *pageWalk) h1(at pagePlace) string {
	here := w.where
	if !w.designed {
		return w.refuse(pageCreateBadInput, here, "level", w.levelReason())
	}
	if at != pageAtHero {
		return w.refuse(pageCreateH1OutsideHero, here, "level",
			"a level 1 heading is the title of the page and stands in the hero; use level 2 here")
	}
	if w.h1s++; w.h1s > pageCreateMaxH1 {
		return w.refuse(pageCreateLayoutInvalid, here, "level", "a page has one level 1 heading, in the hero; use level 2 here")
	}
	return ""
}

// buttonStyles are the styles a button of this outline may take.
func (w *pageWalk) buttonStyles() []string {
	if w.designed {
		return pageButtonStylesDesigned
	}
	return pageButtonStyles
}

// ---------------------------------------------------------------------------
// Patterns
// ---------------------------------------------------------------------------

// patternFieldKind is how the value of an item field is judged.
type patternFieldKind int

const (
	fieldText    patternFieldKind = iota // a string; its text rules are the agent's
	fieldIcon                            // a name of pageIcons
	fieldImage                           // an attachment, counted as an image of the page
	fieldFlag                            // true or false
	fieldCount                           // a whole number, 0..pageCreateMaxStatValue
	fieldBullets                         // 1..pageCreateMaxPlanBullets strings
	fieldLink                            // {text, url}, the url judged as a button's is
)

type patternField struct {
	key      string
	kind     patternFieldKind
	required bool
}

// patternSpec is one pattern. A pattern that holds blocks (hero, cta) names
// them, their most and the place they stand at; any other keeps items in one
// array, counted by min and max and shaped by fields.
type patternSpec struct {
	variants    []string
	blocks      []string
	maxChildren int
	place       pagePlace
	array       string
	min, max    int
	fields      []patternField
}

var patternSpecs = map[string]patternSpec{
	"hero": {
		variants: []string{"background", "split", "centered"},
		blocks:   []string{"heading", "paragraph", "buttons", "image"}, maxChildren: pageCreateMaxHeroChildren, place: pageAtHero,
	},
	"cta": {
		variants: []string{"band"},
		blocks:   []string{"heading", "paragraph", "buttons"}, maxChildren: pageCreateMaxCTAChildren, place: pageAtCTA,
	},
	"features": {
		variants: []string{"split", "bento", "grid"}, array: "features", min: 3, max: 8,
		fields: []patternField{{"icon", fieldIcon, true}, {"title", fieldText, true}, {"text", fieldText, true}},
	},
	"testimonials": {
		variants: []string{"featured"}, array: "testimonials", min: 1, max: 6,
		fields: []patternField{{"quote", fieldText, true}, {"name", fieldText, true}, {"role", fieldText, false}, {"attachment_id", fieldImage, false}},
	},
	"pricing": {
		variants: []string{"three-plans"}, array: "plans", min: 1, max: 4,
		fields: []patternField{
			{"name", fieldText, true}, {"price", fieldText, true}, {"period", fieldText, false},
			{"features", fieldBullets, true}, {"cta", fieldLink, true}, {"highlighted", fieldFlag, false},
		},
	},
	"faq": {
		variants: []string{"accordion"}, array: "faq", min: 1, max: 12,
		fields: []patternField{{"question", fieldText, true}, {"answer", fieldText, true}},
	},
	"logos": {
		variants: []string{"row"}, array: "logos", min: 3, max: 10,
		fields: []patternField{{"attachment_id", fieldImage, true}, {"alt", fieldText, true}},
	},
	"stats": {
		variants: []string{"counters"}, array: "stats", min: 2, max: 4,
		fields: []patternField{{"value", fieldCount, true}, {"suffix", fieldText, false}, {"label", fieldText, true}},
	},
}

// patternBlocksAt is the node types a hero or a cta may hold at place at, nil
// for any other place.
func patternBlocksAt(at pagePlace) []string {
	switch at {
	case pageAtHero:
		return patternSpecs["hero"].blocks
	case pageAtCTA:
		return patternSpecs["cta"].blocks
	}
	return nil
}

// patternNameAt is the pattern whose blocks stand at place at.
func patternNameAt(at pagePlace) string {
	if at == pageAtHero {
		return "hero"
	}
	return "cta"
}

// pattern is a pattern node: a child of a section, closed, with a name and a
// variant from the lists and either children or its array of items.
func (w *pageWalk) pattern(n map[string]json.RawMessage, at pagePlace) string {
	here := w.where
	if at != pageAtSection {
		return w.refuse(pageCreateLayoutInvalid, here, "type", "a pattern is a child of a section; it cannot sit here")
	}
	name, ok := jsonStringOf(n["pattern"])
	spec, known := patternSpecs[name]
	if !ok || !known {
		return w.refuse(pageCreateBadInput, here, "pattern", "must be one of the patterns", slices.Sorted(maps.Keys(patternSpecs))...)
	}
	// The hero opens the page: the first child of the first section.
	if name == "hero" && (w.sections != 1 || w.childAt != 0) {
		return w.refuse(pageCreateHeroMisplaced, here, "pattern",
			"the hero is the first block of the first section; move it there, or leave the hero out")
	}
	body := spec.array
	if body == "" {
		body = "children"
	}
	if code := w.members(here, "a "+name+" pattern", n, []string{"type", "pattern", "variant", body}, "type", "pattern", "variant", body); code != "" {
		return code
	}
	if variant, ok := jsonStringOf(n["variant"]); !ok || !slices.Contains(spec.variants, variant) {
		return w.refuse(pageCreateBadInput, here, "variant", "must be one of the variants of the "+name+" pattern", spec.variants...)
	}
	if spec.array == "" {
		return w.patternBlocks(n[body], spec, name)
	}
	return w.patternItems(n[body], spec)
}

// patternBlocks is the children of a hero or a cta: 1..maxChildren nodes of
// the blocks the pattern names. w.where is the pattern's address.
func (w *pageWalk) patternBlocks(raw json.RawMessage, spec patternSpec, name string) string {
	here := w.where
	list, ok := jsonArrayOf(raw)
	if !ok {
		return w.refuse(pageCreateBadInput, here, "children", "must be a list of blocks")
	}
	if len(list) == 0 || len(list) > spec.maxChildren {
		return w.refuse(pageCreateLayoutInvalid, here, "children",
			fmt.Sprintf("a %s holds 1 to %d blocks; this one has %d", name, spec.maxChildren, len(list)))
	}
	for i, child := range list {
		w.where = here + ".children[" + strconv.Itoa(i) + "]"
		if code := w.node(child, spec.place); code != "" {
			return code
		}
	}
	return ""
}

// patternItems is the array of an item pattern. Its count is judged before
// its items, so a list of the wrong length is told so whatever the items hold.
// The icons that are not in the list are told together once every item has
// been read; any other problem of an item is told first. w.where is the
// pattern's address.
func (w *pageWalk) patternItems(raw json.RawMessage, spec patternSpec) string {
	here := w.where
	items, ok := jsonArrayOf(raw)
	if !ok {
		return w.refuse(pageCreateBadInput, here, spec.array, "must be a list of items")
	}
	if len(items) < spec.min || len(items) > spec.max {
		return w.refuse(pageCreateItemsOutOfRange, here, spec.array,
			fmt.Sprintf("%s take %d to %d items; this one has %d", spec.array, spec.min, spec.max, len(items)))
	}
	w.icons = nil
	for i, it := range items {
		if code := w.patternItem(it, spec, here, i); code != "" {
			return code
		}
	}
	if len(w.icons) > 0 {
		w.fails = append(w.fails, w.icons...)
		w.icons = nil
		return pageCreateIconUnknown
	}
	return ""
}

func (w *pageWalk) patternItem(raw json.RawMessage, spec patternSpec, base string, i int) string {
	at := strconv.Itoa(i)
	item, ok := jsonObjectOf(raw)
	if !ok {
		return w.refuse(pageCreateBadInput, base, spec.array+"["+at+"]", "must be an object")
	}
	where := base + "." + spec.array + "[" + at + "]"
	allowed := make([]string, len(spec.fields))
	var required []string
	for i, f := range spec.fields {
		allowed[i] = f.key
		if f.required {
			required = append(required, f.key)
		}
	}
	if code := w.members(where, "this item", item, allowed, required...); code != "" {
		return code
	}
	for _, f := range spec.fields {
		if v, has := item[f.key]; has {
			if code := w.patternValue(f, v, where); code != "" {
				return code
			}
		}
	}
	return ""
}

// patternValue judges one field of an item; where is the address of the item.
func (w *pageWalk) patternValue(f patternField, raw json.RawMessage, where string) string {
	switch f.kind {
	case fieldText:
		if !isJSONString(raw) {
			return w.refuse(pageCreateBadInput, where, f.key, "must be a string")
		}
	case fieldIcon:
		name, ok := jsonStringOf(raw)
		if !ok {
			return w.refuse(pageCreateBadInput, where, f.key, "must be the name of an icon", pageIcons...)
		}
		if !slices.Contains(pageIcons, name) {
			w.icons = append(w.icons, refusalFailure{Path: where, Key: f.key, Reason: "is not the name of an icon in the list", Allowed: pageIcons})
		}
	case fieldImage:
		id, ok := jsonIntOf(raw)
		if !ok || id < 1 || id > pageCreateMaxAttachmentID {
			return w.refuse(pageCreateBadInput, where, f.key, "must be a whole number from 1 to "+strconv.Itoa(pageCreateMaxAttachmentID))
		}
		return w.media(id, where, f.key)
	case fieldFlag:
		if _, ok := jsonBoolOf(raw); !ok {
			return w.refuse(pageCreateBadInput, where, f.key, "must be true or false")
		}
	case fieldCount:
		if n, ok := jsonIntOf(raw); !ok || n < 0 || n > pageCreateMaxStatValue {
			return w.refuse(pageCreateBadInput, where, f.key, "must be a whole number from 0 to "+strconv.Itoa(pageCreateMaxStatValue))
		}
	case fieldBullets:
		list, ok := jsonArrayOf(raw)
		if !ok {
			return w.refuse(pageCreateBadInput, where, f.key, "must be a list of short lines")
		}
		if len(list) == 0 || len(list) > pageCreateMaxPlanBullets {
			return w.refuse(pageCreateItemsOutOfRange, where, f.key,
				fmt.Sprintf("a plan lists 1 to %d features; this one has %d", pageCreateMaxPlanBullets, len(list)))
		}
		for i, b := range list {
			if !isJSONString(b) {
				return w.refuse(pageCreateBadInput, where, f.key+"["+strconv.Itoa(i)+"]", "must be a string")
			}
		}
	case fieldLink:
		link, ok := jsonObjectOf(raw)
		if !ok {
			return w.refuse(pageCreateBadInput, where, f.key, "must be an object with text and url")
		}
		at := where + "." + f.key
		if code := w.members(at, "a plan button", link, []string{"text", "url"}, "text", "url"); code != "" {
			return code
		}
		if !isJSONString(link["text"]) {
			return w.refuse(pageCreateBadInput, at, "text", "must be a string")
		}
		url, ok := jsonStringOf(link["url"])
		if !ok {
			return w.refuse(pageCreateBadInput, at, "url", "must be a string")
		}
		return w.linkCode(url, at)
	}
	return ""
}
