package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// designedCreateInput is the page-create input of a designed draft: outline
// version 3 with the spec design.
const designedCreateInput = `{"post_type":"page","editor":"builder:elementor","elementor_format":"classic",` +
	`"outline_version":3,"title":"Home","design":` + specDesignJSON + `,` +
	`"outline":[{"type":"section","anchor":"top","children":[{"type":"paragraph","text":"Hi"}]}]}`

// plainCreateInput is the page-create input of a version 2 draft: no design.
const plainCreateInput = `{"post_type":"page","editor":"builder:elementor","elementor_format":"classic",` +
	`"title":"Home","outline":[{"type":"paragraph","text":"Hi"}]}`

// editOps wraps operations (JSON text) into a page-edit input of post 418,
// with extra members (JSON text, or "") after them.
func editOps(operations string, extra string) []byte {
	in := `{"post_id":418,"base_fingerprint":"` + targetTestFingerprint + `","operations":` + operations
	if extra != "" {
		in += "," + extra
	}
	return []byte(in + "}")
}

const (
	opsInsertParagraph = `[{"op":"insert","after":"3c4d5e6","outline":[{"type":"paragraph","text":"Added"}]}]`
	opsInsertSection   = `[{"op":"insert","after":"3c4d5e6","outline":[{"type":"section","anchor":"press",` +
		`"children":[{"type":"paragraph","text":"Press"}]}]}]`
	opsSetText = `[{"op":"set_text","ref":"3c4d5e6","field":"text","text":"One"}]`
)

// fakeDraftCreateInputs is the store behind a draft's design: the input of
// the page-create request that made the draft.
type fakeDraftCreateInputs struct {
	input []byte
	err   error
	calls []fakeDraftCreateInputsCall
}

type fakeDraftCreateInputsCall struct {
	principal domain.Principal
	siteID    uuid.UUID
	postID    int64
}

func (f *fakeDraftCreateInputs) EligibleDraftCreateInput(_ context.Context, p domain.Principal, siteID uuid.UUID, postID int64) ([]byte, error) {
	f.calls = append(f.calls, fakeDraftCreateInputsCall{p, siteID, postID})
	return f.input, f.err
}

func designEngine(store *fakeDraftCreateInputs) *abilityEngine {
	e := &abilityEngine{}
	if store != nil {
		e.designs = store
	}
	return e
}

func designSite() abilitySite {
	return abilitySite{row: sqlc.Site{ID: uuid.New()}, p: domain.Principal{TenantID: uuid.New()}}
}

// membersOf decodes the top-level members of a JSON object.
func membersOf(t *testing.T, in []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(in, &m); err != nil {
		t.Fatalf("not a JSON object: %v: %s", err, in)
	}
	return m
}

func inheritDesign(t *testing.T, store *fakeDraftCreateInputs, site abilitySite, input []byte) ([]byte, pageEditFacts, *toolRefusal, error) {
	t.Helper()
	facts, code := validatePageEditInput(input)
	if code != "" {
		t.Fatalf("the honest input was refused %q: %s", code, input)
	}
	return (&Service{}).pageEditInheritDesign(context.Background(), designEngine(store), site, input, facts)
}

// TestPageEditInheritsDesign: a page-edit that adds nodes to a draft made with
// a design is sent with that design, read from the page-create request that
// made the draft; a version 2 draft has none to give.
func TestPageEditInheritsDesign(t *testing.T) {
	t.Run("an insert is sent with the design of its draft", func(t *testing.T) {
		store := &fakeDraftCreateInputs{input: []byte(designedCreateInput)}
		site := designSite()
		in := editOps(opsInsertParagraph, "")
		got, facts, refusal, err := inheritDesign(t, store, site, in)
		if err != nil || refusal != nil {
			t.Fatalf("err=%v refusal=%v", err, refusal)
		}
		members := membersOf(t, got)
		var design pageDesign
		if err := json.Unmarshal(members["design"], &design); err != nil || !reflect.DeepEqual(design, specDesign) {
			t.Fatalf("the input carries design %s (%v), want the draft's %+v", members["design"], err, specDesign)
		}
		if facts.design == nil || !reflect.DeepEqual(*facts.design, specDesign) {
			t.Fatalf("the facts hold design %+v", facts.design)
		}
		// Everything the AI sent is still there, byte for byte, in order.
		if !bytes.HasPrefix(got, bytes.TrimSuffix(in, []byte("}"))) {
			t.Fatalf("the AI's members changed:\n got %s\nsent %s", got, in)
		}
		if _, code := validatePageEditInput(got); code != "" {
			t.Fatalf("the input sent to the site is no valid page-edit input: %q", code)
		}
		if len(store.calls) != 1 {
			t.Fatalf("the draft was read %d times, want once", len(store.calls))
		}
		c := store.calls[0]
		if c.principal.TenantID != site.p.TenantID || c.siteID != site.row.ID || c.postID != 418 {
			t.Fatalf("the read was not scoped to this tenant, site and post: %+v", c)
		}
	})

	t.Run("a replace is sent with the design too", func(t *testing.T) {
		store := &fakeDraftCreateInputs{input: []byte(designedCreateInput)}
		got, _, refusal, err := inheritDesign(t, store, designSite(),
			editOps(`[{"op":"replace","ref":"3c4d5e6","outline":[{"type":"paragraph","text":"New"}]}]`, ""))
		if err != nil || refusal != nil || membersOf(t, got)["design"] == nil {
			t.Fatalf("err=%v refusal=%v input=%s", err, refusal, got)
		}
	})

	t.Run("a version 2 draft has no design and its edit is unchanged", func(t *testing.T) {
		store := &fakeDraftCreateInputs{input: []byte(plainCreateInput)}
		in := editOps(opsInsertParagraph, "")
		got, facts, refusal, err := inheritDesign(t, store, designSite(), in)
		if err != nil || refusal != nil || !bytes.Equal(got, in) || facts.design != nil {
			t.Fatalf("err=%v refusal=%v design=%+v input=%s", err, refusal, facts.design, got)
		}
	})

	t.Run("new sections on a draft without a design are not ready", func(t *testing.T) {
		for name, createInput := range map[string][]byte{
			"a version 2 draft":                  []byte(plainCreateInput),
			"no creation found":                  nil,
			"an unreadable request":              []byte(`not json`),
			"a design the validator refuses now": []byte(strings.Replace(designedCreateInput, `"ground": "#F6F5F2", `, "", 1)),
		} {
			store := &fakeDraftCreateInputs{input: createInput}
			section := json.RawMessage(`{"type":"section","anchor":"press","children":[{"type":"paragraph","text":"Press"}]}`)
			facts := pageEditFacts{postID: 418, ops: []pageEditOp{{Op: pageEditOpInsert, Outline: []json.RawMessage{section}}}}
			_, _, refusal, err := (&Service{}).pageEditInheritDesign(context.Background(), designEngine(store), designSite(),
				editOps(opsInsertSection, ""), facts)
			if err != nil || refusal == nil {
				t.Fatalf("%s: err=%v refusal=%v, want design_not_ready", name, err, refusal)
			}
			d := refusalDetails(t, refusal)
			if d["code"] != pageDesignNotReady {
				t.Errorf("%s: code = %v, want %s", name, d["code"], pageDesignNotReady)
			}
			if fails, _ := d["failures"].([]refusalFailure); len(fails) == 0 || fails[0].Key != "design" {
				t.Errorf("%s: failures = %#v, want the missing design named", name, d["failures"])
			}
		}
	})

	t.Run("an edit that adds no node does not read the draft", func(t *testing.T) {
		store := &fakeDraftCreateInputs{input: []byte(designedCreateInput)}
		in := editOps(opsSetText, "")
		got, facts, refusal, err := inheritDesign(t, store, designSite(), in)
		if err != nil || refusal != nil || !bytes.Equal(got, in) || facts.design != nil || len(store.calls) != 0 {
			t.Fatalf("err=%v refusal=%v input=%s reads=%d", err, refusal, got, len(store.calls))
		}
	})

	t.Run("a design in the input is the design, and the draft is not read", func(t *testing.T) {
		store := &fakeDraftCreateInputs{input: []byte(designedCreateInput)}
		in := editOps(opsInsertParagraph, `"design":`+specDesignJSON)
		got, facts, refusal, err := inheritDesign(t, store, designSite(), in)
		if err != nil || refusal != nil || !bytes.Equal(got, in) || len(store.calls) != 0 {
			t.Fatalf("err=%v refusal=%v reads=%d", err, refusal, len(store.calls))
		}
		if facts.design == nil || !reflect.DeepEqual(*facts.design, specDesign) {
			t.Fatalf("the facts hold design %+v", facts.design)
		}
	})

	t.Run("a failed read fails the call", func(t *testing.T) {
		store := &fakeDraftCreateInputs{err: errors.New("db down")}
		_, _, refusal, err := inheritDesign(t, store, designSite(), editOps(opsInsertParagraph, ""))
		if err == nil || refusal != nil {
			t.Fatalf("err=%v refusal=%v, want the error", err, refusal)
		}
	})

	t.Run("a store that cannot read designs leaves a plain edit alone", func(t *testing.T) {
		in := editOps(opsInsertParagraph, "")
		got, _, refusal, err := inheritDesign(t, nil, designSite(), in)
		if err != nil || refusal != nil || !bytes.Equal(got, in) {
			t.Fatalf("err=%v refusal=%v", err, refusal)
		}
	})
}

// TestPageEditSectionsAreCompiledWithTheDraftsDesign: a version 3 section
// inserted into a designed draft is sent with the draft's design; into a
// draft without one it is not ready.
func TestPageEditSectionsAreCompiledWithTheDraftsDesign(t *testing.T) {
	in := editOps(opsInsertSection, "")
	got, facts, refusal, err := inheritDesign(t, &fakeDraftCreateInputs{input: []byte(designedCreateInput)}, designSite(), in)
	if err != nil || refusal != nil || facts.design == nil || membersOf(t, got)["design"] == nil {
		t.Fatalf("err=%v refusal=%v design=%+v input=%s", err, refusal, facts.design, got)
	}
	_, _, refusal, err = inheritDesign(t, &fakeDraftCreateInputs{input: []byte(plainCreateInput)}, designSite(), in)
	if err != nil || refusal == nil || refusalDetails(t, refusal)["code"] != pageDesignNotReady {
		t.Fatalf("a draft without a design: err=%v refusal=%v, want design_not_ready", err, refusal)
	}
}

// TestPageEditDesignOnTheWire: runPageEdit sends the site the input with the
// draft's design in it, and sends a design the input carries as it is.
func TestPageEditDesignOnTheWire(t *testing.T) {
	run := func(t *testing.T, input []byte, store *fakeDraftCreateInputs) []byte {
		t.Helper()
		agent := newAnsweringAgent(t, map[string]any{
			"ok": false, "outcome": "refused", "code": pageEditNodeNotFound, "detail": "x", "retryable": false,
		})
		e := catalogueRow(AbilityPageEdit, "wpmgr", "write")
		e.ApprovalMode, e.Snapshot = "per_call", "builder_document"
		e.Limits = []byte(`{"builders_enabled":["elementor"]}`)
		_, sum, _ := testEntryEncoder(e)
		e.EntrySha256 = &sum
		eng := &abilityEngine{agent: agent, entry: testEntryEncoder, readLimit: newAbilityReadLimiter(), drafts: inputDraftStore{}}
		if store != nil {
			eng.designs = store
		}
		site := abilitySite{row: sqlc.Site{ID: uuid.New(), Url: agent.srv.URL}}
		_, err := (&Service{}).runPageEdit(context.Background(), AuthorizedRequest{GrantID: uuid.New()}, eng, site, &e, "example.com", input)
		var r *toolRefusal
		if !errors.As(err, &r) {
			t.Fatalf("got %v, want the site's refusal", err)
		}
		if len(agent.calls) != 1 || agent.calls[0].Mode != agentcmd.AbilityRunModePrecheck {
			t.Fatalf("the precheck was not sent once: %d calls", len(agent.calls))
		}
		return agent.calls[0].Input
	}

	t.Run("the draft's design", func(t *testing.T) {
		sent := run(t, editOps(opsInsertParagraph, ""), &fakeDraftCreateInputs{input: []byte(designedCreateInput)})
		var design pageDesign
		if err := json.Unmarshal(membersOf(t, sent)["design"], &design); err != nil || !reflect.DeepEqual(design, specDesign) {
			t.Fatalf("the site was sent %s", sent)
		}
	})
	t.Run("a version 2 draft's edit is sent as the AI wrote it", func(t *testing.T) {
		in := editOps(opsInsertParagraph, "")
		if sent := run(t, in, &fakeDraftCreateInputs{input: []byte(plainCreateInput)}); !bytes.Equal(sent, in) {
			t.Fatalf("the site was sent %s", sent)
		}
	})
	t.Run("the input's own design", func(t *testing.T) {
		store := &fakeDraftCreateInputs{input: []byte(designedCreateInput)}
		in := editOps(opsInsertParagraph, `"design":`+specDesignJSON)
		if sent := run(t, in, store); !bytes.Equal(sent, in) || len(store.calls) != 0 {
			t.Fatalf("the site was sent %s; the draft was read %d times", sent, len(store.calls))
		}
	})
}

// TestPageEditDesignOverrideValidated: a design the input carries is judged
// as page-create judges it, with the same codes and failures, before the
// operations are read.
func TestPageEditDesignOverrideValidated(t *testing.T) {
	honest := editOps(opsInsertParagraph, `"design":`+specDesignJSON)
	facts, code := validatePageEditInput(honest)
	if code != "" || facts.design == nil || !reflect.DeepEqual(*facts.design, specDesign) {
		t.Fatalf("the honest design: code=%q design=%+v", code, facts.design)
	}
	if _, r := pageEditPreInputRefusal(honest); r != nil {
		t.Fatalf("the honest design was refused: %v", r.err)
	}

	typeScale := designMember(t, "scale")
	cases := []struct {
		name     string
		design   json.RawMessage
		code     string
		path     string
		key      string
		contains string
	}{
		{"a font left out", designEdit(t, designChange{"design.fonts", "body", ""}), pageDesignNotReady, "design.fonts", "body", ""},
		{"the whole scale left out", designEdit(t, designChange{"design", typeScale, ""}), pageDesignNotReady, "design", typeScale, ""},
		{"ink on ground too close", designEdit(t, designChange{"design.colors", "ink", `"#F6F5F2"`}), pageDesignContrastLow, "design.colors", "ink", ""},
		{"a radius outside the set", designEdit(t, designChange{"design", "radius", `"huge"`}), pageCreateBadInput, "design", "radius", ""},
		{"a colour without its hash", designEdit(t, designChange{"design.colors", "accent", `"0D7A63"`}), pageCreateBadInput, "design.colors", "accent", ""},
		{"a key the design does not have", designEdit(t, designChange{"design", "shadow", `"deep"`}), pageCreateBadInput, "design", "shadow", ""},
		{"not an object", json.RawMessage(`"Manrope"`), pageCreateBadInput, "", "design", ""},
		{"null", json.RawMessage(`null`), pageCreateBadInput, "", "design", ""},
	}
	for _, c := range cases {
		in := editOps(opsInsertParagraph, `"design":`+string(c.design))
		if _, code := validatePageEditInput(in); code != c.code {
			t.Errorf("%s: code = %q, want %q", c.name, code, c.code)
			continue
		}
		_, r := pageEditPreInputRefusal(in)
		if r == nil {
			t.Errorf("%s: the design was accepted", c.name)
			continue
		}
		d := refusalDetails(t, r)
		fails, _ := d["failures"].([]refusalFailure)
		found := false
		for _, f := range fails {
			found = found || (f.Path == c.path && f.Key == c.key)
		}
		if !found {
			t.Errorf("%s: failures %+v do not name %s %s", c.name, fails, c.path, c.key)
		}
		if c.code == pageCreateBadInput {
			if !reflect.DeepEqual(d["schema"], pageEditInputSchema) {
				t.Errorf("%s: a bad value must carry the page-edit schema, got %T", c.name, d["schema"])
			}
		} else if d["code"] != c.code {
			t.Errorf("%s: refusal code = %v, want %s", c.name, d["code"], c.code)
		}
	}

	// The design is read before the operations, so a page without a usable
	// one is told that first.
	both := editOps(`[{"op":"remove"}]`, `"design":`+string(designEdit(t, designChange{"design", "density", ""})))
	if _, code := validatePageEditInput(both); code != pageDesignNotReady {
		t.Errorf("a bad design and bad operations: code = %q, want %s", code, pageDesignNotReady)
	}
	// A design is only ever a member of the input, once.
	if _, code := validatePageEditInput(editOps(opsInsertParagraph, `"design":`+specDesignJSON+`,"page_frame":"theme"`)); code != pageCreateBadInput {
		t.Errorf("page_frame is no page-edit member: code = %q", code)
	}
}

// --- the draft's creation, read in the connection's transaction -----------------

type fakeEligibleDraftInputQueries struct {
	draft      sqlc.GetEligibleCreatedDraftRow
	draftErr   error
	input      sqlc.GetEligibleCreatedDraftInputRow
	inputErr   error
	draftArgs  []sqlc.GetEligibleCreatedDraftParams
	inputArgs  []sqlc.GetEligibleCreatedDraftInputParams
	inputReads int
}

func (f *fakeEligibleDraftInputQueries) GetEligibleCreatedDraft(_ context.Context, a sqlc.GetEligibleCreatedDraftParams) (sqlc.GetEligibleCreatedDraftRow, error) {
	f.draftArgs = append(f.draftArgs, a)
	return f.draft, f.draftErr
}

func (f *fakeEligibleDraftInputQueries) GetEligibleCreatedDraftInput(_ context.Context, a sqlc.GetEligibleCreatedDraftInputParams) (sqlc.GetEligibleCreatedDraftInputRow, error) {
	f.inputArgs = append(f.inputArgs, a)
	f.inputReads++
	return f.input, f.inputErr
}

func TestEligibleDraftCreateInputNamesOneRequest(t *testing.T) {
	tenant, site := uuid.New(), uuid.New()
	post := int64(418)
	id := uuid.New()
	q := &fakeEligibleDraftInputQueries{
		draft: sqlc.GetEligibleCreatedDraftRow{ID: id, CreatedPostID: &post},
		input: sqlc.GetEligibleCreatedDraftInputRow{ID: id, InputJson: designedCreateInput},
	}
	got, err := eligibleDraftCreateInput(context.Background(), q, tenant, site, post)
	if err != nil || string(got) != designedCreateInput {
		t.Fatalf("got %q, %v", got, err)
	}
	want := sqlc.GetEligibleCreatedDraftInputParams{TenantID: tenant, SiteID: site, PostID: post}
	if len(q.inputArgs) != 1 || q.inputArgs[0] != want {
		t.Fatalf("the input was not read for this tenant, site and post: %+v", q.inputArgs)
	}
	if len(q.draftArgs) != 1 || q.draftArgs[0] != (sqlc.GetEligibleCreatedDraftParams{TenantID: tenant, SiteID: site, PostID: post}) {
		t.Fatalf("the draft was not read for this tenant, site and post: %+v", q.draftArgs)
	}

	// Two statements that name two requests are never trusted.
	q.input.ID = uuid.New()
	if got, err = eligibleDraftCreateInput(context.Background(), q, tenant, site, post); err == nil || got != nil {
		t.Fatalf("different request ids must fail the read, got %q, %v", got, err)
	}

	// No eligible draft is no input, and no error.
	none := &fakeEligibleDraftInputQueries{draftErr: pgx.ErrNoRows}
	if got, err = eligibleDraftCreateInput(context.Background(), none, tenant, site, post); err != nil || got != nil || none.inputReads != 0 {
		t.Fatalf("no draft: got %q, %v, input reads %d", got, err, none.inputReads)
	}
	gone := &fakeEligibleDraftInputQueries{draft: q.draft, inputErr: pgx.ErrNoRows}
	if got, err = eligibleDraftCreateInput(context.Background(), gone, tenant, site, post); err != nil || got != nil {
		t.Fatalf("the draft went between the statements: got %q, %v", got, err)
	}
	other := int64(419)
	wrong := &fakeEligibleDraftInputQueries{draft: sqlc.GetEligibleCreatedDraftRow{ID: id, CreatedPostID: &other}}
	if got, err = eligibleDraftCreateInput(context.Background(), wrong, tenant, site, post); err != nil || got != nil || wrong.inputReads != 0 {
		t.Fatalf("a creation of another post: got %q, %v", got, err)
	}
	broken := &fakeEligibleDraftInputQueries{draftErr: errors.New("db down")}
	if got, err = eligibleDraftCreateInput(context.Background(), broken, tenant, site, post); err == nil || got != nil {
		t.Fatalf("a failed read must fail the call, got %q, %v", got, err)
	}
	if got, err = eligibleDraftCreateInput(context.Background(), q, tenant, site, 0); err != nil || got != nil {
		t.Fatalf("post 0 names no draft: got %q, %v", got, err)
	}
}

func TestDraftDesignOf(t *testing.T) {
	if d := draftDesignOf([]byte(designedCreateInput)); d == nil || !reflect.DeepEqual(*d, specDesign) {
		t.Fatalf("a designed draft's design = %+v", d)
	}
	for name, in := range map[string]string{
		"a version 2 input":          plainCreateInput,
		"version 2 with a stray key": strings.Replace(plainCreateInput, `"title"`, `"design":`+specDesignJSON+`,"title"`, 1),
		"version 3 without a design": strings.Replace(designedCreateInput, `"design":`+specDesignJSON+`,`, "", 1),
		"a design that is not ready": strings.Replace(designedCreateInput, `"density": "normal"`, `"density": "tight"`, 1),
		"not an object":              `["design"]`,
		"empty":                      ``,
		"version 4":                  strings.Replace(designedCreateInput, `"outline_version":3`, `"outline_version":4`, 1),
	} {
		if d := draftDesignOf([]byte(in)); d != nil {
			t.Errorf("%s: design = %+v, want none", name, d)
		}
	}
}
