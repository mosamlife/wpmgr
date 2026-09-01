package tenant

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// --- fakes -------------------------------------------------------------------

// fakeRepo records what actually reached the persistence layer. The point of
// every assertion below is what it did or did NOT receive: a cross-tenant
// refusal that still called PauseAssistant would be a refusal in name only.
type fakeRepo struct {
	state map[uuid.UUID]*AssistantState

	pauseCalls  []pauseCall
	resumeCalls []uuid.UUID

	// onCommitErr, when set, is what the audit hook returns — used to prove the
	// pause rolls back with a failed audit append.
	onCommitErr error
}

type pauseCall struct {
	tenantID uuid.UUID
	userID   uuid.UUID
	reason   *string
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{state: map[uuid.UUID]*AssistantState{}}
}

func (f *fakeRepo) Create(context.Context, CreateInput) (Tenant, error) { return Tenant{}, nil }
func (f *fakeRepo) GetForUser(context.Context, uuid.UUID, uuid.UUID) (Tenant, error) {
	return Tenant{}, nil
}
func (f *fakeRepo) ListForUser(context.Context, uuid.UUID, ListInput) ([]Tenant, error) {
	return nil, nil
}
func (f *fakeRepo) GetByID(context.Context, uuid.UUID) (Tenant, error) { return Tenant{}, nil }

func (f *fakeRepo) GetAssistantState(_ context.Context, tenantID, _ uuid.UUID) (AssistantState, error) {
	st, ok := f.state[tenantID]
	if !ok {
		return AssistantState{}, domain.NotFound("tenant_not_found", "tenant not found")
	}
	return *st, nil
}

func (f *fakeRepo) PauseAssistant(_ context.Context, tenantID, userID uuid.UUID, reason *string, onCommit func(tx pgx.Tx) error) (AssistantState, error) {
	f.pauseCalls = append(f.pauseCalls, pauseCall{tenantID: tenantID, userID: userID, reason: reason})
	st, ok := f.state[tenantID]
	if !ok {
		return AssistantState{}, domain.NotFound("tenant_not_found", "tenant not found")
	}
	// The real repo runs onCommit INSIDE the transaction and returns its error,
	// which rolls the UPDATE back. Model that exactly: apply the write only
	// when the hook succeeds.
	if f.onCommitErr != nil {
		_ = onCommit(nil)
		return AssistantState{}, f.onCommitErr
	}
	if err := onCommit(nil); err != nil {
		return AssistantState{}, err
	}
	now := time.Now().UTC()
	st.PausedAt = &now
	st.PausedReason = reason
	// The real repo reads the row back in this same transaction and returns it;
	// the service no longer re-reads. Return the post-write state here too, or
	// every assertion below would be checking a stale zero value.
	return *st, nil
}

func (f *fakeRepo) ResumeAssistant(_ context.Context, tenantID, _ uuid.UUID, onCommit func(tx pgx.Tx) error) (AssistantState, error) {
	f.resumeCalls = append(f.resumeCalls, tenantID)
	st, ok := f.state[tenantID]
	if !ok {
		return AssistantState{}, domain.NotFound("tenant_not_found", "tenant not found")
	}
	// Already released: the real query matches no row, so no audit entry is
	// appended — but the state read still happens and still returns the row.
	if st.PausedAt == nil {
		return *st, nil
	}
	if err := onCommit(nil); err != nil {
		return AssistantState{}, err
	}
	st.PausedAt = nil
	st.PausedReason = nil
	return *st, nil
}

type capturingRecorder struct {
	events []audit.Event
	err    error
}

func (c *capturingRecorder) RecordInTx(_ context.Context, _ pgx.Tx, e audit.Event) (audit.Entry, error) {
	c.events = append(c.events, e)
	return audit.Entry{}, c.err
}

// only returns the single captured event with the given action, failing loudly
// when there is none. A helper that returned a zero Event for "not found" would
// let every assertion below pass vacuously against an empty chain.
func (c *capturingRecorder) only(t *testing.T, action string) audit.Event {
	t.Helper()
	var found []audit.Event
	for _, e := range c.events {
		if e.Action == action {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly 1 %q audit event, got %d (all actions: %v)", action, len(found), c.actions())
	}
	return found[0]
}

func (c *capturingRecorder) actions() []string {
	out := make([]string, 0, len(c.events))
	for _, e := range c.events {
		out = append(out, e.Action)
	}
	return out
}

func newSvc(t *testing.T) (*Service, *fakeRepo, *capturingRecorder) {
	t.Helper()
	repo := newFakeRepo()
	rec := &capturingRecorder{}
	return NewService(repo, domain.NewValidator(), domain.SystemClock{}), repo, rec
}

// apiKeyOwnerOf is a TENANT API KEY holding tenant:manage — a machine, not a
// person. That principal type can reach authz.PermTenantManage, so it can
// engage and release the kill switch.
func apiKeyOwnerOf(tenantID uuid.UUID) domain.Principal {
	return domain.Principal{
		Type:     domain.PrincipalAPIKey,
		TenantID: tenantID,
		APIKeyID: uuid.New(),
		Role:     "owner",
	}
}

func ownerOf(tenantID uuid.UUID) domain.Principal {
	return domain.Principal{
		Type:     domain.PrincipalUser,
		TenantID: tenantID,
		UserID:   uuid.New(),
		Role:     "owner",
	}
}

// --- 1. THE CROSS-TENANT BOUNDARY, ASSERTED FIRST ----------------------------
//
// This is the leak assertion and it is ordered first deliberately. `tenants`
// has NO RLS (m130 DECISION 1), so nothing in the database refuses another
// organisation's row — assertOwnTenant in service.go is the entire boundary. A
// test suite that checked the happy path first and this last would be
// describing a control whose most dangerous failure it had not yet reached.
func TestPauseRefusesAnotherOrganisationsAssistant(t *testing.T) {
	svc, repo, rec := newSvc(t)

	mine, theirs := uuid.New(), uuid.New()
	repo.state[mine] = &AssistantState{}
	repo.state[theirs] = &AssistantState{}

	// An owner of `mine` aims the kill switch at `theirs`.
	_, err := svc.PauseAssistant(context.Background(), ownerOf(mine), theirs, PauseInput{Reason: "not my org"}, rec)

	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindNotFound {
		t.Fatalf("cross-tenant pause was NOT refused as NotFound: err=%v", err)
	}

	// Mutation-test target (M1). The refusal must have stopped the write, not merely
	// changed the status code after it. This is the assertion that names what
	// got through.
	if len(repo.pauseCalls) != 0 {
		t.Fatalf("CROSS-TENANT KILL SWITCH: an owner of org %s reached the repo and paused org %s "+
			"(pause calls: %+v). tenants has no RLS, so nothing downstream would have stopped this.",
			mine, theirs, repo.pauseCalls)
	}
	if repo.state[theirs].Paused() {
		t.Fatalf("CROSS-TENANT KILL SWITCH: org %s is now PAUSED and its owner never asked for it", theirs)
	}
	if len(rec.events) != 0 {
		t.Fatalf("a refused cross-tenant pause still wrote %v to the audit chain", rec.actions())
	}
}

func TestResumeRefusesAnotherOrganisationsAssistant(t *testing.T) {
	svc, repo, rec := newSvc(t)

	mine, theirs := uuid.New(), uuid.New()
	pausedAt := time.Now().UTC()
	reason := "incident 42"
	repo.state[mine] = &AssistantState{}
	repo.state[theirs] = &AssistantState{PausedAt: &pausedAt, PausedReason: &reason}

	_, err := svc.ResumeAssistant(context.Background(), ownerOf(mine), theirs, rec)
	if de, ok := domain.AsDomain(err); !ok || de.Kind != domain.KindNotFound {
		t.Fatalf("cross-tenant resume was NOT refused as NotFound: err=%v", err)
	}
	if len(repo.resumeCalls) != 0 {
		t.Fatalf("CROSS-TENANT RESUME: reached the repo for org %s (calls: %v)", theirs, repo.resumeCalls)
	}
	// The worst outcome: another org's incident stop lifted by a stranger.
	if !repo.state[theirs].Paused() {
		t.Fatalf("CROSS-TENANT RESUME: org %s was UN-PAUSED during another org's incident", theirs)
	}
}

// A principal with no active tenant must not be able to aim at a real one by
// supplying the id in the path. This is the honest case the guard above must
// also cover, and the input that makes a `p.TenantID == uuid.Nil`-blind
// comparison fail.
func TestPauseRefusesPrincipalWithNoActiveTenant(t *testing.T) {
	svc, repo, rec := newSvc(t)
	target := uuid.New()
	repo.state[target] = &AssistantState{}

	p := domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New()} // TenantID zero
	_, err := svc.PauseAssistant(context.Background(), p, target, PauseInput{}, rec)
	if de, ok := domain.AsDomain(err); !ok || de.Kind != domain.KindNotFound {
		t.Fatalf("tenant-less principal was not refused: err=%v", err)
	}
	if len(repo.pauseCalls) != 0 {
		t.Fatalf("tenant-less principal paused org %s", target)
	}
}

// --- 2. THE PAUSE ITSELF -----------------------------------------------------

func TestPauseEngagesTheKillSwitchAndRecordsWhoAndWhy(t *testing.T) {
	svc, repo, rec := newSvc(t)
	org := uuid.New()
	repo.state[org] = &AssistantState{}
	p := ownerOf(org)

	st, err := svc.PauseAssistant(context.Background(), p, org, PauseInput{Reason: "prompt-injection incident"}, rec)
	if err != nil {
		t.Fatalf("pause failed: %v", err)
	}

	// Mutation-test target (M1): the switch is actually engaged.
	if !st.Paused() {
		t.Fatal("PAUSE DID NOT TAKE EFFECT: the returned state reports the assistant is still running")
	}
	if st.PausedReason == nil || *st.PausedReason != "prompt-injection incident" {
		t.Fatalf("pause reason not stored: %v", st.PausedReason)
	}

	// WHO and WHY are on the audit event, not merely in the row.
	ev := rec.only(t, "tenant.assistant.paused")
	if ev.ActorID != p.ActorID() || ev.ActorID == "" {
		t.Fatalf("audit event does not name WHO paused: actor=%q want %q", ev.ActorID, p.ActorID())
	}
	if ev.TenantID != org {
		t.Fatalf("audit event recorded against the wrong org: %s want %s", ev.TenantID, org)
	}
	if got := ev.Metadata["reason"]; got != "prompt-injection incident" {
		t.Fatalf("audit event does not carry WHY: %v", got)
	}
	if got := ev.Metadata["reason_given"]; got != true {
		t.Fatalf("reason_given should be true when a reason was supplied, got %v", got)
	}
}

// The 3am case: stop it now, explain later. A control that refuses to fire
// without a justification string costs seconds it does not have.
func TestPauseWithNoReasonStoresNullNotBlank(t *testing.T) {
	svc, repo, rec := newSvc(t)
	org := uuid.New()
	repo.state[org] = &AssistantState{}

	st, err := svc.PauseAssistant(context.Background(), ownerOf(org), org, PauseInput{Reason: "   "}, rec)
	if err != nil {
		t.Fatalf("pause with a blank reason must succeed, got %v", err)
	}
	if !st.Paused() {
		t.Fatal("pause with no reason did not engage the switch")
	}
	// tenants_assistant_paused_reason_check REFUSES a present-but-blank reason,
	// so a whitespace-only string reaching the query is a 500 at 3am. It must
	// have been normalised to NULL before the write.
	if len(repo.pauseCalls) != 1 {
		t.Fatalf("want 1 pause call, got %d", len(repo.pauseCalls))
	}
	if got := repo.pauseCalls[0].reason; got != nil {
		t.Fatalf("a whitespace-only reason reached the DB as %q; the check constraint "+
			"tenants_assistant_paused_reason_check would have rejected it", *got)
	}
	if ev := rec.only(t, "tenant.assistant.paused"); ev.Metadata["reason_given"] != false {
		t.Fatalf("reason_given should be false when none was given, got %v", ev.Metadata["reason_given"])
	}
}

// FAIL CLOSED. If the audit append fails the pause must fail with it: a kill
// switch nobody can prove was thrown is not a record of an incident action.
func TestPauseRollsBackWhenTheAuditAppendFails(t *testing.T) {
	svc, repo, rec := newSvc(t)
	org := uuid.New()
	repo.state[org] = &AssistantState{}
	rec.err = errors.New("audit chain unavailable")
	repo.onCommitErr = rec.err

	_, err := svc.PauseAssistant(context.Background(), ownerOf(org), org, PauseInput{Reason: "x"}, rec)
	if err == nil {
		t.Fatal("pause SUCCEEDED while its audit append failed; the RecordInTx contract is not fail-closed here")
	}
	if repo.state[org].Paused() {
		t.Fatal("the pause committed even though the audit append failed inside its transaction")
	}
}

// --- 3. RESUME IS A SEPARATE, DELIBERATE ACTION ------------------------------

// The brief's requirement: the same click that paused must not un-pause. There
// is no toggle method on Service, and this test is what keeps it that way —
// calling the pause path twice must leave the surface STOPPED, never flipped
// back on.
func TestPausingTwiceNeverResumes(t *testing.T) {
	svc, repo, rec := newSvc(t)
	org := uuid.New()
	repo.state[org] = &AssistantState{}
	p := ownerOf(org)

	if _, err := svc.PauseAssistant(context.Background(), p, org, PauseInput{Reason: "first"}, rec); err != nil {
		t.Fatalf("first pause: %v", err)
	}
	st, err := svc.PauseAssistant(context.Background(), p, org, PauseInput{Reason: "escalated"}, rec)
	if err != nil {
		t.Fatalf("second pause: %v", err)
	}

	// Mutation-test target (M2).
	if !st.Paused() {
		t.Fatal("TOGGLE: the second pause click RESUMED the assistant. An incident control " +
			"whose repeat press restarts the surface is the exact shape m130 DECISION 2 refuses.")
	}
	if st.PausedReason == nil || *st.PausedReason != "escalated" {
		t.Fatalf("re-engaging should refresh the reason for an escalating incident, got %v", st.PausedReason)
	}
	if len(rec.events) != 2 {
		t.Fatalf("want 2 audit events for 2 pauses, got %v", rec.actions())
	}
}

// Releasing the switch must not enable a surface nobody chose to enable
// (m130 DECISION 2 — the whole reason there are two columns).
func TestResumeDoesNotEnableADisabledOrganisation(t *testing.T) {
	svc, repo, rec := newSvc(t)
	org := uuid.New()
	pausedAt := time.Now().UTC()
	reason := "incident"
	// Deliberately off BEFORE the incident: EnabledAt nil.
	repo.state[org] = &AssistantState{PausedAt: &pausedAt, PausedReason: &reason}

	st, err := svc.ResumeAssistant(context.Background(), ownerOf(org), org, rec)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if st.Paused() {
		t.Fatal("resume did not release the kill switch")
	}
	if st.PausedReason != nil {
		t.Fatalf("resume left a stale reason behind: %q", *st.PausedReason)
	}
	// Mutation-test target (M2).
	if st.EnabledAt != nil {
		t.Fatal("RESUME ENABLED A DISABLED ORG: releasing the kill switch wrote assistant_enabled_at. " +
			"An organisation that was deliberately off before the incident must still be off after it.")
	}
	rec.only(t, "tenant.assistant.resumed")
}

// A resume that released nothing must not write a false incident-end marker
// into the hash chain.
func TestResumeOnARunningOrganisationRecordsNothing(t *testing.T) {
	svc, repo, rec := newSvc(t)
	org := uuid.New()
	repo.state[org] = &AssistantState{} // never paused

	st, err := svc.ResumeAssistant(context.Background(), ownerOf(org), org, rec)
	if err != nil {
		t.Fatalf("resume on a running org should be a successful no-op, got %v", err)
	}
	if st.Paused() {
		t.Fatal("state says paused after resuming a running org")
	}
	if len(rec.events) != 0 {
		t.Fatalf("a resume that released nothing wrote %v into the audit chain", rec.actions())
	}
}

// --- 4. ENABLEMENT IS OUT OF SCOPE, AND THAT IS ASSERTED ---------------------
//
// m130 DECISION 5 holds assistant_enabled_at out of the `authorized` verdict
// until a follow-up MIGRATION adds the predicate together with DECISION 6's
// backfill. Nothing in this package may write it in the meantime. This test is
// the guard against a later change quietly widening the kill switch into an
// enablement control without that migration.
func TestNoAssistantControlWritesEnablement(t *testing.T) {
	svc, repo, rec := newSvc(t)
	org := uuid.New()
	repo.state[org] = &AssistantState{}
	p := ownerOf(org)

	if _, err := svc.PauseAssistant(context.Background(), p, org, PauseInput{Reason: "r"}, rec); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if repo.state[org].EnabledAt != nil {
		t.Fatal("PauseAssistant wrote assistant_enabled_at; enablement needs its own migration first (m130 DECISION 5)")
	}
	if _, err := svc.ResumeAssistant(context.Background(), p, org, rec); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if repo.state[org].EnabledAt != nil {
		t.Fatal("ResumeAssistant wrote assistant_enabled_at; enablement needs its own migration first (m130 DECISION 5)")
	}
}

// --- 5. THE AUDIT ROW MUST NOT SAY A PERSON WHEN A MACHINE ACTED -------------
//
// A tenant API key holding tenant:manage can pause and resume.
// domain.Principal.ActorID returns APIKeyID for that principal type, so pairing
// it with a hard-coded audit.ActorUser writes a row whose actor resolves to
// NEITHER a user NOR an api key. The incident trail then answers "who stopped
// the assistant, and with what credential" with a shrug, at exactly the moment
// someone is asking it.
//
// audit.ActorFor is this codebase's existing answer and both events go through
// it. These assertions exist so an edit back to a literal audit.ActorUser
// cannot pass — which is precisely how the line got written the first time.
func TestPauseAttributesAnAPIKeyToTheAPIKeyActorType(t *testing.T) {
	svc, repo, rec := newSvc(t)
	org := uuid.New()
	repo.state[org] = &AssistantState{}
	p := apiKeyOwnerOf(org)

	if _, err := svc.PauseAssistant(context.Background(), p, org, PauseInput{Reason: "automated stop"}, rec); err != nil {
		t.Fatalf("pause: %v", err)
	}

	ev := rec.only(t, "tenant.assistant.paused")
	if ev.ActorType != audit.ActorAPIKey {
		t.Fatalf("THE AUDIT ROW SAYS A PERSON ACTED: actor_type=%q for an API-key principal, want %q. "+
			"An incident reader cannot tell which credential stopped the assistant.",
			ev.ActorType, audit.ActorAPIKey)
	}
	// The id must be the KEY's id, so actor resolution has something to match.
	if ev.ActorID != p.APIKeyID.String() {
		t.Fatalf("actor_id=%q, want the api key id %q", ev.ActorID, p.APIKeyID)
	}
}

func TestResumeAttributesAnAPIKeyToTheAPIKeyActorType(t *testing.T) {
	svc, repo, rec := newSvc(t)
	org := uuid.New()
	pausedAt := time.Now().UTC()
	reason := "incident"
	repo.state[org] = &AssistantState{PausedAt: &pausedAt, PausedReason: &reason}
	p := apiKeyOwnerOf(org)

	if _, err := svc.ResumeAssistant(context.Background(), p, org, rec); err != nil {
		t.Fatalf("resume: %v", err)
	}

	ev := rec.only(t, "tenant.assistant.resumed")
	if ev.ActorType != audit.ActorAPIKey {
		t.Fatalf("THE AUDIT ROW SAYS A PERSON ACTED: actor_type=%q for an API-key principal, want %q. "+
			"Releasing an incident stop is as much a which-credential question as engaging one.",
			ev.ActorType, audit.ActorAPIKey)
	}
	if ev.ActorID != p.APIKeyID.String() {
		t.Fatalf("actor_id=%q, want the api key id %q", ev.ActorID, p.APIKeyID)
	}
}

// The other arm, so the fix cannot be "always api_key". A human owner must
// still record as a user: an assertion that passes for every principal type is
// not an assertion.
func TestPauseAndResumeStillAttributeAUserToTheUserActorType(t *testing.T) {
	svc, repo, rec := newSvc(t)
	org := uuid.New()
	repo.state[org] = &AssistantState{}
	p := ownerOf(org)

	if _, err := svc.PauseAssistant(context.Background(), p, org, PauseInput{Reason: "human stop"}, rec); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if ev := rec.only(t, "tenant.assistant.paused"); ev.ActorType != audit.ActorUser {
		t.Fatalf("a human owner recorded as actor_type=%q, want %q", ev.ActorType, audit.ActorUser)
	}
	if _, err := svc.ResumeAssistant(context.Background(), p, org, rec); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if ev := rec.only(t, "tenant.assistant.resumed"); ev.ActorType != audit.ActorUser {
		t.Fatalf("a human owner recorded as actor_type=%q, want %q", ev.ActorType, audit.ActorUser)
	}
}
