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

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
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

// pageCreateFacts are what the control plane reads from a valid input.
type pageCreateFacts struct {
	postType string
	editor   string
	title    string
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
)

// pageWalk carries the page-wide counters through one validation.
type pageWalk struct {
	nodes, images, buttons, tables int
	blockOnly                      bool
	usesLayout                     bool
	ids                            []int64
	seen                           map[int64]struct{}
}

// validatePageCreateInput checks input against the outline grammar. It
// returns the facts, or the refusal code of the first problem.
func validatePageCreateInput(input []byte) (pageCreateFacts, string) {
	top, ok := jsonObjectOf(input)
	if !ok {
		return pageCreateFacts{}, pageCreateBadInput
	}
	for k := range top {
		switch k {
		case "post_type", "editor", "title", "outline":
		default:
			return pageCreateFacts{}, pageCreateBadInput
		}
	}
	var f pageCreateFacts
	if f.postType, ok = jsonStringOf(top["post_type"]); !ok || (f.postType != "page" && f.postType != "post") {
		return pageCreateFacts{}, pageCreateBadInput
	}
	if f.editor, ok = jsonStringOf(top["editor"]); !ok || (f.editor != pageEditorBlocks && f.editor != pageEditorClassic) {
		return pageCreateFacts{}, pageCreateBadInput
	}
	if f.title, ok = jsonStringOf(top["title"]); !ok || strings.TrimSpace(f.title) == "" {
		return pageCreateFacts{}, pageCreateBadInput
	}
	outline, ok := jsonArrayOf(top["outline"])
	if !ok || len(outline) == 0 || len(outline) > pageCreateMaxTopLevelNodes {
		return pageCreateFacts{}, pageCreateBadInput
	}
	w := pageWalk{seen: map[int64]struct{}{}}
	for _, n := range outline {
		if code := w.node(n, pageAtTop); code != "" {
			return pageCreateFacts{}, code
		}
	}
	if f.editor == pageEditorClassic && w.blockOnly {
		return pageCreateFacts{}, pageCreateNeedsBlockEditor
	}
	f.usesLayout, f.mediaIDs = w.usesLayout, w.ids
	return f, ""
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
	switch kind {
	case "heading":
		level, ok := jsonIntOf(n["level"])
		if !keysWithin(n, "type", "level", "text") || !ok || level < 2 || level > 4 || !isJSONString(n["text"]) {
			return pageCreateBadInput
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
		if at != pageAtTop {
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
	if w.images++; w.images > pageCreateMaxImages {
		return pageCreateLayoutInvalid
	}
	if _, dup := w.seen[id]; !dup {
		w.seen[id] = struct{}{}
		w.ids = append(w.ids, id)
	}
	return ""
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
		if !pageLinkUsable(url) {
			return pageCreateLinkInvalid
		}
		if raw, has := b["style"]; has {
			switch style, _ := jsonStringOf(raw); style {
			case "fill", "outline":
			default:
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
	for _, child := range list {
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
)

// pageURLShapeUsable is the shape every link and image address shares: 1 to
// 2048 bytes of ASCII from the link character set, every % followed by two
// hex digits.
func pageURLShapeUsable(u string) bool {
	if len(u) < 1 || len(u) > pageCreateMaxURLBytes || !pageURLCharset.MatchString(u) {
		return false
	}
	for i := 0; i < len(u); i++ {
		if u[i] == '%' && (i+2 >= len(u) || !isHexDigit(u[i+1]) || !isHexDigit(u[i+2])) {
			return false
		}
	}
	return true
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

// pageLinkUsable is a button link the grammar accepts: an https:// address
// (lowercase scheme, a DNS host with at least one dot, an optional port
// 1..65535, no user name or password) or a path on the site that starts with
// one '/'.
func pageLinkUsable(u string) bool {
	if !pageURLShapeUsable(u) {
		return false
	}
	if u[0] == '/' {
		return len(u) == 1 || (u[1] != '/' && u[1] != '\\')
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
// in the shared shape.
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
// wpmgr/page-create input: MinAgentVersionForPageLayout when the outline
// holds a block beyond heading, paragraph and list (or the input cannot be
// read), MinAgentVersionForPageCreate otherwise. The run path and the
// dispatch worker both decide with it.
func PageCreateAgentFloor(input []byte) string {
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

// pageCardFacts is card_facts for a page-create request with images: what
// the approval card shows for each image. Every string is the site's,
// cleaned and capped; the base fingerprint binds the raw facts.
type pageCardFacts struct {
	Kind  string          `json:"kind"`
	Media []PageCardMedia `json:"media"`
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
// nil when the outline has no image.
func pageCardFactsJSON(media []pageMediaFact) ([]byte, error) {
	if len(media) == 0 {
		return nil, nil
	}
	card := pageCardFacts{Kind: pageCardFactsKind, Media: make([]PageCardMedia, 0, len(media))}
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
	top, ok := jsonObjectOf(stored)
	if !ok || len(top) != 2 {
		return nil, false
	}
	if kind, ok := jsonStringOf(top["kind"]); !ok || kind != pageCardFactsKind {
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
		if c.ID < 1 || c.ID > pageCreateMaxAttachmentID || c.Width < 0 || c.Height < 0 {
			return nil, false
		}
		out = append(out, c)
	}
	return out, true
}
