package mcp

// wpmgr/page-edit input: the control plane's half of the rules the agent's
// PageEditValidator::parse() holds, the ones that need no page. Parse, don't
// strip: an unknown key, a wrong JSON type, a value outside an enum or a
// limit breach refuses; nothing is rewritten.
//
//   - the input is the exact JSON text of an object of at most 65536 bytes,
//     valid UTF-8, nested at most 32 deep, with no unpaired UTF-16 surrogate
//     escape (the agent's decoder refuses each of those);
//   - post_id, base_fingerprint and 1..25 operations, and an optional design
//     (page-create's design object, judged as page-create judges it), and
//     nothing else;
//   - each operation in the published schema's shape: one anchor per insert
//     or move, position only with into, refs of 1..32
//     letters, digits, "_" or "-";
//   - set_text's field a plain field or a pattern path ("plans.1.price"),
//     matched whole, and its text through wpmgr/page-create's rule for the
//     field ("text" the paragraph text rule, "caption" the caption rule, "alt"
//     the alt rule, "url" the button link rule); a path takes the rule of its
//     last step (url the link rule, alt the alt rule, cta.text the button text
//     rule, any other the text rule), and a link may name a section of the
//     page with "#" and its anchor. Which paths a node offers is the page's;
//   - each outline (1..50 nodes) through the page-create outline grammar
//     (page_create_input.go), version 3 included: a section, a pattern or a
//     block of version 2, each judged by what it is. Where a node may stand
//     and whether an anchor or a link is the page's own is the page's:
//     anchors and links are judged by the agent on the page the whole call
//     leaves;
//   - a ref named at most once in the whole call, as a target or an anchor
//     (ops_invalid).
//
// Duplicate raw keys and invalid native scalar shapes are native_settings_invalid;
// widget and outline operations mixed in one call are ops_invalid;
// every other refusal is bad_input. The rules that need the page (a ref on
// it, a field a node offers, a locked node, an operation the builder
// declares, the node counts) are the agent's alone. The shared case table
// (apps/agent/tests/fixtures/ability-run/page-edit-ops-cases.json) pins this
// side's answer for every case in its "go" column.

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// AbilityPageEdit is wpmgr/page-edit: one batch of operations on a draft
// WPMgr created with a page builder, asked for as one request.
const AbilityPageEdit = "wpmgr/page-edit"

// pageEditInputSchema is the input schema the AI sees in describe and in an
// input refusal: the agent's BuilderContract::pageEditInputSchema() byte for
// byte (a test compares it with the agent's fixture).
//
//go:embed page_edit_schema.json
var pageEditInputSchemaFile []byte

var pageEditInputSchema = json.RawMessage(strings.TrimSuffix(string(pageEditInputSchemaFile), "\n"))

// Limits of the page-edit input: the agent's BuilderContract constants.
const (
	pageEditMaxInputBytes   = 65536
	pageEditMaxDepth        = 32
	pageEditMaxOps          = 25
	pageEditMaxOutlinePerOp = 50
	pageEditMaxTextChars    = 5000
	pageEditMaxCaptionChars = 500
	pageEditMaxAltChars     = 300
	// pageEditMaxButtonTextChars is the length of a button's text: page-create's.
	pageEditMaxButtonTextChars = 80
	pageEditNativeSectionHint  = "This section holds Elementor widgets: remove it and add widgets with widget operations, or edit its text with set_text in a separate edit."
	pageEditMixedHint          = "Send widget operations and text or outline operations in separate edits."
	pageEditOpsInvalid         = "ops_invalid"
	pageEditOpSetText          = "set_text"
	pageEditOpInsert           = "insert"
	pageEditOpReplace          = "replace"
	pageEditOpRemove           = "remove"
	pageEditOpMove             = "move"
	pageEditTextRuleText       = "text"
	pageEditTextRuleAlt        = "alt"
	pageEditAnchorInto         = "into"
	pageEditFieldURL           = "url"
	pageEditFieldAlt           = "alt"
	pageEditFieldCaption       = "caption"
	pageEditFieldText          = "text"
	pageEditPositionFirst      = "first"
	pageEditPositionLast       = "last"
)

var (
	// pageEditRefPattern is BuilderContract::RE_REF: a node ref from
	// wpmgr/page-structure.
	pageEditRefPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
	// pageEditFingerprintPattern is BuilderContract::FINGERPRINT_PATTERN.
	pageEditFingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// pageEditPathPattern is what set_text may name beyond the plain fields,
	// the pattern the published schema gives the field: the pattern, the
	// position of the item in it from 0, and up to three steps down into the
	// item. Go's $ matches at the end of the text only.
	pageEditPathPattern = regexp.MustCompile(`^(features|testimonials|plans|faq|logos|stats)\.(0|[1-9][0-9]?)(\.[a-z0-9_]{1,24}){1,3}$`)
	// pageTextForbiddenRunes is page-create's class of control characters
	// (line breaks included), bidi overrides and isolates, the BOM and the
	// noncharacters U+FFFE and U+FFFF.
	pageTextForbiddenRunes = regexp.MustCompile("[\\x{0000}-\\x{001F}\\x{007F}-\\x{009F}\\x{200B}-\\x{200F}" +
		"\\x{202A}-\\x{202E}\\x{2066}-\\x{2069}\\x{FEFF}\\x{FFFE}\\x{FFFF}]")
	// pageTextBracketedNumber is the one bracketed text page-create allows.
	pageTextBracketedNumber = regexp.MustCompile(`\[[0-9]{1,4}\]`)
)

// pageTextForbiddenSequences are refused anywhere in a page-create text.
var pageTextForbiddenSequences = []string{"<", ">", "{{", "}}", "{%", "%}", "<!--", "-->", "`"}

// pageEditOp is one operation of a valid input, in the agent's normalised
// form: Ref is the target (set_text, replace, remove, move), Anchor and
// AnchorRef the place (insert, move), Outline the new nodes (insert,
// replace), each outline node's exact JSON text.
type pageEditOp struct {
	Index     int // Original input position used to derive node ids.
	Op        string
	Ref       string
	Field     string
	Text      string
	Anchor    string
	AnchorRef string
	Position  string
	Outline   []json.RawMessage
	Widget    string
	Settings  json.RawMessage
	Parsed    any
	Rules     []string
}

// refs is every ref the operation names: its target, then its anchor.
func (o pageEditOp) refs() []string {
	var out []string
	if o.Ref != "" {
		out = append(out, o.Ref)
	}
	if o.AnchorRef != "" {
		out = append(out, o.AnchorRef)
	}
	return out
}

// pageEditFacts are what the control plane reads from a valid input.
type pageEditFacts struct {
	postID          int64
	baseFingerprint string
	ops             []pageEditOp
	// design is the input's own design member, every field checked and its
	// colours upper case; nil when the input sends none.
	design *pageDesign
}

// addsNodes reports whether the call puts new nodes on the page: an insert or
// a replace.
func (f pageEditFacts) addsNodes() bool {
	for _, op := range f.ops {
		if len(op.Outline) > 0 {
			return true
		}
	}
	return false
}

// validatePageEditInput checks input against every page-edit rule that needs
// no page. It returns the facts, or the agent's refusal code for the first
// problem: ops_invalid for a ref named twice, bad_input for anything else,
// and for a design the codes of page-create's design.
func validatePageEditInput(input []byte) (pageEditFacts, string) {
	f, refusal := checkPageEditInput(input)
	return f, refusal.Code
}

// checkPageEditInput is validatePageEditInput with the failures behind a
// refusal: a design that is refused says which of its fields. The design is
// judged before the operations, as page-create judges it before the outline.
func checkPageEditInput(input []byte) (pageEditFacts, pageCreateRefusal) {
	bad := pageCreateRefusal{Code: pageCreateBadInput}
	if len(input) > pageEditMaxInputBytes || !utf8.Valid(input) || jsonNestedDeeperThan(input, pageEditMaxDepth) ||
		jsonHasLoneSurrogate(input) {
		return pageEditFacts{}, bad
	}
	decoded, valid := phpDecodeDocument(input, pageEditMaxDepth, true)
	if !valid {
		return pageEditFacts{}, bad
	}
	if failures := pageEditNativeDuplicateFailures(decoded); len(failures) > 0 {
		ops, _ := nativeList(nativeGet(decoded, "operations"))
		for i, op := range ops {
			name := nativeString(nativeGet(op, "op"))
			if name != "set_native" && name != "add_native" {
				continue
			}
			if settings, ok := nativeMap(nativeGet(op, "settings")); ok {
				for _, f := range pageEditNativeValueFailures(settings, fmt.Sprintf("operations[%d].settings", i)) {
					if len(failures) == refusalMaxFailures {
						break
					}
					failures = append(failures, f)
				}
			}
		}
		return pageEditFacts{}, pageCreateRefusal{Code: "native_settings_invalid", Failures: failures}
	}
	top, ok := jsonObjectOf(input)
	if !ok || !keysExactly(top, []string{"post_id", "base_fingerprint", "operations", "design"},
		"post_id", "base_fingerprint", "operations") {
		if !ok {
			return pageEditFacts{}, pageInputRefusal("", "input", "must be a JSON object")
		}
		failure := pageInputFieldsRefusal(input, "", []string{"post_id", "base_fingerprint", "operations", "design"},
			"post_id", "base_fingerprint", "operations")
		return pageEditFacts{}, pageInputRefusal(failure.Path, failure.Key, failure.Reason)
	}
	var f pageEditFacts
	if f.postID, ok = jsonIntOf(top["post_id"]); !ok || f.postID < 1 {
		return pageEditFacts{}, pageInputRefusal("", "post_id", "must be a positive integer")
	}
	if f.baseFingerprint, ok = jsonStringOf(top["base_fingerprint"]); !ok ||
		!pageEditFingerprintPattern.MatchString(f.baseFingerprint) {
		return pageEditFacts{}, pageInputRefusal("", "base_fingerprint", "must be a string of 64 lowercase hexadecimal characters from wpmgr/page-structure")
	}
	if raw, has := top["design"]; has {
		design, problem := validatePageDesign(raw)
		if problem != nil {
			return pageEditFacts{}, pageCreateRefusal{Code: problem.Code, Failures: refusalFailuresOfDesign(problem.Failures)}
		}
		f.design = &design
	}
	ops, ok := jsonArrayOf(top["operations"])
	if !ok || len(ops) == 0 || len(ops) > pageEditMaxOps {
		return pageEditFacts{}, pageInputRefusal("", "operations", fmt.Sprintf("must be a list of 1 to %d operations", pageEditMaxOps))
	}
	named := make(map[string]struct{}, len(ops)*2)
	f.ops = make([]pageEditOp, 0, len(ops))
	var nativeFailures []refusalFailure
	showsOn := false
	for i, raw := range ops {
		if item, valid := jsonObjectOf(raw); valid {
			name, _ := jsonStringOf(item["op"])
			if name == "set_native" || name == "add_native" {
				if settings, has := item["settings"]; has {
					if v, failure := nativeParseSettings(settings); failure == nil {
						m, _ := nativeMap(v)
						for _, f := range pageEditNativeValueFailures(m, fmt.Sprintf("operations[%d].settings", i)) {
							if len(nativeFailures) == refusalMaxFailures {
								break
							}
							nativeFailures = append(nativeFailures, f)
						}
					}
				}
			}
		}
		if len(nativeFailures) > 0 {
			continue
		}
		op, failure := checkPageEditOperation(raw, fmt.Sprintf("operations[%d]", i))
		if failure != nil {
			return pageEditFacts{}, pageInputRefusal(failure.Path, failure.Key, failure.Reason)
		}
		if op.Op == "set_shows_on" {
			if showsOn {
				return pageEditFacts{}, pageCreateRefusal{Code: pageEditOpsInvalid, Failures: []refusalFailure{{Path: fmt.Sprintf("operations[%d]", i), Key: "op", Reason: "set_shows_on is allowed at most once per edit"}}}
			}
			showsOn = true
		}
		op.Index = i
		refs := op.refs()
		if op.Op == "add_native" {
			refs = nil
		}
		if op.Op == "duplicate" {
			refs = []string{op.Ref}
		}
		for _, ref := range refs {
			if _, twice := named[ref]; twice {
				return pageEditFacts{}, pageCreateRefusal{Code: pageEditOpsInvalid}
			}
			named[ref] = struct{}{}
		}
		f.ops = append(f.ops, op)
	}
	if len(nativeFailures) > 0 {
		return pageEditFacts{}, pageCreateRefusal{Code: "native_settings_invalid", Failures: nativeFailures}
	}
	if pageEditMixedOperations(f.ops, nil, nil) {
		return pageEditFacts{}, pageCreateRefusal{Code: pageEditOpsInvalid, Hint: pageEditMixedHint}
	}
	return f, pageCreateRefusal{}
}

// Outline edits use a separate call from structural operations covering native widgets.
func pageEditNativeSectionOperations(ops []pageEditOp, kinds, parents map[string]string) bool {
	outline := false
	for _, op := range ops {
		outline = outline || op.Op == "set_text" || op.Op == "insert" || op.Op == "replace"
	}
	if !outline {
		return false
	}
	for _, op := range ops {
		if op.Op != "remove" && op.Op != "replace" {
			continue
		}
		for ref, kind := range kinds {
			if kind != "native" {
				continue
			}
			up := ref
			if op.Op == "remove" {
				up = parents[ref]
			}
			for depth := 0; up != "" && up != "root" && depth <= pageEditTreeMaxDepth; depth++ {
				if up == op.Ref {
					return true
				}
				up = parents[up]
			}
		}
	}
	return false
}

// Explicit operations are classified at parse time; remove/move use the page's kinds.
func pageEditMixedOperations(ops []pageEditOp, kinds, parents map[string]string) bool {
	if pageEditNativeSectionOperations(ops, kinds, parents) {
		return true
	}
	native, outline := false, false
	for _, op := range ops {
		switch op.Op {
		case "add_native", "set_native", "duplicate":
			native = true
		case pageEditOpSetText, pageEditOpInsert, pageEditOpReplace:
			outline = true
		case pageEditOpRemove, pageEditOpMove:
			kind := kinds[op.Ref]
			if kind == "native" {
				native = true
			} else if kind != "" && kind != "locked" {
				outline = true
			}
		}
	}
	return native && outline
}

// pageEditOperation is one operation in its normalised form, or ok=false.
func pageEditOperation(raw json.RawMessage) (pageEditOp, bool) {
	op, failure := checkPageEditOperation(raw, "")
	return op, failure == nil
}

// checkPageEditOperation keeps the operation grammar's first failure at its
// address in operations. Reasons contain rule text, never caller values.
func checkPageEditOperation(raw json.RawMessage, path string) (pageEditOp, *refusalFailure) {
	fail := func(key, reason string) (pageEditOp, *refusalFailure) {
		return pageEditOp{}, &refusalFailure{Path: path, Key: key, Reason: reason}
	}
	fieldsRefusal := func(allowed []string, required ...string) (pageEditOp, *refusalFailure) {
		failure := pageInputFieldsRefusal(raw, path, allowed, required...)
		return pageEditOp{}, &failure
	}
	const refReason = "must be a node ref of 1 to 32 letters, digits, underscores or hyphens from wpmgr/page-structure"
	const opReason = "must be set_text, insert, replace, remove, set_native, move, duplicate, add_native or set_shows_on"
	const fieldReason = "must be text, alt, caption, url or a pattern field path"
	outlineReason := fmt.Sprintf("must be a list of 1 to %d nodes following the page outline grammar", pageEditMaxOutlinePerOp)
	o, ok := jsonObjectOf(raw)
	if !ok {
		return fail("input", "must be a JSON object")
	}
	name, ok := jsonStringOf(o["op"])
	if !ok {
		return fail("op", opReason)
	}
	parsed, valid := phpDecodeDocument(raw, pageEditMaxDepth, true)
	if !valid {
		return fail("input", "must be valid JSON within the input depth limit")
	}
	op := pageEditOp{Op: name, Parsed: parsed}
	switch name {
	case "set_shows_on":
		if !keysExactly(o, []string{"op", "rules"}, "op", "rules") {
			return fieldsRefusal([]string{"op", "rules"}, "op", "rules")
		}
		var problem string
		op.Rules, problem = CheckThemeTemplateRules(o["rules"])
		if problem != "" {
			return fail("rules", problem)
		}
		return op, nil
	case pageEditOpSetText:
		if !keysExactly(o, []string{"op", "ref", "field", "text"}, "op", "ref", "field", "text") {
			return fieldsRefusal([]string{"op", "ref", "field", "text"}, "op", "ref", "field", "text")
		}
		if op.Field, ok = jsonStringOf(o["field"]); !ok {
			return fail("field", fieldReason)
		}
		if op.Text, ok = jsonStringOf(o["text"]); !ok || utf8.RuneCountInString(op.Text) > pageEditMaxTextChars {
			return fail("text", fmt.Sprintf("must be a string of at most %d characters", pageEditMaxTextChars))
		}
		if !pageEditFieldTextUsable(op.Field, op.Text) {
			if !stringIn(op.Field, []string{pageEditFieldText, pageEditFieldAlt, pageEditFieldCaption, pageEditFieldURL}) && !pageEditPathPattern.MatchString(op.Field) {
				return fail("field", fieldReason)
			}
			return fail("text", "must follow the plain text, length and link rules for the selected field")
		}
		if op.Ref, ok = pageEditRef(o["ref"]); !ok {
			return fail("ref", refReason)
		}
		return op, nil
	case pageEditOpInsert:
		if op.Anchor, ok = pageEditAnchor(o, "after", "before", pageEditAnchorInto); !ok {
			return fail("after", "send exactly one of after, before or into")
		}
		_, hasPosition := o["position"]
		if op.Anchor != pageEditAnchorInto && hasPosition {
			return fail("position", "is accepted only with into")
		}
		allowed := []string{"op", op.Anchor, "outline"}
		if op.Anchor == pageEditAnchorInto {
			allowed = []string{"op", pageEditAnchorInto, "position", "outline"}
		}
		if !keysExactly(o, allowed, "op", op.Anchor, "outline") {
			return fieldsRefusal(allowed, "op", op.Anchor, "outline")
		}
		if op.AnchorRef, ok = pageEditRef(o[op.Anchor]); !ok {
			return fail(op.Anchor, refReason)
		}
		if hasPosition {
			op.Position, ok = jsonStringOf(o["position"])
			if !ok || (op.Position != pageEditPositionFirst && op.Position != pageEditPositionLast) {
				return fail("position", "must be first or last")
			}
		}
		if op.Outline, ok = pageEditOutline(o["outline"]); !ok {
			return fail("outline", outlineReason)
		}
		return op, nil
	case pageEditOpReplace:
		if !keysExactly(o, []string{"op", "ref", "outline"}, "op", "ref", "outline") {
			return fieldsRefusal([]string{"op", "ref", "outline"}, "op", "ref", "outline")
		}
		if op.Ref, ok = pageEditRef(o["ref"]); !ok {
			return fail("ref", refReason)
		}
		if op.Outline, ok = pageEditOutline(o["outline"]); !ok {
			return fail("outline", outlineReason)
		}
		return op, nil
	case pageEditOpRemove:
		if !keysExactly(o, []string{"op", "ref"}, "op", "ref") {
			return fieldsRefusal([]string{"op", "ref"}, "op", "ref")
		}
		if op.Ref, ok = pageEditRef(o["ref"]); !ok {
			return fail("ref", refReason)
		}
		return op, nil
	case "set_native":
		if !keysExactly(o, []string{"op", "ref", "settings"}, "op", "ref", "settings") {
			return fieldsRefusal([]string{"op", "ref", "settings"}, "op", "ref", "settings")
		}
		if op.Ref, ok = pageEditRef(o["ref"]); !ok {
			return fail("ref", refReason)
		}
		op.Settings = o["settings"]
		if !pageEditNativeInputSettings(op.Settings) {
			return fail("settings", "must be an object of native settings")
		}
		return op, nil
	case pageEditOpMove, "duplicate", "add_native":
		if op.Anchor, ok = pageEditAnchor(o, "after", "before", "into"); !ok {
			return fail("after", "send exactly one of after, before or into")
		}
		allowed := []string{"op", op.Anchor}
		required := []string{"op", op.Anchor}
		if op.Anchor == "into" {
			allowed = append(allowed, "position")
		}
		if name == "add_native" {
			allowed = append(allowed, "widget", "settings")
			required = append(required, "widget", "settings")
		} else {
			allowed = append(allowed, "ref")
			required = append(required, "ref")
		}
		if !keysExactly(o, allowed, required...) {
			return fieldsRefusal(allowed, required...)
		}
		if name != "add_native" {
			if op.Ref, ok = pageEditRef(o["ref"]); !ok {
				return fail("ref", refReason)
			}
		}
		if op.AnchorRef, ok = pageEditRef(o[op.Anchor]); !ok {
			return fail(op.Anchor, refReason)
		}
		if raw, has := o["position"]; has {
			if op.Position, ok = jsonStringOf(raw); !ok || (op.Position != "first" && op.Position != "last") {
				return fail("position", "must be first or last")
			}
		}
		if name == "add_native" {
			if op.Widget, ok = jsonStringOf(o["widget"]); !ok || !nativeWidgetPattern.MatchString(op.Widget) {
				return fail("widget", "must be a widget name starting with a lowercase letter, followed by up to 63 lowercase letters, digits, underscores or hyphens")
			}
			op.Settings = o["settings"]
			if !pageEditNativeInputSettings(op.Settings) {
				return fail("settings", "must be an object of native settings")
			}
		}
		return op, nil
	}
	return fail("op", opReason)
}

// pageEditAnchor is the one anchor field an operation names, of anchors.
func pageEditAnchor(o map[string]json.RawMessage, anchors ...string) (string, bool) {
	named := ""
	for _, a := range anchors {
		if _, has := o[a]; has {
			if named != "" {
				return "", false
			}
			named = a
		}
	}
	return named, named != ""
}

// pageEditRef is a node ref, or ok=false.
func pageEditRef(raw json.RawMessage) (string, bool) {
	ref, ok := jsonStringOf(raw)
	if !ok || !pageEditRefPattern.MatchString(ref) {
		return "", false
	}
	return ref, true
}

// pageEditOutline is an outline of 1..50 nodes that the page-create outline
// grammar accepts, version 3 included. A section is read where a section
// stands, at the top; every other node as a child of a section, which is
// where the top of a version 2 outline stands, so a pattern, a block or a
// group is read as the grammar reads it there. Whether the page offers the
// place, and whether an anchor or a link is the page's own, is the agent's:
// the links of the outline are not resolved here. Whatever code the grammar
// gives, page-edit refuses it as bad_input, as the agent does.
func pageEditOutline(raw json.RawMessage) ([]json.RawMessage, bool) {
	list, ok := jsonArrayOf(raw)
	if !ok || len(list) == 0 || len(list) > pageEditMaxOutlinePerOp {
		return nil, false
	}
	w := pageWalk{seen: map[int64]struct{}{}, designed: true}
	for _, n := range list {
		at := pageAtSection
		if pageEditNodeIsSection(n) {
			at = pageAtTop
		}
		if code := w.node(n, at); code != "" {
			return nil, false
		}
	}
	return list, true
}

// pageEditNodeIsSection reports whether an outline node is a section.
func pageEditNodeIsSection(raw json.RawMessage) bool {
	n, ok := jsonObjectOf(raw)
	if !ok {
		return false
	}
	kind, _ := jsonStringOf(n["type"])
	return kind == "section"
}

// pageEditFieldTextUsable applies page-create's rule for a set_text field: a
// plain field, or a pattern path, which takes the rule of its last step.
func pageEditFieldTextUsable(field, text string) bool {
	switch field {
	case pageEditFieldURL:
		return pageEditLinkUsable(text)
	case pageEditFieldAlt:
		return pageTextProblem(text, pageEditMaxAltChars, pageEditTextRuleAlt) == ""
	case pageEditFieldCaption:
		return pageTextProblem(text, pageEditMaxCaptionChars, pageEditTextRuleText) == ""
	case pageEditFieldText:
		return pageTextProblem(text, pageEditMaxTextChars, pageEditTextRuleText) == ""
	}
	if pageEditPathPattern.MatchString(field) {
		return pageEditPathTextUsable(field, text)
	}
	return false
}

// pageEditPathTextUsable is the rule for the text of a pattern path: url the
// link rule, alt the alt rule, cta.text the button text rule, any other step
// the paragraph text rule.
func pageEditPathTextUsable(path, text string) bool {
	switch {
	case strings.HasSuffix(path, ".url"):
		return pageEditLinkUsable(text)
	case strings.HasSuffix(path, ".alt"):
		return pageTextProblem(text, pageEditMaxAltChars, pageEditTextRuleAlt) == ""
	case strings.HasSuffix(path, ".cta.text"):
		return pageTextProblem(text, pageEditMaxButtonTextChars, pageEditTextRuleText) == ""
	}
	return pageTextProblem(text, pageEditMaxTextChars, pageEditTextRuleText) == ""
}

// pageEditLinkUsable is a link set_text may give: page-create's link, or a
// link to a section of the page, "#" and an anchor of the anchor's shape.
// Whether the page has that anchor is the agent's to judge.
func pageEditLinkUsable(u string) bool {
	if slug, ok := strings.CutPrefix(u, "#"); ok {
		return pageAnchorPattern.MatchString(slug)
	}
	return pageLinkUsable(u)
}

// pageTextProblem is page-create's text rule (PageCreateBuilder::textProblem):
// why text is not acceptable under rule ("text" or "alt") with at most max
// characters, or "". Alt text may be empty; no text may be only white space.
func pageTextProblem(text string, max int, rule string) string {
	if !utf8.ValidString(text) {
		return "not valid UTF-8"
	}
	if text == "" && rule != pageEditTextRuleText {
		return ""
	}
	// PHP's trim() set: space, tab, line feed, carriage return, NUL, vertical tab.
	if strings.Trim(text, " \t\n\r\x00\x0B") == "" {
		return "empty"
	}
	if utf8.RuneCountInString(text) > max {
		return "too long"
	}
	if pageTextForbiddenRunes.MatchString(text) {
		return "contains a control or invisible formatting character"
	}
	for _, seq := range pageTextForbiddenSequences {
		if strings.Contains(text, seq) {
			return fmt.Sprintf("contains %q", seq)
		}
	}
	if rule == pageEditTextRuleAlt {
		if strings.ContainsAny(text, "[]") {
			return "contains a square bracket"
		}
		if pageCharacterReference.MatchString(text) {
			return "contains a character reference"
		}
		return ""
	}
	if strings.ContainsAny(pageTextBracketedNumber.ReplaceAllString(text, ""), "[]") {
		return "contains a square bracket"
	}
	return ""
}

// ---------------------------------------------------------------------------
// What the agent's JSON decoder refuses and encoding/json lets through
// ---------------------------------------------------------------------------

// jsonNestedDeeperThan reports whether the JSON text nests objects and
// arrays more than max deep (PHP's json_decode depth: "[1]" is depth 1).
func jsonNestedDeeperThan(raw []byte, max int) bool {
	depth, inString, escaped := 0, false, false
	for _, c := range raw {
		switch {
		case inString:
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
		case c == '"':
			inString = true
		case c == '{' || c == '[':
			if depth++; depth > max {
				return true
			}
		case c == '}' || c == ']':
			depth--
		}
	}
	return false
}

// jsonHasLoneSurrogate reports whether a string in the JSON text holds a
// \u escape of a UTF-16 surrogate that is not part of a high-low pair.
// encoding/json decodes one to U+FFFD; the agent's decoder refuses the input.
func jsonHasLoneSurrogate(raw []byte) bool {
	inString := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if !inString {
			if c == '"' {
				inString = true
			}
			continue
		}
		switch c {
		case '"':
			inString = false
		case '\\':
			if i+1 >= len(raw) {
				return false
			}
			if raw[i+1] != 'u' {
				i++
				continue
			}
			v, ok := jsonHex4(raw, i+2)
			if !ok {
				return false // not valid JSON; the strict reads refuse it
			}
			switch {
			case v >= 0xDC00 && v <= 0xDFFF:
				return true
			case v >= 0xD800 && v <= 0xDBFF:
				if i+7 >= len(raw) || raw[i+6] != '\\' || raw[i+7] != 'u' {
					return true
				}
				low, ok := jsonHex4(raw, i+8)
				if !ok || low < 0xDC00 || low > 0xDFFF {
					return true
				}
				i += 11
			default:
				i += 5
			}
		}
	}
	return false
}

// jsonHex4 reads four hex digits at raw[at:].
func jsonHex4(raw []byte, at int) (int, bool) {
	if at+4 > len(raw) {
		return 0, false
	}
	v := 0
	for _, c := range raw[at : at+4] {
		var d int
		switch {
		case c >= '0' && c <= '9':
			d = int(c - '0')
		case c >= 'a' && c <= 'f':
			d = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = int(c-'A') + 10
		default:
			return 0, false
		}
		v = v*16 + d
	}
	return v, true
}

// Native settings keep string scalars and collection identity. Null is a top-level deletion.
func pageEditNativeInputSettings(raw []byte) bool {
	v, failure := nativeParseSettings(raw)
	if failure != nil {
		return false
	}
	m, ok := nativeMap(v)
	if !ok {
		return false
	}
	return len(pageEditNativeValueFailures(m, "settings")) == 0
}
