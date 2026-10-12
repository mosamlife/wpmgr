package mcp

// wpmgr/page-create input, outline grammar v2: the control plane's half of
// the contract the agent's PageCreateBuilder enforces. Parse, don't strip: an
// unknown key, a wrong JSON type, a value outside an enum, a placement the
// grammar does not allow or a limit breach refuses; nothing is rewritten.
//
// The text rules (characters, emptiness and the per-field lengths of every
// text value) are the agent's alone. This side checks that each text value is
// a JSON string and stops there, so a text problem reaches the agent and
// comes back as create_content_invalid with our fixed hint.
//
// The checks run in the agent's order, so an input with one structural
// problem gets the agent's own refusal code. The shared case table
// (apps/agent/tests/fixtures/ability-run/page-create-layout-cases.json) pins
// both sides.
//
// An input that sends outline_version 3 is a designed page. It is admitted for
// Elementor's classic format alone, and the format must be stated: the
// default, site_default, is resolved by the site and not here. It then needs a
// design, and may send page_frame. Without outline_version 3, design and
// page_frame are unknown members, as before.

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/humantext"
)

// pageCreateInputSchema is the input schema the AI sees in describe and in
// an input refusal. It is the agent's PageCreateBuilder::inputSchema() byte
// for byte (a test compares it with the agent's fixture).
//
//go:embed page_create_schema.json
var pageCreateInputSchemaFile []byte

var pageCreateInputSchema = json.RawMessage(strings.TrimSuffix(string(pageCreateInputSchemaFile), "\n"))

// Limits of the outline grammar: the agent's PageCreateBuilder constants.
// The m162 catalogue entry publishes the same numbers to the AI as limits.
const (
	pageCreateMaxTopLevelNodes   = 200
	pageCreateMaxNodes           = 400
	pageCreateMaxItems           = 50
	pageCreateMinColumns         = 2
	pageCreateMaxColumns         = 4
	pageCreateMaxChildren        = 50
	pageCreateMaxImages          = 20
	pageCreateMaxButtons         = 12
	pageCreateMaxButtonsPerBlock = 3
	pageCreateMaxTables          = 10
	pageCreateMaxTableRows       = 50
	pageCreateMaxTableColumns    = 6
	pageCreateMaxQuoteParagraphs = 10
	pageCreateMinColumnWidth     = 10
	pageCreateMaxColumnWidth     = 90
	pageCreateMaxAttachmentID    = 2147483647
	pageCreateMaxURLBytes        = 2048
	pageCreateMaxFilenameChars   = 255
	pageCreateMaxImageDimension  = 100000
)

// Refusal codes of the outline grammar, the agent's own.
const (
	pageCreateBadInput         = "bad_input"
	pageCreateLayoutInvalid    = "layout_invalid"
	pageCreateLinkInvalid      = "link_invalid"
	pageCreateNeedsBlockEditor = "layout_needs_block_editor"
)

const (
	pageEditorBlocks  = "wordpress_blocks"
	pageEditorClassic = "wordpress_classic"
)

// pageOutlineVersionDesigned is the one outline_version an input may send. An
// input without it holds outline grammar v2.
const pageOutlineVersionDesigned = 3

// The page frames of a designed page: how the page sits in the theme. theme
// keeps the theme's header, footer and page title; full_width keeps the
// header and footer and hides the title; canvas removes the header and footer
// as well, so it is never a default.
const (
	pageFrameTheme     = "theme"
	pageFrameFullWidth = "full_width"
	pageFrameCanvas    = "canvas"
)

// pageFrames are the values page_frame may hold.
var pageFrames = []string{pageFrameTheme, pageFrameFullWidth, pageFrameCanvas}

// pageCreateFacts are what the control plane reads from a valid input.
type pageCreateFacts struct {
	postType string
	editor   string
	title    string
	// builder is the page builder a builder editor names ("elementor" for
	// "builder:elementor"), "" for a WordPress editor; elementorFormat is
	// that input's elementor_format, site_default when it sends none.
	builder         string
	elementorFormat string
	// outlineVersion is pageOutlineVersionDesigned for a designed page, 0 for
	// an input without outline_version. design is that page's design, every
	// field checked and its colours upper case, and pageFrame the frame it
	// sits in: the input's page_frame, else full_width when the outline has a
	// hero and theme when it has none. Both are empty for a version 2 input.
	outlineVersion int
	design         *pageDesign
	pageFrame      string
	// usesLayout: the outline holds a block beyond heading, paragraph and
	// list, so the request needs MinAgentVersionForPageLayout and is shown
	// with card copy version 2.
	usesLayout bool
	// mediaIDs are the distinct attachment ids in document order (depth
	// first), the order of preview.media and of the base fingerprint rows.
	mediaIDs []int64
}

type pagePlace int

const (
	pageAtTop pagePlace = iota
	pageAtGroup
	pageAtColumn
	// pageAtSection is a child of a section, which stands where the top of a
	// version 2 outline stands. pageAtHero and pageAtCTA are the children of
	// those two patterns.
	pageAtSection
	pageAtHero
	pageAtCTA
)

// pageWalk carries the page-wide counters through one validation. designed is
// set for an outline of version 3: its top level holds sections only.
type pageWalk struct {
	nodes, images, buttons, tables int
	sections                       int
	designed                       bool
	blockOnly                      bool
	usesLayout                     bool
	ids                            []int64
	seen                           map[int64]struct{}
	// Of a designed outline: the anchors taken, the slugs that links point at,
	// the level 1 headings read, and the index of the section child being read.
	anchors map[string]struct{}
	links   []string
	h1s     int
	childAt int
}

// validatePageCreateInput checks input against the outline grammar. It
// returns the facts, or the refusal code of the first problem.
func validatePageCreateInput(input []byte) (pageCreateFacts, string) {
	if len(input) > abilityRunDefaultInputBytes {
		return pageCreateFacts{}, pageCreateBadInput
	}
	top, ok := jsonObjectOf(input)
	if !ok {
		return pageCreateFacts{}, pageCreateBadInput
	}
	for k := range top {
		switch k {
		case "post_type", "editor", "title", "outline", "elementor_format", "outline_version", "design", "page_frame":
		default:
			return pageCreateFacts{}, pageCreateBadInput
		}
	}
	var f pageCreateFacts
	if f.postType, ok = jsonStringOf(top["post_type"]); !ok || (f.postType != "page" && f.postType != "post") {
		return pageCreateFacts{}, pageCreateBadInput
	}
	if f.editor, ok = jsonStringOf(top["editor"]); !ok {
		return pageCreateFacts{}, pageCreateBadInput
	}
	switch f.editor {
	case pageEditorBlocks, pageEditorClassic:
	case pageEditorBuilderElementor:
		f.builder, f.elementorFormat = pageBuilderElementor, pageElementorFormatDefault
	default:
		return pageCreateFacts{}, pageCreateBadInput
	}
	// elementor_format belongs to builder:elementor alone and holds one of the
	// published values; the facts of every other editor carry no format.
	if raw, has := top["elementor_format"]; has {
		format, ok := jsonStringOf(raw)
		if !ok || f.builder != pageBuilderElementor || !pageElementorFormatKnown(format) {
			return pageCreateFacts{}, pageCreateBadInput
		}
		f.elementorFormat = format
	}
	if f.outlineVersion, ok = pageOutlineVersionOf(top); !ok {
		return pageCreateFacts{}, pageCreateBadInput
	}
	designed := f.outlineVersion == pageOutlineVersionDesigned
	if designed {
		// A designed page is built with Elementor's classic format, and an
		// input that does not state it has site_default, which only the site
		// can resolve.
		if f.builder != pageBuilderElementor || f.elementorFormat != elementorFormatClassic {
			return pageCreateFacts{}, pageCreateBadInput
		}
	} else if _, has := top["design"]; has {
		return pageCreateFacts{}, pageCreateBadInput
	} else if _, has := top["page_frame"]; has {
		return pageCreateFacts{}, pageCreateBadInput
	}
	if f.title, ok = jsonStringOf(top["title"]); !ok || strings.TrimSpace(f.title) == "" {
		return pageCreateFacts{}, pageCreateBadInput
	}
	// The design is judged before the outline, so a page without one is
	// told that rather than anything about its nodes.
	if designed {
		design, problem := validatePageDesign(top["design"])
		if problem != nil {
			return pageCreateFacts{}, problem.Code
		}
		f.design = &design
	}
	outline, ok := jsonArrayOf(top["outline"])
	// The top level of a designed page is bounded by its section and hero
	// caps, which answer layout_invalid however long the list is.
	if !ok || len(outline) == 0 || (!designed && len(outline) > pageCreateMaxTopLevelNodes) {
		return pageCreateFacts{}, pageCreateBadInput
	}
	if designed {
		if f.pageFrame, ok = pageFrameOf(top, outline); !ok {
			return pageCreateFacts{}, pageCreateBadInput
		}
		if pageOutlineHeroes(outline) > pageCreateMaxHeroes {
			return pageCreateFacts{}, pageCreateLayoutInvalid
		}
	}
	w := pageWalk{seen: map[int64]struct{}{}, designed: designed}
	for _, n := range outline {
		if code := w.node(n, pageAtTop); code != "" {
			return pageCreateFacts{}, code
		}
	}
	if code := w.resolveLinks(); code != "" {
		return pageCreateFacts{}, code
	}
	if f.editor == pageEditorClassic && w.blockOnly {
		return pageCreateFacts{}, pageCreateNeedsBlockEditor
	}
	f.usesLayout, f.mediaIDs = w.usesLayout, w.ids
	return f, ""
}

// pageOutlineVersionOf reads outline_version from the input's top level:
// absent is 0 (outline grammar v2), and a member that is sent must be the
// whole number pageOutlineVersionDesigned, so 3.0, "3" and every other number
// are refused.
func pageOutlineVersionOf(top map[string]json.RawMessage) (int, bool) {
	raw, has := top["outline_version"]
	if !has {
		return 0, true
	}
	if n, ok := jsonIntOf(raw); ok && n == pageOutlineVersionDesigned {
		return pageOutlineVersionDesigned, true
	}
	return 0, false
}

// pageFrameOf reads page_frame from the input's top level and applies its
// default. A member that is sent must be one of pageFrames; one that is not
// sent is full_width when the outline has a hero and theme when it has none.
// Canvas is only ever asked for.
func pageFrameOf(top map[string]json.RawMessage, outline []json.RawMessage) (string, bool) {
	raw, has := top["page_frame"]
	if !has {
		if pageOutlineHasHero(outline) {
			return pageFrameFullWidth, true
		}
		return pageFrameTheme, true
	}
	frame, ok := jsonStringOf(raw)
	if !ok || !slices.Contains(pageFrames, frame) {
		return "", false
	}
	return frame, true
}

// pageOutlineHasHero reports whether a section of a version 3 outline holds a
// hero pattern as one of its children. Only the shape is read: where a hero
// may stand is the grammar's rule, and a node that is not an object is ignored.
func pageOutlineHasHero(outline []json.RawMessage) bool {
	return pageOutlineHeroes(outline) > 0
}

// pageOutlineHeroes counts the hero patterns among the children of the
// sections of a version 3 outline, read by shape alone.
func pageOutlineHeroes(outline []json.RawMessage) int {
	heroes := 0
	for _, raw := range outline {
		section, ok := jsonObjectOf(raw)
		if !ok {
			continue
		}
		children, ok := jsonArrayOf(section["children"])
		if !ok {
			continue
		}
		for _, child := range children {
			n, ok := jsonObjectOf(child)
			if !ok {
				continue
			}
			if kind, ok := jsonStringOf(n["type"]); !ok || kind != "pattern" {
				continue
			}
			if name, ok := jsonStringOf(n["pattern"]); ok && name == "hero" {
				heroes++
			}
		}
	}
	return heroes
}

// node is one outline node at a placement.
func (w *pageWalk) node(raw json.RawMessage, at pagePlace) string {
	n, ok := jsonObjectOf(raw)
	if !ok {
		return pageCreateBadInput
	}
	if w.nodes++; w.nodes > pageCreateMaxNodes {
		return pageCreateLayoutInvalid
	}
	kind, ok := jsonStringOf(n["type"])
	if !ok {
		return pageCreateBadInput
	}
	if w.designed {
		if code, handled := w.designedNode(n, kind, at); handled {
			return code
		}
	}
	switch kind {
	case "heading":
		level, ok := jsonIntOf(n["level"])
		if !keysWithin(n, "type", "level", "text") || !ok || level < 1 || level > 4 || !isJSONString(n["text"]) {
			return pageCreateBadInput
		}
		if level == 1 {
			return w.h1(at)
		}
		return ""
	case "paragraph":
		if !keysWithin(n, "type", "text") || !isJSONString(n["text"]) {
			return pageCreateBadInput
		}
		return ""
	case "list":
		if !keysWithin(n, "type", "ordered", "items") {
			return pageCreateBadInput
		}
		if _, ok := jsonBoolOf(n["ordered"]); !ok {
			return pageCreateBadInput
		}
		items, ok := jsonArrayOf(n["items"])
		if !ok || len(items) == 0 || len(items) > pageCreateMaxItems {
			return pageCreateBadInput
		}
		for _, it := range items {
			if !isJSONString(it) {
				return pageCreateBadInput
			}
		}
		return ""
	}
	// Everything below is outline grammar v2.
	w.usesLayout = true
	switch kind {
	case "image":
		return w.image(n)
	case "buttons":
		return w.buttonsNode(n)
	case "quote":
		return quoteNode(n)
	case "separator":
		if !keysExactly(n, []string{"type"}, "type") {
			return pageCreateBadInput
		}
		return ""
	case "spacer":
		if !keysExactly(n, []string{"type", "size"}, "type", "size") {
			return pageCreateBadInput
		}
		switch size, _ := jsonStringOf(n["size"]); size {
		case "small", "medium", "large":
		default:
			return pageCreateBadInput
		}
		w.blockOnly = true
		return ""
	case "table":
		return w.table(n)
	case "group":
		if at != pageAtTop && at != pageAtSection {
			return pageCreateLayoutInvalid
		}
		if !keysExactly(n, []string{"type", "children"}, "type", "children") {
			return pageCreateBadInput
		}
		if code := w.children(n["children"], pageAtGroup); code != "" {
			return code
		}
		w.blockOnly = true
		return ""
	case "columns":
		if at == pageAtColumn {
			return pageCreateLayoutInvalid
		}
		return w.columns(n)
	}
	// Not a block of the grammar. The agent answers create_content_invalid
	// here (as in v1); for the control plane it does not match the schema.
	return pageCreateBadInput
}

func (w *pageWalk) image(n map[string]json.RawMessage) string {
	if !keysExactly(n, []string{"type", "attachment_id", "alt", "caption", "align"}, "type", "attachment_id", "alt") {
		return pageCreateBadInput
	}
	id, ok := jsonIntOf(n["attachment_id"])
	if !ok || id < 1 || id > pageCreateMaxAttachmentID {
		return pageCreateBadInput
	}
	// Alt text's own rules (no square bracket, no character reference) are
	// text rules: the agent refuses them as create_content_invalid, and the
	// shared case table has this side pass them on.
	if !isJSONString(n["alt"]) {
		return pageCreateBadInput
	}
	if raw, has := n["caption"]; has {
		if !isJSONString(raw) {
			return pageCreateBadInput
		}
		w.blockOnly = true
	}
	if raw, has := n["align"]; has {
		switch align, _ := jsonStringOf(raw); align {
		case "none", "center", "wide", "full":
		default:
			return pageCreateBadInput
		}
	}
	return w.media(id)
}

func (w *pageWalk) buttonsNode(n map[string]json.RawMessage) string {
	if !keysExactly(n, []string{"type", "align", "buttons"}, "type", "buttons") {
		return pageCreateBadInput
	}
	if raw, has := n["align"]; has {
		switch align, _ := jsonStringOf(raw); align {
		case "left", "center":
		default:
			return pageCreateBadInput
		}
	}
	list, ok := jsonArrayOf(n["buttons"])
	if !ok {
		return pageCreateBadInput
	}
	if len(list) == 0 || len(list) > pageCreateMaxButtonsPerBlock {
		return pageCreateLayoutInvalid
	}
	for _, raw := range list {
		b, ok := jsonObjectOf(raw)
		if !ok {
			return pageCreateBadInput
		}
		if w.nodes++; w.nodes > pageCreateMaxNodes {
			return pageCreateLayoutInvalid
		}
		if !keysExactly(b, []string{"text", "url", "style"}, "text", "url") || !isJSONString(b["text"]) {
			return pageCreateBadInput
		}
		url, ok := jsonStringOf(b["url"])
		if !ok {
			return pageCreateBadInput
		}
		if code := w.linkCode(url); code != "" {
			return code
		}
		if raw, has := b["style"]; has {
			if style, _ := jsonStringOf(raw); !slices.Contains(w.buttonStyles(), style) {
				return pageCreateBadInput
			}
		}
		if w.buttons++; w.buttons > pageCreateMaxButtons {
			return pageCreateLayoutInvalid
		}
	}
	w.blockOnly = true
	return ""
}

func quoteNode(n map[string]json.RawMessage) string {
	if !keysExactly(n, []string{"type", "paragraphs", "citation"}, "type", "paragraphs") {
		return pageCreateBadInput
	}
	paras, ok := jsonArrayOf(n["paragraphs"])
	if !ok {
		return pageCreateBadInput
	}
	if len(paras) == 0 || len(paras) > pageCreateMaxQuoteParagraphs {
		return pageCreateLayoutInvalid
	}
	for _, p := range paras {
		if !isJSONString(p) {
			return pageCreateBadInput
		}
	}
	if raw, has := n["citation"]; has && !isJSONString(raw) {
		return pageCreateBadInput
	}
	return ""
}

func (w *pageWalk) table(n map[string]json.RawMessage) string {
	if !keysExactly(n, []string{"type", "header", "rows"}, "type", "rows") {
		return pageCreateBadInput
	}
	width := -1
	if raw, has := n["header"]; has {
		cells, code := tableCells(raw)
		if code != "" {
			return code
		}
		width = cells
	}
	rows, ok := jsonArrayOf(n["rows"])
	if !ok {
		return pageCreateBadInput
	}
	if len(rows) == 0 || len(rows) > pageCreateMaxTableRows {
		return pageCreateLayoutInvalid
	}
	for _, row := range rows {
		cells, code := tableCells(row)
		if code != "" {
			return code
		}
		if width >= 0 && cells != width {
			return pageCreateLayoutInvalid
		}
		width = cells
	}
	if w.tables++; w.tables > pageCreateMaxTables {
		return pageCreateLayoutInvalid
	}
	return ""
}

// tableCells is one row or the header: 1..6 strings (empty allowed).
func tableCells(raw json.RawMessage) (int, string) {
	cells, ok := jsonArrayOf(raw)
	if !ok {
		return 0, pageCreateBadInput
	}
	if len(cells) == 0 || len(cells) > pageCreateMaxTableColumns {
		return 0, pageCreateLayoutInvalid
	}
	for _, c := range cells {
		if !isJSONString(c) {
			return 0, pageCreateBadInput
		}
	}
	return len(cells), ""
}

func (w *pageWalk) columns(n map[string]json.RawMessage) string {
	if !keysExactly(n, []string{"type", "widths", "columns"}, "type", "columns") {
		return pageCreateBadInput
	}
	cols, ok := jsonArrayOf(n["columns"])
	if !ok {
		return pageCreateBadInput
	}
	if len(cols) < pageCreateMinColumns || len(cols) > pageCreateMaxColumns {
		return pageCreateLayoutInvalid
	}
	if raw, has := n["widths"]; has {
		list, ok := jsonArrayOf(raw)
		if !ok {
			return pageCreateBadInput
		}
		widths := make([]int64, 0, len(list))
		for _, r := range list {
			v, ok := jsonIntOf(r)
			if !ok {
				return pageCreateBadInput
			}
			widths = append(widths, v)
		}
		if len(widths) != len(cols) {
			return pageCreateLayoutInvalid
		}
		var sum int64
		for _, v := range widths {
			if v < pageCreateMinColumnWidth || v > pageCreateMaxColumnWidth {
				return pageCreateLayoutInvalid
			}
			sum += v
		}
		if sum != 100 {
			return pageCreateLayoutInvalid
		}
	}
	for _, raw := range cols {
		c, ok := jsonObjectOf(raw)
		if !ok {
			return pageCreateBadInput
		}
		if w.nodes++; w.nodes > pageCreateMaxNodes {
			return pageCreateLayoutInvalid
		}
		if !keysExactly(c, []string{"children"}, "children") {
			return pageCreateBadInput
		}
		if code := w.children(c["children"], pageAtColumn); code != "" {
			return code
		}
	}
	w.blockOnly = true
	return ""
}

// children is a container's list: 1..50 nodes at one placement.
func (w *pageWalk) children(raw json.RawMessage, at pagePlace) string {
	list, ok := jsonArrayOf(raw)
	if !ok {
		return pageCreateBadInput
	}
	if len(list) == 0 || len(list) > pageCreateMaxChildren {
		return pageCreateLayoutInvalid
	}
	for i, child := range list {
		if at == pageAtSection {
			w.childAt = i
		}
		if code := w.node(child, at); code != "" {
			return code
		}
	}
	return ""
}

// keysWithin: every key is one of allowed (the v1 nodes' rule; a missing
// field fails its own type check).
func keysWithin(n map[string]json.RawMessage, allowed ...string) bool {
	for k := range n {
		found := false
		for _, a := range allowed {
			if k == a {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// keysExactly: every key is allowed and every required key is present.
func keysExactly(n map[string]json.RawMessage, allowed []string, required ...string) bool {
	if !keysWithin(n, allowed...) {
		return false
	}
	for _, r := range required {
		if _, ok := n[r]; !ok {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Strict JSON reads. encoding/json decodes null into a string, a number or a
// bool as a silent no-op, and matches struct keys case-insensitively, so the
// grammar reads every value through these instead: the value's own JSON type
// decides, and objects decode to maps with exact keys.
// ---------------------------------------------------------------------------

func jsonLeadByte(raw []byte) byte {
	for _, c := range raw {
		switch c {
		case ' ', '\t', '\r', '\n':
			continue
		}
		return c
	}
	return 0
}

func jsonObjectOf(raw []byte) (map[string]json.RawMessage, bool) {
	if jsonLeadByte(raw) != '{' {
		return nil, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil, false
	}
	return m, true
}

func jsonArrayOf(raw []byte) ([]json.RawMessage, bool) {
	if jsonLeadByte(raw) != '[' {
		return nil, false
	}
	var a []json.RawMessage
	if json.Unmarshal(raw, &a) != nil {
		return nil, false
	}
	return a, true
}

func jsonStringOf(raw []byte) (string, bool) {
	if jsonLeadByte(raw) != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

func isJSONString(raw []byte) bool {
	_, ok := jsonStringOf(raw)
	return ok
}

// jsonIntOf reads a JSON integer: no fraction, no exponent, within int64.
func jsonIntOf(raw []byte) (int64, bool) {
	if c := jsonLeadByte(raw); c != '-' && (c < '0' || c > '9') {
		return 0, false
	}
	var n int64
	if json.Unmarshal(raw, &n) != nil {
		return 0, false
	}
	return n, true
}

// jsonNumberOf reads a JSON number of any shape (a fraction and an exponent
// are allowed) that fits a float64.
func jsonNumberOf(raw []byte) (float64, bool) {
	if c := jsonLeadByte(raw); c != '-' && (c < '0' || c > '9') {
		return 0, false
	}
	var n float64
	if json.Unmarshal(raw, &n) != nil {
		return 0, false
	}
	return n, true
}

func jsonBoolOf(raw []byte) (bool, bool) {
	switch strings.TrimSpace(string(raw)) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

// ---------------------------------------------------------------------------
// Links and image addresses
// ---------------------------------------------------------------------------

var (
	pageURLCharset       = regexp.MustCompile(`^[A-Za-z0-9\-._~:/?#!$&()*+,;=%@]+$`)
	pageLinkAuthority    = regexp.MustCompile(`^((?:[A-Za-z0-9-]+\.)+[A-Za-z0-9-]+)(?::([0-9]{1,5}))?$`)
	pageImageAuthority   = regexp.MustCompile(`^[A-Za-z0-9.-]+(?::[0-9]{1,5})?$`)
	pageModifiedGMTShape = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}$`)
	// pageCharacterReference is an "&" that starts a character reference: a
	// name, a decimal number or a hex number, ended by ";". The agent's
	// CHARACTER_REFERENCE, which links, image addresses and alt text refuse.
	pageCharacterReference = regexp.MustCompile(`&(?:[A-Za-z0-9]+|#[0-9]+|#[xX][0-9A-Fa-f]+);`)
)

// pageURLShapeUsable is the shape every link and image address shares: 1 to
// 2048 bytes of ASCII from the link character set, every % followed by two
// hex digits, and no "&" that starts a character reference.
func pageURLShapeUsable(u string) bool {
	if len(u) < 1 || len(u) > pageCreateMaxURLBytes || !pageURLCharset.MatchString(u) {
		return false
	}
	for i := 0; i < len(u); i++ {
		if u[i] == '%' && (i+2 >= len(u) || !isHexDigit(u[i+1]) || !isHexDigit(u[i+2])) {
			return false
		}
	}
	return !pageCharacterReference.MatchString(u)
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// authorityOf is the part of rest before the first '/', '?' or '#'.
func authorityOf(rest string) string {
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		return rest[:i]
	}
	return rest
}

// pageLinkUsable is a button link the grammar accepts, in the shared shape:
// an https:// address (lowercase scheme, a DNS host with at least one dot, an
// optional port 1..65535, no user name or password) or a path on the site
// that starts with one '/' and holds no ':' and no "&#" anywhere (a colon is
// written %3A).
func pageLinkUsable(u string) bool {
	if !pageURLShapeUsable(u) {
		return false
	}
	if u[0] == '/' {
		if len(u) > 1 && (u[1] == '/' || u[1] == '\\') {
			return false
		}
		return !strings.Contains(u, ":") && !strings.Contains(u, "&#")
	}
	if !strings.HasPrefix(u, "https://") {
		return false
	}
	auth := authorityOf(u[len("https://"):])
	if strings.Contains(auth, "@") {
		return false
	}
	m := pageLinkAuthority.FindStringSubmatch(auth)
	if m == nil {
		return false
	}
	if m[2] != "" {
		port, err := strconv.Atoi(m[2])
		if err != nil || port < 1 || port > 65535 {
			return false
		}
	}
	return true
}

// pageImageURLUsable is an address the site gave for an image: http:// or
// https:// (lowercase), a plain host with an optional port and no user name,
// in the shared shape (so no character reference either: the agent refuses
// such an address as image_url_unusable).
func pageImageURLUsable(u string) bool {
	if !pageURLShapeUsable(u) {
		return false
	}
	var rest string
	switch {
	case strings.HasPrefix(u, "https://"):
		rest = u[len("https://"):]
	case strings.HasPrefix(u, "http://"):
		rest = u[len("http://"):]
	default:
		return false
	}
	return pageImageAuthority.MatchString(authorityOf(rest))
}

// ---------------------------------------------------------------------------
// The per-input agent floor
// ---------------------------------------------------------------------------

// pageCreateUsesLayout reports whether a page-create input's outline holds a
// block beyond heading, paragraph and list. It reads only the top-level node
// types: none of those three holds another node, so any v2 block puts a v2
// node at the top. An input it cannot read counts as layout, so a floor
// derived from it fails closed.
func pageCreateUsesLayout(input []byte) bool {
	top, ok := jsonObjectOf(input)
	if !ok {
		return true
	}
	outline, ok := jsonArrayOf(top["outline"])
	if !ok || len(outline) == 0 {
		return true
	}
	for _, raw := range outline {
		n, ok := jsonObjectOf(raw)
		if !ok {
			return true
		}
		switch kind, _ := jsonStringOf(n["type"]); kind {
		case "heading", "paragraph", "list":
		default:
			return true
		}
	}
	return false
}

// PageCreateAgentFloor is the first agent release that runs this
// wpmgr/page-create input: MinAgentVersionForBuilderAdapters when it asks
// for a page builder, MinAgentVersionForPageLayout when the outline holds a
// block beyond heading, paragraph and list (or the input cannot be read),
// MinAgentVersionForPageCreate otherwise. The run path and the dispatch
// worker both decide with it.
func PageCreateAgentFloor(input []byte) string {
	if pageCreateUsesBuilder(input) {
		return agentcmd.MinAgentVersionForBuilderAdapters
	}
	if pageCreateUsesLayout(input) {
		return agentcmd.MinAgentVersionForPageLayout
	}
	return agentcmd.MinAgentVersionForPageCreate
}

// ---------------------------------------------------------------------------
// Images in the precheck answer
// ---------------------------------------------------------------------------

// pageMediaFact is one image the site resolved, as precheck answers it in
// preview.media. Every field is bound into the base fingerprint.
type pageMediaFact struct {
	ID          int64
	URL         string
	Filename    string
	Mime        string
	Width       int64
	Height      int64
	ModifiedGMT string
}

var pageImageMimes = map[string]struct{}{
	"image/jpeg": {}, "image/png": {}, "image/gif": {}, "image/webp": {}, "image/avif": {},
}

// parsePageMedia reads preview.media strictly: a list of objects with
// exactly the seven facts, each usable.
func parsePageMedia(raw []byte) ([]pageMediaFact, bool) {
	list, ok := jsonArrayOf(raw)
	if !ok {
		return nil, false
	}
	out := make([]pageMediaFact, 0, len(list))
	for _, item := range list {
		m, ok := jsonObjectOf(item)
		if !ok || len(m) != 7 {
			return nil, false
		}
		var f pageMediaFact
		var oks [7]bool
		f.ID, oks[0] = jsonIntOf(m["id"])
		f.URL, oks[1] = jsonStringOf(m["url"])
		f.Filename, oks[2] = jsonStringOf(m["filename"])
		f.Mime, oks[3] = jsonStringOf(m["mime"])
		f.Width, oks[4] = jsonIntOf(m["width"])
		f.Height, oks[5] = jsonIntOf(m["height"])
		f.ModifiedGMT, oks[6] = jsonStringOf(m["modified_gmt"])
		for _, ok := range oks {
			if !ok {
				return nil, false
			}
		}
		if !f.usable() {
			return nil, false
		}
		out = append(out, f)
	}
	return out, true
}

// usable is the agent's mediaFactProblem, inverted.
func (f pageMediaFact) usable() bool {
	if f.ID < 1 || f.ID > pageCreateMaxAttachmentID || !pageImageURLUsable(f.URL) {
		return false
	}
	if f.Filename == "" || !utf8.ValidString(f.Filename) || utf8.RuneCountInString(f.Filename) > pageCreateMaxFilenameChars {
		return false
	}
	if _, ok := pageImageMimes[f.Mime]; !ok {
		return false
	}
	if f.Width < 0 || f.Width > pageCreateMaxImageDimension || f.Height < 0 || f.Height > pageCreateMaxImageDimension {
		return false
	}
	return pageModifiedGMTShape.MatchString(f.ModifiedGMT)
}

// pageCreateBaseFingerprint is the agent's base fingerprint for a new post:
// sha256 of PHP's json_encode(["new_post", post_type]) without images, and
// of json_encode(["new_post", post_type, [[id, url, filename, mime, width,
// height, modified_gmt], ...]]) with them, rows in media order.
func pageCreateBaseFingerprint(postType string, media []pageMediaFact) (string, bool) {
	pt, ok := phpJSONString(postType)
	if !ok {
		return "", false
	}
	var b strings.Builder
	b.WriteString(`["new_post",` + pt)
	if len(media) > 0 {
		b.WriteString(",[")
		for i, m := range media {
			if i > 0 {
				b.WriteByte(',')
			}
			u, ok1 := phpJSONString(m.URL)
			fn, ok2 := phpJSONString(m.Filename)
			mi, ok3 := phpJSONString(m.Mime)
			mod, ok4 := phpJSONString(m.ModifiedGMT)
			if !ok1 || !ok2 || !ok3 || !ok4 {
				return "", false
			}
			fmt.Fprintf(&b, "[%d,%s,%s,%s,%d,%d,%s]", m.ID, u, fn, mi, m.Width, m.Height, mod)
		}
		b.WriteByte(']')
	}
	b.WriteByte(']')
	return sha256Hex([]byte(b.String())), true
}

// pageContentImagesMatch: every image tag in the previewed content carries
// the address of one of the facts, and every fact's address is in the
// content, written as the builder writes it (`<img src="` + the address with
// & as &amp; + `"`). Text never holds '<' (the agent refuses it, and a text
// node escapes it), so every "<img" is a tag the builder wrote.
func pageContentImagesMatch(content string, media []pageMediaFact) bool {
	tags := make(map[string]bool, len(media))
	for _, m := range media {
		tags[`<img src="`+strings.ReplaceAll(m.URL, "&", "&amp;")+`"`] = false
	}
	for i := 0; i+4 <= len(content); i++ {
		if content[i] != '<' || !strings.EqualFold(content[i+1:i+4], "img") {
			continue
		}
		found := false
		for tag := range tags {
			if strings.HasPrefix(content[i:], tag) {
				tags[tag], found = true, true
				break
			}
		}
		if !found {
			return false
		}
	}
	for _, seen := range tags {
		if !seen {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// The card facts a page-create request stores
// ---------------------------------------------------------------------------

// pageCardFacts is card_facts for a page-create request with images or a
// page builder: what the approval card shows for each image, and of the
// page builder that builds the page. Every image string is the site's,
// cleaned and capped; the base fingerprint binds the raw facts. A request
// with images and no builder stores exactly {kind, media}.
type pageCardFacts struct {
	Kind    string           `json:"kind"`
	Media   []PageCardMedia  `json:"media,omitempty"`
	Builder *PageCardBuilder `json:"builder,omitempty"`
}

// PageCardBuilder is the page builder of a page-create request's card, as
// the site's precheck named it: the builder, the format and layout it
// builds, and its version (site text, of a checked shape).
type PageCardBuilder struct {
	Builder string `json:"builder"`
	Format  string `json:"format"`
	Version string `json:"version"`
	Layout  string `json:"layout"`
}

// PageCardMedia is one image of a page-create request's card: the
// attachment id and what the site said about it at precheck. Filename is
// site text.
type PageCardMedia struct {
	ID       int64  `json:"id"`
	Filename string `json:"filename"`
	Mime     string `json:"mime"`
	Width    int64  `json:"width"`
	Height   int64  `json:"height"`
}

// pageCardFactsKind is card_facts.kind for a page-create request.
const pageCardFactsKind = "page_create"

// pageCardFactsJSON is the stored card_facts for a page-create request, or
// nil when the outline has no image and no page builder builds the page.
func pageCardFactsJSON(media []pageMediaFact, builder *PageCardBuilder) ([]byte, error) {
	if len(media) == 0 && builder == nil {
		return nil, nil
	}
	card := pageCardFacts{Kind: pageCardFactsKind, Builder: builder}
	for _, m := range media {
		card.Media = append(card.Media, PageCardMedia{
			ID: m.ID, Filename: humantext.CapRunes(humantext.Clean(m.Filename), pageCreateMaxFilenameChars),
			Mime: m.Mime, Width: m.Width, Height: m.Height,
		})
	}
	b, err := json.Marshal(card)
	if err != nil {
		return nil, fmt.Errorf("encode page card facts: %w", err)
	}
	return b, nil
}

// ReadPageCardFacts reads a page-create request's stored card_facts back:
// its images in outline order, or ok=false for anything that is not a
// complete page-create card (a reader then shows no image facts, and a card
// with an image node but no facts cannot be approved).
func ReadPageCardFacts(stored []byte) ([]PageCardMedia, bool) {
	top, ok := readPageCardTop(stored)
	if !ok {
		return nil, false
	}
	list, ok := jsonArrayOf(top["media"])
	if !ok || len(list) == 0 || len(list) > pageCreateMaxImages {
		return nil, false
	}
	out := make([]PageCardMedia, 0, len(list))
	for _, raw := range list {
		m, ok := jsonObjectOf(raw)
		if !ok || len(m) != 5 {
			return nil, false
		}
		var c PageCardMedia
		var oks [5]bool
		c.ID, oks[0] = jsonIntOf(m["id"])
		c.Filename, oks[1] = jsonStringOf(m["filename"])
		c.Mime, oks[2] = jsonStringOf(m["mime"])
		c.Width, oks[3] = jsonIntOf(m["width"])
		c.Height, oks[4] = jsonIntOf(m["height"])
		for _, ok := range oks {
			if !ok {
				return nil, false
			}
		}
		if _, known := pageImageMimes[c.Mime]; !known || c.ID < 1 || c.ID > pageCreateMaxAttachmentID ||
			c.Width < 0 || c.Width > pageCreateMaxImageDimension || c.Height < 0 || c.Height > pageCreateMaxImageDimension {
			return nil, false
		}
		out = append(out, c)
	}
	return out, true
}

// readPageCardTop reads a stored page-create card's members: kind is
// page_create, media and builder are the only others, and a builder member,
// when there is one, is a complete one. ok is false for anything else, so a
// card with a damaged builder shows neither its images nor its builder.
func readPageCardTop(stored []byte) (map[string]json.RawMessage, bool) {
	top, ok := jsonObjectOf(stored)
	if !ok {
		return nil, false
	}
	for k := range top {
		if k != "kind" && k != "media" && k != "builder" {
			return nil, false
		}
	}
	if kind, ok := jsonStringOf(top["kind"]); !ok || kind != pageCardFactsKind {
		return nil, false
	}
	if raw, has := top["builder"]; has {
		if _, ok := readPageCardBuilder(raw); !ok {
			return nil, false
		}
	}
	return top, true
}

// ReadPageCardBuilder reads the page builder of a page-create request's
// stored card_facts, or ok=false when the card names none (the page is
// built with a WordPress editor) or is not a complete page-create card.
func ReadPageCardBuilder(stored []byte) (PageCardBuilder, bool) {
	top, ok := readPageCardTop(stored)
	if !ok {
		return PageCardBuilder{}, false
	}
	raw, has := top["builder"]
	if !has {
		return PageCardBuilder{}, false
	}
	return readPageCardBuilder(raw)
}

// readPageCardBuilder: exactly the four members, each a value this control
// plane stores (the builder and format it builds, a layout, a version of
// the checked shape).
func readPageCardBuilder(raw json.RawMessage) (PageCardBuilder, bool) {
	m, ok := jsonObjectOf(raw)
	if !ok || len(m) != 4 {
		return PageCardBuilder{}, false
	}
	var b PageCardBuilder
	var oks [4]bool
	b.Builder, oks[0] = jsonStringOf(m["builder"])
	b.Format, oks[1] = jsonStringOf(m["format"])
	b.Version, oks[2] = jsonStringOf(m["version"])
	b.Layout, oks[3] = jsonStringOf(m["layout"])
	for _, ok := range oks {
		if !ok {
			return PageCardBuilder{}, false
		}
	}
	if b.Builder != pageBuilderElementor || b.Format != elementorFormatClassic ||
		(b.Layout != elementorLayoutBoxes && b.Layout != elementorLayoutRows) || !elementorVersionPattern.MatchString(b.Version) {
		return PageCardBuilder{}, false
	}
	return b, true
}

// ---------------------------------------------------------------------------
// The design object of outline v3
// ---------------------------------------------------------------------------

// Refusal codes of the design object. A value the design may not hold is the
// grammar's own bad_input; a field left out and a pair of colours that is too
// close have their own.
const (
	pageDesignNotReady    = "design_not_ready"
	pageDesignContrastLow = "design_contrast_low"
)

// designFailure is one problem found in a design object. Path and Key
// together are the dotted address of the offending field in the input
// ("design.colors" and "ink"); Reason is WPMgr's own words for the fix;
// Allowed lists the values or keys that would be accepted there, when the
// set is closed. code is the refusal code the failure belongs to.
type designFailure struct {
	Path    string   `json:"path"`
	Key     string   `json:"key"`
	Reason  string   `json:"reason"`
	Allowed []string `json:"allowed,omitempty"`
	code    string
}

// designColours are the six colours of a design, upper case.
type designColours struct {
	Ground   string `json:"ground"`
	Surface  string `json:"surface"`
	Ink      string `json:"ink"`
	Muted    string `json:"muted"`
	Accent   string `json:"accent"`
	OnAccent string `json:"on_accent"`
}

// slot is the field that holds a role's colour, nil for a name that is not
// one of designColourRoles.
func (c *designColours) slot(role string) *string {
	switch role {
	case "ground":
		return &c.Ground
	case "surface":
		return &c.Surface
	case "ink":
		return &c.Ink
	case "muted":
		return &c.Muted
	case "accent":
		return &c.Accent
	case "on_accent":
		return &c.OnAccent
	}
	return nil
}

// designColourRoles are the six colours of a design, in the order failures
// are reported. A design holds exactly these.
var designColourRoles = []string{"ground", "surface", "ink", "muted", "accent", "on_accent"}

const (
	// designColourExample is the near-black the failure texts point to.
	designColourExample = "#15171C"
	designPureBlack     = "#000000"
	// designUnknownKeyReports caps how many keys of one object that the
	// design does not know are reported, and designKeyEchoRunes how much of
	// such a key (the caller's text) is repeated.
	designUnknownKeyReports = 3
	designKeyEchoRunes      = 40
)

// designColourPattern is the shape of a colour once its letters are upper
// case. Go's $ matches at the end of the text only, never before a final
// line feed.
var designColourPattern = regexp.MustCompile(`^#[0-9A-F]{6}$`)

// parseDesignColours reads the colours of a design. raw is the value of the
// design's "colors" key, empty when the key is absent. Each role must be a
// six-digit hex colour in either letter case, which is returned upper case,
// and never pure black; the pairs that carry text need designMinContrast.
// The failures are every problem found: the roles in order, then unknown
// keys, then low contrast. A role that failed is left empty in the result.
func parseDesignColours(raw json.RawMessage) (designColours, []designFailure) {
	var out designColours
	obj, fails := designGroupObject(raw, "colors", designColourRoles, "the six colours")
	if fails != nil {
		return out, fails
	}
	for _, role := range designColourRoles {
		v, has := obj[role]
		if !has {
			fails = append(fails, designFailure{
				Path: "design.colors", Key: role, code: pageDesignNotReady,
				Reason: "missing; set it to a six-digit hex colour such as " + designColourExample,
			})
			continue
		}
		hex, reason := designColour(v)
		if reason != "" {
			fails = append(fails, designFailure{Path: "design.colors", Key: role, code: pageCreateBadInput, Reason: reason})
			continue
		}
		if slot := out.slot(role); slot != nil {
			*slot = hex
		}
	}
	fails = append(fails, designUnknownKeys("design.colors", obj, designColourRoles)...)
	return out, append(fails, designContrastFailures(out)...)
}

// designColour reads one colour: a # and six hex digits in either letter
// case, returned upper case. The second result is the reason a value is
// refused, "" when it is not.
func designColour(raw json.RawMessage) (string, string) {
	s, ok := jsonStringOf(raw)
	if !ok {
		return "", "must be a six-digit hex colour such as " + designColourExample
	}
	hex := upperASCII(s)
	if !designColourPattern.MatchString(hex) {
		return "", "must be a six-digit hex colour such as " + designColourExample
	}
	if hex == designPureBlack {
		return "", "pure black is not allowed; use a near-black such as " + designColourExample
	}
	return hex, ""
}

// upperASCII upper-cases a to z and leaves every other character alone, so a
// letter from another script can never become one of A to F.
func upperASCII(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' {
			return r - ('a' - 'A')
		}
		return r
	}, s)
}

// designUnknownKeys reports the keys of obj that are not in allowed: sorted,
// at most designUnknownKeyReports of them, each cleaned and capped because
// the key is the caller's text.
func designUnknownKeys(path string, obj map[string]json.RawMessage, allowed []string) []designFailure {
	var unknown []string
	for k := range obj {
		if !slices.Contains(allowed, k) {
			unknown = append(unknown, k)
		}
	}
	slices.Sort(unknown)
	if len(unknown) > designUnknownKeyReports {
		unknown = unknown[:designUnknownKeyReports]
	}
	fails := make([]designFailure, 0, len(unknown))
	for _, k := range unknown {
		fails = append(fails, designFailure{
			Path: path, Key: humantext.CapRunes(humantext.Clean(k), designKeyEchoRunes), code: pageCreateBadInput,
			Reason: "not a key a design has; send only the keys listed", Allowed: slices.Clone(allowed),
		})
	}
	return fails
}

// designMinContrast is the least contrast ratio a pair that carries text may
// have.
const designMinContrast = 4.5

// designContrastPairs are the pairs a text colour sits on, in report order:
// the text role, then the role it is set on.
var designContrastPairs = []struct{ text, on string }{
	{"ink", "ground"},
	{"ink", "surface"},
	{"on_accent", "accent"},
}

// designContrast is the WCAG contrast ratio of two #RRGGBB colours, 1 to 21,
// the same whichever is passed first. A colour that is not #RRGGBB counts as
// black.
func designContrast(a, b string) float64 {
	la, lb := designLuminance(a), designLuminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// designLuminance is the WCAG relative luminance of a #RRGGBB colour.
func designLuminance(hex string) float64 {
	if len(hex) != 7 || hex[0] != '#' {
		return 0
	}
	var lin [3]float64
	for i := range lin {
		v, err := strconv.ParseUint(hex[1+2*i:3+2*i], 16, 8)
		if err != nil {
			return 0
		}
		c := float64(v) / 255
		if c <= 0.04045 {
			lin[i] = c / 12.92
		} else {
			lin[i] = math.Pow((c+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*lin[0] + 0.7152*lin[1] + 0.0722*lin[2]
}

// designContrastFailures judges the three pairs that carry text, each of
// which needs designMinContrast. A pair with a colour that is not set (its
// own failure says why) is not judged. The reason names the pair and gives
// the measured ratio cut, not rounded, to two places, so a ratio just under
// the minimum is never shown as the minimum.
func designContrastFailures(c designColours) []designFailure {
	colour := func(role string) string {
		if slot := c.slot(role); slot != nil {
			return *slot
		}
		return ""
	}
	var fails []designFailure
	for _, p := range designContrastPairs {
		text, on := colour(p.text), colour(p.on)
		if text == "" || on == "" {
			continue
		}
		ratio := designContrast(text, on)
		if ratio >= designMinContrast {
			continue
		}
		fails = append(fails, designFailure{
			Path: "design.colors", Key: p.text, code: pageDesignContrastLow,
			Reason: fmt.Sprintf("%s on %s has a contrast of %.2f:1; at least %g:1 is needed, so change %s or %s",
				p.text, p.on, math.Floor(ratio*100)/100, designMinContrast, p.text, p.on),
		})
	}
	return fails
}

// designFonts are the two font families of a design.
type designFonts struct {
	Heading string `json:"heading"`
	Body    string `json:"body"`
}

// designScale is the type scale of a design.
type designScale struct {
	BasePx int64   `json:"base_px"`
	Ratio  float64 `json:"ratio"`
}

// pageDesign is a design object that passed every check, its colours upper
// case. It marshals as the spec writes the object.
type pageDesign struct {
	Colors  designColours `json:"colors"`
	Fonts   designFonts   `json:"fonts"`
	Scale   designScale   `json:"scale"`
	Radius  string        `json:"radius"`
	Density string        `json:"density"`
}

// designProblem is why a design was refused: the refusal code and every
// failure found.
type designProblem struct {
	Code     string          `json:"code"`
	Failures []designFailure `json:"failures"`
}

// The design's own keys and closed sets, in the order failures are reported.
var (
	designTopKeys   = []string{"colors", "fonts", "scale", "radius", "density"}
	designFontKeys  = []string{"heading", "body"}
	designScaleKeys = []string{"base_px", "ratio"}
	designRadii     = []string{"none", "sm", "md", "lg"}
	designDensities = []string{"compact", "normal", "airy"}
)

const (
	// designFontSystem as a family means "set no family".
	designFontSystem = "system"
	designBasePxMin  = 15
	designBasePxMax  = 20
	designRatioMin   = 1.125
	designRatioMax   = 1.333
)

// designFontPattern is the closed set a font family name comes from: it is
// all the control plane lets through, and the agent checks the name against
// Elementor's registry. Go's $ matches at the end of the text only.
var designFontPattern = regexp.MustCompile(`^[A-Za-z0-9 ]{1,60}$`)

// validatePageDesign checks a design object as page-create and page-edit
// send it (elementor.md 2.3), before any agent call. raw is the value of the
// input's "design" key, empty when the key is absent.
//
// The design is ready only when every field is present, so a field left out
// is design_not_ready; a field that is there with a value it may not hold is
// the grammar's bad_input; two colours too close are design_contrast_low.
// Every failure is collected, in the spec's order (colours, then fonts,
// scale, radius, density, then keys the design does not have), and the
// problem's code is the first failure's. A refused design returns the zero
// pageDesign. Nothing in raw is rewritten: the colours of the result are the
// upper-case form of what was sent.
func validatePageDesign(raw json.RawMessage) (pageDesign, *designProblem) {
	if len(raw) == 0 {
		return pageDesign{}, newDesignProblem(designFailure{
			Key: "design", code: pageDesignNotReady, Allowed: slices.Clone(designTopKeys),
			Reason: "missing; send the whole design: colors, fonts, scale, radius and density",
		})
	}
	obj, ok := jsonObjectOf(raw)
	if !ok {
		return pageDesign{}, newDesignProblem(designFailure{
			Key: "design", code: pageCreateBadInput, Allowed: slices.Clone(designTopKeys),
			Reason: "must be an object holding colors, fonts, scale, radius and density",
		})
	}
	var d pageDesign
	var fails, more []designFailure
	d.Colors, more = parseDesignColours(obj["colors"])
	fails = append(fails, more...)
	d.Fonts, more = parseDesignFonts(obj["fonts"])
	fails = append(fails, more...)
	d.Scale, more = parseDesignScale(obj["scale"])
	fails = append(fails, more...)
	d.Radius, more = designChoice(obj, "radius", designRadii)
	fails = append(fails, more...)
	d.Density, more = designChoice(obj, "density", designDensities)
	fails = append(fails, more...)
	fails = append(fails, designUnknownKeys("design", obj, designTopKeys)...)
	if len(fails) > 0 {
		return pageDesign{}, newDesignProblem(fails...)
	}
	return d, nil
}

// newDesignProblem is the problem for these failures, coded as the first.
func newDesignProblem(fails ...designFailure) *designProblem {
	return &designProblem{Code: fails[0].code, Failures: fails}
}

// designGroupObject reads one of the design's nested objects (colors, fonts
// or scale). raw is empty when the key is absent: the design is not ready.
// A value that is not an object is bad input. Either way the failures are
// returned and the object is not; what says what the object holds.
func designGroupObject(raw json.RawMessage, key string, keys []string, what string) (map[string]json.RawMessage, []designFailure) {
	if len(raw) == 0 {
		return nil, []designFailure{{
			Path: "design", Key: key, code: pageDesignNotReady, Allowed: slices.Clone(keys),
			Reason: "missing; a design needs " + what,
		}}
	}
	obj, ok := jsonObjectOf(raw)
	if !ok {
		return nil, []designFailure{{
			Path: "design", Key: key, code: pageCreateBadInput, Allowed: slices.Clone(keys),
			Reason: "must be an object holding " + what,
		}}
	}
	return obj, nil
}

// parseDesignFonts reads the two font families. A family is a name of 1 to
// 60 letters, digits and spaces, or system.
func parseDesignFonts(raw json.RawMessage) (designFonts, []designFailure) {
	var out designFonts
	obj, fails := designGroupObject(raw, "fonts", designFontKeys, "a heading font and a body font")
	if fails != nil {
		return out, fails
	}
	for _, key := range designFontKeys {
		v, has := obj[key]
		if !has {
			fails = append(fails, designFailure{
				Path: "design.fonts", Key: key, code: pageDesignNotReady,
				Reason: "missing; set it to a font family name, or " + designFontSystem + " to set no family",
			})
			continue
		}
		name, ok := jsonStringOf(v)
		if !ok || !designFontPattern.MatchString(name) {
			fails = append(fails, designFailure{
				Path: "design.fonts", Key: key, code: pageCreateBadInput,
				Reason: "must be a font family name of 1 to 60 letters, digits and spaces, or " + designFontSystem + " to set no family",
			})
			continue
		}
		switch key {
		case "heading":
			out.Heading = name
		case "body":
			out.Body = name
		}
	}
	return out, append(fails, designUnknownKeys("design.fonts", obj, designFontKeys)...)
}

// parseDesignScale reads the type scale: base_px a whole number from 15 to
// 20 and ratio a number from 1.125 to 1.333.
func parseDesignScale(raw json.RawMessage) (designScale, []designFailure) {
	var out designScale
	obj, fails := designGroupObject(raw, "scale", designScaleKeys, "a base size and a ratio")
	if fails != nil {
		return out, fails
	}
	if v, has := obj["base_px"]; !has {
		fails = append(fails, designFailure{
			Path: "design.scale", Key: "base_px", code: pageDesignNotReady,
			Reason: fmt.Sprintf("missing; set it to a whole number from %d to %d", designBasePxMin, designBasePxMax),
		})
	} else if n, ok := jsonIntOf(v); !ok || n < designBasePxMin || n > designBasePxMax {
		fails = append(fails, designFailure{
			Path: "design.scale", Key: "base_px", code: pageCreateBadInput,
			Reason: fmt.Sprintf("must be a whole number from %d to %d", designBasePxMin, designBasePxMax),
		})
	} else {
		out.BasePx = n
	}
	if v, has := obj["ratio"]; !has {
		fails = append(fails, designFailure{
			Path: "design.scale", Key: "ratio", code: pageDesignNotReady,
			Reason: fmt.Sprintf("missing; set it to a number from %g to %g", designRatioMin, designRatioMax),
		})
	} else if r, ok := jsonNumberOf(v); !ok || r < designRatioMin || r > designRatioMax {
		fails = append(fails, designFailure{
			Path: "design.scale", Key: "ratio", code: pageCreateBadInput,
			Reason: fmt.Sprintf("must be a number from %g to %g", designRatioMin, designRatioMax),
		})
	} else {
		out.Ratio = r
	}
	return out, append(fails, designUnknownKeys("design.scale", obj, designScaleKeys)...)
}

// designChoice reads a field of the design that holds one of a closed set of
// strings (radius, density).
func designChoice(obj map[string]json.RawMessage, key string, allowed []string) (string, []designFailure) {
	raw, has := obj[key]
	if !has {
		return "", []designFailure{{
			Path: "design", Key: key, code: pageDesignNotReady, Allowed: slices.Clone(allowed),
			Reason: "missing; set it to one of the listed values",
		}}
	}
	if s, ok := jsonStringOf(raw); ok && slices.Contains(allowed, s) {
		return s, nil
	}
	return "", []designFailure{{
		Path: "design", Key: key, code: pageCreateBadInput, Allowed: slices.Clone(allowed),
		Reason: "must be one of the listed values",
	}}
}
