package mcp

// wpmgr/page-edit's precheck answer, bound to the input the control plane
// holds (R2), and the request it becomes.
//
// The preview is
//
//	{post_id, builder, builder_version, format, title, changes,
//	 after_outline, tree_sha256, tree}
//
// and is accepted only when every one of these holds:
//
//   - post_id is the input's post, builder elementor, format classic, the
//     version well formed, the base fingerprint the input's;
//   - tree re-encodes (PHP's json_encode, default flags) to bytes whose
//     sha256 is tree_sha256, and preview_digest is sha256(json_encode(
//     ["wpmgr.page_edit.v1", builder, builder_version, post_id,
//     base_fingerprint, tree_json])) over those bytes; the precheck digest is
//     the one the entry, the input, the base fingerprint and the preview
//     digest give;
//   - changes are one per operation, in order, each naming the operation's
//     op, ref and anchor; a set_text's after is exactly the input's text, a
//     removed or replaced node is no longer on the page;
//   - the nodes an insert or replace made (its new_refs) are exactly the
//     elements under the new top-level nodes, and project to the projection
//     of the input's outline mapped for the same place;
//   - after_outline is the projection of the tree, recomputed here.
//
// Anything else refuses the request as precheck_mismatch; nothing is
// stored. The card shows the recomputed outline and the checked changes.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/humantext"
)

const (
	// pageEditDigestDomain is BuilderPageEdit::DIGEST_DOMAIN.
	pageEditDigestDomain = "wpmgr.page_edit.v1"
	// pageEditTreeMaxDepth bounds the decode of an edited tree, which may
	// hold settings a person set in Elementor.
	pageEditTreeMaxDepth = 64
	// PageEditCardKind is card_facts.kind of a page edit (m169's CHECK).
	PageEditCardKind = "builder_edit"
	// pageEditCardMaxBytes is the card_facts column's size limit.
	pageEditCardMaxBytes = 65536
	// pageEditCodePrecheckMismatch is the refusal for a precheck answer
	// that does not bind to the input.
	pageEditCodePrecheckMismatch = "precheck_mismatch"
)

// pageEditPreview is precheck's preview member.
type pageEditPreview struct {
	PostID         int64           `json:"post_id"`
	Builder        string          `json:"builder"`
	BuilderVersion string          `json:"builder_version"`
	Format         string          `json:"format"`
	Title          string          `json:"title"`
	Changes        json.RawMessage `json:"changes"`
	AfterOutline   json.RawMessage `json:"after_outline"`
	TreeSHA256     string          `json:"tree_sha256"`
	Tree           json.RawMessage `json:"tree"`
}

// phpTextMap is a {field: text} member, which PHP writes as [] when empty.
type phpTextMap map[string]string

func (m *phpTextMap) UnmarshalJSON(b []byte) error {
	if string(bytes.TrimSpace(b)) == "[]" {
		*m = phpTextMap{}
		return nil
	}
	var v map[string]string
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*m = v
	return nil
}

// pageEditChange is one member of the preview's changes.
type pageEditChange struct {
	Op      string                `json:"op"`
	Ref     string                `json:"ref,omitempty"`
	Kind    string                `json:"kind,omitempty"`
	Level   *int64                `json:"level,omitempty"`
	Before  *phpTextMap           `json:"before,omitempty"`
	After   *phpTextMap           `json:"after,omitempty"`
	NewRefs []string              `json:"new_refs,omitempty"`
	Anchor  *pageEditChangeAnchor `json:"anchor,omitempty"`
}

// pageEditChangeAnchor is a change's anchor.
type pageEditChangeAnchor struct {
	Ref      string  `json:"ref"`
	How      string  `json:"how"`
	Position *string `json:"position,omitempty"`
	Kind     string  `json:"kind"`
	Level    *int64  `json:"level,omitempty"`
	Label    *string `json:"label,omitempty"`
}

// checkedPageEdit is a page-edit precheck answer the control plane verified.
type checkedPageEdit struct {
	precheckDigest  string
	previewDigest   string
	baseFingerprint string
	postID          int64
	title           string
	builderVersion  string
	format          string
	changes         []pageEditChange
	afterOutline    map[string]any
}

// treeElement is one element of a decoded tree and where it stands.
type treeElement struct {
	node   any
	path   []int
	parent string // projectionRoot at the top
	elType string
}

// indexTree records every element of the tree, at any depth, by id. ok is
// false when an id is not a string or appears twice.
func indexTree(tree any) (map[string]*treeElement, bool) {
	out := map[string]*treeElement{}
	var walk func(list []any, parent string, path []int) bool
	walk = func(list []any, parent string, path []int) bool {
		for i, n := range list {
			id, ok := phpStringAt(n, "id")
			if !ok {
				return false
			}
			if _, twice := out[id]; twice {
				return false
			}
			elType, _ := phpStringAt(n, "elType")
			at := append(append([]int(nil), path...), i)
			out[id] = &treeElement{node: n, path: at, parent: parent, elType: elType}
			if children, _ := phpGet(n, "elements"); phpIsArray(children) {
				if !walk(phpValues(children), id, at) {
					return false
				}
			}
		}
		return true
	}
	return out, walk(phpValues(tree), projectionRoot, nil)
}

// pageEditPreviewDigest is BuilderPageEdit::previewDigest().
func pageEditPreviewDigest(builder, version string, postID int64, baseFP string, treeJSON []byte) (string, bool) {
	parts := make([]string, 0, 6)
	for _, s := range []string{pageEditDigestDomain, builder, version} {
		enc, ok := phpJSONString(s)
		if !ok {
			return "", false
		}
		parts = append(parts, enc)
	}
	parts = append(parts, strconv.FormatInt(postID, 10))
	for _, s := range []string{baseFP, string(treeJSON)} {
		enc, ok := phpJSONString(s)
		if !ok {
			return "", false
		}
		parts = append(parts, enc)
	}
	return sha256Hex([]byte("[" + strings.Join(parts, ",") + "]")), true
}

// verifyPageEditPrecheck checks the site's page-edit precheck answer
// against the input this control plane sent (R2).
func verifyPageEditPrecheck(resp agentcmd.AbilityRunResponse, entrySum string, input []byte, f pageEditFacts) (checkedPageEdit, bool) {
	if !precheckDigestsWellFormed(resp) || resp.BaseFingerprint != f.baseFingerprint {
		return checkedPageEdit{}, false
	}
	var pv pageEditPreview
	dec := json.NewDecoder(bytes.NewReader(resp.Preview))
	dec.DisallowUnknownFields()
	if len(resp.Preview) == 0 || dec.Decode(&pv) != nil || dec.More() || len(pv.Tree) == 0 {
		return checkedPageEdit{}, false
	}
	if pv.PostID != f.postID || pv.Builder != pageBuilderElementor || pv.Format != elementorFormatClassic ||
		!elementorVersionPattern.MatchString(pv.BuilderVersion) || !hex64Pattern.MatchString(pv.TreeSHA256) {
		return checkedPageEdit{}, false
	}
	tree, ok := phpDecodeDocument(pv.Tree, pageEditTreeMaxDepth, true)
	if !ok {
		return checkedPageEdit{}, false
	}
	treeJSON, ok := phpEncodeBytes(tree)
	if !ok || sha256Hex(treeJSON) != pv.TreeSHA256 {
		return checkedPageEdit{}, false
	}
	digest, ok := pageEditPreviewDigest(pv.Builder, pv.BuilderVersion, pv.PostID, resp.BaseFingerprint, treeJSON)
	if !ok || digest != resp.PreviewDigest || !precheckDigestMatches(resp, entrySum, input) {
		return checkedPageEdit{}, false
	}
	proj, err := elementorProject(tree)
	if err != nil {
		return checkedPageEdit{}, false
	}
	after, ok := proj.answer(pageStructureMaxNodes, pageStructureMaxBytes)
	if !ok {
		return checkedPageEdit{}, false
	}
	mine, ok1 := projectionJSON(after)
	theirs, ok2 := decodeLoose(pv.AfterOutline)
	if !ok1 || !ok2 || !reflect.DeepEqual(mine, theirs) {
		return checkedPageEdit{}, false
	}
	elements, ok := indexTree(tree)
	if !ok {
		return checkedPageEdit{}, false
	}
	var changes []pageEditChange
	cd := json.NewDecoder(bytes.NewReader(pv.Changes))
	cd.DisallowUnknownFields()
	if cd.Decode(&changes) != nil || cd.More() || len(changes) != len(f.ops) {
		return checkedPageEdit{}, false
	}
	made := map[string]bool{}
	for i, op := range f.ops {
		if !pageEditChangeMatches(changes[i], op, elements, made) {
			return checkedPageEdit{}, false
		}
		if op.Op == "insert" || op.Op == "replace" {
			if !pageEditNewNodesMatch(changes[i].NewRefs, op, tree, elements, proj) {
				return checkedPageEdit{}, false
			}
		}
		if op.Op == "set_text" && !pageEditTextLanded(op, proj) {
			return checkedPageEdit{}, false
		}
	}
	return checkedPageEdit{
		precheckDigest: resp.PrecheckDigest, previewDigest: resp.PreviewDigest,
		baseFingerprint: resp.BaseFingerprint, postID: pv.PostID, title: pv.Title,
		builderVersion: pv.BuilderVersion, format: pv.Format, changes: changes, afterOutline: after,
	}, true
}

// pageEditChangeMatches: change c is operation op's, as the agent's
// LayoutOps describes it. made collects every new_ref of the call, which
// no two changes share.
func pageEditChangeMatches(c pageEditChange, op pageEditOp, elements map[string]*treeElement, made map[string]bool) bool {
	if c.Op != op.Op || (c.Level != nil && (*c.Level < 1 || *c.Level > 6)) {
		return false
	}
	kindOK := func(kind string) bool { return kind == "locked" || projectionKinds[kind] }
	anchorOK := func() bool {
		a := c.Anchor
		if a == nil || a.Ref != op.AnchorRef || a.How != op.Anchor || !kindOK(a.Kind) ||
			(a.Level != nil && (*a.Level < 1 || *a.Level > 6)) {
			return false
		}
		if a.Label != nil && (a.Kind != "locked" || !elementorLockedLabelWords(*a.Label)) {
			return false
		}
		switch {
		case op.Position != "":
			return a.Position != nil && *a.Position == op.Position
		case a.Position == nil:
			return true
		default:
			return op.Anchor == "into" && (*a.Position == "first" || *a.Position == "last")
		}
	}
	newRefsOK := func() bool {
		if len(c.NewRefs) == 0 {
			return false
		}
		for _, r := range c.NewRefs {
			if !pageEditRefPattern.MatchString(r) || made[r] || elements[r] == nil {
				return false
			}
			made[r] = true
		}
		return true
	}
	switch op.Op {
	case "set_text":
		if c.Ref != op.Ref || !projectionKinds[c.Kind] || c.After == nil || len(*c.After) != 1 ||
			c.NewRefs != nil || c.Anchor != nil || elements[c.Ref] == nil {
			return false
		}
		text, ok := (*c.After)[op.Field]
		return ok && text == op.Text
	case "insert":
		return c.Ref == "" && c.Kind == "" && c.Before == nil && c.After == nil && anchorOK() && newRefsOK()
	case "replace":
		return c.Ref == op.Ref && projectionKinds[c.Kind] && c.After == nil && c.Anchor == nil &&
			elements[c.Ref] == nil && newRefsOK()
	case "remove":
		return c.Ref == op.Ref && projectionKinds[c.Kind] && c.After == nil && c.Anchor == nil &&
			c.NewRefs == nil && elements[c.Ref] == nil
	case "move":
		return c.Ref == op.Ref && projectionKinds[c.Kind] && c.After == nil && c.NewRefs == nil &&
			elements[c.Ref] != nil && anchorOK()
	}
	return false
}

// pageEditTextLanded: after a set_text, its node in the edited tree offers
// the field and holds exactly the input's text. A node a later operation of
// the call took off the page with its parent is not on it to check.
func pageEditTextLanded(op pageEditOp, proj *projection) bool {
	at, onPage := proj.refs[op.Ref]
	if !onPage {
		return true
	}
	for _, f := range proj.nodes[at].text {
		if f.field == op.Field {
			return f.text == op.Text
		}
	}
	return false
}

// pageEditNewNodesMatch: the elements an insert or replace made are exactly
// the elements under its new top-level nodes, which stand together in one
// parent, and they project as the operation's outline does when mapped for
// that place.
func pageEditNewNodesMatch(newRefs []string, op pageEditOp, tree any, elements map[string]*treeElement, proj *projection) bool {
	set := make(map[string]bool, len(newRefs))
	for _, r := range newRefs {
		set[r] = true
	}
	var tops []*treeElement
	topIDs := map[string]bool{}
	for _, r := range newRefs {
		if e := elements[r]; !set[e.parent] {
			tops = append(tops, e)
			topIDs[r] = true
		}
	}
	if len(tops) == 0 {
		return false
	}
	// The new top-level nodes stand together, in order, in one parent.
	sortTops(tops)
	parent, first := tops[0].parent, tops[0].path[len(tops[0].path)-1]
	for i, t := range tops {
		if t.parent != parent || t.path[len(t.path)-1] != first+i {
			return false
		}
	}
	// Every element under them is one the call made.
	under := 0
	for id, e := range elements {
		for cur, curID := e, id; ; {
			if topIDs[curID] {
				under++
				break
			}
			if cur.parent == projectionRoot {
				break
			}
			curID = cur.parent
			cur = elements[curID]
		}
	}
	if under != len(newRefs) {
		return false
	}
	place, containers, parentKind, ok := pageEditPlace(tree, parent, elements, proj, tops[0])
	if !ok {
		return false
	}
	want, ok := elementorFragment(op.Outline, place, containers)
	if !ok {
		return false
	}
	got := make(phpList, 0, len(tops))
	for _, t := range tops {
		got = append(got, t.node)
	}
	return sameProjection(got, want, parentKind)
}

// sortTops orders elements of one parent by their place in it.
func sortTops(tops []*treeElement) {
	for i := 1; i < len(tops); i++ {
		for j := i; j > 0 && tops[j].path[len(tops[j].path)-1] < tops[j-1].path[len(tops[j-1].path)-1]; j-- {
			tops[j], tops[j-1] = tops[j-1], tops[j]
		}
	}
}

// Mapping contexts of ElementorClassicMapper::mapFragment().
const (
	fragmentTop   = "top"
	fragmentGroup = "group"
	fragmentInner = "inner"
)

// pageEditPlace is LayoutOps::place() for new nodes that stand in parent:
// the mapping context, the layout they take, and the parent's projection
// kind ("" at the top of the page).
func pageEditPlace(tree any, parent string, elements map[string]*treeElement, proj *projection, first *treeElement) (string, bool, string, bool) {
	if parent == projectionRoot {
		switch first.elType {
		case "container":
			return fragmentTop, true, "", true
		case "section":
			return fragmentTop, false, "", true
		}
		return "", false, "", false
	}
	p := elements[parent]
	at, known := proj.refs[parent]
	if p == nil || !known {
		return "", false, "", false
	}
	kind := proj.nodes[at].kind
	switch {
	case p.elType == "container" && (kind == "group" || kind == "column"):
		ctx := fragmentInner
		if len(p.path) == 1 && kind == "group" {
			ctx = fragmentGroup
		}
		return ctx, true, kind, true
	case p.elType == "column" && kind == "column":
		ctx := fragmentInner
		if len(p.path) == 2 {
			if top, _ := phpStringAt(phpValues(tree)[p.path[0]], "elType"); top == "section" {
				ctx = fragmentGroup
			}
		}
		return ctx, false, kind, true
	}
	return "", false, "", false
}

// elementorFragment maps an outline for a place, as
// ElementorClassicMapper::mapFragment() does, with throwaway ids and image
// addresses: only its projection is used.
func elementorFragment(outline []json.RawMessage, context string, containers bool) (phpList, bool) {
	b := &elementorBuild{
		ids:  &elementorIDs{requestID: "r2-projection", taken: map[string]bool{}},
		urls: map[int64]string{}, containers: containers, anyImage: true,
	}
	if len(outline) == 0 {
		return nil, false
	}
	switch context {
	case fragmentTop:
		return b.top(outline, "outline")
	case fragmentGroup, fragmentInner:
		out := phpList{}
		for j, raw := range outline {
			at := fmt.Sprintf("outline[%d]", j)
			n, ok := jsonObjectOf(raw)
			if !ok {
				return nil, false
			}
			switch kind, _ := jsonStringOf(n["type"]); kind {
			case "group":
				return nil, false
			case "columns":
				if context == fragmentInner {
					return nil, false
				}
				built, ok := b.columns(n, at, true)
				if !ok {
					return nil, false
				}
				out = append(out, built)
			default:
				widgets, ok := b.leaf(n, at)
				if !ok {
					return nil, false
				}
				out = append(out, widgets...)
			}
		}
		return out, true
	}
	return nil, false
}

// sameProjection: two forests of elements project to the same nodes in the
// same order and shape, refs aside.
func sameProjection(got, want phpList, parentKind string) bool {
	project := func(forest phpList) ([]projNode, bool) {
		enc, ok := phpEncodeBytes(forest)
		if !ok {
			return nil, false
		}
		decoded, ok := phpDecodeDocument(enc, pageEditTreeMaxDepth, true)
		if !ok {
			return nil, false
		}
		p := newProjection()
		if elementorProjectNodes(phpValues(decoded), projectionRoot, parentKind, p) != nil {
			return nil, false
		}
		return p.nodes, true
	}
	a, ok1 := project(got)
	b, ok2 := project(want)
	if !ok1 || !ok2 || len(a) != len(b) {
		return false
	}
	shape := func(nodes []projNode) []string {
		at := map[string]int{}
		out := make([]string, len(nodes))
		for i, n := range nodes {
			at[n.ref] = i
			parent := -1
			if j, ok := at[n.parent]; ok {
				parent = j
			}
			n.ref, n.parent = "", strconv.Itoa(parent)
			enc, _ := n.encodePHP()
			out[i] = enc
		}
		return out
	}
	return reflect.DeepEqual(shape(a), shape(b))
}

// ---------------------------------------------------------------------------
// card_facts and the request
// ---------------------------------------------------------------------------

// PageEditCardFacts is a page edit's card_facts. Every value under a
// from_the_site member came from the site; after is the AI's text.
type PageEditCardFacts struct {
	Kind                string               `json:"kind"`
	Post                PageEditCardPost     `json:"post"`
	Builder             PageEditCardBuilder  `json:"builder"`
	Changes             []PageEditCardChange `json:"changes"`
	AfterOutline        json.RawMessage      `json:"after_outline,omitempty"`
	AfterOutlineOmitted bool                 `json:"after_outline_omitted,omitempty"`
	CheckedAt           string               `json:"checked_at"`
}

// PageEditCardPost is the post a page edit changes.
type PageEditCardPost struct {
	ID          int64                `json:"id"`
	FromTheSite PageEditCardPostSite `json:"from_the_site"`
}

// PageEditCardPostSite is the post's own text.
type PageEditCardPostSite struct {
	Title string `json:"title"`
}

// PageEditCardBuilder is the page builder the edit is saved through.
type PageEditCardBuilder struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Format  string `json:"format"`
}

// PageEditCardChange is one operation as the card shows it.
type PageEditCardChange struct {
	Op          string                  `json:"op"`
	Ref         string                  `json:"ref,omitempty"`
	Kind        string                  `json:"kind,omitempty"`
	Level       *int64                  `json:"level,omitempty"`
	Field       string                  `json:"field,omitempty"`
	After       *string                 `json:"after,omitempty"`
	NewRefs     []string                `json:"new_refs,omitempty"`
	Anchor      *PageEditCardAnchor     `json:"anchor,omitempty"`
	FromTheSite *PageEditCardChangeSite `json:"from_the_site,omitempty"`
}

// PageEditCardAnchor is where an insert or move puts its nodes. Label is
// WPMgr's words for a node it does not edit.
type PageEditCardAnchor struct {
	Ref      string  `json:"ref"`
	How      string  `json:"how"`
	Position *string `json:"position,omitempty"`
	Kind     string  `json:"kind"`
	Level    *int64  `json:"level,omitempty"`
	Label    *string `json:"label,omitempty"`
}

// PageEditCardChangeSite is the node's text before the change.
type PageEditCardChangeSite struct {
	Before map[string]string `json:"before"`
}

// buildPageEditCardFacts renders the card from a verified precheck. The
// outline after the edit is left out (after_outline_omitted) when the card
// would not fit the column with it.
func buildPageEditCardFacts(pc checkedPageEdit, now time.Time) ([]byte, error) {
	card := PageEditCardFacts{
		Kind:    PageEditCardKind,
		Post:    PageEditCardPost{ID: pc.postID, FromTheSite: PageEditCardPostSite{Title: cardSiteText(pc.title)}},
		Builder: PageEditCardBuilder{ID: pageBuilderElementor, Version: pc.builderVersion, Format: pc.format},
		Changes: make([]PageEditCardChange, 0, len(pc.changes)),
		// The time WPMgr checked the request on the site.
		CheckedAt: now.UTC().Format(time.RFC3339),
	}
	for _, c := range pc.changes {
		out := PageEditCardChange{Op: c.Op, Ref: c.Ref, Kind: c.Kind, Level: c.Level, NewRefs: c.NewRefs}
		if c.After != nil {
			for field, text := range *c.After {
				t := text
				out.Field, out.After = field, &t
			}
		}
		if c.Before != nil {
			before := make(map[string]string, len(*c.Before))
			for field, text := range *c.Before {
				before[field] = cardSiteText(text)
			}
			out.FromTheSite = &PageEditCardChangeSite{Before: before}
		}
		if a := c.Anchor; a != nil {
			out.Anchor = &PageEditCardAnchor{Ref: a.Ref, How: a.How, Position: a.Position, Kind: a.Kind, Level: a.Level, Label: a.Label}
		}
		card.Changes = append(card.Changes, out)
	}
	outline, err := json.Marshal(pc.afterOutline)
	if err != nil {
		return nil, fmt.Errorf("encode the outline after the edit: %w", err)
	}
	card.AfterOutline = outline
	b, err := json.Marshal(card)
	if err != nil {
		return nil, fmt.Errorf("encode card facts: %w", err)
	}
	if len(b) <= pageEditCardMaxBytes {
		return b, nil
	}
	card.AfterOutline, card.AfterOutlineOmitted = nil, true
	if b, err = json.Marshal(card); err != nil {
		return nil, fmt.Errorf("encode card facts: %w", err)
	}
	if len(b) > pageEditCardMaxBytes {
		return nil, errPageEditCardTooLarge
	}
	return b, nil
}

var errPageEditCardTooLarge = errors.New("the page edit card does not fit")

// ReadPageEditCardFacts reads a stored page-edit card, or ok=false.
func ReadPageEditCardFacts(b []byte) (PageEditCardFacts, bool) {
	var card PageEditCardFacts
	if json.Unmarshal(b, &card) != nil || card.Kind != PageEditCardKind {
		return PageEditCardFacts{}, false
	}
	return card, true
}

// pageEditEditor is the request's editor column for an Elementor edit.
const pageEditEditor = pageEditorBuilderElementor

// buildPageEditRequestFacts computes the stored facts and presented_digest
// of a page-edit request. The digest covers the exact input, the target
// post, the precheck and preview digests and the exact card_facts bytes.
func buildPageEditRequestFacts(auth AuthorizedRequest, row sqlc.Site, host string, e *sqlc.AbilityCatalogue, entrySum string, input, card []byte, pc checkedPageEdit, now time.Time) (abilityRequestFacts, error) {
	f := abilityRequestFacts{
		siteLabel:    humantext.CapRunes(humantext.Clean(row.Name), siteLabelRunes),
		siteHost:     host,
		grantLabel:   humantext.CapRunes(humantext.Clean(auth.GrantName), grantLabelRunes),
		grantVia:     grantViaToken,
		setupClient:  auth.SetupClient,
		titleExcerpt: humantext.CapRunes(humantext.Clean(pc.title), abilityTitleExcerptRunes),
		expiresAt:    now.UTC().Add(abilityRequestWindow).Truncate(time.Microsecond),
		card:         card,
		copyVersion:  AbilityCardCopyVersion,
	}
	if strings.TrimSpace(f.grantLabel) == "" {
		f.grantLabel = unnamedConnectionLabel
	}
	if auth.ViaOAuth {
		f.grantVia = grantViaBrowserSignIn
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return abilityRequestFacts{}, fmt.Errorf("read digest nonce: %w", err)
	}
	f.nonce = hex.EncodeToString(nonce[:])
	f.digest = pageEditDigest(auth, row, e, entrySum, input, pc, f)
	return f, nil
}

// pageEditDigest is the presented_digest over a page-edit request's facts.
func pageEditDigest(auth AuthorizedRequest, row sqlc.Site, e *sqlc.AbilityCatalogue, entrySum string, input []byte, pc checkedPageEdit, f abilityRequestFacts) string {
	canonical := map[string]any{
		"copy_version":     f.copyVersion,
		"mode":             "write",
		"ability_name":     e.Name,
		"entry_id":         e.EntryID.String(),
		"entry_sha256":     entrySum,
		"operator_perm":    *e.OperatorPermission,
		"input_json":       string(input),
		"input_sha256":     sha256Hex(input),
		"target":           fmt.Sprintf("post:%d", pc.postID),
		"title_excerpt":    f.titleExcerpt,
		"card_facts":       string(f.card),
		"editor":           pageEditEditor,
		"precheck_digest":  pc.precheckDigest,
		"preview_digest":   pc.previewDigest,
		"base_fingerprint": pc.baseFingerprint,
		"snapshot":         e.Snapshot,
		"effect_copy":      e.EffectCopy,
		"site_id":          row.ID.String(),
		"site_label":       f.siteLabel,
		"site_host":        f.siteHost,
		"grant_id":         auth.GrantID.String(),
		"grant_label":      f.grantLabel,
		"grant_via":        f.grantVia,
		"setup_client":     f.setupClient,
		"expires_at":       f.expiresAt.Format(time.RFC3339Nano),
		"digest_nonce":     f.nonce,
	}
	b, _ := json.Marshal(canonical)
	return sha256Hex(b)
}

// createPageEditRequest is step 8 for page-edit, in one connection-scoped
// transaction: the same lock, expiry, caps and one-pending dedupe as
// createAbilityRequest, keyed on the target post, stored under requestID,
// the id its precheck was sent with (new node ids derive from it).
func (s *Service) createPageEditRequest(ctx context.Context, store AbilityRequestStore, auth AuthorizedRequest, row sqlc.Site, host string, e *sqlc.AbilityCatalogue, entrySum string, input, card []byte, pc checkedPageEdit, requestID uuid.UUID) (abilityCreatedResult, error) {
	var out abilityCreatedResult
	targetKey := fmt.Sprintf("post:%d", pc.postID)
	targetID := pc.postID
	err := store.RunAbilityRequestTx(ctx, connectionScopedPrincipal(auth), func(tx pgx.Tx, q abilityRequestQueries) error {
		if err := q.TakeAssistantRequestXactLock(ctx, sqlc.TakeAssistantRequestXactLockParams{
			LockKey: grantRequestLockKey, LockID: auth.GrantID.String(),
		}); err != nil {
			return fmt.Errorf("take the connection's request lock: %w", err)
		}
		expired, err := q.ExpireLapsedPendingAbilityRequestsForGrantSite(ctx, sqlc.ExpireLapsedPendingAbilityRequestsForGrantSiteParams{
			TenantID: auth.TenantID, SiteID: row.ID, ProposedByGrantID: auth.GrantID,
		})
		if err != nil {
			return fmt.Errorf("expire lapsed ability requests: %w", err)
		}
		readExisting := func() (sqlc.AssistantAbilityRequest, bool, error) {
			ex, err := q.GetPendingAbilityRequestForTarget(ctx, sqlc.GetPendingAbilityRequestForTargetParams{
				TenantID: auth.TenantID, SiteID: row.ID, ProposedByGrantID: auth.GrantID,
				AbilityName: e.Name, TargetKey: &targetKey,
			})
			if errors.Is(err, pgx.ErrNoRows) {
				return ex, false, nil
			}
			return ex, err == nil, err
		}
		if capRef, err := checkAbilityRequestCaps(ctx, q, auth, row.ID, e.Name); err != nil {
			return err
		} else if capRef != nil {
			ex, found, err := readExisting()
			if err != nil {
				return fmt.Errorf("read the waiting ability request: %w", err)
			}
			if !found {
				return capRef
			}
			out = abilityResultFromRow(ex, true)
			return s.recordAbilityCreation(ctx, tx, auth, expired, out, *e.OperatorPermission)
		}
		f, err := buildPageEditRequestFacts(auth, row, host, e, entrySum, input, card, pc, s.now())
		if err != nil {
			return err
		}
		editor, preview := pageEditEditor, pc.previewDigest
		done := false
		for attempt := 0; attempt < 2 && !done; attempt++ {
			ins, err := q.InsertAbilityRequest(ctx, sqlc.InsertAbilityRequestParams{
				ID:       uuidToPG(requestID),
				TenantID: auth.TenantID, SiteID: row.ID, ProposedByGrantID: auth.GrantID,
				EntryID: e.EntryID, EntrySha256: entrySum, AbilityName: e.Name,
				OperatorPermission: *e.OperatorPermission,
				InputJson:          string(input), InputSha256: sha256Hex(input), TargetPostID: &targetID,
				PrecheckDigest: pc.precheckDigest, PreviewDigest: &preview, BaseFingerprint: pc.baseFingerprint,
				SiteLabel: f.siteLabel, SiteHost: f.siteHost, GrantLabel: f.grantLabel, GrantVia: f.grantVia,
				SetupClient: f.setupClient, TitleExcerpt: &f.titleExcerpt, Editor: &editor,
				EffectCopy: e.EffectCopy, Snapshot: e.Snapshot, CardCopyVersion: f.copyVersion,
				DigestNonce: f.nonce, PresentedDigest: f.digest, ExpiresAt: f.expiresAt,
				CardFacts: card,
			})
			if err == nil {
				out = abilityResultFromRow(ins, false)
				done = true
				break
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("insert the ability request: %w", err)
			}
			ex, found, err := readExisting()
			if err != nil {
				return fmt.Errorf("read the waiting ability request: %w", err)
			}
			if !found {
				continue
			}
			out = abilityResultFromRow(ex, true)
			done = true
		}
		if !done {
			return errors.New("the ability insert conflicted twice and the waiting row was gone both times")
		}
		return s.recordAbilityCreation(ctx, tx, auth, expired, out, *e.OperatorPermission)
	})
	if err != nil {
		return abilityCreatedResult{}, err
	}
	return out, nil
}
