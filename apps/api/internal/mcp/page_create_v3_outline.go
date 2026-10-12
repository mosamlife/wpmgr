package mcp

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
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
	if blocks := patternBlocksAt(at); blocks != nil && slices.Contains(pageNodeTypes, kind) && !slices.Contains(blocks, kind) {
		return pageCreateLayoutInvalid, true
	}
	if kind == "pattern" {
		return w.pattern(n, at), true
	}
	return "", false
}

// section is one section: at most pageCreateMaxSections on a page, a closed
// set of members, an anchor that is its own on the page, and
// 1..pageCreateMaxChildren children.
func (w *pageWalk) section(n map[string]json.RawMessage) string {
	if w.sections++; w.sections > pageCreateMaxSections {
		return pageCreateLayoutInvalid
	}
	w.usesLayout = true
	if !keysExactly(n, []string{"type", "anchor", "width", "tone", "background", "spacing", "align", "min_height", "children"},
		"type", "anchor", "children") {
		return pageCreateBadInput
	}
	anchor, ok := jsonStringOf(n["anchor"])
	if !ok {
		return pageCreateBadInput
	}
	if code := w.anchor(anchor); code != "" {
		return code
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

// anchor judges the anchor of a section and keeps it: it has the shape of an
// anchor, is not an id the theme owns, and is not one an earlier section took.
func (w *pageWalk) anchor(a string) string {
	if !pageAnchorPattern.MatchString(a) {
		return pageCreateAnchorInvalid
	}
	if slices.Contains(pageReservedAnchors, a) {
		return pageCreateAnchorReserved
	}
	if _, taken := w.anchors[a]; taken {
		return pageCreateAnchorDuplicate
	}
	if w.anchors == nil {
		w.anchors = map[string]struct{}{}
	}
	w.anchors[a] = struct{}{}
	return ""
}

// linkCode is the refusal of a link target, "" when the grammar accepts it.
// The buttons of a page and the calls to action of a plan share it. A designed
// page may also point at one of its own sections, with "#" and the anchor; the
// target is kept and judged by resolveLinks once the whole outline is read, so
// a section further down resolves it.
func (w *pageWalk) linkCode(url string) string {
	if w.designed && strings.HasPrefix(url, "#") {
		slug := url[1:]
		if !pageAnchorPattern.MatchString(slug) {
			return pageCreateLinkInvalid
		}
		w.links = append(w.links, slug)
		return ""
	}
	if !pageLinkUsable(url) {
		return pageCreateLinkInvalid
	}
	return ""
}

// resolveLinks is the refusal of a link to an anchor the page does not have,
// judged after every section is read.
func (w *pageWalk) resolveLinks() string {
	for _, slug := range w.links {
		if _, ok := w.anchors[slug]; !ok {
			return pageCreateLinkAnchorMissing
		}
	}
	return ""
}

// h1 judges a level 1 heading, of which a designed page has one, inside its
// hero. A version 2 outline has no level 1 heading.
func (w *pageWalk) h1(at pagePlace) string {
	if !w.designed {
		return pageCreateBadInput
	}
	if at != pageAtHero {
		return pageCreateH1OutsideHero
	}
	if w.h1s++; w.h1s > pageCreateMaxH1 {
		return pageCreateLayoutInvalid
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

// pattern is a pattern node: a child of a section, closed, with a name and a
// variant from the lists and either children or its array of items.
func (w *pageWalk) pattern(n map[string]json.RawMessage, at pagePlace) string {
	if at != pageAtSection {
		return pageCreateLayoutInvalid
	}
	name, ok := jsonStringOf(n["pattern"])
	spec, known := patternSpecs[name]
	if !ok || !known {
		return pageCreateBadInput
	}
	// The hero opens the page: the first child of the first section.
	if name == "hero" && (w.sections != 1 || w.childAt != 0) {
		return pageCreateHeroMisplaced
	}
	body := spec.array
	if body == "" {
		body = "children"
	}
	if !keysExactly(n, []string{"type", "pattern", "variant", body}, "type", "pattern", "variant", body) {
		return pageCreateBadInput
	}
	if variant, ok := jsonStringOf(n["variant"]); !ok || !slices.Contains(spec.variants, variant) {
		return pageCreateBadInput
	}
	if spec.array == "" {
		return w.patternBlocks(n[body], spec)
	}
	return w.patternItems(n[body], spec)
}

// patternBlocks is the children of a hero or a cta: 1..maxChildren nodes of
// the blocks the pattern names.
func (w *pageWalk) patternBlocks(raw json.RawMessage, spec patternSpec) string {
	list, ok := jsonArrayOf(raw)
	if !ok {
		return pageCreateBadInput
	}
	if len(list) == 0 || len(list) > spec.maxChildren {
		return pageCreateLayoutInvalid
	}
	for _, child := range list {
		if code := w.node(child, spec.place); code != "" {
			return code
		}
	}
	return ""
}

// patternItems is the array of an item pattern. Its count is judged before
// its items, so a list of the wrong length is told so whatever the items hold.
func (w *pageWalk) patternItems(raw json.RawMessage, spec patternSpec) string {
	items, ok := jsonArrayOf(raw)
	if !ok {
		return pageCreateBadInput
	}
	if len(items) < spec.min || len(items) > spec.max {
		return pageCreateItemsOutOfRange
	}
	for _, it := range items {
		if code := w.patternItem(it, spec.fields); code != "" {
			return code
		}
	}
	return ""
}

func (w *pageWalk) patternItem(raw json.RawMessage, fields []patternField) string {
	item, ok := jsonObjectOf(raw)
	if !ok {
		return pageCreateBadInput
	}
	allowed := make([]string, len(fields))
	var required []string
	for i, f := range fields {
		allowed[i] = f.key
		if f.required {
			required = append(required, f.key)
		}
	}
	if !keysExactly(item, allowed, required...) {
		return pageCreateBadInput
	}
	for _, f := range fields {
		if v, has := item[f.key]; has {
			if code := w.patternValue(f.kind, v); code != "" {
				return code
			}
		}
	}
	return ""
}

func (w *pageWalk) patternValue(kind patternFieldKind, raw json.RawMessage) string {
	switch kind {
	case fieldText:
		if !isJSONString(raw) {
			return pageCreateBadInput
		}
	case fieldIcon:
		name, ok := jsonStringOf(raw)
		if !ok {
			return pageCreateBadInput
		}
		if !slices.Contains(pageIcons, name) {
			return pageCreateIconUnknown
		}
	case fieldImage:
		id, ok := jsonIntOf(raw)
		if !ok || id < 1 || id > pageCreateMaxAttachmentID {
			return pageCreateBadInput
		}
		return w.media(id)
	case fieldFlag:
		if _, ok := jsonBoolOf(raw); !ok {
			return pageCreateBadInput
		}
	case fieldCount:
		if n, ok := jsonIntOf(raw); !ok || n < 0 || n > pageCreateMaxStatValue {
			return pageCreateBadInput
		}
	case fieldBullets:
		list, ok := jsonArrayOf(raw)
		if !ok {
			return pageCreateBadInput
		}
		if len(list) == 0 || len(list) > pageCreateMaxPlanBullets {
			return pageCreateItemsOutOfRange
		}
		for _, b := range list {
			if !isJSONString(b) {
				return pageCreateBadInput
			}
		}
	case fieldLink:
		link, ok := jsonObjectOf(raw)
		if !ok || !keysExactly(link, []string{"text", "url"}, "text", "url") || !isJSONString(link["text"]) {
			return pageCreateBadInput
		}
		url, ok := jsonStringOf(link["url"])
		if !ok {
			return pageCreateBadInput
		}
		return w.linkCode(url)
	}
	return ""
}
