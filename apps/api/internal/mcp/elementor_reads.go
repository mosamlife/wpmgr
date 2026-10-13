package mcp

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// WPMgr's widget discovery reads expose identifiers and fenced site metadata.
const (
	AbilityElementorWidgets      = "wpmgr/elementor-widgets"
	AbilityElementorWidgetSchema = "wpmgr/elementor-widget-schema"
	elementorReadMaxBytes        = 32768
)

var (
	elementorIdentifier = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_-]{0,127}$`)
	elementorWidgetType = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	elementorOptionKey  = regexp.MustCompile(`^(?:[A-Za-z0-9_][A-Za-z0-9 _.:%/+-]{0,63})?$`)
)

const elementorIDSchema = `{"type":"string","pattern":"^[a-zA-Z_][a-zA-Z0-9_-]{0,127}$","maxLength":128}`

var elementorWidgetsInputSchema = json.RawMessage(`{"type":"object","properties":{"category":` + elementorIDSchema + `,"search":{"type":"string","maxLength":128}},"additionalProperties":false}`)
var elementorWidgetSchemaInputSchema = json.RawMessage(`{"type":"object","properties":{"widget":` + elementorIDSchema + `,"controls":{"type":"array","maxItems":128,"uniqueItems":true,"items":` + elementorIDSchema + `}},"required":["widget"],"additionalProperties":false}`)

func isElementorRead(name string) bool {
	return name == AbilityElementorWidgets || name == AbilityElementorWidgetSchema
}

func elementorReadInputBad(name string, input []byte) bool {
	top, ok := jsonObjectOf(input)
	if !ok {
		return true
	}
	if name == AbilityElementorWidgets {
		if !keysExactly(top, []string{"category", "search"}) {
			return true
		}
		if raw, has := top["category"]; has {
			s, ok := jsonStringOf(raw)
			if !ok || !elementorIdentifier.MatchString(s) {
				return true
			}
		}
		if raw, has := top["search"]; has {
			s, ok := jsonStringOf(raw)
			if !ok || len(s) > 128 || !utf8.ValidString(s) {
				return true
			}
		}
		return false
	}
	if !keysExactly(top, []string{"widget", "controls"}, "widget") {
		return true
	}
	s, ok := jsonStringOf(top["widget"])
	if !ok || !elementorIdentifier.MatchString(s) {
		return true
	}
	if raw, has := top["controls"]; has {
		ids, ok := jsonArrayOf(raw)
		if !ok || len(ids) > 128 {
			return true
		}
		seen := map[string]bool{}
		for _, raw := range ids {
			s, ok := jsonStringOf(raw)
			if !ok || !elementorIdentifier.MatchString(s) || seen[s] {
				return true
			}
			seen[s] = true
		}
	}
	return false
}

// readShape rejects malformed answers and omits unsupported string option keys.
// Map keys exposed as identifiers are validated; free metadata is fenced.
type readShape struct {
	fields   map[string]*readShape
	required []string
	items    *readShape
	values   *readShape
	scalar   string
	token    *regexp.Regexp
	// Option keys containing spaces are site text, even within their closed pattern.
	fenceSpaces bool
	nullable    bool
	free        bool
}

var readText = &readShape{scalar: "string"}
var readBool = &readShape{scalar: "bool"}
var readID = &readShape{token: elementorIdentifier}
var readOptionKey = &readShape{token: elementorOptionKey, fenceSpaces: true}
var readNullableText = &readShape{scalar: "string", nullable: true}
var readNullableID = &readShape{token: elementorIdentifier, nullable: true}
var readAny = &readShape{free: true, nullable: true}
var readMap = &readShape{scalar: "object", free: true}

func readObject(fields map[string]*readShape, required ...string) *readShape {
	if required == nil {
		required = make([]string, 0, len(fields))
		for key := range fields {
			required = append(required, key)
		}
	}
	return &readShape{fields: fields, required: required}
}
func readList(items *readShape) *readShape { return &readShape{items: items} }
func readControlShape(depth int) *readShape {
	fields := map[string]*readShape{
		"type": readText, "options": readList(readOptionKey), "default": readAny,
		"responsive": readObject(map[string]*readShape{"devices": readList(readText), "min": readNullableText, "max": readNullableText}),
		"group":      readObject(map[string]*readShape{"family": readText, "prefix": readText, "activator": readNullableID}),
		"condition":  readMap, "conditions": readMap, "return_value": readText, "size_units": readList(readText), "range": readMap, "multiple": readBool,
	}
	fields["responsive"].nullable = true
	fields["group"].nullable = true
	if depth < 8 {
		fields["fields"] = &readShape{values: readControlShape(depth + 1)}
	} else {
		fields["fields"] = &readShape{values: &readShape{}}
	}
	return readObject(fields, "type", "options", "default", "responsive", "group")
}

var elementorCatalogueShape = readObject(map[string]*readShape{
	"builder": {token: regexp.MustCompile(`^elementor$`)}, "builder_version": readText, "truncated": readBool,
	"widgets": readList(readObject(map[string]*readShape{"name": readID, "title": readText, "categories": readList(readText), "pro": readBool, "atomic": readBool, "usable": readBool})),
})

func elementorSchemaShape(mode string) *readShape {
	fields := map[string]*readShape{"builder": {token: regexp.MustCompile(`^elementor$`)}, "builder_version": readText, "truncated": readBool, "widget": readID, "atomic": readBool, "usable": readBool, "mode": {token: regexp.MustCompile(`^(?:skeleton|controls)$`)}}
	if mode == "skeleton" {
		fields["tabs"] = readList(readObject(map[string]*readShape{"id": readText, "sections": readList(readObject(map[string]*readShape{"id": readText, "controls": readList(readObject(map[string]*readShape{"id": readID, "type": readText}))}))}))
	} else if mode == "controls" {
		fields["controls"] = &readShape{values: readControlShape(0)}
	} else {
		return nil
	}
	return readObject(fields)
}

func (s *readShape) project(v any) (any, bool) {
	if v == nil {
		return nil, s.nullable
	}
	if s.fields != nil {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		for _, key := range s.required {
			if _, ok := m[key]; !ok {
				return nil, false
			}
		}
		out := make(map[string]any, len(m))
		for key, value := range m {
			sub, ok := s.fields[key]
			if !ok {
				return nil, false
			}
			p, ok := sub.project(value)
			if !ok {
				return nil, false
			}
			out[key] = p
		}
		return out, true
	}
	if s.items != nil {
		a, ok := v.([]any)
		if !ok {
			return nil, false
		}
		out := make([]any, 0, len(a))
		for _, value := range a {
			if s.items == readOptionKey {
				if key, ok := value.(string); ok && !elementorOptionKey.MatchString(key) {
					continue
				}
			}
			p, ok := s.items.project(value)
			if !ok {
				return nil, false
			}
			out = append(out, p)
		}
		return out, true
	}
	if s.values != nil {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		out := make(map[string]any, len(m))
		for key, value := range m {
			if !elementorIdentifier.MatchString(key) {
				return nil, false
			}
			p, ok := s.values.project(value)
			if !ok {
				return nil, false
			}
			out[key] = p
		}
		return out, true
	}
	if s.token != nil {
		str, ok := v.(string)
		if !ok || !s.token.MatchString(str) {
			return nil, false
		}
		if s.fenceSpaces && strings.Contains(str, " ") {
			return fenceSiteText(str), true
		}
		return str, true
	}
	if s.free {
		if s.scalar == "object" {
			if _, ok := v.(map[string]any); !ok {
				return nil, false
			}
		}
		return fenceReadData(v), true
	}
	if s.scalar == "string" {
		str, ok := v.(string)
		return fenceSiteText(str), ok
	}
	if s.scalar == "bool" {
		b, ok := v.(bool)
		return b, ok
	}
	return nil, false
}

// Defaults and condition metadata retain their JSON value types. Their strings,
// including arbitrary object keys, are data rather than instructions.
func fenceReadData(v any) any {
	switch x := v.(type) {
	case string:
		return fenceSiteText(x)
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = fenceReadData(v)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for key, v := range x {
			out[fenceSiteText(key)] = fenceReadData(v)
		}
		return out
	default:
		return v
	}
}

// decodeReadJSON accepts one UTF-8 JSON value with unique object members.
func decodeReadJSON(raw []byte) (any, bool) {
	if !utf8.Valid(raw) {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if !readJSONValue(dec, 0) {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	var v any
	dec = json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		return nil, false
	}
	return v, true
}
func readJSONValue(dec *json.Decoder, depth int) bool {
	if depth > 32 {
		return false
	}
	token, err := dec.Token()
	if err != nil {
		return false
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return true
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return false
			}
			s, ok := key.(string)
			if !ok || seen[s] {
				return false
			}
			seen[s] = true
			if !readJSONValue(dec, depth+1) {
				return false
			}
		}
	case '[':
		for dec.More() {
			if !readJSONValue(dec, depth+1) {
				return false
			}
		}
	default:
		return false
	}
	end, err := dec.Token()
	return err == nil && ((delim == '{' && end == json.Delim('}')) || (delim == '[' && end == json.Delim(']')))
}

func fenceElementorRead(name string, raw []byte, maxBytes int) (json.RawMessage, bool) {
	if maxBytes > elementorReadMaxBytes {
		maxBytes = elementorReadMaxBytes
	}
	if len(raw) == 0 || len(raw) > maxBytes {
		return json.RawMessage(`null`), true
	}
	v, ok := decodeReadJSON(raw)
	if !ok {
		return json.RawMessage(`null`), true
	}
	shape := elementorCatalogueShape
	if name == AbilityElementorWidgetSchema {
		m, ok := v.(map[string]any)
		if !ok {
			return json.RawMessage(`null`), true
		}
		mode, _ := m["mode"].(string)
		shape = elementorSchemaShape(mode)
	}
	if shape == nil {
		return json.RawMessage(`null`), true
	}
	out, ok := shape.project(v)
	if !ok {
		return json.RawMessage(`null`), true
	}
	m := out.(map[string]any)
	if name == AbilityElementorWidgets {
		for _, row := range m["widgets"].([]any) {
			widget := row.(map[string]any)
			if !elementorWidgetType.MatchString(widget["name"].(string)) {
				widget["usable"] = false
			}
		}
	} else if !elementorWidgetType.MatchString(m["widget"].(string)) {
		m["usable"] = false
	}
	b, err := json.Marshal(out)
	if err != nil || len(b) > maxBytes {
		return json.RawMessage(`null`), true
	}
	return b, false
}

// projectNativeStructure preserves the old projection for every old node.
// Native nodes expose only identity, widget type and a bounded fenced summary.
func projectNativeStructure(v any) (any, bool) {
	out := projectOutput(v, pageStructureOutputShape)
	top, ok := v.(map[string]any)
	if !ok {
		return out, true
	}
	nodes, ok := top["nodes"].([]any)
	if !ok {
		return out, true
	}
	projected := out.(map[string]any)["nodes"].([]any)
	for i, value := range nodes {
		node, ok := value.(map[string]any)
		if !ok || node["kind"] != "native" {
			continue
		}
		if len(node) != 5 {
			return nil, false
		}
		shape := readObject(map[string]*readShape{"ref": {token: pageEditRefPattern}, "parent": {token: pageEditRefPattern}, "kind": {token: regexp.MustCompile(`^native$`)}, "widget": {token: elementorWidgetType}, "summary": readText})
		p, ok := shape.project(node)
		if !ok {
			return nil, false
		}
		summary := node["summary"].(string)
		if utf8.RuneCountInString(summary) > 120 {
			return nil, false
		}
		projected[i] = p
	}
	return out, true
}

const msgNativeEditOutdated = "Native widget reads and edits need the WPMgr plugin " + agentcmd.MinAgentVersionForNativeEdit + " or later on this site. Nothing was asked."

func malformedReadRefusal() *toolRefusal {
	return refuse(reasonAbilityAgentRefused, domain.Validation(ErrCodeInvalidToolArguments,
		"The site returned a malformed or oversized read answer. Nothing from that answer was returned.").
		WithDetails(map[string]any{"code": "malformed_output", "retryable": false}))
}

func hasNativeStructureNodes(raw []byte) bool {
	var top map[string]any
	if json.Unmarshal(raw, &top) != nil {
		return false
	}
	nodes, _ := top["nodes"].([]any)
	for _, value := range nodes {
		if node, ok := value.(map[string]any); ok && node["kind"] == "native" {
			return true
		}
	}
	return false
}
