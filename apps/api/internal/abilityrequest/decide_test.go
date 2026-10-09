package abilityrequest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/aipolicy"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

var (
	dTenant  = uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001")
	dSite    = uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000002")
	dGrant   = uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000003")
	dRequest = uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000004")
	dOwner   = uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000005")
	dAdmin   = uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000006")
	dSetAt   = time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)
)

// fakePolicy is a PolicyStore over in-memory snapshots. snaps[i] is what
// the i-th read returns (the last one repeats), so a test can move the
// setting between the read before the lock and the read under it.
type fakePolicy struct {
	snaps      []PolicySnapshot
	reads      int
	usage      aipolicy.Usage
	usageErr   error
	approveErr []error // per ApproveByPolicy call; nil entries succeed
	approves   []PolicyApproval
	asks       []PolicyAsk
	audits     []audit.Event
	auditErr   error
	inApproval bool
	committed  int
}

func (f *fakePolicy) next() PolicySnapshot {
	i := f.reads
	if i >= len(f.snaps) {
		i = len(f.snaps) - 1
	}
	f.reads++
	return f.snaps[i]
}

func (f *fakePolicy) Snapshot(context.Context, uuid.UUID, uuid.UUID) (PolicySnapshot, error) {
	return f.next(), nil
}

func (f *fakePolicy) Approving(ctx context.Context, _ uuid.UUID, fn func(PolicyTx) error) error {
	f.inApproval = true
	defer func() { f.inApproval = false }()
	// A rolled-back transaction keeps nothing it wrote.
	approves, asks, audits := len(f.approves), len(f.asks), len(f.audits)
	if err := fn(f); err != nil {
		f.approves, f.asks, f.audits = f.approves[:approves], f.asks[:asks], f.audits[:audits]
		return err
	}
	f.committed++
	return nil
}

func (f *fakePolicy) DraftUsage(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (aipolicy.Usage, error) {
	return f.usage, f.usageErr
}

func (f *fakePolicy) ApproveByPolicy(_ context.Context, a PolicyApproval) (sqlc.AssistantAbilityRequest, error) {
	n := len(f.approves)
	f.approves = append(f.approves, a)
	if n < len(f.approveErr) && f.approveErr[n] != nil {
		f.approves = f.approves[:n]
		return sqlc.AssistantAbilityRequest{}, f.approveErr[n]
	}
	return sqlc.AssistantAbilityRequest{ID: a.RequestID, State: "approved", ApprovalSource: "policy"}, nil
}

func (f *fakePolicy) RecordAsk(_ context.Context, a PolicyAsk) error {
	f.asks = append(f.asks, a)
	return nil
}

func (f *fakePolicy) Audit(_ context.Context, e audit.Event) error {
	if f.auditErr != nil {
		return f.auditErr
	}
	f.audits = append(f.audits, e)
	return nil
}

// fakeSetters resolves people from a table, and records any lookup made
// while the approving transaction is open.
type fakeSetters struct {
	people         map[uuid.UUID]aipolicy.Setter
	store          *fakePolicy
	calls          []uuid.UUID
	insideApproval int
}

func (f *fakeSetters) ResolveSetter(_ context.Context, tenantID, userID uuid.UUID) aipolicy.Setter {
	f.calls = append(f.calls, userID)
	if f.store != nil && f.store.inApproval {
		f.insideApproval++
	}
	if s, ok := f.people[userID]; ok {
		return s
	}
	return aipolicy.Setter{UserID: userID}
}

func person(id uuid.UUID, role authz.Role) aipolicy.Setter {
	return aipolicy.Setter{
		UserID:        id,
		Principal:     domain.Principal{Type: domain.PrincipalUser, UserID: id, TenantID: dTenant, Role: string(role), Scope: domain.ScopeOrg},
		AccountStatus: aipolicy.AccountActive,
	}
}

// pageCreate is a pending page creation on an Auto-for-AI-drafts site,
// from a connection a person allowed.
func pageCreate() PolicySnapshot {
	return PolicySnapshot{
		Request: sqlc.AssistantAbilityRequest{
			ID: dRequest, TenantID: dTenant, SiteID: dSite, ProposedByGrantID: dGrant,
			AbilityName: "wpmgr/page-create", OperatorPermission: string(authz.PermSiteContentEdit), State: "pending",
		},
		StoredClass:      aipolicy.ClassAIDraft,
		SiteMode:         aipolicy.ModeAIDrafts,
		SiteModeSource:   "person",
		SiteModeVersion:  1,
		SiteSetter:       dOwner,
		SiteSetAt:        dSetAt,
		ConnectionAuto:   aipolicy.AutoSiteSetting,
		ConnectionSetter: dAdmin,
	}
}

func restWrite(status string, undoExact string, aiDraft bool) PolicySnapshot {
	s := pageCreate()
	route := "wp-v2-posts-update-fields"
	s.Request.AbilityName = "wpmgr/rest-write"
	s.Request.RouteID = &route
	s.Request.CheckedTargetStatus = &status
	s.Request.CardFacts = []byte(`{"undo_exact":` + undoExact + `}`)
	s.StoredClass = aipolicy.StoredByTargetStatus
	s.AIDraft = aiDraft
	return s
}

func newDecider(snaps ...PolicySnapshot) (*Service, *fakePolicy, *fakeSetters) {
	store := &fakePolicy{snaps: snaps, usage: aipolicy.Usage{Changes: 3, Sites: 1}}
	setters := &fakeSetters{store: store, people: map[uuid.UUID]aipolicy.Setter{
		dOwner: person(dOwner, authz.RoleOwner),
		dAdmin: person(dAdmin, authz.RoleAdmin),
	}}
	s := &Service{enabled: true, logger: discardLogger()}
	s.SetPolicy(store, setters)
	return s, store, setters
}

func TestDecideApprovesAnAIDraftBySetting(t *testing.T) {
	s, store, setters := newDecider(pageCreate())
	res, err := s.Decide(context.Background(), dTenant, dRequest)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Decided || res.Outcome != aipolicy.OutcomeAutoBySetting || res.Class != aipolicy.ClassAIDraft {
		t.Fatalf("got %+v, want approved by the setting", res)
	}
	if len(store.approves) != 1 || len(store.asks) != 0 || store.committed != 1 {
		t.Fatalf("approves %d asks %d commits %d", len(store.approves), len(store.asks), store.committed)
	}
	a := store.approves[0]
	if a.SiteMode != aipolicy.ModeAIDrafts || a.ModeSource != "person" || a.ModeVersion != 1 || a.SetterUserID != dOwner || !a.SetterSetAt.Equal(dSetAt) ||
		a.BaseClass != aipolicy.ClassAIDraft || a.Class != aipolicy.ClassAIDraft || a.DispatchWindowSeconds != dispatchWindowSeconds {
		t.Fatalf("approval relied on the wrong setting: %+v", a)
	}
	if len(store.audits) != 1 {
		t.Fatalf("%d audit rows, want 1", len(store.audits))
	}
	ev := store.audits[0]
	if ev.ActorType != audit.ActorPolicy || ev.ActorID != dSite.String() || ev.Action != audit.ActionAbilityRequestApproved {
		t.Fatalf("audit row %+v, want a policy approval naming the site", ev)
	}
	if ev.ActorType == audit.ActorUser || ev.Metadata["approval_source"] != "policy" {
		t.Fatalf("an automatic approval names a person: %+v", ev)
	}
	if setters.insideApproval != 0 {
		t.Fatalf("%d setter lookups ran inside the approving transaction", setters.insideApproval)
	}
	if len(setters.calls) != 2 {
		t.Fatalf("looked up %d people, want the site's setter and the connection's", len(setters.calls))
	}
}

func TestDecideNotWiredDecidesNothing(t *testing.T) {
	s := &Service{enabled: true, logger: discardLogger()}
	res, err := s.Decide(context.Background(), dTenant, dRequest)
	if err != nil || res.Decided {
		t.Fatalf("got %+v %v, want nothing decided", res, err)
	}
	s2, store, _ := newDecider(pageCreate())
	s2.enabled = false
	if res, err := s2.Decide(context.Background(), dTenant, dRequest); err != nil || res.Decided || store.reads != 0 {
		t.Fatalf("write tools off: got %+v %v reads %d", res, err, store.reads)
	}
}

func TestDecideAlreadyDecidedIsLeftAlone(t *testing.T) {
	snap := pageCreate()
	snap.Request.State = "approved"
	s, store, _ := newDecider(snap)
	if res, err := s.Decide(context.Background(), dTenant, dRequest); err != nil || res.Decided || len(store.approves)+len(store.asks) != 0 {
		t.Fatalf("got %+v %v", res, err)
	}
	snap = pageCreate()
	snap.Request.PolicyCheckedAt.Valid = true
	s, store, _ = newDecider(snap)
	if res, err := s.Decide(context.Background(), dTenant, dRequest); err != nil || res.Decided || len(store.approves)+len(store.asks) != 0 {
		t.Fatalf("checked row: got %+v %v", res, err)
	}
}

func decideAsk(t *testing.T, s *Service, store *fakePolicy, want aipolicy.AskReason) DecideResult {
	t.Helper()
	res, err := s.Decide(context.Background(), dTenant, dRequest)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != aipolicy.OutcomeAsk || res.Ask != want {
		t.Fatalf("got %+v, want ask %s", res, want)
	}
	if len(store.approves) != 0 || len(store.asks) != 1 || store.asks[0].Reason != want {
		t.Fatalf("approves %v asks %v", store.approves, store.asks)
	}
	if len(store.audits) != 1 || store.audits[0].Action != audit.ActionAbilityRequestAsked ||
		store.audits[0].ActorType != audit.ActorPolicy || store.audits[0].Metadata["ask_reason"] != string(want) {
		t.Fatalf("audit rows %+v", store.audits)
	}
	return res
}

func TestDecideAsks(t *testing.T) {
	removed := person(dOwner, authz.RoleOwner)
	removed.Principal.TenantID = uuid.Nil
	disabled := person(dOwner, authz.RoleOwner)
	disabled.AccountStatus = "disabled"
	lookupFailed := person(dOwner, authz.RoleOwner)
	lookupFailed.LookupFailed = true
	demoted := person(dAdmin, authz.RoleOperator)
	siteOnly := person(dAdmin, authz.RoleAdmin)
	siteOnly.Principal.Scope = domain.ScopeSite
	siteOnly.Principal.AllowedSiteIDs = []uuid.UUID{dSite}

	cases := []struct {
		name   string
		snap   func() PolicySnapshot
		people map[uuid.UUID]aipolicy.Setter
		usage  *aipolicy.Usage
		want   aipolicy.AskReason
	}{
		{"site set to ask", func() PolicySnapshot {
			s := pageCreate()
			s.SiteMode, s.SiteSetter = aipolicy.ModeAsk, uuid.Nil
			return s
		}, nil, nil, aipolicy.AskSiteModeAsk},
		{"key-minted connection", func() PolicySnapshot {
			s := pageCreate()
			s.ConnectionAuto, s.ConnectionSetter = aipolicy.AutoNever, uuid.Nil
			return s
		}, nil, nil, aipolicy.AskConnectionNeverAuto},
		{"site setter removed from the organisation", pageCreate, map[uuid.UUID]aipolicy.Setter{dOwner: removed}, nil, aipolicy.AskSetterLacksPermission},
		{"site setter disabled", pageCreate, map[uuid.UUID]aipolicy.Setter{dOwner: disabled}, nil, aipolicy.AskSetterLacksPermission},
		{"site setter lookup failed", pageCreate, map[uuid.UUID]aipolicy.Setter{dOwner: lookupFailed}, nil, aipolicy.AskNotChecked},
		{"connection setter demoted", pageCreate, map[uuid.UUID]aipolicy.Setter{dAdmin: demoted}, nil, aipolicy.AskConnectionSetterInvalid},
		{"connection setter now a site collaborator", pageCreate, map[uuid.UUID]aipolicy.Setter{dAdmin: siteOnly}, nil, aipolicy.AskConnectionSetterInvalid},
		{"row needs an admin-rank permission", func() PolicySnapshot {
			s := pageCreate()
			s.Request.OperatorPermission = string(authz.PermSiteFilesWrite)
			return s
		}, map[uuid.UUID]aipolicy.Setter{dOwner: person(dOwner, authz.RoleOperator)}, nil, aipolicy.AskSetterLacksPermission},
		{"stored always_ask on an AI draft", func() PolicySnapshot {
			s := restWrite("draft", "true", true)
			s.StoredClass = aipolicy.ClassAlwaysAsk
			return s
		}, nil, nil, aipolicy.AskKindAlwaysAsks},
		{"rest-write whose undo is not exact", func() PolicySnapshot { return restWrite("draft", "false", true) }, nil, nil, aipolicy.AskKindAlwaysAsks},
		{"rest-write with no undo fact", func() PolicySnapshot {
			s := restWrite("draft", "true", true)
			s.Request.CardFacts = []byte(`{}`)
			return s
		}, nil, nil, aipolicy.AskKindAlwaysAsks},
		{"rest-write on a published page", func() PolicySnapshot { return restWrite("publish", "true", false) }, nil, nil, aipolicy.AskKindNotInMode},
		{"rest-write on a scheduled AI draft", func() PolicySnapshot { return restWrite("future", "true", true) }, nil, nil, aipolicy.AskKindNotInMode},
		{"rest-write on a person's draft", func() PolicySnapshot { return restWrite("draft", "true", false) }, nil, nil, aipolicy.AskKindNotInMode},
		{"rest-write on a raw status that is not exactly draft", func() PolicySnapshot { return restWrite("Draft", "true", true) }, nil, nil, aipolicy.AskUnknownTargetState},
		{"601st draft change this hour", pageCreate, nil, &aipolicy.Usage{Changes: aipolicy.DraftChangesPerConnection}, aipolicy.AskOverChangeBudget},
		{"31st site this hour", pageCreate, nil, &aipolicy.Usage{Changes: 40, Sites: aipolicy.DraftSitesPerConnection}, aipolicy.AskOverSiteCap},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, store, setters := newDecider(tc.snap())
			for id, p := range tc.people {
				setters.people[id] = p
			}
			if tc.usage != nil {
				store.usage = *tc.usage
			}
			decideAsk(t, s, store, tc.want)
			if setters.insideApproval != 0 {
				t.Fatalf("%d setter lookups inside the approving transaction", setters.insideApproval)
			}
		})
	}
}

func TestDecideRestWriteOnAIDraftRuns(t *testing.T) {
	s, store, _ := newDecider(restWrite("draft", "true", true))
	res, err := s.Decide(context.Background(), dTenant, dRequest)
	if err != nil || res.Outcome != aipolicy.OutcomeAutoBySetting || res.Class != aipolicy.ClassAIDraft {
		t.Fatalf("got %+v %v", res, err)
	}
	if store.approves[0].BaseClass != aipolicy.StoredByTargetStatus || store.approves[0].Class != aipolicy.ClassAIDraft {
		t.Fatalf("approval %+v, want base by_target_status, class ai_draft", store.approves[0])
	}
}

func TestDecideKeyMintedConnectionLooksUpNoConnectionSetter(t *testing.T) {
	snap := pageCreate()
	snap.ConnectionAuto, snap.ConnectionSetter = aipolicy.AutoNever, uuid.Nil
	s, store, setters := newDecider(snap)
	decideAsk(t, s, store, aipolicy.AskConnectionNeverAuto)
	for _, id := range setters.calls {
		if id == uuid.Nil {
			t.Fatal("looked up a connection setter that is not on record")
		}
	}
}

// The site's setter changes after the check and before the lock: Decide
// starts again and decides on the new setter.
func TestDecideSetterChangedBetweenCheckAndLock(t *testing.T) {
	before := pageCreate()
	after := pageCreate()
	after.SiteSetter, after.SiteModeVersion = dAdmin, 2
	removed := person(dAdmin, authz.RoleAdmin)
	removed.Principal.TenantID = uuid.Nil
	// read, re-read (moved), read, re-read (same)
	s, store, setters := newDecider(before, after, after, after)
	setters.people[dAdmin] = removed // also the connection setter: invalid
	res, err := s.Decide(context.Background(), dTenant, dRequest)
	if err != nil {
		t.Fatal(err)
	}
	if res.Ask != aipolicy.AskConnectionSetterInvalid || store.reads != 4 || store.committed != 1 {
		t.Fatalf("got %+v after %d reads and %d commits", res, store.reads, store.committed)
	}
}

// The site's mode source changes after the check and before the lock, and
// nothing else does: Decide starts again, and the approval carries the
// source read under the lock, which the compare-and-set and the backstop
// both compare with the site's.
func TestDecideModeSourceChangedBetweenCheckAndLock(t *testing.T) {
	before := pageCreate()
	before.SiteModeSource = "enable_default"
	after := pageCreate()
	// read, re-read (moved), read, re-read (same)
	s, store, _ := newDecider(before, after, after, after)
	res, err := s.Decide(context.Background(), dTenant, dRequest)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != aipolicy.OutcomeAutoBySetting || store.reads != 4 || store.committed != 1 || len(store.approves) != 1 {
		t.Fatalf("got %+v after %d reads, %d commits and %d approvals; want an approval after a restart",
			res, store.reads, store.committed, len(store.approves))
	}
	if got := store.approves[0].ModeSource; got != "person" {
		t.Fatalf("approval carries mode source %q, want the source read under the lock (person)", got)
	}
}

// A setting that moves under every attempt leaves the request waiting with
// not_checked on the last one.
func TestDecideSettingMovesEveryTimeAsksNotChecked(t *testing.T) {
	var snaps []PolicySnapshot
	for i := 0; i < 2*decideAttempts; i++ {
		s := pageCreate()
		s.SiteModeVersion = int64(i + 1)
		snaps = append(snaps, s)
	}
	s, store, _ := newDecider(snaps...)
	res, err := s.Decide(context.Background(), dTenant, dRequest)
	if err != nil {
		t.Fatal(err)
	}
	if res.Ask != aipolicy.AskNotChecked || len(store.approves) != 0 || len(store.asks) != 1 || store.committed != 1 {
		t.Fatalf("got %+v approves %d asks %d commits %d", res, len(store.approves), len(store.asks), store.committed)
	}
}

// The mode is lowered between the read under the lock and the
// compare-and-set: the statement touches no row, Decide starts again and the
// lowered mode makes the request wait.
func TestDecideModeLoweredBeforeCompareAndSet(t *testing.T) {
	lowered := pageCreate()
	lowered.SiteMode, lowered.SiteSetter, lowered.SiteModeVersion = aipolicy.ModeAsk, uuid.Nil, 2
	s, store, _ := newDecider(pageCreate(), pageCreate(), lowered, lowered)
	store.approveErr = []error{pgx.ErrNoRows}
	res, err := s.Decide(context.Background(), dTenant, dRequest)
	if err != nil {
		t.Fatal(err)
	}
	if res.Ask != aipolicy.AskSiteModeAsk || len(store.approves) != 0 || len(store.asks) != 1 {
		t.Fatalf("got %+v approves %v asks %v", res, store.approves, store.asks)
	}
}

// Nothing is approved unless its audit row commits with it.
func TestDecideAuditFailureApprovesNothing(t *testing.T) {
	s, store, _ := newDecider(pageCreate())
	store.auditErr = errors.New("audit insert failed")
	res, err := s.Decide(context.Background(), dTenant, dRequest)
	if err == nil || res.Decided || store.committed != 0 || len(store.approves) != 0 {
		t.Fatalf("got %+v %v commits %d approves %d", res, err, store.committed, len(store.approves))
	}
}

func TestDecideUsageReadFailureApprovesNothing(t *testing.T) {
	s, store, _ := newDecider(pageCreate())
	store.usageErr = errors.New("count failed")
	res, err := s.Decide(context.Background(), dTenant, dRequest)
	if err == nil || res.Decided || len(store.approves) != 0 {
		t.Fatalf("got %+v %v approves %d", res, err, len(store.approves))
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestUndoFact(t *testing.T) {
	route := "r"
	cases := []struct {
		route *string
		facts string
		want  aipolicy.Undo
	}{
		{nil, ``, aipolicy.UndoByAbility},
		{&route, `{"undo_exact":true}`, aipolicy.UndoExact},
		{&route, `{"undo_exact":false}`, aipolicy.UndoNotExact},
		{&route, `{}`, aipolicy.UndoUnknown},
		{&route, `{"undo_exact":"true"}`, aipolicy.UndoUnknown},
		{&route, `not json`, aipolicy.UndoUnknown},
	}
	for _, tc := range cases {
		got := undoFact(sqlc.AssistantAbilityRequest{RouteID: tc.route, CardFacts: []byte(tc.facts)})
		if got != tc.want {
			t.Errorf("route %v facts %q: got %d, want %d", tc.route != nil, tc.facts, got, tc.want)
		}
	}
}
