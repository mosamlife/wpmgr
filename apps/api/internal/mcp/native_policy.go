package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/mosamlife/wpmgr/apps/api/internal/elementornative"
)

const nativeSettingsLimit = 65536
const nativeSchemaLimit = 32768
const nativeFailureLimit = 20

// NativeMediaFact is a separately verified attachment and its canonical URL.
// A schema or an AI settings object cannot supply this authority.
type NativeMediaFact struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// NativePolicyInput holds raw documents so duplicate keys and PHP insertion
// order survive validation. Before may be absent for an add. Result, when
// supplied, is checked against the exact merge; otherwise Settings in the
// answer provides the bytes the result verifier must compare.
type NativePolicyInput struct {
	Widget           string
	ElementorVersion string
	Schema           json.RawMessage
	Before           json.RawMessage
	Settings         json.RawMessage
	Result           json.RawMessage
	Breakpoints      []string
	Media            []NativeMediaFact
}

type NativePolicyFailure struct {
	Path    string   `json:"path"`
	Key     string   `json:"key"`
	Reason  string   `json:"reason"`
	Allowed []string `json:"allowed,omitempty"`
}

type NativePolicyResult struct {
	OK       bool                  `json:"ok"`
	Failures []NativePolicyFailure `json:"failures,omitempty"`
	Settings json.RawMessage       `json:"-"`
}

// CheckNativeSettings checks AI-written leaves against independent owned
// policy and the live schema's intersection. It never normalises a setting.
func CheckNativeSettings(input NativePolicyInput) NativePolicyResult {
	s := nativePolicyState{media: input.Media, active: nativeStringList(input.Breakpoints)}
	answer := func(settings any) NativePolicyResult {
		r := NativePolicyResult{OK: len(s.failures) == 0, Failures: s.failures}
		if r.OK {
			r.Settings, _ = phpEncodeBytes(settings)
		}
		return r
	}
	if len(input.Schema) > nativeSchemaLimit || !utf8.Valid(input.Schema) || !nativeUnicodeEscapes(input.Schema) {
		s.fail("schema", "controls", "supply a bounded live control map")
		return answer(nil)
	}
	var ok bool
	s.schema, ok = phpDecodeDocument(input.Schema, 32, true)
	if !ok || nativeDuplicate(s.schema, "schema") != nil {
		s.fail("schema", "controls", "supply one valid live schema with unique keys")
		return answer(nil)
	}
	s.owned = elementornative.Lookup(input.Widget, input.ElementorVersion)
	if len(s.owned) == 0 || input.Widget != nativeString(nativeGet(s.schema, "widget")) {
		s.fail("schema", "widget", "use a core classic widget at a supported pinned version")
		return answer(nil)
	}
	written, failure := nativeParseSettings(input.Settings)
	if failure != nil {
		s.fail(failure.Path, failure.Key, failure.Reason)
		return answer(nil)
	}
	before := any(phpObject{})
	if len(input.Before) > 0 {
		if len(input.Before) > nativeSettingsLimit || !utf8.Valid(input.Before) || !nativeUnicodeEscapes(input.Before) {
			s.fail("input", "settings", "before settings exceed the input limit")
			return answer(nil)
		}
		before, ok = phpDecodeDocument(input.Before, 32, true)
		if !ok || nativeDuplicate(before, "settings") != nil {
			s.fail("input", "settings", "supply valid before settings with unique keys")
			return answer(nil)
		}
	}
	merged := nativeMerge(before, written)
	if len(input.Result) > 0 {
		if len(input.Result) > nativeSettingsLimit || !utf8.Valid(input.Result) || !nativeUnicodeEscapes(input.Result) {
			s.fail("input", "settings", "result settings exceed the input limit")
			return answer(nil)
		}
		merged, ok = phpDecodeDocument(input.Result, 32, true)
		if !ok || nativeDuplicate(merged, "settings") != nil {
			s.fail("input", "settings", "supply valid result settings with unique keys")
			return answer(nil)
		}
	}
	s.validateWritten(before, written, merged)
	return answer(merged)
}

type nativePolicyState struct {
	schema   any
	active   any
	media    []NativeMediaFact
	owned    map[string]elementornative.Control
	failures []NativePolicyFailure
}

var nativeBreakpoints = []string{"mobile", "mobile_extra", "tablet", "tablet_extra", "laptop", "desktop", "widescreen"}
var nativeUnits = []string{"px", "em", "rem", "%", "vh", "vw", "vmin", "vmax", "ch", "ex"}
var nativeTypes = []string{"text", "textarea", "wysiwyg", "number", "slider", "dimensions", "color", "select", "select2", "choose", "switcher", "popover_toggle", "url", "media", "icons", "font", "box_shadow", "text_shadow", "repeater"}
var nativeDeniedKeys = []string{"__globals__", "__dynamic__", "custom_css", "_attributes", "custom_attributes", "_css_classes", "_element_id", "_element_cache", "_grid_column_custom", "_grid_row_custom"}
var nativeWidgetPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var nativeCSSTokens = regexp.MustCompile(`^[A-Za-z0-9 ./%-]{0,64}$`)
var nativeDecimal = regexp.MustCompile(`^-?(?:0|[1-9][0-9]{0,5})(?:\.[0-9]{1,4})?$`)
var nativeColour = regexp.MustCompile(`^(?:#[0-9A-F]{6}|transparent|rgba\((?:(?:25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9]),){3}(?:0|1|0\.[0-9]{1,2})\))$`)
var nativeRowID = regexp.MustCompile(`^[0-9a-f]{7}$`)
var nativeAttachmentID = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
var nativeShadowOffset = regexp.MustCompile(`^-?[0-9]{1,3}$`)
var nativeOption = regexp.MustCompile(`^[A-Za-z0-9 _%\-]*$`)
var nativeEvent = regexp.MustCompile(`(?i)\bon[a-z]+\s*=`)
var nativeShortcode = regexp.MustCompile(`(?i)\[[a-z]`)
var nativeAnchor = regexp.MustCompile(`^#[a-z][a-z0-9-]{1,39}$`)
var nativeNumericKey = regexp.MustCompile(`^(?:0|-?[1-9][0-9]*)$`)
var nativeRichTag = regexp.MustCompile(`^<(/?)([A-Za-z][A-Za-z0-9]*)([^<>]*)>`)
var nativeRichAttr = regexp.MustCompile(`^\s+([A-Za-z][A-Za-z0-9_-]*)\s*=\s*("[^"<>]*"|'[^'<>]*')`)
var nativeRichRel = regexp.MustCompile(`^(?:nofollow|noopener|noreferrer)(?: (?:nofollow|noopener|noreferrer))*$`)
var nativeFailureWhitespace = regexp.MustCompile(`[\x00-\x20\x7F]+`)
var nativeIcons = map[string]*regexp.Regexp{
	"fa-solid":   regexp.MustCompile(`^fas fa-[a-z0-9]+(?:-[a-z0-9]+)*$`),
	"fa-regular": regexp.MustCompile(`^far fa-[a-z0-9]+(?:-[a-z0-9]+)*$`),
	"fa-brands":  regexp.MustCompile(`^fab fa-[a-z0-9]+(?:-[a-z0-9]+)*$`),
	"eicons":     regexp.MustCompile(`^eicon-[a-z0-9]+(?:-[a-z0-9]+)*$`),
}

func nativeString(v any) string { s, _ := v.(string); return s }
func nativeGet(v any, key string) any {
	o, _ := v.(phpObject)
	for _, p := range o {
		if p.key == key {
			return p.value
		}
	}
	return nil
}
func nativeHas(v any, key string) bool {
	o, _ := v.(phpObject)
	for _, p := range o {
		if p.key == key {
			return true
		}
	}
	return false
}
func nativeMap(v any) (phpObject, bool) {
	if l, ok := v.(phpList); ok && len(l) == 0 {
		return phpObject{}, true
	}
	o, ok := v.(phpObject)
	if !ok {
		return nil, false
	}
	for _, p := range o {
		if p.key == "" || nativeNumericKey.MatchString(p.key) {
			return nil, false
		}
	}
	return o, true
}
func nativeList(v any) (phpList, bool) {
	if o, ok := v.(phpObject); ok && len(o) == 0 {
		return phpList{}, true
	}
	l, ok := v.(phpList)
	return l, ok
}
func nativeStrings(v any, empty bool) ([]string, bool) {
	l, ok := nativeList(v)
	if !ok || (!empty && len(l) == 0) {
		return nil, false
	}
	out := []string{}
	for _, v := range l {
		s, ok := v.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}
func nativeStringList(v []string) phpList {
	l := phpList{}
	for _, s := range v {
		l = append(l, s)
	}
	return l
}
func nativeMembers(v any, keys ...string) bool {
	o, ok := nativeMap(v)
	if !ok || len(o) != len(keys) {
		return false
	}
	for _, k := range keys {
		if !nativeHas(o, k) {
			return false
		}
	}
	return true
}
func nativeBounded(v any, limit int) bool { b, ok := phpEncodeBytes(v); return ok && len(b) <= limit }
func nativeEqual(a, b any) bool {
	aa, ok1 := phpEncodeBytes(a)
	bb, ok2 := phpEncodeBytes(b)
	return ok1 && ok2 && bytes.Equal(aa, bb)
}
func nativeSet(o phpObject, key string, value any) phpObject {
	out := slices.Clone(o)
	for i := range out {
		if out[i].key == key {
			out[i].value = value
			return out
		}
	}
	return append(out, phpPair{key, value})
}

func nativeMerge(before, written any) any {
	b, ok1 := nativeMap(before)
	w, ok2 := nativeMap(written)
	if !ok1 || !ok2 {
		return nil
	}
	out := slices.Clone(b)
	for _, p := range w {
		out = nativeSet(out, p.key, p.value)
	}
	result := phpObject{}
	for _, p := range out {
		if nativeHas(w, p.key) && nativeGet(w, p.key) == nil {
			continue
		}
		result = append(result, p)
	}
	return result
}

func nativeParseSettings(raw []byte) (any, *NativePolicyFailure) {
	bad := &NativePolicyFailure{Path: "input", Key: "settings", Reason: "write valid bounded settings JSON"}
	if len(raw) > nativeSettingsLimit || !utf8.Valid(raw) || !nativeUnicodeEscapes(raw) {
		return nil, bad
	}
	v, ok := phpDecodeDocument(raw, 17, true)
	if !ok || !nativeJSONDepth(v, 0) {
		return nil, bad
	}
	if _, ok := nativeMap(v); !ok {
		return nil, bad
	}
	if f := nativeDuplicate(v, "settings"); f != nil {
		return nil, f
	}
	return v, nil
}

func nativeJSONDepth(v any, depth int) bool {
	if depth > 16 {
		return false
	}
	switch x := v.(type) {
	case phpObject:
		for _, p := range x {
			if !nativeJSONDepth(p.value, depth+1) {
				return false
			}
		}
	case phpList:
		for _, item := range x {
			if !nativeJSONDepth(item, depth+1) {
				return false
			}
		}
	}
	return true
}

// JSON strings must contain paired surrogates; encoding/json otherwise
// replaces an unpaired escape, which would change the requested bytes.
func nativeUnicodeEscapes(raw []byte) bool {
	in := false
	for i := 0; i < len(raw); i++ {
		if raw[i] == '"' {
			in = !in
			continue
		}
		if !in || raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) {
			return false
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return false
		}
		n, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if n >= 0xDC00 && n <= 0xDFFF {
			return false
		}
		if n >= 0xD800 && n <= 0xDBFF {
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return false
			}
			next, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
			if err != nil || next < 0xDC00 || next > 0xDFFF {
				return false
			}
			i += 6
		}
	}
	return true
}

func nativeDuplicate(v any, path string) *NativePolicyFailure {
	switch x := v.(type) {
	case phpObject:
		seen := map[string]bool{}
		for _, p := range x {
			if seen[p.key] {
				return &NativePolicyFailure{Path: path, Key: p.key, Reason: "object keys must be unique, including escaped spellings"}
			}
			seen[p.key] = true
			if f := nativeDuplicate(p.value, path+"."+p.key); f != nil {
				return f
			}
		}
	case phpList:
		for i, c := range x {
			if f := nativeDuplicate(c, fmt.Sprintf("%s.%d", path, i)); f != nil {
				return f
			}
		}
	}
	return nil
}

func nativeDeniedKey(k string) bool {
	k = strings.ToLower(k)
	base, _ := nativeSplit(k)
	if strings.HasSuffix(k, "_css_id") || strings.HasSuffix(base, "_css_id") {
		return true
	}
	for _, d := range nativeDeniedKeys {
		if k == d || strings.HasPrefix(k, d+"_") {
			return true
		}
	}
	for _, p := range []string{"motion_fx_", "sticky", "_motion_", "motion_", "_sticky"} {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}
func nativeDeniedWidget(w string) bool {
	w = strings.ToLower(w)
	return slices.Contains([]string{"html", "shortcode", "text-path", "menu-anchor", "sidebar", "template"}, w) || strings.HasPrefix(w, "e-") || strings.HasPrefix(w, "wp-widget-") || strings.HasPrefix(w, "nested-")
}
func nativeContainsDenied(v any) bool {
	switch x := v.(type) {
	case phpObject:
		for _, p := range x {
			if nativeDeniedKey(p.key) || nativeContainsDenied(p.value) {
				return true
			}
		}
	case phpList:
		for _, c := range x {
			if nativeContainsDenied(c) {
				return true
			}
		}
	}
	return false
}

// Exact control IDs take precedence over responsive suffixes.
func nativeControlKey(key string, controls any) (string, string) {
	if nativeHas(controls, key) {
		return key, ""
	}
	return nativeSplit(key)
}

func nativeSplit(key string) (string, string) {
	for _, b := range nativeBreakpoints {
		suffix := "_" + b
		if strings.HasSuffix(key, suffix) {
			return strings.TrimSuffix(key, suffix), b
		}
	}
	return key, ""
}

func nativeResponsive(window any, b string, active []string) bool {
	if b == "desktop" || !slices.Contains(active, b) {
		return false
	}
	if window == true {
		return true
	}
	w, ok := nativeMap(window)
	if !ok || len(w) == 0 {
		return false
	}
	for _, p := range w {
		if p.key != "min" && p.key != "max" && p.key != "devices" {
			return false
		}
	}
	if nativeHas(w, "devices") {
		devices, ok := nativeStrings(nativeGet(w, "devices"), true)
		if !ok || !slices.Contains(devices, b) {
			return false
		}
		for _, d := range devices {
			if !slices.Contains(nativeBreakpoints, d) {
				return false
			}
		}
	}
	min, max := "mobile", "widescreen"
	if nativeHas(w, "min") {
		min = nativeString(nativeGet(w, "min"))
	}
	if nativeHas(w, "max") {
		max = nativeString(nativeGet(w, "max"))
	}
	i, lo, hi := slices.Index(nativeBreakpoints, b), slices.Index(nativeBreakpoints, min), slices.Index(nativeBreakpoints, max)
	return lo >= 0 && hi >= 0 && i >= lo && i <= hi
}

func nativeOwnedResponsive(w *elementornative.Window, b string) bool {
	// The dump folds registered device variants into this exact device set.
	return w != nil && slices.Contains(w.Devices, b)
}

func nativeUnknownOverride(key string, controls any) bool {
	if nativeHas(controls, key) {
		return false
	}
	o, _ := nativeMap(controls)
	for _, p := range o {
		r := nativeGet(p.value, "responsive")
		if r != nil && r != false && strings.HasPrefix(key, p.key+"_") {
			return true
		}
	}
	return false
}

func (s *nativePolicyState) fail(path, key, why string, allowed ...[]string) {
	if len(s.failures) >= nativeFailureLimit {
		return
	}
	key = nativeFailureLine(key, 60)
	if key == "" {
		key = "control"
	}
	f := NativePolicyFailure{Path: nativeFailureLine(path, 120), Key: key, Reason: nativeFailureLine(why, 200)}
	if len(allowed) > 0 {
		for _, value := range allowed[0] {
			if value = nativeFailureLine(value, 40); value != "" {
				f.Allowed = append(f.Allowed, value)
			}
			if len(f.Allowed) == 12 {
				break
			}
		}
	}
	s.failures = append(s.failures, f)
}

func nativeFailureLine(text string, limit int) string {
	text = strings.TrimSpace(nativeFailureWhitespace.ReplaceAllString(text, " "))
	runes := []rune(text)
	if len(runes) > limit {
		return strings.TrimSpace(string(runes[:limit-3])) + "..."
	}
	return text
}

func (s *nativePolicyState) validContext(settings any) bool {
	w := nativeString(nativeGet(s.schema, "widget"))
	if !nativeWidgetPattern.MatchString(w) || nativeDeniedWidget(w) || nativeGet(s.schema, "atomic") != false {
		s.fail("schema", "widget", "use a known classic widget from the live catalogue")
		return false
	}
	if _, ok := nativeMap(nativeGet(s.schema, "controls")); !ok || !nativeBounded(s.schema, nativeSchemaLimit) {
		s.fail("schema", "controls", "supply a compact live control map within the schema limit")
		return false
	}
	active, ok := nativeStrings(s.active, true)
	if !ok {
		s.fail("site", "breakpoints", "supply the site active breakpoint names")
		return false
	}
	for _, b := range active {
		if !slices.Contains(nativeBreakpoints, b) {
			s.fail("site", "breakpoints", "supply the site active breakpoint names")
			return false
		}
	}
	if _, ok := nativeMap(settings); !ok || !nativeBounded(settings, nativeSettingsLimit) {
		s.fail("input", "settings", "settings must be a map within the input limit")
		return false
	}
	return true
}

func (s *nativePolicyState) validateStored(settings any) {
	if !s.validContext(settings) {
		return
	}
	s.settings(nativeGet(s.schema, "controls"), s.owned, settings, settings, "settings", 0, false)
}

func (s *nativePolicyState) validateWritten(before, written, merged any) {
	if !s.validContext(written) {
		return
	}
	for _, v := range []any{before, merged} {
		if _, ok := nativeMap(v); !ok || !nativeBounded(v, nativeSettingsLimit) {
			s.fail("input", "settings", "before, AI settings and result must be bounded maps")
			return
		}
	}
	expected := nativeMerge(before, written)
	if !nativeEqual(expected, merged) {
		e, _ := nativeMap(expected)
		m, _ := nativeMap(merged)
		keys := []string{}
		for _, p := range append(slices.Clone(e), m...) {
			if !slices.Contains(keys, p.key) {
				keys = append(keys, p.key)
			}
		}
		for _, k := range keys {
			if !nativeHas(e, k) || !nativeHas(m, k) || !nativeEqual(nativeGet(e, k), nativeGet(m, k)) {
				s.fail("settings", k, "the result must equal the exact replace-and-delete merge; carried leaves cannot change")
			}
		}
		if len(s.failures) == 0 {
			s.fail("input", "settings", "the result must retain the PHP merge key order")
		}
		return
	}
	w, _ := nativeMap(written)
	controls := nativeGet(s.schema, "controls")
	accepted := phpObject{}
	for _, p := range w {
		if nativeDeniedKey(p.key) || nativeContainsDenied(nativeGet(before, p.key)) {
			s.fail("settings", p.key, "a denied key or a value containing one cannot be deleted or replaced")
			continue
		}
		if s.overridden(p.key, before, controls) {
			s.fail("settings", p.key, "an existing dynamic or global binding would override this write")
			continue
		}
		if p.value != nil {
			accepted = append(accepted, p)
			continue
		}
		if _, why := s.control(p.key, controls, s.owned); why != "" {
			s.fail("settings", p.key, "only a supported known control at an active responsive breakpoint may be deleted")
		}
	}
	s.settings(controls, s.owned, accepted, merged, "settings", 0, true)
}

func (s *nativePolicyState) overridden(key string, before, controls any) bool {
	base, _ := nativeControlKey(key, controls)
	activation := nativeString(nativeGet(nativeGet(nativeGet(controls, base), "activator"), "key"))
	if own, ok := s.owned[base]; ok && own.Group != nil && own.Group.Activator != nil {
		activation = *own.Group.Activator
	}
	for _, binding := range []string{"__dynamic__", "__globals__"} {
		if !nativeHas(before, binding) {
			continue
		}
		entries, ok := nativeMap(nativeGet(before, binding))
		if !ok {
			return true
		}
		for _, p := range entries {
			if p.value == nil || nativeEqual(p.value, "") || nativeEqual(p.value, phpList{}) {
				continue
			}
			bound, _ := nativeControlKey(p.key, controls)
			if bound == base || bound == activation {
				return true
			}
		}
	}
	return false
}

func (s *nativePolicyState) control(key string, controls any, owned map[string]elementornative.Control) (phpObject, string) {
	base, b := nativeControlKey(key, controls)
	c, ok := nativeMap(nativeGet(controls, base))
	typ := nativeString(nativeGet(c, "type"))
	if !ok || typ == "" {
		return nil, "use a known control from the compact widget schema"
	}
	var ceiling elementornative.Control
	if owned != nil {
		var exists bool
		ceiling, exists = owned[base]
		if !exists || ceiling.Type != typ {
			return nil, "use a control with the owned key and type"
		}
	}
	if nativeHas(c, "raw_css_value") {
		if _, valid := nativeGet(c, "raw_css_value").(bool); !valid {
			return nil, "supply boolean CSS selector metadata"
		}
	}
	if ceiling.RawCSSValue {
		c = nativeSet(c, "raw_css_value", true)
	}
	if !slices.Contains(nativeTypes, typ) {
		return nil, "this control type has no supported safe shape"
	}
	if typ == "switcher" || typ == "popover_toggle" {
		liveReturn, valid := nativeSwitchReturn(c)
		if !valid {
			return nil, "supply a nonempty switch return value"
		}
		if owned != nil {
			ownedReturn := ""
			if ceiling.ReturnValue != nil {
				ownedReturn = *ceiling.ReturnValue
			} else if typ == "switcher" {
				ownedReturn = "yes"
			}
			if liveReturn != ownedReturn {
				return nil, "use the owned switch return value"
			}
		}
	}
	active, _ := nativeStrings(s.active, true)
	_, chained := nativeSplit(base)
	if b != "" && (chained != "" || !nativeResponsive(nativeGet(c, "responsive"), b, active) || (owned != nil && !nativeOwnedResponsive(ceiling.Responsive, b))) {
		return nil, "use an active breakpoint inside this control responsive window"
	}
	if b == "" && nativeUnknownOverride(key, controls) {
		return nil, "use a recognised active responsive breakpoint suffix"
	}
	if owned != nil {
		if opts, ok := nativeStrings(nativeGet(c, "options"), true); ok {
			intersection := []string{}
			for _, o := range opts {
				if slices.Contains(ceiling.Options, o) {
					intersection = append(intersection, o)
				}
			}
			c = nativeSet(c, "options", nativeStringList(intersection))
		}
		if units, ok := nativeStrings(nativeGet(c, "units"), false); ok {
			intersection := []string{}
			for _, u := range units {
				if slices.Contains(nativeUnits, u) {
					intersection = append(intersection, u)
				}
			}
			c = nativeSet(c, "units", nativeStringList(intersection))
		}
		if ceiling.Group != nil {
			g := ceiling.Group
			activation := nativeGet(c, "activator")
			if nativeString(nativeGet(c, "group")) != g.Family || g.Activator == nil || nativeString(nativeGet(activation, "key")) != *g.Activator {
				return nil, "supply the owned group and activator"
			}
			values, ok := nativeStrings(nativeGet(activation, "values"), false)
			if !ok {
				return nil, "supply the owned group activating values"
			}
			allowed := nativeGroupValues(g.Family)
			if root, exists := owned[*g.Activator]; exists && root.Type == "popover_toggle" {
				if root.ReturnValue == nil || *root.ReturnValue == "" {
					return nil, "supply the owned popover return value"
				}
				liveRoot, ok := nativeMap(nativeGet(controls, *g.Activator))
				liveReturn, valid := nativeSwitchReturn(liveRoot)
				if !ok || !valid || nativeString(nativeGet(liveRoot, "type")) != root.Type || liveReturn != *root.ReturnValue {
					return nil, "use the owned group return value"
				}
				allowed = []string{*root.ReturnValue}
			}
			next := []string{}
			for _, v := range values {
				if slices.Contains(allowed, v) {
					next = append(next, v)
				}
			}
			c = nativeSet(c, "activator", phpObject{{"key", *g.Activator}, {"values", nativeStringList(next)}})
		}
	}
	return c, ""
}

func nativeGroupValues(group string) []string {
	switch group {
	case "typography":
		return []string{"custom"}
	case "background":
		return []string{"classic", "gradient"}
	case "border":
		return []string{"solid", "double", "dotted", "dashed", "groove"}
	case "box_shadow", "box-shadow", "text-shadow", "text-stroke":
		return []string{"yes"}
	case "css-filter":
		return []string{"custom"}
	}
	return nil
}
func nativeGroupOf(key string) string {
	groups := map[string]string{"typography": "typography_typography", "title_typography": "title_typography_typography", "background": "background_background", "background_overlay": "background_overlay_background", "button_background_hover": "button_background_hover_background", "border": "border_border", "box_shadow": "box_shadow_box_shadow_type"}
	found := ""
	for g := range groups {
		if strings.HasPrefix(key, g+"_") && len(g) > len(found) {
			found = g
		}
	}
	if key == groups[found] || key == "border_radius" {
		return ""
	}
	return found
}

func (s *nativePolicyState) settings(controls any, owned map[string]elementornative.Control, settings, context any, path string, depth int, written bool) {
	o, _ := nativeMap(settings)
	for _, p := range o {
		if len(s.failures) >= nativeFailureLimit {
			return
		}
		base, _ := nativeControlKey(p.key, controls)
		if nativeDeniedKey(p.key) || nativeDeniedKey(base) {
			s.fail(path, p.key, "this control is never written")
			continue
		}
		c, why := s.control(p.key, controls, owned)
		if why != "" {
			s.fail(path, p.key, why)
			continue
		}
		typ := nativeString(nativeGet(c, "type"))
		group := nativeString(nativeGet(c, "group"))
		if group == "" {
			group = nativeGroupOf(base)
		}
		if typ == "box_shadow" && group == "" {
			group = "box_shadow"
		}
		if typ == "text_shadow" && group == "" {
			group = "text-shadow"
		}
		if group != "" {
			a := nativeGet(c, "activator")
			key := nativeString(nativeGet(a, "key"))
			values, ok := nativeStrings(nativeGet(a, "values"), false)
			root, _ := nativeMap(nativeGet(controls, key))
			popover := nativeString(nativeGet(root, "type")) == "popover_toggle"
			ret, validReturn := nativeSwitchReturn(root)
			activationOK := nativeContainsString(values, nativeGet(context, key))
			if popover {
				ok = ok && validReturn && slices.Contains(values, ret)
				activationOK = nativeGet(context, key) == ret
			}
			if !slices.Contains([]string{"typography", "background", "border", "box_shadow", "box-shadow", "text-shadow", "text-stroke", "css-filter"}, group) || !ok || key == "" || !nativeHas(controls, key) || (base != key && !activationOK) {
				s.fail(path, p.key, "supply this group activator with one of its activating values")
				continue
			}
			if owned != nil {
				if _, why := s.control(key, controls, owned); why != "" {
					s.fail(path, p.key, "use an owned supported group activator")
					continue
				}
			}
		}
		if strings.HasSuffix(base, "font_family") {
			v, ok := p.value.(string)
			if !ok || !designFontPattern.MatchString(v) {
				s.fail(path, p.key, "use a font family of letters, digits and spaces")
				continue
			}
		}
		if typ == "repeater" {
			var fields map[string]elementornative.Control
			if owned != nil {
				fields = map[string]elementornative.Control{}
				for _, f := range owned[base].Fields {
					fields[f.ID] = f
				}
			}
			s.repeater(c, fields, p.value, path, p.key, depth, written)
			continue
		}
		// Free-text values that Elementor writes into CSS must be plain tokens.
		if written && nativeGet(c, "raw_css_value") == true {
			value, good := p.value.(string)
			if !good || !nativeCSSTokens.MatchString(value) {
				s.fail(path, p.key, "free-text CSS values must be plain tokens of at most 64 characters")
				continue
			}
		}
		rich := path == "settings" && nativeString(nativeGet(s.schema, "widget")) == "text-editor" && base == "editor"
		if written {
			why = nativeLeafProblem(p.value, typ, rich, base, 0)
		}
		if why == "" {
			why = s.valueProblem(c, p.value, written, rich)
		}
		if why != "" {
			if slices.Contains([]string{"select", "select2", "choose"}, typ) {
				if options, ok := nativeStrings(nativeGet(c, "options"), true); ok {
					s.fail(path, p.key, why, options)
					continue
				}
			}
			s.fail(path, p.key, why)
		}
	}
}

func nativeContainsString(values []string, v any) bool {
	s, ok := v.(string)
	return ok && slices.Contains(values, s)
}

func (s *nativePolicyState) repeater(c phpObject, owned map[string]elementornative.Control, value any, path, key string, depth int, written bool) {
	rows, ok := nativeList(value)
	fields := nativeGet(c, "fields")
	_, mapOK := nativeMap(fields)
	if depth >= 8 || !ok || !mapOK {
		s.fail(path, key, "write a bounded list of repeater rows described by the live fields")
		return
	}
	ids := map[string]bool{}
	for i, row := range rows {
		if len(s.failures) >= nativeFailureLimit {
			return
		}
		at := fmt.Sprintf("%s.%s.%d", path, key, i)
		r, ok := nativeMap(row)
		if !ok {
			s.fail(path, key, "each repeater row must be a map")
			continue
		}
		id := nativeString(nativeGet(r, "_id"))
		if !nativeRowID.MatchString(id) || ids[id] {
			s.fail(at, "_id", "write a unique seven-character lower-case hex row id")
		} else {
			ids[id] = true
		}
		values := phpObject{}
		for _, p := range r {
			if p.key != "_id" {
				values = append(values, p)
			}
		}
		s.settings(fields, owned, values, values, at, depth+1, written)
	}
}

func (s *nativePolicyState) valueProblem(c phpObject, value any, written, rich bool) string {
	typ := nativeString(nativeGet(c, "type"))
	v, isString := value.(string)
	switch typ {
	case "text", "textarea":
		if !isString {
			return "write text as a string"
		}
		if written {
			return nativeTextProblem(v, rich)
		}
		return nativeStoredTextProblem(v)
	case "wysiwyg":
		if written && rich && isString {
			return nativeTextProblem(v, true)
		}
		return "rich text is permitted only on owned rich-text keys"
	case "number":
		if isString && nativeDecimal.MatchString(v) {
			return ""
		}
		return "write a decimal number as a numeric string"
	case "slider", "dimensions":
		units, ok := nativeStrings(nativeGet(c, "units"), false)
		if !ok {
			return "use units offered by the control"
		}
		if typ == "slider" && nativeMembers(value, "unit", "size") && nativeContainsString(units, nativeGet(value, "unit")) {
			n, ok := nativeGet(value, "size").(string)
			if ok && nativeDecimal.MatchString(n) {
				return ""
			}
		}
		if typ == "dimensions" && nativeMembers(value, "unit", "top", "right", "bottom", "left", "isLinked") && nativeGet(value, "unit") == "px" && slices.Contains(units, "px") {
			valid := true
			for _, k := range []string{"top", "right", "bottom", "left"} {
				n, ok := nativeGet(value, k).(string)
				valid = valid && ok && nativeDecimal.MatchString(n) && !strings.HasPrefix(n, "-")
			}
			if written {
				valid = valid && nativeFlag(nativeGet(value, "isLinked"), "yes")
			} else {
				_, ok := nativeGet(value, "isLinked").(bool)
				valid = valid && ok
			}
			if valid {
				return ""
			}
		}
		return "use the exact control shape, numeric strings and a unit offered by the control"
	case "color":
		if isString && nativeColour.MatchString(v) {
			return ""
		}
		return "use an upper-case hex colour, transparent or bounded rgba without spaces"
	case "select", "select2", "choose":
		options, ok := nativeStrings(nativeGet(c, "options"), true)
		if !ok {
			return "the live control must list its option keys"
		}
		multiple := false
		if nativeHas(c, "multiple") {
			var ok bool
			multiple, ok = nativeGet(c, "multiple").(bool)
			if !ok {
				return "the live control must list a boolean multiple flag"
			}
		}
		if typ == "select2" && multiple {
			values, ok := nativeStrings(value, true)
			if ok {
				for _, v := range values {
					if !slices.Contains(options, v) {
						return "write a list containing only offered option keys"
					}
				}
				return ""
			}
			return "write a list containing only offered option keys"
		}
		if isString && slices.Contains(options, v) {
			return ""
		}
		return "choose one of the offered option keys"
	case "switcher", "popover_toggle":
		ret, valid := nativeSwitchReturn(c)
		if valid && isString && (v == "" || v == ret) {
			return ""
		}
		return "write the control return value or the empty string"
	case "url":
		if !nativeMembers(value, "url", "is_external", "nofollow") || !nativeFlag(nativeGet(value, "is_external"), "on") || !nativeFlag(nativeGet(value, "nofollow"), "on") {
			return "write exactly url, is_external and nofollow with on or empty flags"
		}
		if u, ok := nativeGet(value, "url").(string); ok && nativeLinkUsable(u) {
			return ""
		}
		return "use the closed link rule"
	case "media":
		id := nativeString(nativeGet(value, "id"))
		url := nativeString(nativeGet(value, "url"))
		if !nativeMembers(value, "id", "url") || !nativeAttachmentID.MatchString(id) || !pageImageURLUsable(url) {
			return "write exactly a library image id as a numeric string and its URL"
		}
		for _, image := range s.media {
			if image.ID == id && image.URL == url {
				return ""
			}
		}
		return "use a verified image from this site library with its canonical URL"
	case "icons":
		library := nativeString(nativeGet(value, "library"))
		class := nativeString(nativeGet(value, "value"))
		pattern := nativeIcons[library]
		if nativeMembers(value, "value", "library") && pattern != nil && len(class) <= 100 && pattern.MatchString(class) {
			return ""
		}
		return "write value and library for a supported font icon library with matching classes"
	case "font":
		if isString && designFontPattern.MatchString(v) {
			return ""
		}
		return "use a font family of letters, digits and spaces"
	case "box_shadow", "text_shadow":
		offsets := []string{"horizontal", "vertical", "blur"}
		if typ == "box_shadow" {
			offsets = append(offsets, "spread")
		}
		members := append(slices.Clone(offsets), "color")
		if !nativeMembers(value, members...) {
			return "write one shadow with exact offsets and color"
		}
		for _, k := range offsets {
			n, ok := nativeGet(value, k).(string)
			if !ok || !nativeShadowOffset.MatchString(n) {
				return "write bounded whole numeric shadow offsets"
			}
			if k == "blur" {
				i, _ := strconv.Atoi(n)
				if i < 0 {
					return "write a nonnegative blur"
				}
			}
		}
		if colour, ok := nativeGet(value, "color").(string); ok && nativeColour.MatchString(colour) {
			return ""
		}
		return "use a pinned shadow colour"
	}
	return "this control type has no supported safe shape"
}

func nativeFlag(v any, yes string) bool { s, ok := v.(string); return ok && (s == yes || s == "") }
func nativeLinkUsable(u string) bool {
	return pageLinkUsable(u) || (pageURLShapeUsable(u) && nativeAnchor.MatchString(u))
}

func nativeLeafProblem(v any, typ string, rich bool, field string, depth int) string {
	if depth > 8 {
		return "written values are nested too deeply"
	}
	switch x := v.(type) {
	case phpObject:
		for _, p := range x {
			if nativeDeniedKey(p.key) {
				return "this control is never written"
			}
			if why := nativeLeafProblem(p.value, typ, false, p.key, depth+1); why != "" {
				return why
			}
		}
		return ""
	case phpList:
		for _, c := range x {
			if why := nativeLeafProblem(c, typ, false, "", depth+1); why != "" {
				return why
			}
		}
		return ""
	}
	text, ok := v.(string)
	if !ok {
		return "every AI-written scalar must be a string"
	}
	if field == "url" || field == "href" {
		if nativeLinkUsable(text) {
			return ""
		}
		return "use the closed link rule"
	}
	if field == "unit" {
		if slices.Contains(nativeUnits, text) {
			return ""
		}
		return "use a closed CSS unit"
	}
	if typ == "number" || slices.Contains([]string{"size", "top", "right", "bottom", "left", "horizontal", "vertical", "blur", "spread", "font_size", "font_weight", "line_height", "letter_spacing", "word_spacing", "z_index"}, field) {
		if nativeDecimal.MatchString(text) {
			return ""
		}
		return "use a closed decimal numeric string"
	}
	if field == "isLinked" {
		if nativeFlag(v, "yes") {
			return ""
		}
		return "write yes or the empty string for a flag"
	}
	if why := nativeTextProblem(text, rich); why != "" {
		return why
	}
	if slices.Contains([]string{"select", "select2", "choose"}, typ) && !nativeOption.MatchString(text) {
		return "option values must also be closed inert tokens"
	}
	return ""
}

func nativeSequenceProblem(text string) string {
	lower := strings.ToLower(text)
	for _, seq := range []string{"<script", "<iframe", "<object", "<embed", "<style", "<?", "{echo:"} {
		if strings.Contains(lower, seq) {
			return "text contains a forbidden sequence"
		}
	}
	compact := strings.Map(func(r rune) rune {
		if r <= 0x20 {
			return -1
		}
		return r
	}, lower)
	for _, scheme := range []string{"javascript:", "vbscript:", "data:text/html"} {
		if strings.Contains(compact, scheme) {
			return "text contains a forbidden scheme"
		}
	}
	if nativeEvent.MatchString(text) || nativeShortcode.MatchString(text) || strings.ContainsAny(text, "{}") {
		return "text cannot contain handlers, shortcodes or braces"
	}
	return ""
}

func nativeDecodedProblem(text string) string {
	current := text
	for i := 0; i <= 3; i++ {
		if why := nativeSequenceProblem(current); why != "" {
			return why
		}
		next := phpHTMLEntityDecode(current)
		if next == current {
			return ""
		}
		if i == 3 {
			return "character references are nested too deeply"
		}
		current = next
	}
	return ""
}

func nativeInvisible(text string, plain bool) bool {
	for _, r := range text {
		if r <= 8 || r == 11 || r == 12 || (r >= 14 && r <= 31) || (r >= 127 && r <= 159) || (r >= 0x200b && r <= 0x200f) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) || r == 0xfeff || r == 0xfffe || r == 0xffff {
			return true
		}
		if plain && (r <= 31 || r == '<' || r == '>' || r == '[' || r == ']') {
			return true
		}
	}
	return false
}

func nativePlainProblem(text string) string {
	if nativeInvisible(text, true) {
		return "plain text cannot contain markup, brackets or control characters"
	}
	for i := 0; i < len(text); i++ {
		if text[i] != '&' {
			continue
		}
		if len(text)-i < 3 {
			return "use a recognised character entity"
		}
		_, n, ok := phpEntityAt(text[i:])
		if !ok {
			return "use a recognised character entity"
		}
		i += n - 1
	}
	current := text
	for i := 0; i < 3; i++ {
		current = phpHTMLEntityDecode(current)
		if nativeInvisible(current, true) {
			return "decoded plain text cannot contain markup, brackets or control characters"
		}
	}
	return ""
}

func nativeTextProblem(text string, rich bool) string {
	if len(text) > 20000 || !utf8.ValidString(text) || utf8.RuneCountInString(text) > 5000 {
		return "write bounded valid UTF-8 text"
	}
	if why := nativeDecodedProblem(text); why != "" {
		return why
	}
	if nativeInvisible(text, false) {
		return "text cannot contain control or invisible formatting characters"
	}
	if rich {
		return nativeMarkupProblem(text)
	}
	return nativePlainProblem(text)
}

func nativeStoredTextProblem(text string) string {
	if len(text) == 0 || !utf8.ValidString(text) || utf8.RuneCountInString(text) > 5000 {
		return "write bounded nonempty plain text"
	}
	current := text
	for i := 0; i <= 3; i++ {
		if strings.ContainsAny(current, "<>\r\n\t") || strings.IndexFunc(current, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
			return "write plain text"
		}
		if why := nativeSequenceProblem(current); why != "" {
			return why
		}
		next := phpHTMLEntityDecode(current)
		if next == current {
			return ""
		}
		if i == 3 {
			return "character references are nested too deeply"
		}
		current = next
	}
	return ""
}

func nativeMarkupProblem(text string) string {
	stack := []string{}
	for len(text) > 0 {
		if text[0] != '<' {
			i := strings.IndexByte(text, '<')
			if i < 0 {
				i = len(text)
			}
			chunk := strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(text[:i])
			if why := nativePlainProblem(chunk); why != "" {
				return why
			}
			text = text[i:]
			continue
		}
		m := nativeRichTag.FindStringSubmatch(text)
		if m == nil {
			return "rich text must use the closed tag grammar"
		}
		tag := strings.ToLower(m[2])
		tail := m[3]
		if !slices.Contains([]string{"p", "br", "strong", "em", "b", "i", "u", "s", "ul", "ol", "li", "blockquote", "a"}, tag) {
			return "rich text uses an unlisted tag"
		}
		if m[1] == "/" {
			if strings.TrimSpace(tail) != "" || len(stack) == 0 || stack[len(stack)-1] != tag {
				return "rich text tags must be balanced"
			}
			stack = stack[:len(stack)-1]
		} else {
			if tag == "br" && (strings.TrimSpace(tail) == "" || strings.TrimSpace(tail) == "/") {
				tail = ""
			}
			seen := map[string]bool{}
			for strings.TrimSpace(tail) != "" {
				a := nativeRichAttr.FindStringSubmatch(tail)
				if a == nil {
					return "rich text attributes must use the closed quoted grammar"
				}
				key := strings.ToLower(a[1])
				if tag != "a" || !slices.Contains([]string{"href", "title", "target", "rel"}, key) || seen[key] {
					return "rich text uses a forbidden or repeated attribute"
				}
				seen[key] = true
				value := a[2][1 : len(a[2])-1]
				if why := nativePlainProblem(value); why != "" {
					return why
				}
				value = phpHTMLEntityDecode(value)
				if (key == "href" && !nativeLinkUsable(value)) || (key == "target" && !slices.Contains([]string{"_blank", "_self"}, value)) || (key == "rel" && !nativeRichRel.MatchString(value)) {
					return "rich text link attributes must use the closed link rules"
				}
				tail = tail[len(a[0]):]
			}
			if tag != "br" {
				if len(stack) >= 8 || (tag == "a" && slices.Contains(stack, "a")) {
					return "rich text nesting must stay within its closed grammar"
				}
				stack = append(stack, tag)
			}
		}
		text = text[len(m[0]):]
	}
	if len(stack) > 0 {
		return "rich text tags must be balanced"
	}
	return ""
}

func (s *nativePolicyState) validateNode(node any) {
	if !nativeMembers(node, "id", "elType", "widgetType", "isInner", "settings", "elements") {
		s.fail("result", "node", "a widget result must hold exactly the classic widget envelope")
		return
	}
	if !nativeRowID.MatchString(nativeString(nativeGet(node, "id"))) {
		s.fail("result", "id", "use a seven-character lower-case hex element id")
	}
	if nativeGet(node, "elType") != "widget" {
		s.fail("result", "elType", "the result element type must be widget")
	}
	if nativeGet(node, "widgetType") != nativeGet(s.schema, "widget") {
		s.fail("result", "widgetType", "the result must use the requested schema widget")
	}
	if _, ok := nativeGet(node, "isInner").(bool); !ok {
		s.fail("result", "isInner", "the node inner flag must be a boolean")
	}
	l, ok := nativeList(nativeGet(node, "elements"))
	if !ok || len(l) != 0 {
		s.fail("result", "elements", "a widget cannot hold children")
	}
	if len(s.failures) == 0 {
		settings := nativeGet(node, "settings")
		s.validateWritten(phpObject{}, settings, settings)
	}
}

func nativeSwitchReturn(c phpObject) (string, bool) {
	if nativeHas(c, "return_value") {
		v, ok := nativeGet(c, "return_value").(string)
		return v, ok && v != ""
	}
	if nativeString(nativeGet(c, "type")) == "switcher" {
		return "yes", true
	}
	return "", false
}
