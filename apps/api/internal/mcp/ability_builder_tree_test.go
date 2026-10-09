package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// elementorGoldenPHPVectors is, per layout, sha256 of PHP's json_encode (no
// flags) of every golden case's tree in fixture order, joined with "\n":
// the bytes the agent's preview digest is computed over. Computed by PHP
// from the fixture:
//
//	php -r '$f=json_decode(file_get_contents("elementor-classic-<layout>.json"),true);
//	  echo hash("sha256", implode("\n", array_map(fn($c)=>json_encode($c["tree"]), $f["cases"])));'
//
// A golden the agent changes changes its vector; recompute it the same way.
var elementorGoldenPHPVectors = map[string]string{
	elementorLayoutBoxes: "56be48be11fb9a557c3874b48f3c1053062044c858c19c2095e9c588af8c4c82",
	elementorLayoutRows:  "efe53284fc95b7f2524dfd771f581604e9d8e9f3799a42584d81ea9c0702542f",
}

// elementorPHPPreviewDigest is PHP's sha256(json_encode(["builder:elementor",
// "classic", "3.35.9", "page", "draft", elementorDigestTitle, tree_json])) for
// the containers golden "mixed-top-level", computed by PHP as above.
const (
	elementorPHPPreviewDigest = "ac82d4cbc8e789b6beb0a99d9495bb2389357ca00b5014e81ddb8567d62ed1c5"
	elementorDigestTitle      = "Our van été / plan"
	elementorTestVersion      = "3.35.9"
)

type elementorGoldenCase struct {
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	Tree  json.RawMessage `json:"tree"`
}

type elementorGolden struct {
	layout    string
	RequestID string `json:"request_id"`
	Media     map[string]struct {
		URL string `json:"url"`
	} `json:"media"`
	Cases []elementorGoldenCase `json:"cases"`
}

func readElementorGoldens(t *testing.T) []elementorGolden {
	t.Helper()
	var out []elementorGolden
	for _, layout := range []string{elementorLayoutBoxes, elementorLayoutRows} {
		path := agentAbilityFixtures + "elementor-classic-" + layout + ".json"
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var g elementorGolden
		if err := json.Unmarshal(raw, &g); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if len(g.Cases) == 0 || g.RequestID == "" {
			t.Fatalf("%s holds no cases or no request id", path)
		}
		g.layout = layout
		out = append(out, g)
	}
	return out
}

// goldenMedia is the usable image facts of ids, with the golden's address.
func goldenMedia(t *testing.T, g elementorGolden, ids []int64) []pageMediaFact {
	t.Helper()
	var out []pageMediaFact
	for _, id := range ids {
		m, ok := g.Media[strconv.FormatInt(id, 10)]
		if !ok {
			t.Fatalf("the golden has no facts for attachment %d", id)
		}
		f := pageMediaFact{ID: id, URL: m.URL, Filename: "image-" + strconv.FormatInt(id, 10) + ".png",
			Mime: "image/png", Width: 1024, Height: 640, ModifiedGMT: "2026-10-01 10:00:00"}
		if !f.usable() {
			t.Fatalf("test facts for %d are not usable", id)
		}
		out = append(out, f)
	}
	return out
}

func mustCanonical(t *testing.T, raw []byte) []byte {
	t.Helper()
	b, ok := phpCanonicalJSON(raw, elementorTreeMaxDepth)
	if !ok {
		t.Fatalf("not a canonical tree: %s", raw)
	}
	return b
}

// TestElementorClassicTreeIsTheAgentGolden: for every golden case of both
// layouts, the tree the control plane builds is the agent's golden tree,
// byte for byte as PHP encodes it, and both are the bytes PHP's json_encode
// gives (the pinned vectors).
func TestElementorClassicTreeIsTheAgentGolden(t *testing.T) {
	for _, g := range readElementorGoldens(t) {
		var built, golden []string
		for _, c := range g.Cases {
			input := []byte(elementorInput(string(c.Input)))
			f, code := validatePageCreateInput(input)
			if code != "" {
				t.Fatalf("%s %s: grammar refused the golden input (%s)", g.layout, c.Name, code)
			}
			tree, ok := elementorClassicTree(input, g.RequestID, g.layout == elementorLayoutBoxes, goldenMedia(t, g, f.mediaIDs))
			if !ok {
				t.Fatalf("%s %s: the control plane could not build the tree", g.layout, c.Name)
			}
			got, ok := phpEncodeBytes(tree)
			if !ok {
				t.Fatalf("%s %s: the tree did not encode", g.layout, c.Name)
			}
			want := mustCanonical(t, c.Tree)
			if !bytes.Equal(got, want) {
				t.Errorf("%s %s: the control plane's tree differs from the agent's golden\n got: %s\nwant: %s", g.layout, c.Name, got, want)
			}
			built, golden = append(built, string(got)), append(golden, string(want))
		}
		if v := sha256Hex([]byte(strings.Join(golden, "\n"))); v != elementorGoldenPHPVectors[g.layout] {
			t.Errorf("%s: the golden trees re-encoded here hash to %s, PHP's json_encode gives %s", g.layout, v, elementorGoldenPHPVectors[g.layout])
		}
		if v := sha256Hex([]byte(strings.Join(built, "\n"))); v != elementorGoldenPHPVectors[g.layout] {
			t.Errorf("%s: the control plane's trees hash to %s, PHP's json_encode of the goldens gives %s", g.layout, v, elementorGoldenPHPVectors[g.layout])
		}
	}
}

// builderAnswer is a precheck answer for a builder page as the agent forms
// it: the preview's members in the agent's order, the tree as given, and
// both digests computed over that tree's PHP bytes (a self-consistent
// answer, whatever its tree).
type builderAnswer struct {
	postType, editor, format, version, layout, title string
	tree                                             json.RawMessage
	media                                            []pageMediaFact
	extra                                            string
	digestTree                                       json.RawMessage
}

func (a builderAnswer) response(t *testing.T, entrySum string, input []byte) agentcmd.AbilityRunResponse {
	t.Helper()
	str := func(s string) string {
		b, _ := json.Marshal(s)
		return string(b)
	}
	var pv strings.Builder
	pv.WriteString(`{"post_type":` + str(a.postType) + `,"editor":` + str(a.editor) + `,"format":` + str(a.format) +
		`,"elementor_version":` + str(a.version) + `,"layout":` + str(a.layout) + `,"status":"draft","title":` + str(a.title) +
		`,"tree":` + string(a.tree))
	if len(a.media) > 0 {
		list := make([]map[string]any, 0, len(a.media))
		for _, m := range a.media {
			list = append(list, map[string]any{"id": m.ID, "url": m.URL, "filename": m.Filename, "mime": m.Mime,
				"width": m.Width, "height": m.Height, "modified_gmt": m.ModifiedGMT})
		}
		b, _ := json.Marshal(list)
		pv.WriteString(`,"media":` + string(b))
	}
	pv.WriteString(a.extra + `}`)
	digestTree := a.tree
	if a.digestTree != nil {
		digestTree = a.digestTree
	}
	treeJSON, ok := phpCanonicalJSON(digestTree, elementorTreeMaxDepth)
	if !ok {
		treeJSON = digestTree
	}
	prev, _ := phpJSONStringArray(a.editor, a.format, a.version, a.postType, "draft", a.title, string(treeJSON))
	base, _ := pageCreateBaseFingerprint(a.postType, a.media)
	pre, _ := phpJSONStringArray(entrySum, sha256Hex(input), base, sha256Hex(prev))
	return agentcmd.AbilityRunResponse{
		OK: true, Mode: agentcmd.AbilityRunModePrecheck, Valid: true, BaseFingerprint: base,
		PreviewDigest: sha256Hex(prev), PrecheckDigest: sha256Hex(pre), Preview: json.RawMessage(pv.String()),
	}
}

func goldenCase(t *testing.T, g elementorGolden, name string) elementorGoldenCase {
	t.Helper()
	for _, c := range g.Cases {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("golden case %q missing", name)
	return elementorGoldenCase{}
}

// treeFor is the control plane's own tree bytes for an input.
func treeFor(t *testing.T, input []byte, requestID string, containers bool, media []pageMediaFact) json.RawMessage {
	t.Helper()
	tree, ok := elementorClassicTree(input, requestID, containers, media)
	if !ok {
		t.Fatal("tree not built")
	}
	b, ok := phpEncodeBytes(tree)
	if !ok {
		t.Fatal("tree not encoded")
	}
	return b
}

// TestElementorPrecheckProjection (R2): the agent's own answer for every
// golden case is accepted, with the builder facts the card shows; an answer
// whose tree is not the one the control plane builds for this input and
// this request id is refused even when its digests are its own, and so is an
// answer whose digest is over another tree, another format, version or
// layout, or another page.
func TestElementorPrecheckProjection(t *testing.T) {
	entrySum := sha256Hex([]byte("entry"))
	goldens := readElementorGoldens(t)
	// mod is one more top-level member of the input (no trailing comma), put
	// after the editor member.
	setup := func(g elementorGolden, c elementorGoldenCase, mod string) ([]byte, pageCreateFacts, builderAnswer) {
		input := []byte(elementorInput(string(c.Input)))
		if mod != "" {
			editor := `"editor":"builder:elementor"`
			withMod := strings.Replace(string(input), editor, editor+","+mod, 1)
			if withMod == string(input) || !json.Valid([]byte(withMod)) {
				t.Fatalf("%s: the member %s did not make a valid input: %.200s", c.Name, mod, withMod)
			}
			input = []byte(withMod)
		}
		f, code := validatePageCreateInput(input)
		if code != "" {
			t.Fatalf("%s: grammar refused (%s)", c.Name, code)
		}
		return input, f, builderAnswer{postType: "page", editor: pageEditorBuilderElementor, format: elementorFormatClassic,
			version: elementorTestVersion, layout: g.layout, title: "T", tree: c.Tree, media: goldenMedia(t, g, f.mediaIDs)}
	}

	// The agent's own answers.
	for _, g := range goldens {
		for _, c := range g.Cases {
			input, f, a := setup(g, c, "")
			checked, ok := verifyBuilderPageCreatePrecheck(a.response(t, entrySum, input), entrySum, input, f, g.RequestID)
			if !ok {
				t.Fatalf("%s %s: the agent's own answer was refused", g.layout, c.Name)
			}
			want := PageCardBuilder{Builder: "elementor", Format: "classic", Version: elementorTestVersion, Layout: g.layout}
			if checked.builder == nil || *checked.builder != want || len(checked.media) != len(f.mediaIDs) {
				t.Fatalf("%s %s: checked %+v, media %d", g.layout, c.Name, checked.builder, len(checked.media))
			}
		}
	}

	boxes, rows := goldens[0], goldens[1]
	mixed := goldenCase(t, boxes, "mixed-top-level")
	plans := goldenCase(t, boxes, "group-with-columns-and-widths")
	image := goldenCase(t, boxes, "image")
	otherRequest := "99999999-2222-4333-8444-777777777777"

	type refusal struct {
		name   string
		golden elementorGolden
		c      elementorGoldenCase
		mod    string
		// wantFormat is the elementor_format the grammar must read from the
		// input once mod is added: the case means what its name says.
		wantFormat string
		edit       func(a *builderAnswer, input []byte, f pageCreateFacts)
		swapTree   func(tree string) string
	}
	cases := []refusal{
		{name: "a text the outline does not hold", golden: boxes, c: plans,
			swapTree: func(s string) string { return strings.Replace(s, "Prices include VAT.", "Prices exclude VAT.", 1) }},
		{name: "a setting the mapper never writes", golden: boxes, c: plans,
			swapTree: func(s string) string {
				return strings.Replace(s, `"header_size": "h2"`, `"header_size": "h2", "_css_classes": "x"`, 1)
			}},
		{name: "members in another order", golden: boxes, c: plans,
			swapTree: func(s string) string {
				return strings.Replace(s, `"title": "Plans", "header_size": "h2"`, `"header_size": "h2", "title": "Plans"`, 1)
			}},
		{name: "a number where the mapper stores a string", golden: boxes, c: plans,
			swapTree: func(s string) string { return strings.Replace(s, `"size": "40"`, `"size": 40`, 1) }},
		{name: "another request's node ids", golden: boxes, c: mixed,
			edit: func(a *builderAnswer, input []byte, f pageCreateFacts) {
				a.tree = treeFor(t, input, otherRequest, true, a.media)
			}},
		{name: "a node dropped", golden: boxes, c: plans,
			swapTree: func(s string) string {
				return strings.Replace(s, `, {"id": "edd9c90", "elType": "widget", "settings": {"editor": "<p>Prices include VAT.</p>"}, "elements": [], "widgetType": "text-editor"}`, ``, 1)
			}},
		{name: "the sections tree under layout containers", golden: rows, c: goldenCase(t, rows, "mixed-top-level"),
			edit: func(a *builderAnswer, _ []byte, _ pageCreateFacts) { a.layout = elementorLayoutBoxes }},
		{name: "a layout the mapper does not build", golden: boxes, c: mixed,
			edit: func(a *builderAnswer, _ []byte, _ pageCreateFacts) { a.layout = "grid" }},
		{name: "the atomic format", golden: boxes, c: mixed,
			edit: func(a *builderAnswer, _ []byte, _ pageCreateFacts) { a.format = "atomic" }},
		{name: "a classic tree for an atomic input", golden: boxes, c: mixed, mod: `"elementor_format":"atomic"`, wantFormat: "atomic"},
		{name: "a version that is not one", golden: boxes, c: mixed,
			edit: func(a *builderAnswer, _ []byte, _ pageCreateFacts) { a.version = "3.35.9<b>" }},
		{name: "another title", golden: boxes, c: mixed,
			edit: func(a *builderAnswer, _ []byte, _ pageCreateFacts) { a.title = "Other" }},
		{name: "a member the preview does not have", golden: boxes, c: mixed,
			edit: func(a *builderAnswer, _ []byte, _ pageCreateFacts) { a.extra = `,"settings":{}` }},
		{name: "an image without its facts", golden: boxes, c: image,
			edit: func(a *builderAnswer, _ []byte, _ pageCreateFacts) { a.media = nil }},
		{name: "a digest over another tree", golden: boxes, c: plans,
			edit: func(a *builderAnswer, _ []byte, _ pageCreateFacts) {
				a.digestTree = json.RawMessage(strings.Replace(string(a.tree), "Prices include VAT.", "Prices exclude VAT.", 1))
			}},
	}
	for _, rc := range cases {
		input, f, a := setup(rc.golden, rc.c, rc.mod)
		if rc.wantFormat != "" && f.elementorFormat != rc.wantFormat {
			t.Fatalf("%s: the grammar read elementor_format %q, want %q", rc.name, f.elementorFormat, rc.wantFormat)
		}
		if rc.swapTree != nil {
			canonicalSpaced := spacedJSON(t, rc.c.Tree)
			swapped := rc.swapTree(canonicalSpaced)
			if swapped == canonicalSpaced {
				t.Fatalf("%s: the edit changed nothing in %s", rc.name, canonicalSpaced)
			}
			a.tree = json.RawMessage(swapped)
		}
		if rc.edit != nil {
			rc.edit(&a, input, f)
		}
		if _, ok := verifyBuilderPageCreatePrecheck(a.response(t, entrySum, input), entrySum, input, f, rc.golden.RequestID); ok {
			t.Errorf("%s: accepted", rc.name)
		}
	}

	// The same answer checked against another request id is refused: the
	// ids are the request's.
	input, f, a := setup(boxes, mixed, "")
	if _, ok := verifyBuilderPageCreatePrecheck(a.response(t, entrySum, input), entrySum, input, f, otherRequest); ok {
		t.Error("an answer for another request id was accepted")
	}
	// The block-editor check never accepts a builder answer.
	if _, ok := verifyPageCreatePrecheck(a.response(t, entrySum, input), entrySum, input, f); ok {
		t.Error("the block-editor check accepted a builder page")
	}
}

// spacedJSON is a tree's JSON as Python's json.dumps writes it (", " and
// ": "), so the refusal edits above can name a node as the fixture shows it.
func spacedJSON(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	canonical := mustCanonical(t, raw)
	v, ok := phpCanonicalValue(canonical)
	if !ok {
		t.Fatal("tree did not decode")
	}
	var b strings.Builder
	var write func(any)
	write = func(v any) {
		switch x := v.(type) {
		case string:
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetEscapeHTML(false)
			_ = enc.Encode(x)
			b.Write(bytes.TrimRight(buf.Bytes(), "\n"))
		case bool:
			b.WriteString(strconv.FormatBool(x))
		case phpList:
			b.WriteByte('[')
			for i, item := range x {
				if i > 0 {
					b.WriteString(", ")
				}
				write(item)
			}
			b.WriteByte(']')
		case phpObject:
			if len(x) == 0 {
				b.WriteString("[]")
				return
			}
			b.WriteByte('{')
			for i, p := range x {
				if i > 0 {
					b.WriteString(", ")
				}
				write(p.key)
				b.WriteString(": ")
				write(p.value)
			}
			b.WriteByte('}')
		}
	}
	write(v)
	return b.String()
}

func phpCanonicalValue(raw []byte) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return phpDecodeValue(dec, elementorTreeMaxDepth)
}

// TestElementorPreviewDigestIsPHPs: the preview digest the control plane
// recomputes is PHP's for the same page (title with non-ASCII and a slash).
func TestElementorPreviewDigestIsPHPs(t *testing.T) {
	boxes := readElementorGoldens(t)[0]
	c := goldenCase(t, boxes, "mixed-top-level")
	input := []byte(strings.Replace(elementorInput(string(c.Input)), `"title":"T"`, `"title":"Our van été / plan"`, 1))
	f, code := validatePageCreateInput(input)
	if code != "" {
		t.Fatalf("grammar refused (%s)", code)
	}
	tree := treeFor(t, input, boxes.RequestID, true, goldenMedia(t, boxes, f.mediaIDs))
	prev, ok := phpJSONStringArray(pageEditorBuilderElementor, elementorFormatClassic, elementorTestVersion, "page", "draft", elementorDigestTitle, string(tree))
	if !ok || sha256Hex(prev) != elementorPHPPreviewDigest {
		t.Fatalf("preview digest %s, PHP gives %s", sha256Hex(prev), elementorPHPPreviewDigest)
	}
	a := builderAnswer{postType: "page", editor: pageEditorBuilderElementor, format: elementorFormatClassic,
		version: elementorTestVersion, layout: elementorLayoutBoxes, title: elementorDigestTitle, tree: c.Tree,
		media: goldenMedia(t, boxes, f.mediaIDs)}
	resp := a.response(t, sha256Hex([]byte("entry")), input)
	if resp.PreviewDigest != elementorPHPPreviewDigest {
		t.Fatalf("the golden's answer digest %s, PHP gives %s", resp.PreviewDigest, elementorPHPPreviewDigest)
	}
	if _, ok := verifyBuilderPageCreatePrecheck(resp, sha256Hex([]byte("entry")), input, f, boxes.RequestID); !ok {
		t.Fatal("the answer PHP's digest describes was refused")
	}
}

// TestPageCardFacts_Builder: a block-editor page with images stores exactly
// the bytes it did before builders; a builder page stores its builder, and
// both read back; a damaged builder member makes the card unreadable.
func TestPageCardFacts_Builder(t *testing.T) {
	media := []pageMediaFact{{ID: 5, URL: "https://example.com/a.png", Filename: "a.png", Mime: "image/png", Width: 10, Height: 20, ModifiedGMT: "2026-10-01 10:00:00"}}
	el := &PageCardBuilder{Builder: "elementor", Format: "classic", Version: "3.35.9", Layout: "containers"}
	if b, err := pageCardFactsJSON(nil, nil); err != nil || b != nil {
		t.Fatalf("no images, no builder: %s %v", b, err)
	}
	b, err := pageCardFactsJSON(media, nil)
	if err != nil || string(b) != `{"kind":"page_create","media":[{"id":5,"filename":"a.png","mime":"image/png","width":10,"height":20}]}` {
		t.Fatalf("block-editor card: %s %v", b, err)
	}
	if _, ok := ReadPageCardBuilder(b); ok {
		t.Fatal("a block-editor card named a builder")
	}
	b, err = pageCardFactsJSON(nil, el)
	if err != nil || string(b) != `{"kind":"page_create","builder":{"builder":"elementor","format":"classic","version":"3.35.9","layout":"containers"}}` {
		t.Fatalf("builder card: %s %v", b, err)
	}
	if got, ok := ReadPageCardBuilder(b); !ok || got != *el {
		t.Fatalf("builder read back %+v %v", got, ok)
	}
	if _, ok := ReadPageCardFacts(b); ok {
		t.Fatal("a card without images gave images")
	}
	b, _ = pageCardFactsJSON(media, el)
	// As jsonb gives it back: members reordered, spaces added.
	stored := `{"kind": "page_create", "builder": {"format": "classic", "layout": "containers", "builder": "elementor", "version": "3.35.9"}, "media": [{"id": 5, "mime": "image/png", "width": 10, "height": 20, "filename": "a.png"}]}`
	for _, s := range []string{string(b), stored} {
		if got, ok := ReadPageCardBuilder([]byte(s)); !ok || got != *el {
			t.Errorf("%s: builder %+v %v", s, got, ok)
		}
		if got, ok := ReadPageCardFacts([]byte(s)); !ok || len(got) != 1 || got[0].ID != 5 {
			t.Errorf("%s: media %+v %v", s, got, ok)
		}
	}
	for _, bad := range []string{
		`{"kind":"page_create","builder":{"builder":"elementor","format":"classic","version":"3.35.9","layout":"grid"}}`,
		`{"kind":"page_create","builder":{"builder":"beaver","format":"classic","version":"3.35.9","layout":"containers"}}`,
		`{"kind":"page_create","builder":{"builder":"elementor","format":"atomic","version":"3.35.9","layout":"containers"}}`,
		`{"kind":"page_create","builder":{"builder":"elementor","format":"classic","version":"<b>3</b>","layout":"containers"}}`,
		`{"kind":"page_create","builder":{"builder":"elementor","format":"classic","version":"3.35.9","layout":"containers","x":1}}`,
		`{"kind":"page_create","builder":"elementor","media":[{"id":5,"filename":"a.png","mime":"image/png","width":10,"height":20}]}`,
		`{"kind":"page_create","other":1,"media":[{"id":5,"filename":"a.png","mime":"image/png","width":10,"height":20}]}`,
	} {
		if _, ok := ReadPageCardBuilder([]byte(bad)); ok {
			t.Errorf("builder read from %s", bad)
		}
		if _, ok := ReadPageCardFacts([]byte(bad)); ok {
			t.Errorf("media read from %s", bad)
		}
	}
}

// treeAgent answers a builder precheck as the agent does, with the tree
// built for idFor(the request id it was sent).
type treeAgent struct {
	t      *testing.T
	golden elementorGolden
	idFor  func(sent string) string
	calls  []agentcmd.AbilityRunCall
}

func (a *treeAgent) AbilityRun(_ context.Context, _ uuid.UUID, _ string, call agentcmd.AbilityRunCall) (agentcmd.AbilityRunResponse, error) {
	a.calls = append(a.calls, call)
	f, code := validatePageCreateInput(call.Input)
	if code != "" {
		a.t.Fatalf("fake agent: input refused (%s)", code)
	}
	media := goldenMedia(a.t, a.golden, f.mediaIDs)
	ans := builderAnswer{postType: f.postType, editor: f.editor, format: elementorFormatClassic, version: elementorTestVersion,
		layout: elementorLayoutBoxes, title: f.title, media: media,
		tree: treeFor(a.t, call.Input, a.idFor(call.RequestID.String()), true, media)}
	return ans.response(a.t, call.EntrySHA256, call.Input), nil
}

// reachedRequestStore records that the write branch reached storage, and
// stops there.
type reachedRequestStore struct{ reached int }

var errStoreStop = errors.New("stopped at the request store")

func (s *reachedRequestStore) RunAbilityRequestTx(context.Context, domain.Principal, func(pgx.Tx, abilityRequestQueries) error) error {
	s.reached++
	return errStoreStop
}

func (s *reachedRequestStore) ReadAbilityRequestStatus(context.Context, domain.Principal, uuid.UUID, uuid.UUID) (AbilityStatusRow, bool, error) {
	return AbilityStatusRow{}, false, nil
}

func (s *reachedRequestStore) ListOpenAbilityRequestStatus(context.Context, domain.Principal, uuid.UUID, int32) ([]AbilityStatusRow, error) {
	return nil, nil
}

// TestPageCreateRun_ElementorTreeIsCheckedAgainstThePrecheckRequest: through
// the real transport, a precheck whose tree carries the ids of the request
// id the control plane sent reaches the request store; one whose tree
// carries another request's ids is refused precheck_unverifiable and
// nothing is stored.
func TestPageCreateRun_ElementorTreeIsCheckedAgainstThePrecheckRequest(t *testing.T) {
	boxes := readElementorGoldens(t)[0]
	in := elementorInput(string(goldenCase(t, boxes, "mixed-top-level").Input))

	store := &reachedRequestStore{}
	agent := &treeAgent{t: t, golden: boxes, idFor: func(sent string) string { return sent }}
	r, site := pageCreateRunRouterStore(t, agentcmd.MinAgentVersionForBuilderAdapters, agent, elementorEnabled, store)
	var input map[string]any
	if err := json.Unmarshal([]byte(in), &input); err != nil {
		t.Fatal(err)
	}
	post(t, r, callBody(ToolSiteAbilityRun, map[string]any{"site_id": site, "name": AbilityPageCreate, "input": input}), nil)
	if len(agent.calls) != 1 || store.reached != 1 {
		t.Fatalf("the site's tree for the sent request: %d precheck calls, store reached %d times; want 1 and 1", len(agent.calls), store.reached)
	}

	store = &reachedRequestStore{}
	agent = &treeAgent{t: t, golden: boxes, idFor: func(string) string { return boxes.RequestID }}
	r, site = pageCreateRunRouterStore(t, agentcmd.MinAgentVersionForBuilderAdapters, agent, elementorEnabled, store)
	ref := runPageCreate(t, r, site, in)
	if len(agent.calls) != 1 || store.reached != 0 {
		t.Fatalf("a tree for another request: %d precheck calls, store reached %d times; want 1 and 0", len(agent.calls), store.reached)
	}
	if !strings.Contains(ref.raw, msgAbilitySiteAnswer) {
		t.Fatalf("a tree for another request: %s", ref.raw)
	}
}
