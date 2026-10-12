package mcp

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

// designedEntrySum is the entry hash the designed answers below are formed for.
var designedEntrySum = sha256Hex([]byte("entry"))

// designedAnswer is the precheck answer the agent gives a golden page for the
// input bytes it is sent: the preview with the golden's tree, release, layout
// and images; the digests the golden holds (the preview digest as the agent
// answered it, and the precheck digest the agent forms over the bytes it was
// sent); and the answer's top-level builder_facts member as the golden has it.
//
// edit changes one member first: the preview, as a builderAnswer, and the text
// of the builder_facts member ("" leaves the member out). It reports whether
// the digests are formed again over the edited preview, as an agent that built
// that preview would; false keeps the golden's digests, which describe the
// golden's own tree.
func designedAnswer(t *testing.T, c designedGoldenCase, entrySum string, input []byte, edit func(a *builderAnswer, facts *string) bool) agentcmd.AbilityRunResponse {
	t.Helper()
	var page struct {
		PostType string `json:"post_type"`
		Editor   string `json:"editor"`
		Title    string `json:"title"`
	}
	if err := json.Unmarshal(input, &page); err != nil {
		t.Fatal(err)
	}
	a := builderAnswer{postType: page.PostType, editor: page.Editor, format: elementorFormatClassic,
		version: c.Facts.ElementorVersion, layout: c.Facts.Layout, title: page.Title, tree: c.Tree, media: c.Facts.media(t)}
	facts := string(c.BuilderFacts)
	formed := false
	if edit != nil {
		formed = edit(&a, &facts)
	}
	resp := a.response(t, entrySum, input)
	if !formed {
		resp.PreviewDigest = c.PreviewDigest
		resp.PrecheckDigest = agentPrecheckDigest(t, entrySum, input, resp.BaseFingerprint, c.PreviewDigest)
	}
	raw := `{"ok":true,"mode":"precheck","valid":true,"preview":` + string(resp.Preview)
	if facts != "" {
		raw += `,"builder_facts":` + facts
	}
	resp.Raw = json.RawMessage(raw + `}`)
	return resp
}

// designedFactsWith is the golden's builder_facts text with one member
// replaced by the JSON text given, or dropped when it is "".
func designedFactsWith(t *testing.T, c designedGoldenCase, member, value string) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(c.BuilderFacts, &m); err != nil {
		t.Fatal(err)
	}
	if value == "" {
		delete(m, member)
	} else {
		m[member] = json.RawMessage(value)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// designedGoldenFrame is the page frame and the theme fact the golden's
// precheck resolved.
func designedGoldenFrame(t *testing.T, c designedGoldenCase) (frame string, blockTheme bool) {
	t.Helper()
	var f struct {
		BlockTheme bool   `json:"block_theme"`
		PageFrame  string `json:"page_frame"`
	}
	if err := json.Unmarshal(c.BuilderFacts, &f); err != nil || f.PageFrame == "" {
		t.Fatalf("%s: builder_facts %s: %v", c.Name, c.BuilderFacts, err)
	}
	return f.PageFrame, f.BlockTheme
}

func designedGoldenByName(t *testing.T) func(name string) designedGoldenCase {
	t.Helper()
	byName := map[string]designedGoldenCase{}
	for _, c := range readDesignedGolden(t) {
		byName[c.Name] = c
	}
	return func(name string) designedGoldenCase {
		t.Helper()
		c, ok := byName[name]
		if !ok {
			t.Fatalf("the golden has no case %q", name)
		}
		return c
	}
}

// designedChecked is the grammar's reading of a golden input.
func designedChecked(t *testing.T, input []byte) pageCreateFacts {
	t.Helper()
	f, refusal := checkPageCreateInput(input)
	if refusal.Code != "" || f.outlineVersion != pageOutlineVersionDesigned {
		t.Fatalf("the grammar does not accept the golden input (%+v): %.200s", refusal, input)
	}
	return f
}

// TestDesignedPrecheckIsTheAgentGolden: the agent's own answer for every
// golden page is accepted, with the builder facts the card shows, and the
// digest the card binds is the one the agent answered.
func TestDesignedPrecheckIsTheAgentGolden(t *testing.T) {
	ran := 0
	for _, c := range readDesignedGolden(t) {
		ran++
		t.Run(c.Name, func(t *testing.T) {
			input := []byte(c.Input)
			f := designedChecked(t, input)
			resp := designedAnswer(t, c, designedEntrySum, input, nil)
			checked, ok := verifyBuilderPageCreatePrecheck(resp, designedEntrySum, input, f, c.Facts.RequestID)
			if !ok {
				t.Fatal("the agent's own answer was refused")
			}
			want := PageCardBuilder{Builder: pageBuilderElementor, Format: elementorFormatClassic, Version: c.Facts.ElementorVersion, Layout: elementorLayoutBoxes}
			if checked.builder == nil || *checked.builder != want {
				t.Errorf("the card shows %+v, want %+v", checked.builder, want)
			}
			if checked.previewDigest != c.PreviewDigest || checked.precheckDigest != resp.PrecheckDigest || len(checked.media) != len(f.mediaIDs) {
				t.Errorf("preview digest %q (want %q), precheck digest %q (want %q), %d images (want %d)",
					checked.previewDigest, c.PreviewDigest, checked.precheckDigest, resp.PrecheckDigest, len(checked.media), len(f.mediaIDs))
			}
		})
	}
	if ran == 0 {
		t.Fatal("no golden case ran")
	}
}

// TestDesignedPrecheckRefusesATreeThatIsNotTheControlPlanes: an answer is
// accepted only when its tree is the one the control plane builds from the
// input, the request id, the images and the breakpoints the answer says the
// tree was compiled for. Each refusal below is made of an answer the agent
// could not have given, and each is placed next to the honest answer for the
// same page, which is accepted.
func TestDesignedPrecheckRefusesATreeThatIsNotTheControlPlanes(t *testing.T) {
	named := designedGoldenByName(t)
	check := func(c designedGoldenCase, requestID string, resp agentcmd.AbilityRunResponse) bool {
		input := []byte(c.Input)
		_, ok := verifyBuilderPageCreatePrecheck(resp, designedEntrySum, input, designedChecked(t, input), requestID)
		return ok
	}
	// changeText is the canonical tree of a case with the first title or text
	// changed.
	changeText := func(c designedGoldenCase) json.RawMessage {
		canon := string(mustCanonical(t, c.Tree))
		for _, member := range []string{`"title":"`, `"editor":"`} {
			if changed := strings.Replace(canon, member, member+"Changed ", 1); changed != canon {
				return json.RawMessage(changed)
			}
		}
		t.Fatalf("%s: the tree holds no title or text to change", c.Name)
		return nil
	}

	type refusal struct {
		name, golden string
		edit         func(c designedGoldenCase, a *builderAnswer, facts *string) bool
	}
	breakpoints := func(raw string) func(c designedGoldenCase, a *builderAnswer, facts *string) bool {
		return func(c designedGoldenCase, _ *builderAnswer, facts *string) bool {
			*facts = designedFactsWith(t, c, "active_breakpoints", raw)
			return false
		}
	}
	rows := []refusal{
		{name: "a tree compiled for mobile and tablet, under facts that name no breakpoint", golden: "responsive-probe-mobile-and-tablet",
			edit: breakpoints(`[]`)},
		{name: "a tree compiled for no breakpoint, under facts that name mobile and tablet", golden: "responsive-probe-no-breakpoints",
			edit: breakpoints(`["mobile","tablet"]`)},
		{name: "a tree compiled for mobile, under facts that name tablet", golden: "responsive-probe-mobile",
			edit: breakpoints(`["tablet"]`)},
		{name: "a tree compiled for mobile and tablet, under facts that cannot say", golden: "responsive-probe-mobile-and-tablet",
			edit: breakpoints(`null`)},
		{name: "builder_facts without active_breakpoints", golden: "responsive-probe-no-breakpoints",
			edit: breakpoints(``)},
		{name: "no builder_facts", golden: "responsive-probe-no-breakpoints",
			edit: func(_ designedGoldenCase, _ *builderAnswer, facts *string) bool { *facts = ""; return false }},
		{name: "active_breakpoints as text", golden: "responsive-probe-mobile-and-tablet", edit: breakpoints(`"mobile"`)},
		{name: "active_breakpoints as an object", golden: "responsive-probe-mobile-and-tablet", edit: breakpoints(`{"mobile":true}`)},
		{name: "active_breakpoints holding a number", golden: "responsive-probe-mobile-and-tablet", edit: breakpoints(`["mobile",1]`)},
		{name: "active_breakpoints holding null", golden: "responsive-probe-mobile-and-tablet", edit: breakpoints(`["mobile",null]`)},
		{name: "another request's node ids, digests formed over that tree", golden: "hero-centered",
			edit: func(c designedGoldenCase, a *builderAnswer, _ *string) bool {
				a.tree = named("hero-centered-another-request").Tree
				return true
			}},
		{name: "the layout sections", golden: "hero-centered",
			edit: func(_ designedGoldenCase, a *builderAnswer, _ *string) bool {
				a.layout = elementorLayoutRows
				return false
			}},
		{name: "a text changed in the tree, digests formed over it", golden: "cta-band",
			edit: func(c designedGoldenCase, a *builderAnswer, _ *string) bool { a.tree = changeText(c); return true }},
		{name: "a text changed in the tree, the golden's digests", golden: "cta-band",
			edit: func(c designedGoldenCase, a *builderAnswer, _ *string) bool { a.tree = changeText(c); return false }},
		{name: "the golden's digests for another release", golden: "faq-at-elementor-3-35-9",
			edit: func(_ designedGoldenCase, a *builderAnswer, _ *string) bool { a.version = "3.35.8"; return false }},
		{name: "the golden's digests for another title", golden: "section-defaults",
			edit: func(_ designedGoldenCase, a *builderAnswer, _ *string) bool { a.title = "Another title"; return false }},
		{name: "an empty tree, digests formed over it", golden: "section-defaults",
			edit: func(_ designedGoldenCase, a *builderAnswer, _ *string) bool {
				a.tree = json.RawMessage(`[]`)
				return true
			}},
	}
	honest := map[string]bool{}
	for _, r := range rows {
		c := named(r.golden)
		if !honest[r.golden] {
			honest[r.golden] = true
			if !check(c, c.Facts.RequestID, designedAnswer(t, c, designedEntrySum, []byte(c.Input), nil)) {
				t.Fatalf("%s: the honest answer was refused", r.golden)
			}
		}
		resp := designedAnswer(t, c, designedEntrySum, []byte(c.Input), func(a *builderAnswer, facts *string) bool { return r.edit(c, a, facts) })
		if check(c, c.Facts.RequestID, resp) {
			t.Errorf("%s: accepted", r.name)
		}
	}

	// Answers that change nothing the tree is a function of are accepted: the
	// compile reads only the names mobile and tablet, once each.
	for _, a := range []struct{ name, golden, breakpoints string }{
		{"extra and repeated names", "responsive-probe-mobile-and-tablet", `["mobile","tablet","mobile","laptop","widescreen"]`},
		{"none, as null", "responsive-probe-no-breakpoints", `null`},
		{"none, as an empty list", "responsive-probe-no-breakpoints-reported", `[]`},
	} {
		c := named(a.golden)
		resp := designedAnswer(t, c, designedEntrySum, []byte(c.Input), func(_ *builderAnswer, facts *string) bool {
			*facts = designedFactsWith(t, c, "active_breakpoints", a.breakpoints)
			return false
		})
		if !check(c, c.Facts.RequestID, resp) {
			t.Errorf("%s: refused", a.name)
		}
	}
}

// requestIDSource yields the bytes of one uuid on every read, so uuid.New
// returns that uuid for as long as the source is installed.
type requestIDSource struct{ id uuid.UUID }

func (s requestIDSource) Read(p []byte) (int, error) { return copy(p, s.id[:]), nil }

// designedAgent answers a designed page's precheck as the agent does for a
// golden page: the golden's tree, digests and builder_facts, for the request id
// the golden was formed for.
type designedAgent struct {
	t       *testing.T
	c       designedGoldenCase
	edit    func(a *builderAnswer, facts *string) bool
	calls   []agentcmd.AbilityRunCall
	answers []agentcmd.AbilityRunResponse
}

func (a *designedAgent) AbilityRun(_ context.Context, _ uuid.UUID, _ string, call agentcmd.AbilityRunCall) (agentcmd.AbilityRunResponse, error) {
	a.calls = append(a.calls, call)
	if call.RequestID.String() != a.c.Facts.RequestID {
		a.t.Fatalf("the precheck was sent as request %s; the golden was formed for %s", call.RequestID, a.c.Facts.RequestID)
	}
	resp := designedAnswer(a.t, a.c, call.EntrySHA256, call.Input, a.edit)
	a.answers = append(a.answers, resp)
	return resp, nil
}

// runDesignedPageCreate sends input for the golden page c through the real
// transport, with the request id the golden was formed for, to a site whose
// agent answers as the golden does (or as edit changes it).
func runDesignedPageCreate(t *testing.T, c designedGoldenCase, input string, edit func(a *builderAnswer, facts *string) bool) (*storingRequestStore, *designedAgent, string) {
	t.Helper()
	store := &storingRequestStore{}
	agent := &designedAgent{t: t, c: c, edit: edit}
	r, site := pageCreateRunRouterStore(t, agentcmd.MinAgentVersionForDesignedPages, agent, elementorEnabled, store)
	id, err := uuid.Parse(c.Facts.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	uuid.SetRand(requestIDSource{id})
	defer uuid.SetRand(nil)
	if got := uuid.New(); got != id {
		t.Fatalf("the request id source gave %s, not %s", got, id)
	}
	// The input is sent as written: its bytes are what the frame is appended to.
	body := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"` + ToolSiteAbilityRun +
		`","arguments":{"site_id":"` + site.String() + `","name":"` + AbilityPageCreate + `","input":` + input + `}}}`
	w := post(t, r, body, nil)
	return store, agent, w.Body.String()
}

var designedFrameMember = regexp.MustCompile(`,"page_frame":"[a-z_]+"\}$`)

// TestPageCreateRun_DesignedPageIsStoredInTheFrameItsPrecheckResolved: every
// golden page, sent through the real transport, is stored. The stored input
// ends with the page_frame member, and is the golden's own bytes: the AI's,
// unchanged before the member. The card's frame and builder are the
// precheck's, and the stored precheck digest is the one the agent forms over
// the bytes that are stored.
func TestPageCreateRun_DesignedPageIsStoredInTheFrameItsPrecheckResolved(t *testing.T) {
	appended := map[string]bool{}
	sent := 0
	for _, c := range readDesignedGolden(t) {
		frame, blockTheme := designedGoldenFrame(t, c)
		if !designedFrameMember.MatchString(c.Input) || !strings.HasSuffix(c.Input, `,"page_frame":`+strconv.Quote(frame)+`}`) {
			t.Fatalf("%s: the golden input does not end with the frame %q the precheck resolved", c.Name, frame)
		}
		aiSent := c.Input
		aiLeft := designedFrameMember.ReplaceAllString(c.Input, `}`)
		leftToPrecheck := false
		if f, refusal := checkPageCreateInput([]byte(aiLeft)); refusal.Code == "" && !f.pageFrameSent {
			if rule, ok := resolvedPageFrame(f, blockTheme); ok && rule == frame {
				leftToPrecheck = true
			}
		}
		for _, v := range []struct {
			name, input string
			bound       bool
		}{{"the AI named the frame", aiSent, false}, {"the precheck named the frame", aiLeft, true}} {
			if v.bound && !leftToPrecheck {
				continue
			}
			t.Run(c.Name+"/"+v.name, func(t *testing.T) {
				store, agent, out := runDesignedPageCreate(t, c, v.input, nil)
				if len(agent.calls) != 1 || len(store.inserts) != 1 {
					t.Fatalf("%d precheck calls and %d inserts, want 1 and 1: %.400s", len(agent.calls), len(store.inserts), out)
				}
				if got := string(agent.calls[0].Input); got != v.input {
					t.Fatalf("the site was sent other bytes than the AI sent:\n%.300s", got)
				}
				ins, answer := store.inserts[0], agent.answers[0]
				if ins.InputJson != c.Input {
					t.Fatalf("the stored input is not the golden's bytes:\n got: %.300s\nwant: %.300s", ins.InputJson, c.Input)
				}
				if ins.InputSha256 != sha256Hex([]byte(ins.InputJson)) {
					t.Error("the stored input hash is not the hash of the stored bytes")
				}
				if ins.PreviewDigest == nil || *ins.PreviewDigest != c.PreviewDigest || ins.BaseFingerprint != answer.BaseFingerprint {
					t.Error("the stored preview digest or base fingerprint is not the agent's answer")
				}
				if want := agentPrecheckDigest(t, ins.EntrySha256, []byte(ins.InputJson), answer.BaseFingerprint, c.PreviewDigest); ins.PrecheckDigest != want {
					t.Errorf("stored precheck digest %s; the agent forms %s over the stored bytes", ins.PrecheckDigest, want)
				}
				if v.bound == (ins.PrecheckDigest == answer.PrecheckDigest) {
					t.Errorf("stored precheck digest equals the answered one: %v; the frame was appended: %v", ins.PrecheckDigest == answer.PrecheckDigest, v.bound)
				}
				_, cardFrame, ok := storedCardDesign(ins.CardFacts)
				if !ok || cardFrame != frame {
					t.Errorf("the card shows frame %q (read %v); the precheck resolved %q", cardFrame, ok, frame)
				}
				builder, ok := ReadPageCardBuilder(ins.CardFacts)
				if want := (PageCardBuilder{Builder: pageBuilderElementor, Format: elementorFormatClassic, Version: c.Facts.ElementorVersion, Layout: elementorLayoutBoxes}); !ok || builder != want {
					t.Errorf("the card shows builder %+v (read %v), want %+v", builder, ok, want)
				}
			})
			if v.bound {
				appended[frame] = true
			} else {
				sent++
			}
		}
	}
	for _, frame := range pageFrames {
		if !appended[frame] {
			t.Errorf("no golden page was sent without its frame and stored with %q appended", frame)
		}
	}
	if sent == 0 {
		t.Fatal("no golden page ran")
	}
}

// TestPageCreateRun_DesignedPageNotBuiltAsAnsweredIsNotStored: through the
// real transport, a precheck whose tree was compiled for other breakpoints
// than its builder_facts name, or that names none, is refused
// precheck_unverifiable and nothing is stored; the honest answer for the same
// page is stored.
func TestPageCreateRun_DesignedPageNotBuiltAsAnsweredIsNotStored(t *testing.T) {
	c := designedGoldenByName(t)("responsive-probe-mobile-and-tablet")
	input := c.Input
	store, agent, out := runDesignedPageCreate(t, c, input, nil)
	if len(agent.calls) != 1 || len(store.inserts) != 1 {
		t.Fatalf("the honest answer: %d precheck calls and %d inserts: %.400s", len(agent.calls), len(store.inserts), out)
	}
	for name, raw := range map[string]string{
		"facts that name no breakpoint":    `[]`,
		"facts that cannot say":            `null`,
		"facts without active_breakpoints": ``,
	} {
		store, agent, out := runDesignedPageCreate(t, c, input, func(_ *builderAnswer, facts *string) bool {
			*facts = designedFactsWith(t, c, "active_breakpoints", raw)
			return false
		})
		if len(agent.calls) != 1 || len(store.inserts) != 0 || !strings.Contains(out, msgAbilitySiteAnswer) {
			t.Errorf("%s: %d precheck calls and %d inserts, want 1 and 0: %.400s", name, len(agent.calls), len(store.inserts), out)
		}
	}
}
