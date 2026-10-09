package mcp

// The structure of an Elementor classic tree, as the agent's
// ElementorClassicMapper::project() and Projection answer it, recomputed by
// the control plane from a tree whose bytes it verified (R2).
//
// A node the mapper classifies is
//
//	{ref, parent, kind, level?, editable: [field...], from_the_site: {field: text}}
//
// and every other node {ref, parent, kind: "locked", label}, its children not
// walked. Text under from_the_site is the stored text with its character
// references decoded as PHP's html_entity_decode(ENT_QUOTES | ENT_HTML5)
// decodes them. The answer {node_count, truncated, nodes} keeps at most
// pageStructureMaxNodes nodes and pageStructureMaxBytes bytes, measured as
// PHP's json_encode writes them, cut after a whole node.

import (
	"encoding/json"
	"errors"
	"html"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// pageStructureMaxBytes is BuilderContract::MAX_STRUCTURE_BYTES.
const pageStructureMaxBytes = 65536

// projectionRoot is Projection::ROOT, the parent of a top-level node.
const projectionRoot = "root"

// projectionKinds is BuilderContract::KINDS without "locked".
var projectionKinds = map[string]bool{
	"heading": true, "paragraph": true, "list": true, "quote": true, "table": true, "image": true,
	"buttons": true, "separator": true, "spacer": true, "group": true, "columns": true, "column": true,
	"section": true,
}

// Label keys and words of ElementorClassicMapper::LOCKED_LABELS.
const (
	elementorLabelFallback   = "*"
	elementorLabelDynamic    = "wpmgr:dynamic"
	elementorLabelCustomText = "wpmgr:custom-text"
	elementorLabelAtomic     = "wpmgr:atomic"
	elementorLabelWPWidget   = "wpmgr:wp-widget"
)

var elementorLockedLabels = map[string]string{
	elementorLabelFallback:   "Elementor element WPMgr does not edit",
	elementorLabelDynamic:    "Elementor element with dynamic content or custom code",
	elementorLabelCustomText: "Elementor text with formatting WPMgr does not edit",
	elementorLabelAtomic:     "Elementor v4 element",
	elementorLabelWPWidget:   "WordPress widget",
	"html":                   "Elementor HTML code",
	"shortcode":              "Elementor shortcode",
	"template":               "Elementor saved template",
	"text-path":              "Elementor text path",
	"menu-anchor":            "Elementor menu anchor",
	"sidebar":                "Elementor sidebar",
	"video":                  "Elementor video",
	"audio":                  "Elementor audio",
	"icon":                   "Elementor icon",
	"icon-box":               "Elementor icon box",
	"icon-list":              "Elementor icon list",
	"image-box":              "Elementor image box",
	"image-gallery":          "Elementor image gallery",
	"image-carousel":         "Elementor image carousel",
	"counter":                "Elementor counter",
	"progress":               "Elementor progress bar",
	"testimonial":            "Elementor testimonial",
	"tabs":                   "Elementor tabs",
	"nested-tabs":            "Elementor tabs",
	"accordion":              "Elementor accordion",
	"nested-accordion":       "Elementor accordion",
	"toggle":                 "Elementor toggle",
	"social-icons":           "Elementor social icons",
	"alert":                  "Elementor alert",
	"google_maps":            "Elementor map",
	"star-rating":            "Elementor star rating",
	"rating":                 "Elementor rating",
	"form":                   "Elementor form",
	"posts":                  "Elementor posts",
	"nav-menu":               "Elementor menu",
	"slides":                 "Elementor slides",
	"price-table":            "Elementor price table",
	"call-to-action":         "Elementor call to action",
	"countdown":              "Elementor countdown",
}

// elementorLockedLabelWords reports whether s is one of WPMgr's label texts.
func elementorLockedLabelWords(s string) bool {
	for _, words := range elementorLockedLabels {
		if s == words {
			return true
		}
	}
	return false
}

// elementorKnownTypes are the element types of ElementorClassicMapper::ALLOWED_KEYS.
var elementorKnownTypes = map[string]bool{
	"container": true, "section": true, "column": true, "heading": true, "text-editor": true,
	"button": true, "image": true, "divider": true, "spacer": true,
}

var elementorLayoutTypes = map[string]bool{"container": true, "section": true, "column": true}

// elementorDynamicKeys are ElementorClassicMapper::DYNAMIC_KEYS.
var elementorDynamicKeys = map[string]bool{
	"__dynamic__": true, "custom_css": true, "_attributes": true, "custom_attributes": true,
}

// elementorTextTemplates are ElementorClassicMapper::TEMPLATES, in order.
var elementorTextTemplates = []struct {
	kind string
	re   *regexp.Regexp
}{
	{"paragraph", regexp.MustCompile(`^<p>[^<>]*</p>$`)},
	{"list", regexp.MustCompile(`^(?:<ul>(?:<li>[^<>]*</li>)+</ul>|<ol>(?:<li>[^<>]*</li>)+</ol>)$`)},
	{"quote", regexp.MustCompile(`^<blockquote>(?:<p>[^<>]*</p>)+(?:<cite>[^<>]*</cite>)?</blockquote>$`)},
	{"table", regexp.MustCompile(`^<table>(?:<thead><tr>(?:<th>[^<>]*</th>)+</tr></thead>)?<tbody>(?:<tr>(?:<td>[^<>]*</td>)+</tr>)+</tbody></table>$`)},
}

var elementorHeaderLevel = regexp.MustCompile(`^h([1-6])$`)

// projField is one from_the_site member, in the order the agent adds it.
type projField struct {
	field string
	text  string
}

// projNode is one node of a projection.
type projNode struct {
	ref      string
	parent   string
	kind     string
	level    int64 // 0: none
	editable []string
	text     []projField
	label    string // locked only
}

func (n projNode) locked() bool { return n.kind == "locked" }

// encodePHP is the node as PHP's json_encode writes it.
func (n projNode) encodePHP() (string, bool) {
	var b strings.Builder
	ref, ok1 := phpJSONString(n.ref)
	parent, ok2 := phpJSONString(n.parent)
	kind, ok3 := phpJSONString(n.kind)
	if !ok1 || !ok2 || !ok3 {
		return "", false
	}
	b.WriteString(`{"ref":` + ref + `,"parent":` + parent + `,"kind":` + kind)
	if n.locked() {
		label, ok := phpJSONString(n.label)
		if !ok {
			return "", false
		}
		b.WriteString(`,"label":` + label + `}`)
		return b.String(), true
	}
	if n.level > 0 {
		b.WriteString(`,"level":` + strconv.FormatInt(n.level, 10))
	}
	b.WriteString(`,"editable":[`)
	for i, f := range n.editable {
		if i > 0 {
			b.WriteByte(',')
		}
		s, _ := phpJSONString(f)
		b.WriteString(s)
	}
	b.WriteString(`],"from_the_site":{`)
	for i, f := range n.text {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := phpJSONString(f.field)
		v, ok := phpJSONString(f.text)
		if !ok {
			return "", false
		}
		b.WriteString(k + ":" + v)
	}
	b.WriteString(`}}`)
	return b.String(), true
}

// wire is the node as the answer carries it.
func (n projNode) wire() map[string]any {
	out := map[string]any{"ref": n.ref, "parent": n.parent, "kind": n.kind}
	if n.locked() {
		out["label"] = n.label
		return out
	}
	if n.level > 0 {
		out["level"] = n.level
	}
	editable := make([]string, 0, len(n.editable))
	editable = append(editable, n.editable...)
	out["editable"] = editable
	text := make(map[string]string, len(n.text))
	for _, f := range n.text {
		text[f.field] = f.text
	}
	out["from_the_site"] = text
	return out
}

// projection is a projection being built: nodes parent first, page order.
type projection struct {
	nodes []projNode
	refs  map[string]int
}

var errProjection = errors.New("the tree cannot be projected")

func newProjection() *projection { return &projection{refs: map[string]int{}} }

func (p *projection) checkPlace(ref, parent string) error {
	if ref == projectionRoot || !pageEditRefPattern.MatchString(ref) {
		return errProjection
	}
	if _, taken := p.refs[ref]; taken {
		return errProjection
	}
	if parent != projectionRoot {
		if _, known := p.refs[parent]; !known {
			return errProjection
		}
	}
	return nil
}

func (p *projection) add(n projNode) error {
	if err := p.checkPlace(n.ref, n.parent); err != nil {
		return err
	}
	p.refs[n.ref] = len(p.nodes)
	p.nodes = append(p.nodes, n)
	return nil
}

func (p *projection) addLocked(ref, parent, labelKey string) error {
	label, ok := elementorLockedLabels[labelKey]
	if !ok {
		label = elementorLockedLabels[elementorLabelFallback]
	}
	return p.add(projNode{ref: ref, parent: parent, kind: "locked", label: label})
}

// phpGet is $obj[$key] ?? null for a decoded PHP array: the last member
// named key of an object, nil for anything else.
func phpGet(v any, key string) (any, bool) {
	obj, ok := v.(phpObject)
	if !ok {
		if list, isList := v.(phpList); isList {
			i, err := strconv.Atoi(key)
			if err == nil && i >= 0 && i < len(list) && strconv.Itoa(i) == key {
				return list[i], list[i] != nil
			}
		}
		return nil, false
	}
	for i := len(obj) - 1; i >= 0; i-- {
		if obj[i].key == key {
			return obj[i].value, obj[i].value != nil
		}
	}
	return nil, false
}

// phpIsArray is PHP's is_array for a decoded value.
func phpIsArray(v any) bool {
	switch v.(type) {
	case phpObject, phpList:
		return true
	}
	return false
}

func phpStringAt(v any, key string) (string, bool) {
	x, _ := phpGet(v, key)
	s, ok := x.(string)
	return s, ok
}

// elementorProject is ElementorClassicMapper::project() over a decoded tree
// (phpDecodeDocument with scalars).
func elementorProject(tree any) (*projection, error) {
	p := newProjection()
	list, ok := tree.(phpList)
	if !ok {
		if obj, isObj := tree.(phpObject); !isObj || len(obj) != 0 {
			return nil, errProjection
		}
	}
	if err := elementorProjectNodes(list, projectionRoot, "", p); err != nil {
		return nil, err
	}
	return p, nil
}

// phpValues is foreach ($nodes as $node) for a decoded PHP array.
func phpValues(v any) []any {
	switch x := v.(type) {
	case phpList:
		return x
	case phpObject:
		out := make([]any, 0, len(x))
		for _, pair := range x {
			out = append(out, pair.value)
		}
		return out
	}
	return nil
}

func elementorProjectNodes(nodes []any, parent, parentKind string, p *projection) error {
	for _, node := range nodes {
		if !phpIsArray(node) {
			return errProjection
		}
		id, ok := phpStringAt(node, "id")
		if !ok {
			return errProjection
		}
		elTypeRaw, _ := phpGet(node, "elType")
		elType, elTypeIsString := elTypeRaw.(string)
		var typeRaw any = elTypeRaw
		if elTypeIsString && elType == "widget" {
			typeRaw, _ = phpGet(node, "widgetType")
		}
		settings, hasSettings := phpGet(node, "settings")
		if !hasSettings {
			settings = phpList{}
		}
		typ, typeIsString := typeRaw.(string)
		if !typeIsString || !phpIsArray(settings) {
			if err := p.addLocked(id, parent, elementorLabelFallback); err != nil {
				return err
			}
			continue
		}
		known := elementorKnownTypes[typ] && (elTypeIsString && elType == "widget") == !elementorLayoutTypes[typ]
		if !known {
			if err := p.addLocked(id, parent, elementorLabelKey(typ)); err != nil {
				return err
			}
			continue
		}
		if elementorHoldsDynamicKey(settings, 1) {
			if err := p.addLocked(id, parent, elementorLabelDynamic); err != nil {
				return err
			}
			continue
		}
		n, classified := elementorClassify(typ, settings, parentKind)
		if !classified {
			key := elementorLabelKey(typ)
			if typ == "text-editor" {
				key = elementorLabelCustomText
			}
			if err := p.addLocked(id, parent, key); err != nil {
				return err
			}
			continue
		}
		n.ref, n.parent = id, parent
		if err := p.add(n); err != nil {
			return err
		}
		if elementorLayoutTypes[typ] {
			children, _ := phpGet(node, "elements")
			if phpIsArray(children) {
				if err := elementorProjectNodes(phpValues(children), id, n.kind, p); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// elementorClassify is ElementorClassicMapper::classify().
func elementorClassify(typ string, settings any, parentKind string) (projNode, bool) {
	switch typ {
	case "container":
		if parentKind == "columns" {
			return projNode{kind: "column"}, true
		}
		if dir, _ := phpStringAt(settings, "flex_direction"); dir == "row" {
			return projNode{kind: "columns"}, true
		}
		return projNode{kind: "group"}, true
	case "section":
		return projNode{kind: "section"}, true
	case "column":
		return projNode{kind: "column"}, true
	case "heading":
		title, ok := phpStringAt(settings, "title")
		if !ok {
			return projNode{}, false
		}
		size := "h2"
		if raw, set := phpGet(settings, "header_size"); set {
			s, isString := raw.(string)
			if !isString {
				return projNode{}, false
			}
			size = s
		}
		var level int64
		if m := elementorHeaderLevel.FindStringSubmatch(size); m != nil {
			level, _ = strconv.ParseInt(m[1], 10, 64)
		}
		return projNode{kind: "heading", level: level, editable: []string{"text"},
			text: []projField{{"text", phpHTMLEntityDecode(title)}}}, true
	case "text-editor":
		html, ok := phpStringAt(settings, "editor")
		if !ok {
			return projNode{}, false
		}
		for _, t := range elementorTextTemplates {
			if !t.re.MatchString(html) {
				continue
			}
			if t.kind == "paragraph" {
				return projNode{kind: "paragraph", editable: []string{"text"},
					text: []projField{{"text", phpHTMLEntityDecode(html[3 : len(html)-4])}}}, true
			}
			return projNode{kind: t.kind}, true
		}
		return projNode{}, false
	case "button":
		text, ok1 := phpStringAt(settings, "text")
		link, _ := phpGet(settings, "link")
		url, ok2 := phpStringAt(link, "url")
		if !ok1 || !ok2 {
			return projNode{}, false
		}
		return projNode{kind: "buttons", editable: []string{"text", "url"},
			text: []projField{{"text", phpHTMLEntityDecode(text)}, {"url", phpHTMLEntityDecode(url)}}}, true
	case "image":
		caption, isString := phpStringAt(settings, "caption")
		if source, _ := phpStringAt(settings, "caption_source"); source == "custom" && isString {
			return projNode{kind: "image", editable: []string{"caption"},
				text: []projField{{"caption", phpHTMLEntityDecode(caption)}}}, true
		}
		return projNode{kind: "image"}, true
	case "divider":
		return projNode{kind: "separator"}, true
	case "spacer":
		return projNode{kind: "spacer"}, true
	}
	return projNode{}, false
}

// elementorHoldsDynamicKey is ElementorClassicMapper::holdsDynamicKey().
func elementorHoldsDynamicKey(settings any, depth int) bool {
	if depth > 8 {
		return true
	}
	visit := func(key string, value any) bool {
		if elementorDynamicKeys[strings.ToLower(key)] {
			return true
		}
		return phpIsArray(value) && elementorHoldsDynamicKey(value, depth+1)
	}
	switch x := settings.(type) {
	case phpObject:
		for _, pair := range x {
			if visit(pair.key, pair.value) {
				return true
			}
		}
	case phpList:
		for i, v := range x {
			if visit(strconv.Itoa(i), v) {
				return true
			}
		}
	}
	return false
}

// elementorLabelKey is ElementorClassicMapper::labelKey().
func elementorLabelKey(typ string) string {
	if _, ok := elementorLockedLabels[typ]; ok {
		return typ
	}
	if strings.HasPrefix(typ, "e-") {
		return elementorLabelAtomic
	}
	if strings.HasPrefix(typ, "wp-widget-") {
		return elementorLabelWPWidget
	}
	return elementorLabelFallback
}

// answer is Projection::toArray() with maxNodes and maxBytes: the node
// count, whether nodes were left out, and the nodes kept.
func (p *projection) answer(maxNodes, maxBytes int) (map[string]any, bool) {
	limit := maxNodes
	if limit > pageStructureMaxNodes {
		limit = pageStructureMaxNodes
	}
	if limit < 0 {
		limit = 0
	}
	total := len(p.nodes)
	bytes := len(`{"node_count":` + strconv.Itoa(total) + `,"truncated":false,"nodes":[]}`)
	kept := make([]map[string]any, 0, len(p.nodes))
	for _, n := range p.nodes {
		if len(kept) >= limit {
			break
		}
		enc, ok := n.encodePHP()
		if !ok {
			return nil, false
		}
		size := len(enc)
		if len(kept) > 0 {
			size++
		}
		if bytes+size > maxBytes {
			break
		}
		bytes += size
		kept = append(kept, n.wire())
	}
	return map[string]any{"node_count": total, "truncated": len(kept) < total, "nodes": kept}, true
}

// ---------------------------------------------------------------------------
// PHP's html_entity_decode($s, ENT_QUOTES | ENT_HTML5, 'UTF-8')
// ---------------------------------------------------------------------------

// phpHTMLEntityDecode decodes a named reference of the HTML5 list (the
// trailing ";" required) and a decimal or hexadecimal reference to a code
// point HTML5 allows as one; anything else is left as it is.
func phpHTMLEntityDecode(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '&' || i+3 >= len(s) {
			b.WriteByte(s[i])
			i++
			continue
		}
		if dec, n, ok := phpEntityAt(s[i:]); ok {
			b.WriteString(dec)
			i += n
			continue
		}
		b.WriteByte('&')
		i++
	}
	return b.String()
}

func phpEntityAt(s string) (string, int, bool) {
	if s[1] == '#' {
		return phpNumericEntityAt(s)
	}
	j := 1
	for j < len(s) && (s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z' || s[j] >= '0' && s[j] <= '9') {
		j++
	}
	if j == 1 || j >= len(s) || s[j] != ';' {
		return "", 0, false
	}
	token := s[:j+1]
	out := html.UnescapeString(token)
	// A whole-name match is the decoded text alone; a longest-prefix match
	// of a legacy name keeps the rest of the token, and so its ";".
	if out == token || (strings.HasSuffix(out, ";") && token != "&semi;") {
		return "", 0, false
	}
	return out, j + 1, true
}

func phpNumericEntityAt(s string) (string, int, bool) {
	j := 2
	hex := j < len(s) && (s[j] == 'x' || s[j] == 'X')
	if hex {
		j++
	}
	start := j
	var code int64
	for j < len(s) {
		c := s[j]
		var d int64
		switch {
		case c >= '0' && c <= '9':
			d = int64(c - '0')
		case hex && c >= 'a' && c <= 'f':
			d = int64(c-'a') + 10
		case hex && c >= 'A' && c <= 'F':
			d = int64(c-'A') + 10
		default:
			d = -1
		}
		if d < 0 {
			break
		}
		if code <= 0x10FFFF {
			if hex {
				code = code*16 + d
			} else {
				code = code*10 + d
			}
		}
		j++
	}
	if j == start || j >= len(s) || s[j] != ';' || code > 0x10FFFF {
		return "", 0, false
	}
	cp := rune(code)
	allowed := (cp >= 0x20 && cp <= 0x7E) || (cp >= 0x09 && cp <= 0x0D && cp != 0x0B) ||
		(cp >= 0xA0 && cp <= 0xD7FF) ||
		(cp >= 0xE000 && cp <= 0x10FFFF && (cp&0xFFFF) < 0xFFFE && (cp < 0xFDD0 || cp > 0xFDEF))
	if !allowed || cp == 0x0D {
		return "", 0, false
	}
	var buf [utf8.UTFMax]byte
	n := utf8.EncodeRune(buf[:], cp)
	return string(buf[:n]), j + 1, true
}

// projectionJSON is v as Go encodes it, decoded again with numbers kept as
// json.Number: the form two answers are compared in.
func projectionJSON(v any) (any, bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	return decodeLoose(b)
}

func decodeLoose(b []byte) (any, bool) {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	var out any
	if dec.Decode(&out) != nil || dec.More() {
		return nil, false
	}
	return out, true
}
