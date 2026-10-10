package mcp

// The Elementor classic tree, recomputed by the control plane (R2).
//
// The precheck of a page built with Elementor answers the tree the agent's
// classic mapper built. The control plane builds the same tree itself, from
// the input it holds, the request id it sent, the layout the site uses and
// the verified image facts, and accepts the precheck only when the site's
// tree is that tree and the preview digest is the one this tree gives. The
// card shows the outline the AI chose; the write rebuilds the tree and
// re-checks that digest before anything is saved, so the page Elementor
// saves is the outline the person approved.
//
// The mapping, fixed for every supported Elementor version by the agent's
// goldens (apps/agent/tests/fixtures/ability-run/elementor-classic-*.json):
//
//	top-level leaves in a row  one boxed column container (containers) or a
//	                           section "10" with one column "100" (sections)
//	group                      the same wrapper around the group's children
//	columns (n = 2..4)         a row container of n full-width child
//	                           containers with a percentage width (the
//	                           outline's widths, else 100/n rounded down), or
//	                           a section "n0" of columns with _column_size
//	                           and, with widths, _inline_size; inside a group
//	                           the row container or section is an inner one
//	heading                    heading: title, h2|h3|h4
//	paragraph, list, quote,    one text-editor holding a fixed tag template
//	table                      with no attributes
//	buttons                    one button per button: text, link, align
//	image                      image: the library address, size "large",
//	                           align center when asked, the caption
//	separator                  divider
//	spacer                     spacer of 24, 48 or 96 px
//
// Every text is HTML-escaped (& < > and the square brackets as numeric
// references). Numbers are strings; no setting is a boolean or null. Node
// ids are the first 7 hex characters of sha256(json_encode([
// "wpmgr.builder.id.v1", request_id, node_path, counter])), parent before
// children in document order, the counter moved past an id already issued;
// a wrapper WPMgr adds is keyed by its first node's path plus "#wrapper".

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

const (
	elementorIDDomain      = "wpmgr.builder.id.v1"
	elementorIDLength      = 7
	elementorIDMaxAttempts = 1000
	elementorFormatClassic = "classic"
	elementorLayoutBoxes   = "containers"
	elementorLayoutRows    = "sections"
	elementorGapPx         = "24"
	// elementorTreeMaxDepth bounds the decode of the site's tree. The deepest
	// tree the mapper builds nests about a dozen levels (a group's section,
	// its column, an inner section, its column, a widget, its settings).
	elementorTreeMaxDepth = 32
)

var (
	elementorSpacerPx    = map[string]string{"small": "24", "medium": "48", "large": "96"}
	elementorHeaderSizes = map[int64]string{2: "h2", 3: "h3", 4: "h4"}
	elementorStructures  = map[int]string{1: "10", 2: "20", 3: "30", 4: "40"}
	elementorColumnSizes = map[int]string{1: "100", 2: "50", 3: "33", 4: "25"}
	// elementorVersionPattern is the Elementor version a precheck may name;
	// the card shows it as the site's text.
	elementorVersionPattern = regexp.MustCompile(`^[0-9]{1,4}\.[0-9]{1,4}(\.[0-9]{1,4})?([.-][0-9A-Za-z]{1,16}){0,2}$`)
	// elementorHTMLText escapes plain text for a field Elementor prints as
	// HTML, byte for byte as the agent stores it.
	elementorHTMLText = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "[", "&#091;", "]", "&#093;")
)

// ---------------------------------------------------------------------------
// PHP's json_encode, for the tree's canonical bytes
// ---------------------------------------------------------------------------

// phpPair is one member of a phpObject.
type phpPair struct {
	key   string
	value any
}

// phpObject is a PHP array with string keys, members in insertion order:
// json_encode writes it as an object, or as [] when it is empty.
type phpObject []phpPair

// phpList is a PHP list: json_encode writes it as an array.
type phpList []any

// phpEncode appends v as PHP's json_encode with no flags writes it. v is a
// phpObject, a phpList, a string, a bool, a json.Number (written as its own
// text) or nil (null); anything else is refused.
func phpEncode(b *strings.Builder, v any) bool {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
		return true
	case json.Number:
		if x == "" {
			return false
		}
		b.WriteString(string(x))
		return true
	case string:
		s, ok := phpJSONString(x)
		b.WriteString(s)
		return ok
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
		return true
	case phpList:
		b.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if !phpEncode(b, item) {
				return false
			}
		}
		b.WriteByte(']')
		return true
	case phpObject:
		if len(x) == 0 {
			b.WriteString("[]")
			return true
		}
		b.WriteByte('{')
		for i, p := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			k, ok := phpJSONString(p.key)
			if !ok {
				return false
			}
			b.WriteString(k)
			b.WriteByte(':')
			if !phpEncode(b, p.value) {
				return false
			}
		}
		b.WriteByte('}')
		return true
	}
	return false
}

// phpEncodeBytes is phpEncode into a new buffer.
func phpEncodeBytes(v any) ([]byte, bool) {
	var b strings.Builder
	if !phpEncode(&b, v) {
		return nil, false
	}
	return []byte(b.String()), true
}

// phpCanonicalJSON re-encodes JSON text as PHP's json_encode with no flags
// writes the value it decodes to: members in their order, strings escaped
// as PHP escapes them, an empty object as []. A number, null, trailing data
// or nesting beyond maxDepth is refused: the trees compared here hold none.
func phpCanonicalJSON(raw []byte, maxDepth int) ([]byte, bool) {
	v, ok := phpDecodeDocument(raw, maxDepth, false)
	if !ok {
		return nil, false
	}
	return phpEncodeBytes(v)
}

// phpDecodeDocument decodes one JSON value into phpObject, phpList, string
// and bool values; with scalars, numbers (as json.Number, their text kept)
// and null (nil) too. Trailing data or nesting beyond maxDepth is refused.
func phpDecodeDocument(raw []byte, maxDepth int, scalars bool) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, ok := phpDecodeAny(dec, maxDepth, scalars)
	if !ok {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return v, true
}

func phpDecodeValue(dec *json.Decoder, depth int) (any, bool) {
	return phpDecodeAny(dec, depth, false)
}

func phpDecodeAny(dec *json.Decoder, depth int, scalars bool) (any, bool) {
	tok, err := dec.Token()
	if err != nil {
		return nil, false
	}
	switch t := tok.(type) {
	case string:
		return t, true
	case bool:
		return t, true
	case json.Number:
		return t, scalars
	case nil:
		return nil, scalars
	case json.Delim:
		if depth <= 0 {
			return nil, false
		}
		switch t {
		case '[':
			list := phpList{}
			for dec.More() {
				item, ok := phpDecodeAny(dec, depth-1, scalars)
				if !ok {
					return nil, false
				}
				list = append(list, item)
			}
			if end, err := dec.Token(); err != nil || end != json.Delim(']') {
				return nil, false
			}
			return list, true
		case '{':
			obj := phpObject{}
			for dec.More() {
				keyTok, err := dec.Token()
				key, isString := keyTok.(string)
				if err != nil || !isString {
					return nil, false
				}
				val, ok := phpDecodeAny(dec, depth-1, scalars)
				if !ok {
					return nil, false
				}
				obj = append(obj, phpPair{key, val})
			}
			if end, err := dec.Token(); err != nil || end != json.Delim('}') {
				return nil, false
			}
			return obj, true
		}
	}
	return nil, false
}

// ---------------------------------------------------------------------------
// The mapper
// ---------------------------------------------------------------------------

// elementorIDs issues the node ids of one request.
type elementorIDs struct {
	requestID string
	taken     map[string]bool
}

func (s *elementorIDs) next(path string) (string, bool) {
	dom, ok1 := phpJSONString(elementorIDDomain)
	req, ok2 := phpJSONString(s.requestID)
	at, ok3 := phpJSONString(path)
	if !ok1 || !ok2 || !ok3 || s.requestID == "" {
		return "", false
	}
	for counter := 0; counter < elementorIDMaxAttempts; counter++ {
		id := sha256Hex([]byte("[" + dom + "," + req + "," + at + "," + strconv.Itoa(counter) + "]"))[:elementorIDLength]
		if !s.taken[id] {
			s.taken[id] = true
			return id, true
		}
	}
	return "", false
}

// elementorBuild maps one valid outline.
type elementorBuild struct {
	ids        *elementorIDs
	urls       map[int64]string
	containers bool
	// anyImage maps an image whose attachment id has no known address
	// with an empty address: for comparing projections, which never show
	// an image's address.
	anyImage bool
}

// elementorLeafAt is a leaf with its outline path.
type elementorLeafAt struct {
	path string
	node map[string]json.RawMessage
}

// elementorClassicTree is the Elementor classic tree the agent builds for a
// valid page-create input: node ids from requestID, containers or sections,
// and each image's address from media. ok is false for an input the mapper
// refuses.
func elementorClassicTree(input []byte, requestID string, containers bool, media []pageMediaFact) (phpList, bool) {
	top, ok := jsonObjectOf(input)
	if !ok {
		return nil, false
	}
	outline, ok := jsonArrayOf(top["outline"])
	if !ok || len(outline) == 0 {
		return nil, false
	}
	urls := make(map[int64]string, len(media))
	for _, m := range media {
		urls[m.ID] = m.URL
	}
	b := &elementorBuild{ids: &elementorIDs{requestID: requestID, taken: map[string]bool{}}, urls: urls, containers: containers}
	return b.top(outline, "outline")
}

// top maps outline nodes as the top of a page: a run of leaves in one
// wrapper, a group or a columns node as a top-level element. A node's path
// is listPath followed by "[i]".
func (b *elementorBuild) top(outline []json.RawMessage, listPath string) (phpList, bool) {
	tree := phpList{}
	var run []elementorLeafAt
	flush := func() bool {
		if len(run) == 0 {
			return true
		}
		w, ok := b.wrapper(run)
		run = nil
		if ok {
			tree = append(tree, w)
		}
		return ok
	}
	for i, raw := range outline {
		path := fmt.Sprintf("%s[%d]", listPath, i)
		n, ok := jsonObjectOf(raw)
		if !ok {
			return nil, false
		}
		switch kind, _ := jsonStringOf(n["type"]); kind {
		case "group", "columns":
			if !flush() {
				return nil, false
			}
			var built phpObject
			if kind == "group" {
				built, ok = b.group(n, path)
			} else {
				built, ok = b.columns(n, path, false)
			}
			if !ok {
				return nil, false
			}
			tree = append(tree, built)
		default:
			run = append(run, elementorLeafAt{path, n})
		}
	}
	if !flush() {
		return nil, false
	}
	return tree, true
}

func elementorLayout(id, elType string, settings phpObject, elements phpList, inner bool) phpObject {
	return phpObject{{"id", id}, {"elType", elType}, {"settings", settings}, {"elements", elements}, {"isInner", inner}}
}

// boxedColumn is the wrapper of a run of leaves or of a group: a boxed
// column container, or a section "10" holding one column "100".
func (b *elementorBuild) boxedColumn(id, columnID string, elements phpList) phpObject {
	if b.containers {
		return elementorLayout(id, "container", phpObject{{"content_width", "boxed"}, {"flex_direction", "column"}}, elements, false)
	}
	column := elementorLayout(columnID, "column", phpObject{{"_column_size", elementorColumnSizes[1]}}, elements, false)
	return elementorLayout(id, "section", phpObject{{"structure", elementorStructures[1]}}, phpList{column}, false)
}

// wrapper holds a run of top-level leaves, keyed by its first leaf's path.
func (b *elementorBuild) wrapper(run []elementorLeafAt) (phpObject, bool) {
	key := run[0].path + "#wrapper"
	id, ok := b.ids.next(key)
	if !ok {
		return nil, false
	}
	columnID := ""
	if !b.containers {
		if columnID, ok = b.ids.next(key + "#column"); !ok {
			return nil, false
		}
	}
	elements, ok := b.leafList(run)
	if !ok {
		return nil, false
	}
	return b.boxedColumn(id, columnID, elements), true
}

func (b *elementorBuild) group(n map[string]json.RawMessage, path string) (phpObject, bool) {
	children, ok := jsonArrayOf(n["children"])
	if !ok || len(children) == 0 {
		return nil, false
	}
	id, ok := b.ids.next(path)
	if !ok {
		return nil, false
	}
	columnID := ""
	if !b.containers {
		if columnID, ok = b.ids.next(path + "#column"); !ok {
			return nil, false
		}
	}
	elements := phpList{}
	for j, raw := range children {
		at := fmt.Sprintf("%s.children[%d]", path, j)
		child, ok := jsonObjectOf(raw)
		if !ok {
			return nil, false
		}
		switch kind, _ := jsonStringOf(child["type"]); kind {
		case "group":
			return nil, false
		case "columns":
			built, ok := b.columns(child, at, true)
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
	return b.boxedColumn(id, columnID, elements), true
}

func (b *elementorBuild) columns(n map[string]json.RawMessage, path string, nested bool) (phpObject, bool) {
	cols, ok := jsonArrayOf(n["columns"])
	if !ok || len(cols) < 2 || len(cols) > 4 {
		return nil, false
	}
	count := len(cols)
	var widths []int64
	if raw, has := n["widths"]; has && strings.TrimSpace(string(raw)) != "null" {
		list, ok := jsonArrayOf(raw)
		if !ok || len(list) != count {
			return nil, false
		}
		for _, item := range list {
			w, ok := jsonIntOf(item)
			if !ok || w < 10 || w > 90 {
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
		if b.containers {
			size := int64(100 / count)
			if widths != nil {
				size = widths[k]
			}
			width := phpObject{{"unit", "%"}, {"size", strconv.FormatInt(size, 10)}, {"sizes", phpList{}}}
			built = append(built, elementorLayout(columnID, "container", phpObject{
				{"content_width", "full"}, {"flex_direction", "column"}, {"width", width},
			}, elements, true))
			continue
		}
		settings := phpObject{{"_column_size", elementorColumnSizes[count]}}
		if widths != nil {
			settings = append(settings, phpPair{"_inline_size", strconv.FormatInt(widths[k], 10)})
		}
		built = append(built, elementorLayout(columnID, "column", settings, elements, nested))
	}
	if b.containers {
		width := "boxed"
		if nested {
			width = "full"
		}
		gap := phpObject{{"unit", "px"}, {"size", elementorGapPx}, {"column", elementorGapPx}, {"row", elementorGapPx}}
		return elementorLayout(id, "container", phpObject{
			{"content_width", width}, {"flex_direction", "row"}, {"flex_direction_mobile", "column"}, {"flex_gap", gap},
		}, built, nested), true
	}
	return elementorLayout(id, "section", phpObject{{"structure", elementorStructures[count]}}, built, nested), true
}

func (b *elementorBuild) leafList(run []elementorLeafAt) (phpList, bool) {
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

func (b *elementorBuild) widget(path, kind string, settings phpObject) (phpList, bool) {
	id, ok := b.ids.next(path)
	if !ok {
		return nil, false
	}
	return phpList{phpObject{{"id", id}, {"elType", "widget"}, {"settings", settings}, {"elements", phpList{}}, {"widgetType", kind}}}, true
}

// elementorTextList reads a list of strings; "" is a member only for cells.
func elementorTextList(raw json.RawMessage, cells bool) ([]string, bool) {
	list, ok := jsonArrayOf(raw)
	if !ok || len(list) == 0 {
		return nil, false
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		s, ok := jsonStringOf(item)
		if !ok || (s == "" && !cells) {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// present: the member is there and is not JSON null.
func present(n map[string]json.RawMessage, key string) (json.RawMessage, bool) {
	raw, has := n[key]
	return raw, has && strings.TrimSpace(string(raw)) != "null"
}

// leaf maps one outline leaf to its widgets (one per button).
func (b *elementorBuild) leaf(n map[string]json.RawMessage, path string) (phpList, bool) {
	textEditor := func(html string) (phpList, bool) {
		return b.widget(path, "text-editor", phpObject{{"editor", html}})
	}
	switch kind, _ := jsonStringOf(n["type"]); kind {
	case "heading":
		level, ok1 := jsonIntOf(n["level"])
		text, ok2 := jsonStringOf(n["text"])
		size, ok3 := elementorHeaderSizes[level]
		if !ok1 || !ok2 || !ok3 {
			return nil, false
		}
		return b.widget(path, "heading", phpObject{{"title", elementorHTMLText.Replace(text)}, {"header_size", size}})
	case "paragraph":
		text, ok := jsonStringOf(n["text"])
		if !ok || elementorOwnAddress.MatchString(text) {
			return nil, false
		}
		return textEditor("<p>" + elementorHTMLText.Replace(text) + "</p>")
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
		return textEditor(h.String() + "</" + tag + ">")
	case "quote":
		paras, ok := elementorTextList(n["paragraphs"], false)
		if !ok {
			return nil, false
		}
		var h strings.Builder
		h.WriteString("<blockquote>")
		for _, p := range paras {
			if elementorOwnAddress.MatchString(p) {
				return nil, false
			}
			h.WriteString("<p>" + elementorHTMLText.Replace(p) + "</p>")
		}
		if raw, ok := present(n, "citation"); ok {
			citation, ok := jsonStringOf(raw)
			if !ok {
				return nil, false
			}
			h.WriteString("<cite>" + elementorHTMLText.Replace(citation) + "</cite>")
		}
		return textEditor(h.String() + "</blockquote>")
	case "table":
		return b.table(n, textEditor)
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

func (b *elementorBuild) table(n map[string]json.RawMessage, textEditor func(string) (phpList, bool)) (phpList, bool) {
	var h strings.Builder
	h.WriteString("<table>")
	if raw, ok := present(n, "header"); ok {
		header, ok := elementorTextList(raw, true)
		if !ok {
			return nil, false
		}
		h.WriteString("<thead><tr>")
		for _, cell := range header {
			h.WriteString("<th>" + elementorHTMLText.Replace(cell) + "</th>")
		}
		h.WriteString("</tr></thead>")
	}
	rows, ok := jsonArrayOf(n["rows"])
	if !ok || len(rows) == 0 {
		return nil, false
	}
	h.WriteString("<tbody>")
	for _, raw := range rows {
		cells, ok := elementorTextList(raw, true)
		if !ok {
			return nil, false
		}
		h.WriteString("<tr>")
		for _, cell := range cells {
			h.WriteString("<td>" + elementorHTMLText.Replace(cell) + "</td>")
		}
		h.WriteString("</tr>")
	}
	return textEditor(h.String() + "</tbody></table>")
}

func (b *elementorBuild) image(n map[string]json.RawMessage, path string) (phpList, bool) {
	id, ok := jsonIntOf(n["attachment_id"])
	if !ok || id < 1 || id > pageCreateMaxAttachmentID {
		return nil, false
	}
	if _, ok := jsonStringOf(n["alt"]); !ok {
		return nil, false
	}
	align := "none"
	if raw, has := n["align"]; has {
		if align, ok = jsonStringOf(raw); !ok {
			return nil, false
		}
	}
	url, known := b.urls[id]
	if !known && b.anyImage {
		known = true
	}
	if !stringIn(align, elementorImageAligns) || !known {
		return nil, false
	}
	settings := phpObject{
		{"image", phpObject{{"url", url}, {"id", strconv.FormatInt(id, 10)}, {"source", "library"}}},
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

func (b *elementorBuild) buttons(n map[string]json.RawMessage, path string) (phpList, bool) {
	align := "left"
	if raw, has := n["align"]; has {
		var ok bool
		if align, ok = jsonStringOf(raw); !ok || (align != "left" && align != "center") {
			return nil, false
		}
	}
	list, ok := jsonArrayOf(n["buttons"])
	if !ok || len(list) == 0 {
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
		if raw, has := button["style"]; has {
			style, ok3 = jsonStringOf(raw)
		}
		if !ok1 || !ok2 || !ok3 || !stringIn(style, elementorButtonStyles) || strings.Contains(url, "&") {
			return nil, false
		}
		w, ok := b.widget(fmt.Sprintf("%s.buttons[%d]", path, j), "button", phpObject{
			{"text", elementorHTMLText.Replace(text)},
			{"link", phpObject{{"url", url}, {"is_external", ""}, {"nofollow", ""}}},
			{"align", align},
		})
		if !ok {
			return nil, false
		}
		widgets = append(widgets, w...)
	}
	return widgets, true
}
