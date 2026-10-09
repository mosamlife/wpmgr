package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// storingRequestStore runs the ability creation transaction the write branch
// hands it over storingAbilityQueries, and keeps every insert and every row
// the insert stored.
type storingRequestStore struct {
	inserts []sqlc.InsertAbilityRequestParams
	rows    []sqlc.AssistantAbilityRequest
}

func (s *storingRequestStore) RunAbilityRequestTx(_ context.Context, _ domain.Principal, fn func(pgx.Tx, abilityRequestQueries) error) error {
	return fn(nil, storingAbilityQueries{s: s})
}

func (s *storingRequestStore) ReadAbilityRequestStatus(context.Context, domain.Principal, uuid.UUID, uuid.UUID) (AbilityStatusRow, bool, error) {
	return AbilityStatusRow{}, false, nil
}

func (s *storingRequestStore) ListOpenAbilityRequestStatus(context.Context, domain.Principal, uuid.UUID, int32) ([]AbilityStatusRow, error) {
	return nil, nil
}

// storingAbilityQueries: no lapsed rows, no cap reached, nothing waiting.
// The insert stores the row under the id it is given or, given none, under a
// fresh one, as InsertAbilityRequest's COALESCE(id, gen_random_uuid()) does.
type storingAbilityQueries struct{ s *storingRequestStore }

func (storingAbilityQueries) TakeAssistantRequestXactLock(context.Context, sqlc.TakeAssistantRequestXactLockParams) error {
	return nil
}

func (storingAbilityQueries) ExpireLapsedPendingAbilityRequestsForGrantSite(context.Context, sqlc.ExpireLapsedPendingAbilityRequestsForGrantSiteParams) ([]uuid.UUID, error) {
	return nil, nil
}

func (storingAbilityQueries) CountLivePendingAbilityRequestsForGrant(context.Context, sqlc.CountLivePendingAbilityRequestsForGrantParams) (int64, error) {
	return 0, nil
}

func (storingAbilityQueries) CountLivePendingAbilityRequestsForGrantSite(context.Context, sqlc.CountLivePendingAbilityRequestsForGrantSiteParams) (int64, error) {
	return 0, nil
}

func (storingAbilityQueries) CountAbilityRequestsForGrantSince(context.Context, sqlc.CountAbilityRequestsForGrantSinceParams) (int64, error) {
	return 0, nil
}

func (storingAbilityQueries) CountAbilityRequestsOnSiteSince(context.Context, sqlc.CountAbilityRequestsOnSiteSinceParams) (int64, error) {
	return 0, nil
}

func (q storingAbilityQueries) InsertAbilityRequest(_ context.Context, arg sqlc.InsertAbilityRequestParams) (sqlc.AssistantAbilityRequest, error) {
	q.s.inserts = append(q.s.inserts, arg)
	id := uuid.New()
	if arg.ID.Valid {
		id = arg.ID.Bytes
	}
	row := sqlc.AssistantAbilityRequest{
		ID: id, TenantID: arg.TenantID, SiteID: arg.SiteID, ProposedByGrantID: arg.ProposedByGrantID,
		AbilityName: arg.AbilityName, Snapshot: arg.Snapshot, ExpiresAt: arg.ExpiresAt,
	}
	q.s.rows = append(q.s.rows, row)
	return row, nil
}

func (storingAbilityQueries) GetPendingAbilityRequestForTarget(context.Context, sqlc.GetPendingAbilityRequestForTargetParams) (sqlc.AssistantAbilityRequest, error) {
	return sqlc.AssistantAbilityRequest{}, pgx.ErrNoRows
}

// textPageAgent answers a block-editor precheck as the agent does, for a
// page without images, and records every call.
type textPageAgent struct{ calls []agentcmd.AbilityRunCall }

func (a *textPageAgent) AbilityRun(_ context.Context, _ uuid.UUID, _ string, call agentcmd.AbilityRunCall) (agentcmd.AbilityRunResponse, error) {
	a.calls = append(a.calls, call)
	f, code := validatePageCreateInput(call.Input)
	if code != "" {
		return agentcmd.AbilityRunResponse{}, &agentcmd.AbilityRunRefusal{Code: code}
	}
	const content = "Body text"
	prev, _ := phpJSONStringArray(f.editor, f.postType, "draft", f.title, content)
	base, _ := pageCreateBaseFingerprint(f.postType, nil)
	pre, _ := phpJSONStringArray(call.EntrySHA256, sha256Hex(call.Input), base, sha256Hex(prev))
	pv, _ := json.Marshal(pagePreview{PostType: f.postType, Editor: f.editor, Status: "draft", Title: f.title, Content: content})
	return agentcmd.AbilityRunResponse{
		OK: true, Mode: agentcmd.AbilityRunModePrecheck, Valid: true, BaseFingerprint: base,
		PreviewDigest: sha256Hex(prev), PrecheckDigest: sha256Hex(pre), Preview: pv,
	}, nil
}

// TestPageCreateRun_RequestIsStoredUnderItsPrecheckID (BF-C F1): through the
// real transport, a page-create request is stored under the request id its
// precheck was sent with, and the answer names that id. The dispatch worker
// sends the write under the stored id, so the write the site receives names
// the request the site prechecked; a page builder's node ids derive from that
// id, so the write rebuilds the tree the card showed.
func TestPageCreateRun_RequestIsStoredUnderItsPrecheckID(t *testing.T) {
	t.Run("block editor", func(t *testing.T) {
		store := &storingRequestStore{}
		agent := &textPageAgent{}
		r, site := pageCreateRunRouterStore(t, agentcmd.MinAgentVersionForPageCreate, agent, []byte(`{}`), store)
		assertStoredUnderPrecheckID(t, r, site, textOnlyPage, store, func() []agentcmd.AbilityRunCall { return agent.calls })
	})
	t.Run("elementor", func(t *testing.T) {
		boxes := readElementorGoldens(t)[0]
		store := &storingRequestStore{}
		agent := &treeAgent{t: t, golden: boxes, idFor: func(sent string) string { return sent }}
		r, site := pageCreateRunRouterStore(t, agentcmd.MinAgentVersionForBuilderAdapters, agent, elementorEnabled, store)
		in := elementorInput(string(goldenCase(t, boxes, "mixed-top-level").Input))
		assertStoredUnderPrecheckID(t, r, site, in, store, func() []agentcmd.AbilityRunCall { return agent.calls })
	})
}

// assertStoredUnderPrecheckID runs site_ability_run for input and fails
// unless the site was sent exactly one precheck, exactly one row was
// inserted, and the precheck's request id, the id the insert carried, the
// stored row's id and the answered request_id are one id.
func assertStoredUnderPrecheckID(t *testing.T, r *gin.Engine, site uuid.UUID, input string, store *storingRequestStore, calls func() []agentcmd.AbilityRunCall) {
	t.Helper()
	var in map[string]any
	if err := json.Unmarshal([]byte(input), &in); err != nil {
		t.Fatal(err)
	}
	w := post(t, r, callBody(ToolSiteAbilityRun, map[string]any{"site_id": site, "name": AbilityPageCreate, "input": in}), nil)
	var res abilityCreatedResult
	if err := json.Unmarshal([]byte(toolText(t, w.Body.String())), &res); err != nil || res.State != stateWaitingForApproval {
		t.Fatalf("not stored (%v): %s", err, w.Body.String())
	}
	sent := calls()
	if len(sent) != 1 || sent[0].Mode != agentcmd.AbilityRunModePrecheck || len(store.inserts) != 1 || len(store.rows) != 1 {
		t.Fatalf("%d calls to the site and %d inserts, want one precheck and one insert: %s", len(sent), len(store.inserts), w.Body.String())
	}
	precheck := sent[0].RequestID
	if precheck == uuid.Nil {
		t.Fatal("the precheck was sent without a request id")
	}
	if got := store.inserts[0].ID; !got.Valid || uuid.UUID(got.Bytes) != precheck {
		t.Fatalf("the insert carried id %v (valid %v); the precheck was sent as request %s", uuid.UUID(got.Bytes), got.Valid, precheck)
	}
	if store.rows[0].ID != precheck || res.RequestID != precheck.String() || res.Existing {
		t.Fatalf("stored as %s and answered as %s (existing %v); the precheck was sent as request %s",
			store.rows[0].ID, res.RequestID, res.Existing, precheck)
	}
}
