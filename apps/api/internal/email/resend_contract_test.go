package email

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

// ---------------------------------------------------------------------------
// GH #520 — the resend_email wire contract
// ---------------------------------------------------------------------------
//
// This file exists because nothing asserted the payload the CP actually POSTs
// for a resend. The Go fake declared
//
//	ResendEmail(_, _, _, _ agentcmd.ResendEmailRequest)
//
// and threw the request away with a blank identifier, while the agent's PHPUnit
// test hand-built ['agent_seq' => N]. Each half tested its own idea of the
// contract and nothing tested that the two agreed — so the CP shipped sending
// log_id, the agent shipped requiring agent_seq, and every Resend on every site
// failed with "missing required field: agent_seq" from commit 3f8e5765 onward.
//
// The assertion below is the whole point: the exact JSON object the agent
// receives. It is compared as a key set plus values, not field-by-field, so an
// EXTRA field is a failure too — an unread field on the command channel is how
// stored message bodies would leak back onto the wire.
//
// The agent's reader is apps/agent/includes/commands/class-resend-email-command.php:
// execute() rejects a params array without agent_seq before anything else runs.

// wantResendKeys is the exact set of JSON keys the agent's resend_email handler
// reads. Keep this in lockstep with class-resend-email-command.php::execute().
var wantResendKeys = []string{"agent_seq"}

// fakeResendAgent captures the ResendEmailRequest the service dispatches.
type fakeResendAgent struct {
	calls    int
	lastReq  agentcmd.ResendEmailRequest
	result   agentcmd.ResendEmailResult
	resulErr error
}

func (f *fakeResendAgent) SyncEmailConfig(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.EmailConfigRequest) (agentcmd.EmailConfigResult, error) {
	return agentcmd.EmailConfigResult{OK: true}, nil
}

func (f *fakeResendAgent) SendTestEmail(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.SendTestEmailRequest) (agentcmd.SendTestEmailResult, error) {
	return agentcmd.SendTestEmailResult{OK: true}, nil
}

func (f *fakeResendAgent) ResendEmail(_ context.Context, _ uuid.UUID, _ string, req agentcmd.ResendEmailRequest) (agentcmd.ResendEmailResult, error) {
	f.calls++
	f.lastReq = req
	if f.resulErr != nil {
		return agentcmd.ResendEmailResult{OK: false, Detail: f.resulErr.Error()}, f.resulErr
	}
	return f.result, nil
}

// fakeResendRepo serves log rows by id and counts resent_count writes.
type fakeResendRepo struct {
	*fakeRepo
	rows      map[uuid.UUID]ResendTarget
	incrCalls int
	incrIDs   []uuid.UUID
}

func newFakeResendRepo(logID uuid.UUID, agentSeq int64) *fakeResendRepo {
	r := &fakeResendRepo{fakeRepo: newFakeRepo(), rows: map[uuid.UUID]ResendTarget{}}
	r.addRow(logID, agentSeq, true)
	return r
}

func (r *fakeResendRepo) addRow(id uuid.UUID, agentSeq int64, bodyStored bool) {
	t := ResendTarget{BodyStored: bodyStored}
	if agentSeq != 0 {
		seq := agentSeq
		t.AgentSeq = &seq
	}
	r.rows[id] = t
}

func (r *fakeResendRepo) GetResendTarget(_ context.Context, _, _, id uuid.UUID) (ResendTarget, error) {
	t, ok := r.rows[id]
	if !ok {
		return ResendTarget{}, ErrNotFound
	}
	return t, nil
}

func (r *fakeResendRepo) IncrEmailLogResentCount(_ context.Context, _, _, id uuid.UUID) error {
	if _, ok := r.rows[id]; !ok {
		return ErrNotFound
	}
	r.incrCalls++
	r.incrIDs = append(r.incrIDs, id)
	return nil
}

// newResendSvc wires a service over the two fakes above.
func newResendSvc(repo repository, agent AgentEmailClient) *Service {
	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo
	svc.SetAgentClient(agent, &fakeSiteLookup{})
	return svc
}

// TestResendEmail_DispatchedPayloadMatchesAgentContract is the regression test
// for GH #520. It asserts the literal JSON the agent would receive.
func TestResendEmail_DispatchedPayloadMatchesAgentContract(t *testing.T) {
	tenantID, siteID, logID := uuid.New(), uuid.New(), uuid.New()
	const agentSeq int64 = 4242

	repo := newFakeResendRepo(logID, agentSeq)
	agent := &fakeResendAgent{result: agentcmd.ResendEmailResult{OK: true, Detail: "resent", MessageID: "<m@site>"}}
	svc := newResendSvc(repo, agent)

	res, err := svc.ResendEmail(context.Background(), tenantID, siteID, logID)
	if err != nil {
		t.Fatalf("ResendEmail: unexpected error: %v", err)
	}
	if !res.OK {
		t.Fatalf("ResendEmail: expected ok=true, got ok=false detail=%q", res.Detail)
	}
	if agent.calls != 1 {
		t.Fatalf("expected exactly 1 agent dispatch, got %d", agent.calls)
	}

	raw, err := json.Marshal(agent.lastReq)
	if err != nil {
		t.Fatalf("marshal dispatched request: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal dispatched request: %v", err)
	}

	// Exact key set — a missing key is the #520 bug, an extra key puts data on
	// the command channel that the agent never reads.
	if len(got) != len(wantResendKeys) {
		t.Errorf("dispatched payload has %d field(s), want %d\n  got:  %s\n  want keys: %v",
			len(got), len(wantResendKeys), raw, wantResendKeys)
	}
	for _, k := range wantResendKeys {
		if _, ok := got[k]; !ok {
			t.Errorf("dispatched payload is missing %q — the agent rejects this request\n  got: %s", k, raw)
		}
	}
	// agent_seq must be the row's own local id, as a JSON number.
	if n, ok := got["agent_seq"].(float64); !ok || int64(n) != agentSeq {
		t.Errorf("agent_seq = %v, want %d\n  got: %s", got["agent_seq"], agentSeq, raw)
	}
}

// ---------------------------------------------------------------------------
// Accounting: resent_count and the audit row move on SUCCESS only
// ---------------------------------------------------------------------------

func TestResendEmail_Success_IncrementsCountOnce(t *testing.T) {
	tenantID, siteID, logID := uuid.New(), uuid.New(), uuid.New()
	repo := newFakeResendRepo(logID, 7)
	agent := &fakeResendAgent{result: agentcmd.ResendEmailResult{OK: true, Detail: "resent", MessageID: "<m@site>"}}
	svc := newResendSvc(repo, agent)

	res, err := svc.ResendEmail(context.Background(), tenantID, siteID, logID)
	if err != nil {
		t.Fatalf("ResendEmail: unexpected error: %v", err)
	}
	if !res.OK || res.MessageID != "<m@site>" {
		t.Fatalf("expected ok=true with the agent's message id, got ok=%v id=%q", res.OK, res.MessageID)
	}
	if repo.incrCalls != 1 {
		t.Errorf("resent_count incremented %d time(s), want 1", repo.incrCalls)
	}
	if meta, ok := resendAuditMeta(logID, res); !ok {
		t.Error("a confirmed resend must be audited")
	} else if meta["message_id"] != "<m@site>" {
		t.Errorf("audit metadata message_id = %v, want <m@site>", meta["message_id"])
	}
}

func TestResendEmail_AgentRefuses_NoCountNoAudit(t *testing.T) {
	tenantID, siteID, logID := uuid.New(), uuid.New(), uuid.New()
	repo := newFakeResendRepo(logID, 7)
	agent := &fakeResendAgent{result: agentcmd.ResendEmailResult{OK: false, Detail: agentcmd.ResendDetailRowNotFound}}
	svc := newResendSvc(repo, agent)

	res, err := svc.ResendEmail(context.Background(), tenantID, siteID, logID)
	if err != nil {
		t.Fatalf("ResendEmail: unexpected error: %v", err)
	}
	if res.OK {
		t.Fatal("expected ok=false when the agent refuses")
	}
	if repo.incrCalls != 0 {
		t.Errorf("resent_count incremented %d time(s) on a refused resend, want 0", repo.incrCalls)
	}
	if _, ok := resendAuditMeta(logID, res); ok {
		t.Error("a refused resend must not be audited as email.resent")
	}
	// The raw contract string must not reach the operator.
	if strings.Contains(res.Detail, agentcmd.ResendDetailRowNotFound) {
		t.Errorf("raw agent detail leaked to the caller: %q", res.Detail)
	}
	if !strings.Contains(res.Detail, "14 days") {
		t.Errorf("expected the retention explanation in the detail, got %q", res.Detail)
	}
}

func TestResendEmail_TransportError_NoCount(t *testing.T) {
	tenantID, siteID, logID := uuid.New(), uuid.New(), uuid.New()
	repo := newFakeResendRepo(logID, 7)
	agent := &fakeResendAgent{resulErr: errors.New("resend_email command rejected by agent: status 404 body=no route")}
	svc := newResendSvc(repo, agent)

	res, err := svc.ResendEmail(context.Background(), tenantID, siteID, logID)
	if err != nil {
		t.Fatalf("ResendEmail: unexpected error: %v", err)
	}
	if res.OK {
		t.Fatal("expected ok=false on a transport failure")
	}
	if repo.incrCalls != 0 {
		t.Errorf("resent_count incremented %d time(s) on a transport failure, want 0", repo.incrCalls)
	}
	if !strings.Contains(res.Detail, "too old") {
		t.Errorf("expected the stale-plugin message for a 404, got %q", res.Detail)
	}
}

// ---------------------------------------------------------------------------
// Preconditions: refuse near-end rather than spend a signed command
// ---------------------------------------------------------------------------

func TestResendEmail_NoAgentSeq_RefusedBeforeDispatch(t *testing.T) {
	tenantID, siteID, logID := uuid.New(), uuid.New(), uuid.New()
	repo := newFakeResendRepo(logID, 0) // body captured, but no agent_seq
	agent := &fakeResendAgent{result: agentcmd.ResendEmailResult{OK: true}}
	svc := newResendSvc(repo, agent)

	_, err := svc.ResendEmail(context.Background(), tenantID, siteID, logID)
	if err == nil {
		t.Fatal("expected a refusal when the row carries no agent_seq")
	}
	if !containsCode(err, "resend_agent_seq_missing") {
		t.Errorf("expected code 'resend_agent_seq_missing', got: %v", err)
	}
	if agent.calls != 0 {
		t.Errorf("a signed command was dispatched for an unaddressable row (%d call(s))", agent.calls)
	}
	if repo.incrCalls != 0 {
		t.Errorf("resent_count incremented %d time(s) on a refused precondition, want 0", repo.incrCalls)
	}
}

func TestResendEmail_BodyNotStored_RefusedBeforeDispatch(t *testing.T) {
	tenantID, siteID, logID := uuid.New(), uuid.New(), uuid.New()
	repo := &fakeResendRepo{fakeRepo: newFakeRepo(), rows: map[uuid.UUID]ResendTarget{}}
	repo.addRow(logID, 7, false) // agent_seq known, body never captured
	agent := &fakeResendAgent{result: agentcmd.ResendEmailResult{OK: true}}
	svc := newResendSvc(repo, agent)

	_, err := svc.ResendEmail(context.Background(), tenantID, siteID, logID)
	if err == nil {
		t.Fatal("expected a refusal when the body was not captured")
	}
	if !containsCode(err, "resend_body_not_stored") {
		t.Errorf("expected code 'resend_body_not_stored', got: %v", err)
	}
	if agent.calls != 0 {
		t.Errorf("a signed command was dispatched for a bodiless row (%d call(s))", agent.calls)
	}
}

// ---------------------------------------------------------------------------
// Bulk: per-entry outcomes, and one honest audit row
// ---------------------------------------------------------------------------

// TestBulkResendEmail_MixedOutcomes drives one batch containing an entry that
// resends, an entry the site refuses, an entry with no agent_seq and an entry
// that does not exist, and asserts that only the successful one is counted.
func TestBulkResendEmail_MixedOutcomes(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	okID, refusedID, noSeqID, missingID := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	repo := &fakeResendRepo{fakeRepo: newFakeRepo(), rows: map[uuid.UUID]ResendTarget{}}
	repo.addRow(okID, 11, true)
	repo.addRow(refusedID, 12, true)
	repo.addRow(noSeqID, 0, true)

	// The agent resends the first row and refuses the second.
	agent := &perSeqResendAgent{results: map[int64]agentcmd.ResendEmailResult{
		11: {OK: true, Detail: "resent", MessageID: "<a@site>"},
		12: {OK: false, Detail: agentcmd.ResendDetailBodyNotStored},
	}}
	svc := newResendSvc(repo, agent)

	ids := []uuid.UUID{okID, refusedID, noSeqID, missingID}
	results, err := svc.BulkResendEmail(context.Background(), tenantID, siteID, ids)
	if err != nil {
		t.Fatalf("BulkResendEmail: unexpected error: %v", err)
	}
	if len(results) != len(ids) {
		t.Fatalf("got %d results for %d ids", len(results), len(ids))
	}

	wantOK := map[uuid.UUID]bool{okID: true, refusedID: false, noSeqID: false, missingID: false}
	for _, r := range results {
		want, known := wantOK[r.LogID]
		if !known {
			t.Errorf("log %s: result carries an id that was never requested", r.LogID)
			continue
		}
		if r.OK != want {
			t.Errorf("log %s: ok=%v, want %v (detail %q)", r.LogID, r.OK, want, r.Detail)
		}
	}

	// Only the two addressable rows may spend a signed command.
	if agent.calls != 2 {
		t.Errorf("agent dispatched %d command(s), want 2 (the unaddressable and missing rows must be refused near-end)", agent.calls)
	}
	// Each addressable row must be dispatched with its own agent_seq, in
	// request order — proof that the bulk path addresses rows individually
	// rather than resending one id (or one seq) for the whole batch.
	wantSeqs := []int64{11, 12}
	seqsMatch := len(agent.seqs) == len(wantSeqs)
	if seqsMatch {
		for i, want := range wantSeqs {
			if agent.seqs[i] != want {
				seqsMatch = false
				break
			}
		}
	}
	if !seqsMatch {
		t.Errorf("agent saw agent_seq sequence %v, want %v (each row must be addressed individually)", agent.seqs, wantSeqs)
	}
	if repo.incrCalls != 1 {
		t.Errorf("resent_count incremented %d time(s), want 1", repo.incrCalls)
	}
	if len(repo.incrIDs) != 1 || repo.incrIDs[0] != okID {
		t.Errorf("resent_count moved on the wrong rows: %v, want [%s]", repo.incrIDs, okID)
	}

	meta, ok := bulkResendAuditMeta(len(ids), results)
	if !ok {
		t.Fatal("a batch with one confirmed resend must be audited")
	}
	if meta["count"] != 1 {
		t.Errorf("audit count = %v, want 1 (the number resent, not the number requested)", meta["count"])
	}
	if meta["requested"] != len(ids) {
		t.Errorf("audit requested = %v, want %d", meta["requested"], len(ids))
	}
}

// TestBulkResendEmail_AllFail_NoAuditRow is the N-commands-zero-emails case:
// the batch used to write one audit row reading {"count": N}.
func TestBulkResendEmail_AllFail_NoAuditRow(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	a, b := uuid.New(), uuid.New()

	repo := &fakeResendRepo{fakeRepo: newFakeRepo(), rows: map[uuid.UUID]ResendTarget{}}
	repo.addRow(a, 21, true)
	repo.addRow(b, 22, true)
	agent := &fakeResendAgent{result: agentcmd.ResendEmailResult{OK: false, Detail: agentcmd.ResendDetailRowNotFound}}
	svc := newResendSvc(repo, agent)

	results, err := svc.BulkResendEmail(context.Background(), tenantID, siteID, []uuid.UUID{a, b})
	if err != nil {
		t.Fatalf("BulkResendEmail: unexpected error: %v", err)
	}
	if repo.incrCalls != 0 {
		t.Errorf("resent_count incremented %d time(s) for a batch that resent nothing, want 0", repo.incrCalls)
	}
	if _, ok := bulkResendAuditMeta(2, results); ok {
		t.Error("a batch that resent nothing must not write an email.resent audit row")
	}
}

// perSeqResendAgent answers per agent_seq, so a bulk test can prove the CP
// addressed each row individually rather than sending one id repeatedly.
type perSeqResendAgent struct {
	calls   int
	seqs    []int64
	results map[int64]agentcmd.ResendEmailResult
}

func (f *perSeqResendAgent) SyncEmailConfig(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.EmailConfigRequest) (agentcmd.EmailConfigResult, error) {
	return agentcmd.EmailConfigResult{OK: true}, nil
}

func (f *perSeqResendAgent) SendTestEmail(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.SendTestEmailRequest) (agentcmd.SendTestEmailResult, error) {
	return agentcmd.SendTestEmailResult{OK: true}, nil
}

func (f *perSeqResendAgent) ResendEmail(_ context.Context, _ uuid.UUID, _ string, req agentcmd.ResendEmailRequest) (agentcmd.ResendEmailResult, error) {
	f.calls++
	f.seqs = append(f.seqs, req.AgentSeq)
	res, ok := f.results[req.AgentSeq]
	if !ok {
		return agentcmd.ResendEmailResult{OK: false, Detail: agentcmd.ResendDetailRowNotFound}, nil
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// Operator-facing wording for the agent's contract strings
// ---------------------------------------------------------------------------

func TestResendFailureMessage(t *testing.T) {
	cases := []struct {
		name    string
		detail  string
		want    string // substring
		wantRaw bool   // detail is returned unchanged
	}{
		{name: "row pruned", detail: agentcmd.ResendDetailRowNotFound, want: "no longer in the site's own email log"},
		{name: "no body", detail: agentcmd.ResendDetailBodyNotStored, want: "did not keep a copy"},
		{name: "unconfigured", detail: "no email config, run sync_email_config first", want: "no email configuration yet"},
		{name: "the #520 string", detail: agentcmd.ResendDetailMissingSeq, want: "update the wpmgr plugin"},
		{name: "bad seq", detail: agentcmd.ResendDetailBadSeq, want: "update the wpmgr plugin"},
		{name: "old plugin", detail: "resend_email command rejected by agent: status 404 body=x", want: "too old"},
		{name: "silent refusal", detail: "", want: "without giving a reason"},
		{
			name:    "provider error passes through",
			detail:  "SMTP error: 550 5.7.1 relay access denied",
			want:    "550 5.7.1 relay access denied",
			wantRaw: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resendFailureMessage(tc.detail)
			if !strings.Contains(got, tc.want) {
				t.Errorf("resendFailureMessage(%q) = %q, want it to contain %q", tc.detail, got, tc.want)
			}
			if tc.wantRaw && got != tc.detail {
				t.Errorf("an unrecognised detail must pass through unchanged: got %q, want %q", got, tc.detail)
			}
			// A mapped message must never be one of the agent's contract codes.
			if !tc.wantRaw && got == tc.detail {
				t.Errorf("detail %q reached the operator unmapped", tc.detail)
			}
		})
	}
}
