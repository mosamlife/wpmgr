package mcp

import (
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/elementornative"
)

const pageEditNativeDigestDomain = "wpmgr.page_edit.native.v1"
const pageEditNativeMaxAnswerBytes = 4 << 20

// Native answers bind the before document and each operation to the stored preview.
// Only checked card changes cross the persistence boundary.
type pageEditNativePreview struct {
	pageEditPreview
	BeforeTree       json.RawMessage `json:"before_tree"`
	BeforeTreeSHA256 string          `json:"before_tree_sha256"`
	Results          json.RawMessage `json:"results"`
	DigestInputs     json.RawMessage `json:"digest_inputs"`
}

type pageEditNativeResult struct {
	Op             json.RawMessage `json:"op"`
	ResultNode     json.RawMessage `json:"result_node,omitempty"`
	BeforeSettings json.RawMessage `json:"before_settings,omitempty"`
	NewIDs         []string        `json:"new_ids,omitempty"`
}

func pageEditNativeAnswer(raw []byte) bool {
	top, ok := jsonObjectOf(raw)
	if !ok {
		return false
	}
	for _, key := range []string{"before_tree", "before_tree_sha256", "results", "digest_inputs"} {
		if _, has := top[key]; has {
			return true
		}
	}
	return false
}

func verifyPageEditNativePrecheck(resp agentcmd.AbilityRunResponse, entrySum string, input []byte, f pageEditFacts) (checkedPageEdit, bool) {
	fail := func() (checkedPageEdit, bool) { return checkedPageEdit{}, false }
	if len(resp.Preview) > pageEditNativeMaxAnswerBytes || !precheckDigestsWellFormed(resp) || resp.BaseFingerprint != f.baseFingerprint || !precheckDigestMatches(resp, entrySum, input) {
		return fail()
	}
	whole, ok := phpDecodeDocument(resp.Preview, pageEditTreeMaxDepth+8, true)
	if !ok || nativeDuplicate(whole, "preview") != nil || jsonHasLoneSurrogate(resp.Preview) || !utf8.Valid(resp.Preview) {
		return fail()
	}
	var pv pageEditNativePreview
	if !nativeStrictJSON(resp.Preview, &pv) || pv.PostID != f.postID || pv.Builder != pageBuilderElementor || pv.Format != elementorFormatClassic || !elementorVersionPattern.MatchString(pv.BuilderVersion) {
		return fail()
	}
	before, ok1 := phpDecodeDocument(pv.BeforeTree, pageEditTreeMaxDepth, true)
	tree, ok2 := phpDecodeDocument(pv.Tree, pageEditTreeMaxDepth, true)
	if !ok1 || !ok2 || !nativeTreeShape(before) || !nativeTreeShape(tree) {
		return fail()
	}
	beforeBytes, ok1 := phpEncodeBytes(before)
	treeBytes, ok2 := phpEncodeBytes(tree)
	if !ok1 || !ok2 || sha256Hex(beforeBytes) != pv.BeforeTreeSHA256 || sha256Hex(treeBytes) != pv.TreeSHA256 {
		return fail()
	}
	original, ok := indexTree(before)
	if !ok {
		return fail()
	}
	kinds, good := nativeEditOperationKinds(before, original, pv.BuilderVersion)
	if !good || pageEditMixedOperations(f.ops, kinds) {
		return fail()
	}
	final, ok := indexTree(tree)
	if !ok {
		return fail()
	}
	digestInputs, ok := phpDecodeDocument(pv.DigestInputs, 32, true)
	if !ok || !nativeObjectKeys(digestInputs, "before_tree_sha256", "widgets", "active_breakpoints", "builder_version", "media") || nativeGet(digestInputs, "before_tree_sha256") != pv.BeforeTreeSHA256 || nativeGet(digestInputs, "builder_version") != pv.BuilderVersion {
		return fail()
	}
	active, ok := nativeEditBreakpoints(nativeGet(digestInputs, "active_breakpoints"))
	if !ok {
		return fail()
	}
	media, ok := nativeEditMedia(nativeGet(digestInputs, "media"))
	if !ok {
		return fail()
	}
	var results []pageEditNativeResult
	if !nativeStrictJSON(pv.Results, &results) || len(results) != len(f.ops) {
		return fail()
	}
	widgets, ok := nativeGet(digestInputs, "widgets").(phpObject)
	if !ok {
		return fail()
	}
	widgetNames := []string{}
	previous := ""
	for _, p := range widgets {
		hash, good := p.value.(string)
		if !good || !hex64Pattern.MatchString(hash) || p.key <= previous || len(elementornative.Lookup(p.key, pv.BuilderVersion)) == 0 {
			return fail()
		}
		previous = p.key
		widgetNames = append(widgetNames, p.key)
	}
	usedWidgets := []string{}
	usedMedia := map[string]bool{}
	taken := map[string]bool{}
	for id := range original {
		taken[id] = true
	}
	for _, op := range f.ops {
		for _, id := range op.refs() {
			taken[id] = true
		}
	}
	edited := before
	cards := []PageEditCardChange{}
	for i, op := range f.ops {
		r := results[i]
		echoed, good := phpDecodeDocument(r.Op, 32, true)
		if !good || !nativeEqual(echoed, nativeParsedOperation(op)) {
			return fail()
		}
		elements, good := indexTree(edited)
		if !good {
			return fail()
		}
		for _, id := range op.refs() {
			if original[id] == nil || elements[id] == nil {
				return fail()
			}
		}
		source := elements[op.Ref]
		card := PageEditCardChange{Op: op.Op, Ref: op.Ref}
		if (op.Op == "move" || op.Op == "remove") && source != nil && source.elType == "widget" &&
			len(elementornative.Lookup(nativeString(nativeGet(source.node, "widgetType")), pv.BuilderVersion)) > 0 &&
			(nativeDesignedAncestor(original[op.Ref], original) || nativeDesignedAncestor(source, elements)) {
			return fail()
		}
		nativeCard := &PageEditNativeCardChange{}
		card.Native = nativeCard
		var next any
		switch op.Op {
		case "set_native", "add_native":
			if nativeGet(digestInputs, "active_breakpoints") == nil {
				return fail()
			}
			if op.Op == "set_native" {
				if source == nil || !nativeLeafWidget(source.node) || nativeDesignedAncestor(source, elements) {
					return fail()
				}
				op.Widget, _ = phpStringAt(source.node, "widgetType")
			}
			if len(elementornative.Lookup(op.Widget, pv.BuilderVersion)) == 0 {
				return fail()
			}
			if !slices.Contains(usedWidgets, op.Widget) {
				usedWidgets = append(usedWidgets, op.Widget)
			}
			written, good := phpDecodeDocument(op.Settings, 17, true)
			if !good {
				return fail()
			}
			old := any(phpList{})
			if source != nil {
				old = nativeGet(source.node, "settings")
			}
			merged := nativeMerge(old, written)
			if op.Op == "add_native" {
				merged = written
			}
			oldBytes, _ := phpEncodeBytes(old)
			mergedBytes, _ := phpEncodeBytes(merged)
			schema, good := nativeOwnedSchema(op.Widget, pv.BuilderVersion, written)
			if !good {
				return fail()
			}
			policy := CheckNativeSettings(NativePolicyInput{Widget: op.Widget, ElementorVersion: pv.BuilderVersion, Schema: schema, Before: oldBytes, Settings: op.Settings, Result: mergedBytes, Breakpoints: active, Media: nativePolicyMedia(media)})
			if !policy.OK || !nativeEditWrittenMedia(written, media, usedMedia) {
				return fail()
			}
			nativeCard.Widget = op.Widget
			nativeCard.Keys = nativeCardKeys(old, written)
			var node any
			if op.Op == "set_native" {
				if len(r.NewIDs) != 0 || !nativeRawEqual(r.BeforeSettings, old) {
					return fail()
				}
				node = nativeSet(slices.Clone(source.node.(phpObject)), "settings", merged)
				edited, good = nativeReplaceNode(edited, source.path, node)
			} else {
				if len(r.BeforeSettings) != 0 || len(r.NewIDs) != 1 || !nativeClaimIDs(r.NewIDs, taken) {
					return fail()
				}
				node = phpObject{{"id", r.NewIDs[0]}, {"elType", "widget"}, {"settings", merged}, {"elements", phpList{}}, {"widgetType", op.Widget}}
				edited, good = nativePlaceNode(edited, op, node, elements, true)
				card.NewRefs = r.NewIDs
			}
			if !good || !nativeRawEqual(r.ResultNode, node) {
				return fail()
			}
			// Results describe the node at this operation; a later ancestor removal can take it off the page.
			if e := final[nativeString(nativeGet(node, "id"))]; e != nil && !nativeEqual(e.node, node) {
				return fail()
			}
		case "remove":
			if len(r.ResultNode) != 0 || len(r.BeforeSettings) != 0 || len(r.NewIDs) != 0 || source == nil {
				return fail()
			}
			nativeCard.Widget, _ = phpStringAt(source.node, "widgetType")
			nativeCard.RemovedCount = nativeSubtreeCount(source.node)
			nativeCard.RemovedOutline = nativeSubtreeOutline(source.node)
			edited, good = nativeSplice(edited, source.path[:len(source.path)-1], source.path[len(source.path)-1], 1, nil)
			if !good {
				return fail()
			}
		case "move":
			if len(r.ResultNode) != 0 || len(r.BeforeSettings) != 0 || len(r.NewIDs) != 0 || source == nil {
				return fail()
			}
			nativeCard.Widget, _ = phpStringAt(source.node, "widgetType")
			nativeCard.From = &PageEditNativePlace{Parent: source.parent, Index: source.path[len(source.path)-1]}
			if nativeDescendant(elements[op.AnchorRef], source) {
				return fail()
			}
			next, good = nativeSplice(edited, source.path[:len(source.path)-1], source.path[len(source.path)-1], 1, nil)
			if !good {
				return fail()
			}
			movedIndex, good := indexTree(next)
			if !good {
				return fail()
			}
			edited, good = nativePlaceNode(next, op, source.node, movedIndex, false)
			if !good {
				return fail()
			}
			landed, _ := indexTree(edited)
			e := landed[op.Ref]
			nativeCard.To = &PageEditNativePlace{Parent: e.parent, Index: e.path[len(e.path)-1]}
		case "duplicate":
			if source == nil || len(r.ResultNode) != 0 || len(r.BeforeSettings) != 0 || len(r.NewIDs) != nativeSubtreeCount(source.node) || !nativeClaimIDs(r.NewIDs, taken) || !nativeCopySafe(source.node, pv.BuilderVersion) || nativeDesignedAncestor(source, elements) {
				return fail()
			}
			idIndex := 0
			copy := nativeCopyIDs(source.node, r.NewIDs, &idIndex)
			if !nativeEditCopiedMedia(copy, media, usedMedia) {
				return fail()
			}
			edited, good = nativePlaceNode(edited, op, copy, elements, true)
			if !good {
				return fail()
			}
			nativeCard.Widget, _ = phpStringAt(source.node, "widgetType")
			card.NewRefs = r.NewIDs
		default:
			return fail()
		}
		cards = append(cards, card)
	}
	sort.Strings(usedWidgets)
	if !reflect.DeepEqual(widgetNames, usedWidgets) || len(usedMedia) != len(media) {
		return fail()
	}
	editedBytes, ok := phpEncodeBytes(edited)
	if !ok || !bytes.Equal(editedBytes, treeBytes) {
		return fail()
	}
	canonicalInputs, ok := nativeDigestInputsBytes(digestInputs)
	if !ok {
		return fail()
	}
	digestBytes, ok := phpEncodeBytes(phpList{pageEditNativeDigestDomain, pv.Builder, pv.BuilderVersion, json.Number(strconv.FormatInt(pv.PostID, 10)), resp.BaseFingerprint, string(treeBytes), string(canonicalInputs)})
	if !ok || sha256Hex(digestBytes) != resp.PreviewDigest {
		return fail()
	}
	after, ok := nativeEditOutline(tree, pv.AfterOutline, pv.BuilderVersion)
	if !ok {
		return fail()
	}
	return checkedPageEdit{precheckDigest: resp.PrecheckDigest, previewDigest: resp.PreviewDigest, baseFingerprint: resp.BaseFingerprint, postID: pv.PostID, title: pv.Title, builderVersion: pv.BuilderVersion, format: pv.Format, afterOutline: after, nativeChanges: cards}, true
}

func nativeStrictJSON(raw []byte, out any) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil {
		return false
	}
	var extra any
	return d.Decode(&extra) == io.EOF
}

func nativeObjectKeys(v any, keys ...string) bool {
	o, ok := v.(phpObject)
	if !ok || len(o) != len(keys) {
		return false
	}
	for i, p := range o {
		if p.key != keys[i] {
			return false
		}
	}
	return true
}

func nativeRawEqual(raw []byte, want any) bool {
	got, ok := phpDecodeDocument(raw, pageEditTreeMaxDepth, true)
	return ok && nativeEqual(got, want)
}

// Parsed operation echoes use the agent's field order and PHP's empty-array encoding.
func nativeParsedOperation(op pageEditOp) any {
	o := phpObject{{"op", op.Op}}
	if op.Ref != "" {
		o = append(o, phpPair{"ref", op.Ref})
	}
	if op.Op == "set_text" {
		o = append(o, phpPair{"field", op.Field}, phpPair{"text", op.Text})
	}
	if op.Anchor != "" {
		o = append(o, phpPair{op.Anchor, op.AnchorRef})
	}
	if op.Position != "" {
		o = append(o, phpPair{"position", op.Position})
	}
	if op.Widget != "" {
		o = append(o, phpPair{"widget", op.Widget})
	}
	if len(op.Settings) > 0 {
		v, _ := phpDecodeDocument(op.Settings, 17, true)
		o = append(o, phpPair{"settings", v})
	}
	if len(op.Outline) > 0 {
		return op.Parsed
	}
	return o
}

func nativeTreeShape(v any) bool {
	list, ok := v.(phpList)
	if !ok {
		return false
	}
	var walk func(phpList) bool
	walk = func(nodes phpList) bool {
		for _, n := range nodes {
			o, ok := n.(phpObject)
			if !ok || nativeDuplicate(o, "node") != nil {
				return false
			}
			id, ok := phpStringAt(n, "id")
			if !ok || !pageEditRefPattern.MatchString(id) || id == projectionRoot {
				return false
			}
			typ, ok := phpStringAt(n, "elType")
			if !ok || !slices.Contains([]string{"container", "section", "column", "widget"}, typ) {
				return false
			}
			if _, ok := nativeMap(nativeGet(n, "settings")); !ok {
				return false
			}
			children, ok := nativeGet(n, "elements").(phpList)
			if !ok || !walk(children) {
				return false
			}
		}
		return true
	}
	return walk(list)
}

func nativeLeafWidget(node any) bool {
	if nativeGet(node, "elType") != "widget" {
		return false
	}
	children, ok := nativeGet(node, "elements").(phpList)
	if !ok || len(children) != 0 {
		return false
	}
	flag := nativeGet(node, "isInner")
	return flag == nil || flag == false || flag == ""
}

func nativeDesignedAncestor(e *treeElement, index map[string]*treeElement) bool {
	for e != nil {
		label, _ := phpStringAt(nativeGet(e.node, "settings"), "_title")
		if strings.HasPrefix(label, "WPMgr · ") {
			return true
		}
		e = index[e.parent]
	}
	return false
}

// Classify remove/move targets from the verified source tree, never an echoed outline.
func nativeEditOperationKinds(tree any, index map[string]*treeElement, version string) (map[string]string, bool) {
	proj, err := elementorProject(tree)
	if err != nil {
		return nil, false
	}
	kinds := make(map[string]string, len(proj.nodes))
	for _, n := range proj.nodes {
		kind := n.kind
		if n.locked() && n.label != elementorLabelDynamic {
			e := index[n.ref]
			if e != nil && e.elType == "widget" && len(elementornative.Lookup(nativeString(nativeGet(e.node, "widgetType")), version)) > 0 {
				kind = "native"
			}
		}
		kinds[n.ref] = kind
	}
	return kinds, true
}

func nativeEditBreakpoints(v any) ([]string, bool) {
	if v == nil {
		return nil, true
	}
	list, ok := v.(phpList)
	if !ok {
		return nil, false
	}
	known := []string{"mobile", "mobile_extra", "tablet", "tablet_extra", "laptop", "widescreen"}
	last := -1
	out := []string{}
	for _, v := range list {
		s, ok := v.(string)
		i := slices.Index(known, s)
		if !ok || i <= last {
			return nil, false
		}
		last = i
		out = append(out, s)
	}
	return out, true
}

func nativeEditMedia(v any) ([]pageMediaFact, bool) {
	list, ok := v.(phpList)
	if !ok {
		return nil, false
	}
	// Reuse page-create's fact verifier, converting only the native contract's string id.
	normalized := phpList{}
	last := int64(0)
	for _, item := range list {
		if !nativeObjectKeys(item, "id", "url", "filename", "mime", "width", "height", "modified_gmt") {
			return nil, false
		}
		id, ok := nativeGet(item, "id").(string)
		if !ok || !nativeAttachmentID.MatchString(id) {
			return nil, false
		}
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil || n <= last {
			return nil, false
		}
		last = n
		normalized = append(normalized, nativeSet(slices.Clone(item.(phpObject)), "id", json.Number(id)))
	}
	b, ok := phpEncodeBytes(normalized)
	if !ok {
		return nil, false
	}
	return parsePageMedia(b)
}

func nativePolicyMedia(media []pageMediaFact) []NativeMediaFact {
	out := []NativeMediaFact{}
	for _, f := range media {
		out = append(out, NativeMediaFact{ID: strconv.FormatInt(f.ID, 10), URL: f.URL})
	}
	return out
}

func nativeEditWrittenMedia(v any, media []pageMediaFact, used map[string]bool) bool {
	switch x := v.(type) {
	case phpObject:
		if nativeHas(x, "id") && nativeHas(x, "url") {
			id, ok1 := nativeGet(x, "id").(string)
			u, ok2 := nativeGet(x, "url").(string)
			if !ok1 || !ok2 {
				return false
			}
			found := false
			for _, m := range media {
				if strconv.FormatInt(m.ID, 10) == id && m.URL == u {
					found = true
					used[id] = true
				}
			}
			if !found {
				return false
			}
		}
		for _, p := range x {
			if !nativeEditWrittenMedia(p.value, media, used) {
				return false
			}
		}
	case phpList:
		for _, item := range x {
			if !nativeEditWrittenMedia(item, media, used) {
				return false
			}
		}
	}
	return true
}

func nativeEditCopiedMedia(node any, media []pageMediaFact, used map[string]bool) bool {
	if !nativeEditWrittenMedia(nativeGet(node, "settings"), media, used) {
		return false
	}
	for _, child := range phpValues(nativeGet(node, "elements")) {
		if !nativeEditCopiedMedia(child, media, used) {
			return false
		}
	}
	return true
}

// The owned allowlist supplies the entire validation authority. No site schema is read.
func nativeOwnedSchema(widget, version string, written any) ([]byte, bool) {
	owned := elementornative.Lookup(widget, version)
	if len(owned) == 0 {
		return nil, false
	}
	keys := map[string]bool{}
	m, ok := nativeMap(written)
	if !ok {
		return nil, false
	}
	for _, p := range m {
		base, _ := nativeSplit(p.key)
		keys[base] = true
	}
	queue := make([]string, 0, len(keys))
	for key := range keys {
		queue = append(queue, key)
	}
	sort.Strings(queue)
	for i := 0; i < len(queue); i++ {
		key := queue[i]
		c, ok := owned[key]
		if !ok {
			return nil, false
		}
		if c.Group != nil && c.Group.Activator != nil {
			if !keys[*c.Group.Activator] {
				keys[*c.Group.Activator] = true
				queue = append(queue, *c.Group.Activator)
			}
			if key == *c.Group.Activator {
				for k, v := range owned {
					if v.Group != nil && v.Group.Family == c.Group.Family && v.Group.Activator != nil && *v.Group.Activator == key {
						if !keys[k] {
							keys[k] = true
							queue = append(queue, k)
						}
					}
				}
			}
		}
	}
	var control func(elementornative.Control) phpObject
	control = func(c elementornative.Control) phpObject {
		responsive := any(false)
		if c.Responsive != nil {
			responsive = true
			w := phpObject{}
			if c.Responsive.Min != nil {
				w = append(w, phpPair{"min", *c.Responsive.Min})
			}
			if c.Responsive.Max != nil {
				w = append(w, phpPair{"max", *c.Responsive.Max})
			}
			if len(w) > 0 {
				responsive = w
			}
		}
		o := phpObject{{"type", c.Type}, {"options", nativeStringList(c.Options)}, {"responsive", responsive}, {"units", nativeStringList(nativeUnits)}}
		if c.RawCSSValue {
			o = append(o, phpPair{"raw_css_value", true})
		}
		if c.Group != nil && c.Group.Activator != nil {
			o = append(o, phpPair{"group", c.Group.Family}, phpPair{"activator", phpObject{{"key", *c.Group.Activator}, {"values", nativeStringList(nativeGroupValues(c.Group.Family))}}})
		}
		if len(c.Fields) > 0 {
			fields := phpObject{}
			for _, f := range c.Fields {
				fields = append(fields, phpPair{f.ID, control(f)})
			}
			o = append(o, phpPair{"fields", fields})
		}
		// Future owned metadata, including return values, stays part of the authority.
		b, _ := json.Marshal(c)
		var metadata map[string]json.RawMessage
		_ = json.Unmarshal(b, &metadata)
		if raw, has := metadata["return_value"]; has {
			v, ok := phpDecodeDocument(raw, 8, true)
			if ok {
				o = append(o, phpPair{"return_value", v})
			}
		}
		return o
	}
	controls := phpObject{}
	names := []string{}
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		c, ok := owned[k]
		if !ok {
			return nil, false
		}
		controls = append(controls, phpPair{k, control(c)})
	}
	return phpEncodeBytes(phpObject{{"widget", widget}, {"atomic", false}, {"controls", controls}})
}

func nativeDigestInputsBytes(v any) ([]byte, bool) {
	o, ok := v.(phpObject)
	if !ok {
		return nil, false
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, p := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, ok := phpJSONString(p.key)
		if !ok {
			return nil, false
		}
		b.WriteString(k)
		b.WriteByte(':')
		if p.key == "widgets" {
			obj, ok := p.value.(phpObject)
			if !ok {
				return nil, false
			}
			if len(obj) == 0 {
				b.WriteString("{}")
				continue
			}
		}
		if !phpEncode(&b, p.value) {
			return nil, false
		}
	}
	b.WriteByte('}')
	return []byte(b.String()), true
}

func nativeClaimIDs(ids []string, taken map[string]bool) bool {
	for _, id := range ids {
		if !nativeRowID.MatchString(id) || taken[id] {
			return false
		}
		taken[id] = true
	}
	return true
}
func nativeDescendant(anchor, source *treeElement) bool {
	return anchor != nil && source != nil && len(anchor.path) >= len(source.path) && slices.Equal(anchor.path[:len(source.path)], source.path)
}
func nativeReplaceNode(tree any, path []int, node any) (any, bool) {
	if len(path) == 0 {
		return nil, false
	}
	return nativeSplice(tree, path[:len(path)-1], path[len(path)-1], 1, phpList{node})
}
func nativeSplice(tree any, parent []int, index, remove int, insert phpList) (any, bool) {
	list, ok := tree.(phpList)
	if !ok {
		return nil, false
	}
	list = slices.Clone(list)
	if len(parent) == 0 {
		if index < 0 || index > len(list) || index+remove > len(list) {
			return nil, false
		}
		return append(append(append(phpList{}, list[:index]...), insert...), list[index+remove:]...), true
	}
	i := parent[0]
	if i < 0 || i >= len(list) {
		return nil, false
	}
	n, ok := list[i].(phpObject)
	if !ok {
		return nil, false
	}
	children, ok := nativeSplice(nativeGet(n, "elements"), parent[1:], index, remove, insert)
	if !ok {
		return nil, false
	}
	list[i] = nativeSet(slices.Clone(n), "elements", children)
	return list, true
}
func nativePlaceNode(tree any, op pageEditOp, node any, index map[string]*treeElement, widgetOnly bool) (any, bool) {
	anchor := index[op.AnchorRef]
	if anchor == nil {
		return nil, false
	}
	parent := anchor.path[:len(anchor.path)-1]
	at := anchor.path[len(anchor.path)-1]
	if op.Anchor == "into" {
		parent = anchor.path
		at = len(phpValues(nativeGet(anchor.node, "elements")))
		if op.Position == "first" {
			at = 0
		}
	} else if op.Anchor == "after" {
		at++
	}
	var host *treeElement
	if len(parent) > 0 {
		for _, e := range index {
			if slices.Equal(e.path, parent) {
				host = e
				break
			}
		}
		if host == nil {
			return nil, false
		}
	}
	typ := nativeString(nativeGet(node, "elType"))
	if widgetOnly || typ == "widget" {
		if host == nil || (host.elType != "container" && host.elType != "column") || elementorHoldsDynamicKey(nativeGet(host.node, "settings"), 1) || nativeDesignedAncestor(host, index) || !nativeLeafFlag(node) {
			return nil, false
		}
	} else if host == nil {
		if typ != "container" && typ != "section" {
			return nil, false
		}
	} else {
		switch typ {
		case "column":
			if host.elType != "section" || nativeGet(host.node, "isInner") != nativeGet(node, "isInner") {
				return nil, false
			}
		case "section":
			if host.elType != "column" || !nativeInnerFlag(node) {
				return nil, false
			}
		case "container":
			if host.elType != "container" {
				return nil, false
			}
		default:
			return nil, false
		}
	}
	return nativeSplice(tree, parent, at, 0, phpList{node})
}
func nativeLeafFlag(node any) bool {
	v := nativeGet(node, "isInner")
	return v == nil || v == false || v == ""
}
func nativeInnerFlag(node any) bool { v := nativeGet(node, "isInner"); return v == true || v == "1" }
func nativeSubtreeCount(node any) int {
	n := 1
	for _, c := range phpValues(nativeGet(node, "elements")) {
		n += nativeSubtreeCount(c)
	}
	return n
}
func nativeCopyIDs(node any, ids []string, i *int) any {
	out := nativeSet(slices.Clone(node.(phpObject)), "id", ids[*i])
	*i++
	children := phpList{}
	for _, c := range phpValues(nativeGet(node, "elements")) {
		children = append(children, nativeCopyIDs(c, ids, i))
	}
	return nativeSet(out, "elements", children)
}
func nativeCopySafe(node any, version string) bool {
	if nativeGet(node, "elType") != "widget" || len(elementornative.Lookup(nativeString(nativeGet(node, "widgetType")), version)) == 0 {
		return false
	}
	var safe func(any) bool
	safe = func(v any) bool {
		switch x := v.(type) {
		case string:
			return nativeStoredTextProblem(x) == ""
		case phpObject:
			for _, p := range x {
				if nativeDeniedKey(p.key) || (p.key == "library" && p.value == "svg") || !safe(p.value) {
					return false
				}
			}
		case phpList:
			for _, v := range x {
				if !safe(v) {
					return false
				}
			}
		}
		return true
	}
	if !safe(node) {
		return false
	}
	for _, c := range phpValues(nativeGet(node, "elements")) {
		if !nativeCopySafe(c, version) {
			return false
		}
	}
	return true
}

func nativeEditOutline(tree any, raw []byte, version string) (map[string]any, bool) {
	proj, err := elementorProject(tree)
	if err != nil {
		return nil, false
	}
	theirs, ok := decodeLoose(raw)
	if !ok {
		return nil, false
	}
	outline, ok := theirs.(map[string]any)
	if !ok {
		return nil, false
	}
	nodes, ok := outline["nodes"].([]any)
	if !ok || len(nodes) != len(proj.nodes) {
		return nil, false
	}
	elements, ok := indexTree(tree)
	if !ok {
		return nil, false
	}
	for i, value := range nodes {
		node, ok := value.(map[string]any)
		if !ok {
			return nil, false
		}
		expected := proj.nodes[i]
		if node["kind"] == "native" {
			if !expected.locked() || expected.label == elementorLabelDynamic {
				return nil, false
			}
			e := elements[expected.ref]
			widget, _ := phpStringAt(e.node, "widgetType")
			if len(elementornative.Lookup(widget, version)) == 0 || node["ref"] != expected.ref || node["parent"] != expected.parent || node["widget"] != widget || len(node) != 5 {
				return nil, false
			}
			summary, ok := node["summary"].(string)
			if !ok || utf8.RuneCountInString(summary) > 120 {
				return nil, false
			}
		} else {
			wire := expected.wire()
			b, _ := json.Marshal(wire)
			canonical, _ := decodeLoose(b)
			if !reflect.DeepEqual(canonical, node) {
				return nil, false
			}
		}
	}
	// The shape and framing are checked through the same native structure projector as reads.
	out, ok := projectNativeStructure(outline)
	if !ok {
		return nil, false
	}
	answer, ok := proj.answer(pageStructureMaxNodes, pageStructureMaxBytes)
	if !ok {
		return nil, false
	}
	expected, ok := projectionJSON(answer)
	if !ok {
		return nil, false
	}
	exp := expected.(map[string]any)
	for key, v := range outline {
		if key != "nodes" && !reflect.DeepEqual(v, exp[key]) {
			return nil, false
		}
	}
	if len(outline) != len(exp) {
		return nil, false
	}
	// Card text is fenced; refs and widget types remain literal tokens.
	return out.(map[string]any), true
}

// The card carries only written keys, their previous values, and structural consequences.
type PageEditNativeCardChange struct {
	Widget         string                  `json:"widget,omitempty"`
	Keys           []PageEditNativeCardKey `json:"keys,omitempty"`
	RemovedCount   int                     `json:"removed_count,omitempty"`
	RemovedOutline []map[string]any        `json:"removed_outline,omitempty"`
	From           *PageEditNativePlace    `json:"from,omitempty"`
	To             *PageEditNativePlace    `json:"to,omitempty"`
}
type PageEditNativePlace struct {
	Parent string `json:"parent"`
	Index  int    `json:"index"`
}
type PageEditNativeCardKey struct {
	Key         string                   `json:"key"`
	FromTheSite map[string]any           `json:"from_the_site,omitempty"`
	After       any                      `json:"after,omitempty"`
	Removed     bool                     `json:"removed,omitempty"`
	Links       []PageEditNativeCardLink `json:"links,omitempty"`
}
type PageEditNativeCardLink struct {
	Text           string `json:"text"`
	ExternalDomain string `json:"external_domain,omitempty"`
}

func nativeCardKeys(before, written any) []PageEditNativeCardKey {
	m, _ := nativeMap(written)
	out := []PageEditNativeCardKey{}
	for _, p := range m {
		if p.value == nil && !nativeHas(before, p.key) {
			continue
		}
		if nativeHas(before, p.key) && nativeEqual(nativeGet(before, p.key), p.value) {
			continue
		}
		key := PageEditNativeCardKey{Key: p.key, Removed: p.value == nil}
		if nativeHas(before, p.key) {
			key.FromTheSite = map[string]any{"before": nativeFenceValue(nativeGet(before, p.key))}
		}
		if p.value != nil {
			key.After = nativeCardValue(p.value)
			key.Links = nativeCardLinks(p.value)
		}
		out = append(out, key)
	}
	return out
}
func nativeCardValue(v any) any {
	b, _ := phpEncodeBytes(v)
	var out any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	_ = d.Decode(&out)
	return out
}
func nativeFenceValue(v any) any {
	switch x := nativeCardValue(v).(type) {
	case string:
		return fenceSiteText(x)
	case map[string]any:
		for k, v := range x {
			x[k] = nativeFenceGoValue(v)
		}
		return x
	case []any:
		for i, v := range x {
			x[i] = nativeFenceGoValue(v)
		}
		return x
	default:
		return x
	}
}
func nativeFenceGoValue(v any) any {
	switch x := v.(type) {
	case string:
		return fenceSiteText(x)
	case map[string]any:
		for k, v := range x {
			x[k] = nativeFenceGoValue(v)
		}
	case []any:
		for i, v := range x {
			x[i] = nativeFenceGoValue(v)
		}
	}
	return v
}
func nativeCardLinks(v any) []PageEditNativeCardLink {
	out := []PageEditNativeCardLink{}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			u, err := url.Parse(x)
			if err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" {
				out = append(out, PageEditNativeCardLink{Text: x, ExternalDomain: u.Hostname()})
			}
		case phpObject:
			for _, p := range x {
				walk(p.value)
			}
		case phpList:
			for _, v := range x {
				walk(v)
			}
		}
	}
	walk(v)
	return out
}
func nativeSubtreeOutline(node any) []map[string]any {
	out := []map[string]any{}
	var walk func(any)
	walk = func(n any) {
		id, _ := phpStringAt(n, "id")
		typ, _ := phpStringAt(n, "elType")
		widget, _ := phpStringAt(n, "widgetType")
		row := map[string]any{"ref": id, "kind": typ}
		if widget != "" {
			row["widget"] = widget
		}
		out = append(out, row)
		for _, c := range phpValues(nativeGet(n, "elements")) {
			walk(c)
		}
	}
	walk(node)
	return out
}

// Postgres jsonb::text uses decoded Unicode, escaped JSON strings and spaced separators.
func pageEditJSONBTextSize(raw []byte) (int, error) {
	var v any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&v); err != nil {
		return 0, err
	}
	var encode func(any) (string, error)
	encode = func(v any) (string, error) {
		switch x := v.(type) {
		case map[string]any:
			keys := []string{}
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			parts := []string{}
			for _, k := range keys {
				value, err := encode(x[k])
				if err != nil {
					return "", err
				}
				name, _ := encode(k)
				parts = append(parts, name+": "+value)
			}
			return "{" + strings.Join(parts, ", ") + "}", nil
		case []any:
			parts := []string{}
			for _, v := range x {
				part, err := encode(v)
				if err != nil {
					return "", err
				}
				parts = append(parts, part)
			}
			return "[" + strings.Join(parts, ", ") + "]", nil
		case json.Number:
			return pageEditJSONBNumber(string(x))
		default:
			var b bytes.Buffer
			e := json.NewEncoder(&b)
			e.SetEscapeHTML(false)
			if err := e.Encode(v); err != nil {
				return "", err
			}
			return strings.TrimSuffix(b.String(), "\n"), nil
		}
	}
	b, err := encode(v)
	return len(b), err
}

// PostgreSQL expands JSONB exponent numbers into their stored decimal scale.
// The card is refused if that expansion alone cannot fit the column.
func pageEditJSONBNumber(number string) (string, error) {
	power := strings.IndexAny(number, "eE")
	if power < 0 {
		return number, nil
	}
	exponent, err := strconv.Atoi(number[power+1:])
	if err != nil || exponent > pageEditCardMaxBytes || exponent < -pageEditCardMaxBytes {
		return "", errPageEditCardTooLarge
	}
	mantissa := number[:power]
	sign := ""
	if strings.HasPrefix(mantissa, "-") {
		sign = "-"
		mantissa = mantissa[1:]
	}
	point := strings.IndexByte(mantissa, '.')
	if point < 0 {
		point = len(mantissa)
	}
	digits := strings.ReplaceAll(mantissa, ".", "")
	point += exponent
	if point < 0 && -point+len(digits) > pageEditCardMaxBytes {
		return "", errPageEditCardTooLarge
	}
	if point > pageEditCardMaxBytes {
		return "", errPageEditCardTooLarge
	}
	var result string
	switch {
	case point <= 0:
		result = "0." + strings.Repeat("0", -point) + digits
	case point >= len(digits):
		result = digits + strings.Repeat("0", point-len(digits))
	default:
		result = digits[:point] + "." + digits[point:]
	}
	if dot := strings.IndexByte(result, '.'); dot >= 0 {
		integer := strings.TrimLeft(result[:dot], "0")
		if integer == "" {
			integer = "0"
		}
		result = integer + result[dot:]
	} else {
		result = strings.TrimLeft(result, "0")
		if result == "" {
			result = "0"
		}
	}
	if len(result)+len(sign) > pageEditCardMaxBytes {
		return "", errPageEditCardTooLarge
	}
	return sign + result, nil
}
