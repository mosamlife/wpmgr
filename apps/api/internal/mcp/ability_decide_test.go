package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/aipolicy"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

const decideTestPublicBase = "https://app.wpmgr.test"

// decideCall is one request the decider was asked to decide, with how many
// request rows the store held at that moment.
type decideCall struct {
	tenantID, requestID uuid.UUID
	storedRows          int
}

// recordingDecider stands in for the approval package: it records every
// request it is asked to decide and answers decision.
type recordingDecider struct {
	store    *storingRequestStore
	decision AbilityDecision
	calls    []decideCall
}

func (d *recordingDecider) DecideAbilityRequest(_ context.Context, tenantID, requestID uuid.UUID) (AbilityDecision, error) {
	d.calls = append(d.calls, decideCall{tenantID: tenantID, requestID: requestID, storedRows: len(d.store.rows)})
	return d.decision, nil
}

// settledRequestStore is storingRequestStore whose status read reports each
// stored request done with outcome, as a request the site's setting
// approved and the dispatch worker sent reads back.
type settledRequestStore struct {
	*storingRequestStore
	outcome string
}

func (s settledRequestStore) ReadAbilityRequestStatus(_ context.Context, _ domain.Principal, _, requestID uuid.UUID) (AbilityStatusRow, bool, error) {
	for _, r := range s.rows {
		if r.ID == requestID {
			outcome := s.outcome
			return AbilityStatusRow{ID: r.ID, SiteID: r.SiteID, AbilityName: r.AbilityName, State: "done",
				Outcome: &outcome, ExpiresAt: r.ExpiresAt}, true, nil
		}
	}
	return AbilityStatusRow{}, false, nil
}

// namedDraftStore is the ability store naming post as a draft WPMgr created
// on the site, as the eligible-draft read does for the AI's own draft.
type namedDraftStore struct {
	*fakeAbilityStore
	post int64
}

func (s namedDraftStore) EligibleDraftIDs(_ context.Context, _ domain.Principal, _ uuid.UUID, postID int64) ([]int64, error) {
	if postID != s.post {
		return []int64{}, nil
	}
	return []int64{s.post}, nil
}

// pageEditFixtureAgent answers a page-edit precheck with the agent's
// recorded preview (page-edit-preview.json), its precheck digest bound to
// the entry and the input it was sent, as the agent binds it.
type pageEditFixtureAgent struct {
	fx    pageEditPreviewFixture
	calls []agentcmd.AbilityRunCall
}

func (a *pageEditFixtureAgent) AbilityRun(_ context.Context, _ uuid.UUID, _ string, call agentcmd.AbilityRunCall) (agentcmd.AbilityRunResponse, error) {
	a.calls = append(a.calls, call)
	resp := fixtureResponse(a.fx, nil)
	pre, _ := phpJSONStringArray(call.EntrySHA256, sha256Hex(call.Input), a.fx.BaseFingerprint, a.fx.PreviewDigest)
	resp.PrecheckDigest = sha256Hex(pre)
	resp.RequestID = call.RequestID.String()
	return resp, nil
}

// decidedWriteRouter is the real transport over a site with content editing
// on, the catalogue entry for ability, writes on, and dec wired as the
// approval package is in cmd/wpmgr.
func decidedWriteRouter(t *testing.T, ability string, agent AbilityAgent, store AbilityRequestStore, dec AbilityDecider) (*gin.Engine, uuid.UUID) {
	t.Helper()
	f := newAbilityFixture(t)
	f.store.recheck.GrantCapabilities = []string{string(CapSitesRead), string(CapAbilityRead), string(CapAbilityRequest)}
	f.store.recheck.GrantOauthScopes = []string{string(ScopeRead), string(ScopeSite)}
	f.store.sites[0].AgentVersion = agentcmd.MinAgentVersionForBuilderEdit
	f.store.sites[0].ContentEditingEnabledAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	perm := "site.content.edit"
	entry := catalogueRow(ability, "wpmgr", "write")
	entry.ApprovalMode, entry.EffectCopy, entry.OperatorPermission = "per_call", "draft", &perm
	minAgent := agentcmd.MinAgentVersionForPageCreate
	entry.Snapshot = "created_post_trash"
	if ability == AbilityPageEdit {
		minAgent = agentcmd.MinAgentVersionForBuilderEdit
		entry.Snapshot = "builder_document"
		entry.Limits = []byte(`{"builders_enabled":["elementor"],"max_operations":25}`)
	}
	entry.MinAgentVersion = &minAgent
	_, sum, _ := testEntryEncoder(entry)
	entry.EntrySha256 = &sum
	f.ab.cat = append(f.ab.cat, entry)
	f.ab.rows = append(f.ab.rows, sqlc.SiteAbilityInventory{SiteID: f.siteID, Name: ability, OwnerKind: "plugin"})

	gin.SetMode(gin.TestMode)
	r := gin.New()
	svc := NewService(railReadyStore{f.store}).WithContextResolver(emptyContextResolver()).withAuditRecorder(&capturingRecorder{})
	if err := svc.EnableAbilityTools(namedDraftStore{fakeAbilityStore: f.ab, post: 120}, agent, testEntryEncoder, "test-secret"); err != nil {
		t.Fatal(err)
	}
	if err := svc.EnableAbilityWrites(store); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetWriteToolsEnabled(true); err != nil {
		t.Fatal(err)
	}
	svc.SetAbilityDecider(dec)
	svc.SetPublicBaseURL(decideTestPublicBase)
	NewTransportHandler(svc, slog.New(slog.DiscardHandler), "test-version").Register(r)
	return r, f.siteID
}

// TestWriteRequestsReachTheDecider: a write the AI asks for through
// site_ability_run is handed to the approval package exactly once, by the
// stored request's id and tenant, after the request row exists, and the
// answer carries what the decider made of it: why it waits, with the link
// where a person decides it, or that the site's setting ran it. Page
// creation is the path that always did this; a page edit is held to the
// same contract.
func TestWriteRequestsReachTheDecider(t *testing.T) {
	fx := readPageEditPreviewFixture(t)
	type write struct {
		ability string
		input   string
		outcome string // the outcome a settled request of this ability reads back
		agent   func() (AbilityAgent, func() int)
	}
	writes := []write{
		{AbilityPageCreate, textOnlyPage, "created", func() (AbilityAgent, func() int) {
			a := &textPageAgent{}
			return a, func() int { return len(a.calls) }
		}},
		{AbilityPageEdit, fx.Input, "applied", func() (AbilityAgent, func() int) {
			a := &pageEditFixtureAgent{fx: fx}
			return a, func() int { return len(a.calls) }
		}},
	}
	for _, wr := range writes {
		t.Run(wr.ability+" waits for a person", func(t *testing.T) {
			store := &storingRequestStore{}
			dec := &recordingDecider{store: store, decision: AbilityDecision{AskReason: string(aipolicy.AskSiteModeAsk)}}
			agent, prechecks := wr.agent()
			r, site := decidedWriteRouter(t, wr.ability, agent, store, dec)
			res := runDecidedWrite(t, r, site, wr.ability, wr.input)
			id := wantDecidedOnce(t, wr.ability, store, dec, prechecks(), res)
			if res.State != stateWaitingForApproval || res.Approval != aipolicy.ApprovalAsk ||
				res.AskReason != string(aipolicy.AskSiteModeAsk) {
				t.Fatalf("%s: state %q approval %q ask_reason %q, want waiting_for_approval, ask, site_mode_ask",
					wr.ability, res.State, res.Approval, res.AskReason)
			}
			if want := decideTestPublicBase + "/ai/requests?request=" + id.String(); res.ApprovalURL != want {
				t.Fatalf("%s: approval_url %q, want %q", wr.ability, res.ApprovalURL, want)
			}
			if res.Message != aipolicy.AskSiteModeAsk.Message() {
				t.Fatalf("%s: message %q, want the site_mode_ask message", wr.ability, res.Message)
			}
		})
		t.Run(wr.ability+" approved by the site's setting", func(t *testing.T) {
			store := &storingRequestStore{}
			dec := &recordingDecider{store: store, decision: AbilityDecision{Approved: true}}
			agent, prechecks := wr.agent()
			r, site := decidedWriteRouter(t, wr.ability, agent, settledRequestStore{storingRequestStore: store, outcome: wr.outcome}, dec)
			res := runDecidedWrite(t, r, site, wr.ability, wr.input)
			wantDecidedOnce(t, wr.ability, store, dec, prechecks(), res)
			if res.State != "done" || res.Approval != aipolicy.ApprovalAuto || res.AskReason != "" || res.ApprovalURL != "" {
				t.Fatalf("%s: state %q approval %q ask_reason %q approval_url %q, want done and auto with no ask",
					wr.ability, res.State, res.Approval, res.AskReason, res.ApprovalURL)
			}
			if res.Outcome == nil || *res.Outcome != wr.outcome || res.Message != aipolicy.MessageDoneBySetting {
				t.Fatalf("%s: outcome %v message %q, want %s and the done-by-setting message", wr.ability, res.Outcome, res.Message, wr.outcome)
			}
		})
	}
}

// runDecidedWrite runs site_ability_run for ability over the transport and
// decodes the answer.
func runDecidedWrite(t *testing.T, r *gin.Engine, site uuid.UUID, ability, input string) abilityCreatedResult {
	t.Helper()
	var in map[string]any
	if err := json.Unmarshal([]byte(input), &in); err != nil {
		t.Fatal(err)
	}
	w := post(t, r, callBody(ToolSiteAbilityRun, map[string]any{"site_id": site, "name": ability, "input": in}), nil)
	var res abilityCreatedResult
	if err := json.Unmarshal([]byte(toolText(t, w.Body.String())), &res); err != nil || res.RequestID == "" {
		t.Fatalf("%s: no request was created (%v): %s", ability, err, w.Body.String())
	}
	return res
}

// wantDecidedOnce fails unless the site was prechecked once, one request
// row was stored, and the decider was called exactly once, for that row's
// id and tenant, after the row was stored; it returns the request's id.
func wantDecidedOnce(t *testing.T, ability string, store *storingRequestStore, dec *recordingDecider, prechecks int, res abilityCreatedResult) uuid.UUID {
	t.Helper()
	if prechecks != 1 || len(store.rows) != 1 || store.rows[0].AbilityName != ability {
		t.Fatalf("%s: %d prechecks and %d rows stored, want 1 and 1", ability, prechecks, len(store.rows))
	}
	row := store.rows[0]
	if len(dec.calls) != 1 {
		t.Fatalf("%s: the decider was called %d times, want 1", ability, len(dec.calls))
	}
	got := dec.calls[0]
	if got.requestID != row.ID || got.tenantID != row.TenantID {
		t.Fatalf("%s: decided request %s in tenant %s, want the stored request %s in tenant %s",
			ability, got.requestID, got.tenantID, row.ID, row.TenantID)
	}
	if got.storedRows != 1 {
		t.Fatalf("%s: decided with %d request rows stored, want the row stored first", ability, got.storedRows)
	}
	if res.RequestID != row.ID.String() || res.Existing {
		t.Fatalf("%s: answered request %s (existing %v), want the stored request %s", ability, res.RequestID, res.Existing, row.ID)
	}
	return row.ID
}
