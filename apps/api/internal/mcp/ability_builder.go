package mcp

// wpmgr/page-create with a page builder: the control plane's half of the
// builder contract the agent's BuilderRegistry and builder mappers enforce.
//
// An input asks for a page builder with editor "builder:<id>". The published
// schema offers one, builder:elementor, with an optional elementor_format.
// Before anything reaches the site the control plane checks, in the agent's
// order, after the outline grammar:
//
//  1. the catalogue entry's limits.builders_enabled names the builder (else
//     builder_not_enabled; a list that is not well formed enables nothing);
//  2. the outline keeps the builder's node rules (else the agent's own code
//     for the rule, naming the outline node, and for
//     node_not_supported_by_builder the setting and the values the builder
//     builds there).
//
// What depends on the site (whether Elementor is active, its version, the
// format the site uses, an image's library alt text) is the agent's to
// decide at precheck. A builder input then needs
// agentcmd.MinAgentVersionForBuilderAdapters on the site (PageCreateAgentFloor).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

const (
	// pageEditorBuilderPrefix starts every page-builder editor value.
	pageEditorBuilderPrefix = "builder:"
	// pageEditorBuilderElementor is the one page-builder editor the published
	// schema offers.
	pageEditorBuilderElementor = pageEditorBuilderPrefix + pageBuilderElementor
	pageBuilderElementor       = "elementor"
	// pageElementorFormatDefault is elementor_format when an input sends none.
	pageElementorFormatDefault = "site_default"
)

// pageElementorFormats are the elementor_format values the schema publishes.
var pageElementorFormats = []string{pageElementorFormatDefault, "classic", "atomic"}

func pageElementorFormatKnown(format string) bool {
	for _, f := range pageElementorFormats {
		if format == f {
			return true
		}
	}
	return false
}

// Refusal codes of the builder half of the contract, the agent's own.
const (
	pageBuilderNotEnabled       = "builder_not_enabled"
	pageBuilderNotAvailable     = "builder_not_available"
	pageNodeNotSupported        = "node_not_supported_by_builder"
	pageImageAltFromLibrary     = "image_alt_from_library"
	pageCreateContentInvalid    = "create_content_invalid"
	pageBuildersEnabledMaxItems = 32
)

// pageBuilderIDPattern is one id in limits.builders_enabled.
var pageBuilderIDPattern = regexp.MustCompile(`^[a-z0-9]{1,32}$`)

// pageBuildersEnabled reads a catalogue entry's limits.builders_enabled as
// the agent does: limits absent or null, or builders_enabled absent or null,
// enable none; otherwise it must be a list of at most 32 distinct builder
// ids. ok is false for any other shape.
func pageBuildersEnabled(limits []byte) ([]string, bool) {
	if len(limits) == 0 || strings.TrimSpace(string(limits)) == "null" {
		return []string{}, true
	}
	top, ok := jsonObjectOf(limits)
	if !ok {
		return nil, false
	}
	raw, has := top["builders_enabled"]
	if !has || strings.TrimSpace(string(raw)) == "null" {
		return []string{}, true
	}
	list, ok := jsonArrayOf(raw)
	if !ok || len(list) > pageBuildersEnabledMaxItems {
		return nil, false
	}
	ids := make([]string, 0, len(list))
	for _, item := range list {
		id, ok := jsonStringOf(item)
		if !ok || !pageBuilderIDPattern.MatchString(id) {
			return nil, false
		}
		for _, seen := range ids {
			if seen == id {
				return nil, false
			}
		}
		ids = append(ids, id)
	}
	return ids, true
}

// pageBuilderEnabled: the entry's limits enable builder. A list that is not
// well formed enables nothing.
func pageBuilderEnabled(limits []byte, builder string) bool {
	ids, ok := pageBuildersEnabled(limits)
	if !ok {
		return false
	}
	for _, id := range ids {
		if id == builder {
			return true
		}
	}
	return false
}

// pageCreateUsesBuilder reports whether a page-create input asks for a page
// builder: its editor is a JSON string that starts with "builder:".
func pageCreateUsesBuilder(input []byte) bool {
	top, ok := jsonObjectOf(input)
	if !ok {
		return false
	}
	editor, ok := jsonStringOf(top["editor"])
	return ok && strings.HasPrefix(editor, pageEditorBuilderPrefix)
}

// ---------------------------------------------------------------------------
// Elementor's node rules
// ---------------------------------------------------------------------------

// Values Elementor builds where the outline offers more.
var (
	elementorButtonStyles = []string{"fill"}
	elementorImageAligns  = []string{"none", "center"}
)

// elementorOwnAddress is a paragraph whose whole text is one http(s)
// address, with the agent's white space (space, tab, line feed, vertical
// tab, form feed, carriage return) around it allowed.
var elementorOwnAddress = regexp.MustCompile(`(?i)^[\t\n\x0B\f\r ]*https?://[^\t\n\x0B\f\r <>"]+[\t\n\x0B\f\r ]*$`)

// pageBuilderProblem is the first builder rule an outline breaks: the
// agent's code for it, the node in the agent's path syntax (outline[2],
// outline[0].children[1], outline[1].columns[0].children[2],
// outline[3].buttons[0]), the field of that node, and for
// node_not_supported_by_builder the values the builder builds there.
type pageBuilderProblem struct {
	code    string
	node    string
	field   string
	allowed []string
	hint    string
}

// elementorOutlineProblem walks a valid outline in document order (the
// agent's mapping order) and returns the first Elementor rule it breaks.
func elementorOutlineProblem(input []byte) *pageBuilderProblem {
	top, _ := jsonObjectOf(input)
	outline, _ := jsonArrayOf(top["outline"])
	for i, raw := range outline {
		if p := elementorNodeProblem(raw, fmt.Sprintf("outline[%d]", i)); p != nil {
			return p
		}
	}
	return nil
}

func elementorNodeProblem(raw json.RawMessage, path string) *pageBuilderProblem {
	n, ok := jsonObjectOf(raw)
	if !ok {
		return nil
	}
	switch kind, _ := jsonStringOf(n["type"]); kind {
	case "paragraph":
		if text, _ := jsonStringOf(n["text"]); elementorOwnAddress.MatchString(text) {
			return &pageBuilderProblem{code: pageCreateContentInvalid, node: path, field: "text", hint: hintElementorAddressParagraph}
		}
	case "quote":
		paras, _ := jsonArrayOf(n["paragraphs"])
		for j, p := range paras {
			if text, _ := jsonStringOf(p); elementorOwnAddress.MatchString(text) {
				return &pageBuilderProblem{code: pageCreateContentInvalid, node: path,
					field: fmt.Sprintf("paragraphs[%d]", j), hint: hintElementorAddressParagraph}
			}
		}
	case "image":
		if raw, has := n["align"]; has {
			if align, _ := jsonStringOf(raw); !stringIn(align, elementorImageAligns) {
				return &pageBuilderProblem{code: pageNodeNotSupported, node: path, field: "align",
					allowed: elementorImageAligns, hint: hintNodeNotSupportedByBuilder}
			}
		}
	case "buttons":
		list, _ := jsonArrayOf(n["buttons"])
		for j, rawButton := range list {
			at := fmt.Sprintf("%s.buttons[%d]", path, j)
			b, _ := jsonObjectOf(rawButton)
			if raw, has := b["style"]; has {
				if style, _ := jsonStringOf(raw); !stringIn(style, elementorButtonStyles) {
					return &pageBuilderProblem{code: pageNodeNotSupported, node: at, field: "style",
						allowed: elementorButtonStyles, hint: hintNodeNotSupportedByBuilder}
				}
			}
			if url, _ := jsonStringOf(b["url"]); strings.Contains(url, "&") {
				return &pageBuilderProblem{code: pageCreateLinkInvalid, node: at, field: "url", hint: hintElementorLinkAmpersand}
			}
		}
	case "group":
		children, _ := jsonArrayOf(n["children"])
		for j, child := range children {
			if p := elementorNodeProblem(child, fmt.Sprintf("%s.children[%d]", path, j)); p != nil {
				return p
			}
		}
	case "columns":
		cols, _ := jsonArrayOf(n["columns"])
		for k, rawCol := range cols {
			col, _ := jsonObjectOf(rawCol)
			children, _ := jsonArrayOf(col["children"])
			for m, child := range children {
				if p := elementorNodeProblem(child, fmt.Sprintf("%s.columns[%d].children[%d]", path, k, m)); p != nil {
					return p
				}
			}
		}
	}
	return nil
}

func stringIn(s string, set []string) bool {
	for _, v := range set {
		if s == v {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Refusals
// ---------------------------------------------------------------------------

const (
	msgAbilityBuilderNotEnabled = "WPMgr does not build pages with this page builder on this site, so nothing was asked"
	msgAbilityBuilderInput      = "the outline uses something this page builder cannot build, so nothing was asked"
	msgAbilityBuilderOutdated   = "Pages built with a page builder need the WPMgr plugin " +
		agentcmd.MinAgentVersionForBuilderAdapters + " or later on this site. Nothing was asked."
)

// Fixed hints for the builder refusals (GH #823 pattern: our words, never
// the site's).
const (
	hintBuilderNotEnabled = "WPMgr does not build pages with this page builder here. Send editor wordpress_blocks, " +
		"or wordpress_classic on a site that uses the classic editor."
	hintBuilderNotAvailable = "This page builder cannot be used for this page on this site now: it may be inactive, " +
		"at a version WPMgr does not support yet, not switched on for this post type, or the format asked for is not " +
		"available. Send elementor_format classic or leave it out, or use editor wordpress_blocks."
	hintNodeNotSupportedByBuilder = "Elementor builds every outline block, with two limits: a button's style is fill " +
		"(leave style out or send fill), and an image's align is none or center. node names the outline item to change, " +
		"field its setting, and allowed the values Elementor builds there."
	hintImageAltFromLibrary = "An image on an Elementor page shows the alt text saved with the image in the media " +
		"library. Set alt to that text exactly (wpmgr/rest-read route wp-v2-media-list gives it as alt_text), or ask " +
		"a person to change it in the media library first."
	hintElementorAddressParagraph = "On an Elementor page, a paragraph or quote paragraph whose whole text is one web " +
		"address becomes an embedded player. Add words around the address, or use a button for the link."
	hintElementorLinkAmpersand = "A button link on an Elementor page cannot contain &. Use a link without one."
)

// pageCreateBuilderRefusal is the refusal for a valid page-create input
// whose page builder the entry does not enable, or whose outline breaks the
// builder's node rules; nil when it may be prechecked (or asks for no
// builder).
func pageCreateBuilderRefusal(f pageCreateFacts, limits, input []byte) *toolRefusal {
	if f.builder == "" {
		return nil
	}
	if !pageBuilderEnabled(limits, f.builder) {
		return &toolRefusal{
			reason: reasonInvalidArguments,
			err: domain.Validation(ErrCodeInvalidToolArguments, msgAbilityBuilderNotEnabled).WithDetails(map[string]any{
				"argument": "input", "code": pageBuilderNotEnabled, "hint": hintBuilderNotEnabled, "retryable": false,
			}),
			meta: map[string]any{"argument": "input", "code": pageBuilderNotEnabled},
		}
	}
	var p *pageBuilderProblem
	switch f.builder {
	case pageBuilderElementor:
		p = elementorOutlineProblem(input)
	}
	if p == nil {
		return nil
	}
	details := map[string]any{
		"argument": "input", "code": p.code, "node": p.node, "field": p.field, "hint": p.hint, "retryable": false,
	}
	if p.allowed != nil {
		details["allowed"] = p.allowed
	}
	return &toolRefusal{
		reason: reasonInvalidArguments,
		err:    domain.Validation(ErrCodeInvalidToolArguments, msgAbilityBuilderInput).WithDetails(details),
		meta:   map[string]any{"argument": "input", "code": p.code},
	}
}

// ---------------------------------------------------------------------------
// The precheck of a page built with Elementor
// ---------------------------------------------------------------------------

// builderPreview is the precheck preview of a page built with Elementor:
// the page, the Elementor format, version and layout that build it, and
// the tree it saves. Media is present exactly when the outline has an
// image.
type builderPreview struct {
	PostType         string          `json:"post_type"`
	Editor           string          `json:"editor"`
	Format           string          `json:"format"`
	ElementorVersion string          `json:"elementor_version"`
	Layout           string          `json:"layout"`
	Status           string          `json:"status"`
	Title            string          `json:"title"`
	Tree             json.RawMessage `json:"tree"`
	Media            json.RawMessage `json:"media,omitempty"`
}

// verifyBuilderPageCreatePrecheck checks the site's precheck answer for a
// page built with Elementor, sent with request id requestID, against what
// this control plane sent (R2). The page and the image facts are checked as
// for any page-create. The format must be classic, the version and the
// layout well formed, and the tree exactly the one the control plane builds
// for this input with requestID, that layout and those facts; the preview
// digest is recomputed over that tree's bytes, so a precheck whose tree
// differs in any node, setting, id or order is refused before anything is
// stored.
func verifyBuilderPageCreatePrecheck(resp agentcmd.AbilityRunResponse, entrySum string, input []byte, f pageCreateFacts, requestID string) (checkedPrecheck, bool) {
	if f.builder != pageBuilderElementor || !precheckDigestsWellFormed(resp) {
		return checkedPrecheck{}, false
	}
	var pv builderPreview
	dec := json.NewDecoder(bytes.NewReader(resp.Preview))
	dec.DisallowUnknownFields()
	if len(resp.Preview) == 0 || dec.Decode(&pv) != nil || len(pv.Tree) == 0 {
		return checkedPrecheck{}, false
	}
	if pv.PostType != f.postType || pv.Editor != f.editor || pv.Status != "draft" || pv.Title != f.title {
		return checkedPrecheck{}, false
	}
	// Only the classic format is built; an input asking for atomic is
	// refused by the site, never answered with a classic tree.
	if pv.Format != elementorFormatClassic || f.elementorFormat == "atomic" {
		return checkedPrecheck{}, false
	}
	if !elementorVersionPattern.MatchString(pv.ElementorVersion) {
		return checkedPrecheck{}, false
	}
	if pv.Layout != elementorLayoutBoxes && pv.Layout != elementorLayoutRows {
		return checkedPrecheck{}, false
	}
	media, ok := verifyPreviewMedia(pv.Media, f)
	if !ok {
		return checkedPrecheck{}, false
	}
	if base, ok := pageCreateBaseFingerprint(f.postType, media); !ok || base != resp.BaseFingerprint {
		return checkedPrecheck{}, false
	}
	tree, ok := elementorClassicTree(input, requestID, pv.Layout == elementorLayoutBoxes, media)
	if !ok {
		return checkedPrecheck{}, false
	}
	want, ok := phpEncodeBytes(tree)
	if !ok {
		return checkedPrecheck{}, false
	}
	got, ok := phpCanonicalJSON(pv.Tree, elementorTreeMaxDepth)
	if !ok || !bytes.Equal(got, want) {
		return checkedPrecheck{}, false
	}
	prev, ok := phpJSONStringArray(pv.Editor, pv.Format, pv.ElementorVersion, pv.PostType, "draft", pv.Title, string(want))
	if !ok || sha256Hex(prev) != resp.PreviewDigest {
		return checkedPrecheck{}, false
	}
	if !precheckDigestMatches(resp, entrySum, input) {
		return checkedPrecheck{}, false
	}
	return checkedPrecheck{
		precheckDigest: resp.PrecheckDigest, previewDigest: resp.PreviewDigest,
		baseFingerprint: resp.BaseFingerprint, media: media,
		builder: &PageCardBuilder{
			Builder: pageBuilderElementor, Format: pv.Format, Version: pv.ElementorVersion, Layout: pv.Layout,
		},
	}, true
}
