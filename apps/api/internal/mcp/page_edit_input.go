package mcp

// wpmgr/page-edit input: the control plane's half of the rules the agent's
// PageEditValidator::parse() holds, the ones that need no page. Parse, don't
// strip: an unknown key, a wrong JSON type, a value outside an enum or a
// limit breach refuses; nothing is rewritten.
//
//   - the input is the exact JSON text of an object of at most 65536 bytes,
//     valid UTF-8, nested at most 32 deep, with no unpaired UTF-16 surrogate
//     escape (the agent's decoder refuses each of those);
//   - post_id, base_fingerprint and 1..25 operations, and nothing else;
//   - each operation in the published schema's shape: one anchor per insert
//     or move, position only on an insert into a node, refs of 1..32
//     letters, digits, "_" or "-";
//   - set_text's text through wpmgr/page-create's rule for its field ("text"
//     the paragraph text rule, "caption" the caption rule, "alt" the alt rule,
//     "url" the button link rule);
//   - each outline (1..50 nodes) through the page-create outline grammar
//     (page_create_input.go), whose text rules stay the agent's;
//   - a ref named at most once in the whole call, as a target or an anchor
//     (ops_invalid).
//
// Every other refusal is bad_input. The rules that need the page (a ref on
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
	pageEditOpsInvalid      = "ops_invalid"
	pageEditOpSetText       = "set_text"
	pageEditOpInsert        = "insert"
	pageEditOpReplace       = "replace"
	pageEditOpRemove        = "remove"
	pageEditOpMove          = "move"
	pageEditTextRuleText    = "text"
	pageEditTextRuleAlt     = "alt"
	pageEditAnchorInto      = "into"
	pageEditFieldURL        = "url"
	pageEditFieldAlt        = "alt"
	pageEditFieldCaption    = "caption"
	pageEditFieldText       = "text"
	pageEditPositionFirst   = "first"
	pageEditPositionLast    = "last"
)

var (
	// pageEditRefPattern is BuilderContract::RE_REF: a node ref from
	// wpmgr/page-structure.
	pageEditRefPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
	// pageEditFingerprintPattern is BuilderContract::FINGERPRINT_PATTERN.
	pageEditFingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
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
	Op        string
	Ref       string
	Field     string
	Text      string
	Anchor    string
	AnchorRef string
	Position  string
	Outline   []json.RawMessage
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
}

// validatePageEditInput checks input against every page-edit rule that needs
// no page. It returns the facts, or the agent's refusal code for the first
// problem: ops_invalid for a ref named twice, bad_input for anything else.
func validatePageEditInput(input []byte) (pageEditFacts, string) {
	if len(input) > pageEditMaxInputBytes || !utf8.Valid(input) || jsonNestedDeeperThan(input, pageEditMaxDepth) ||
		jsonHasLoneSurrogate(input) {
		return pageEditFacts{}, pageCreateBadInput
	}
	top, ok := jsonObjectOf(input)
	if !ok || !keysExactly(top, []string{"post_id", "base_fingerprint", "operations"},
		"post_id", "base_fingerprint", "operations") {
		return pageEditFacts{}, pageCreateBadInput
	}
	var f pageEditFacts
	if f.postID, ok = jsonIntOf(top["post_id"]); !ok || f.postID < 1 {
		return pageEditFacts{}, pageCreateBadInput
	}
	if f.baseFingerprint, ok = jsonStringOf(top["base_fingerprint"]); !ok ||
		!pageEditFingerprintPattern.MatchString(f.baseFingerprint) {
		return pageEditFacts{}, pageCreateBadInput
	}
	ops, ok := jsonArrayOf(top["operations"])
	if !ok || len(ops) == 0 || len(ops) > pageEditMaxOps {
		return pageEditFacts{}, pageCreateBadInput
	}
	named := make(map[string]struct{}, len(ops)*2)
	f.ops = make([]pageEditOp, 0, len(ops))
	for _, raw := range ops {
		op, ok := pageEditOperation(raw)
		if !ok {
			return pageEditFacts{}, pageCreateBadInput
		}
		for _, ref := range op.refs() {
			if _, twice := named[ref]; twice {
				return pageEditFacts{}, pageEditOpsInvalid
			}
			named[ref] = struct{}{}
		}
		f.ops = append(f.ops, op)
	}
	return f, ""
}

// pageEditOperation is one operation in its normalised form, or ok=false.
func pageEditOperation(raw json.RawMessage) (pageEditOp, bool) {
	o, ok := jsonObjectOf(raw)
	if !ok {
		return pageEditOp{}, false
	}
	name, ok := jsonStringOf(o["op"])
	if !ok {
		return pageEditOp{}, false
	}
	op := pageEditOp{Op: name}
	switch name {
	case pageEditOpSetText:
		if !keysExactly(o, []string{"op", "ref", "field", "text"}, "op", "ref", "field", "text") {
			return pageEditOp{}, false
		}
		if op.Field, ok = jsonStringOf(o["field"]); !ok {
			return pageEditOp{}, false
		}
		if op.Text, ok = jsonStringOf(o["text"]); !ok || utf8.RuneCountInString(op.Text) > pageEditMaxTextChars {
			return pageEditOp{}, false
		}
		if !pageEditFieldTextUsable(op.Field, op.Text) {
			return pageEditOp{}, false
		}
		if op.Ref, ok = pageEditRef(o["ref"]); !ok {
			return pageEditOp{}, false
		}
		return op, true
	case pageEditOpInsert:
		if op.Anchor, ok = pageEditAnchor(o, "after", "before", pageEditAnchorInto); !ok {
			return pageEditOp{}, false
		}
		_, hasPosition := o["position"]
		if op.Anchor != pageEditAnchorInto && hasPosition {
			return pageEditOp{}, false
		}
		allowed := []string{"op", op.Anchor, "outline"}
		if op.Anchor == pageEditAnchorInto {
			allowed = []string{"op", pageEditAnchorInto, "position", "outline"}
		}
		if !keysExactly(o, allowed, "op", op.Anchor, "outline") {
			return pageEditOp{}, false
		}
		if op.AnchorRef, ok = pageEditRef(o[op.Anchor]); !ok {
			return pageEditOp{}, false
		}
		if hasPosition {
			op.Position, ok = jsonStringOf(o["position"])
			if !ok || (op.Position != pageEditPositionFirst && op.Position != pageEditPositionLast) {
				return pageEditOp{}, false
			}
		}
		if op.Outline, ok = pageEditOutline(o["outline"]); !ok {
			return pageEditOp{}, false
		}
		return op, true
	case pageEditOpReplace:
		if !keysExactly(o, []string{"op", "ref", "outline"}, "op", "ref", "outline") {
			return pageEditOp{}, false
		}
		if op.Ref, ok = pageEditRef(o["ref"]); !ok {
			return pageEditOp{}, false
		}
		if op.Outline, ok = pageEditOutline(o["outline"]); !ok {
			return pageEditOp{}, false
		}
		return op, true
	case pageEditOpRemove:
		if !keysExactly(o, []string{"op", "ref"}, "op", "ref") {
			return pageEditOp{}, false
		}
		if op.Ref, ok = pageEditRef(o["ref"]); !ok {
			return pageEditOp{}, false
		}
		return op, true
	case pageEditOpMove:
		if op.Anchor, ok = pageEditAnchor(o, "after", "before"); !ok {
			return pageEditOp{}, false
		}
		if !keysExactly(o, []string{"op", "ref", op.Anchor}, "op", "ref", op.Anchor) {
			return pageEditOp{}, false
		}
		if op.Ref, ok = pageEditRef(o["ref"]); !ok {
			return pageEditOp{}, false
		}
		if op.AnchorRef, ok = pageEditRef(o[op.Anchor]); !ok {
			return pageEditOp{}, false
		}
		return op, true
	}
	return pageEditOp{}, false
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
// grammar accepts as the outline of a page of its own. Whatever code the
// grammar gives, page-edit refuses it as bad_input, as the agent does.
func pageEditOutline(raw json.RawMessage) ([]json.RawMessage, bool) {
	list, ok := jsonArrayOf(raw)
	if !ok || len(list) == 0 || len(list) > pageEditMaxOutlinePerOp {
		return nil, false
	}
	w := pageWalk{seen: map[int64]struct{}{}}
	for _, n := range list {
		if code := w.node(n, pageAtTop); code != "" {
			return nil, false
		}
	}
	return list, true
}

// pageEditFieldTextUsable applies page-create's rule for a set_text field.
func pageEditFieldTextUsable(field, text string) bool {
	switch field {
	case pageEditFieldURL:
		return pageLinkUsable(text)
	case pageEditFieldAlt:
		return pageTextProblem(text, pageEditMaxAltChars, pageEditTextRuleAlt) == ""
	case pageEditFieldCaption:
		return pageTextProblem(text, pageEditMaxCaptionChars, pageEditTextRuleText) == ""
	case pageEditFieldText:
		return pageTextProblem(text, pageEditMaxTextChars, pageEditTextRuleText) == ""
	}
	return false
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
