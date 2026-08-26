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

// GH #528 — agent_seq alone is not a safe selector.
//
// agent_seq is a MySQL AUTO_INCREMENT on the site's own table. A database
// restore rolls it back, later sends re-use ids the CP has already bound to
// other messages, and the operator's "Resend Alice's invoice" becomes "send
// Bob's password reset to Bob". The CP now sends the recorded Message-ID
// alongside it so the agent can confirm the row before it sends.
//
// The payload therefore has TWO shapes, and both are pinned below:
//
//	verified   {"agent_seq": N, "message_id": "<...>"}  — CP had a Message-ID
//	unverified {"agent_seq": N}                          — CP had none
//
// The unverified shape is not a degenerate case to wave through. message_id is
// NULL on the CP for every send that failed at the time, which is the most
// common thing anyone resends, so this shape is the common one and it must stay
// exactly one key: an empty "message_id" on the wire would read to the agent as
// "compare against empty" and refuse every such row.

// wantResendKeysVerified is the exact set of JSON keys the agent's resend_email
// handler reads when the CP has a Message-ID to confirm against.
// Keep this in lockstep with class-resend-email-command.php::execute().
var wantResendKeysVerified = []string{"agent_seq", "message_id"}

// wantResendKeysUnverified is the exact set when the CP has no Message-ID.
// message_id must be ABSENT, not empty.
var wantResendKeysUnverified = []string{"agent_seq"}

// assertResendPayload marshals the request the service actually dispatched and
// asserts it against an exact key set and exact values.
//
// Exact in all four directions, which is the whole point of this file:
//   - a MISSING key fails the per-key loop (the #520 bug),
//   - an EXTRA key fails the length check (data on the command channel the
//     agent never reads — how message bodies would leak back onto the wire),
//   - a RENAMED key fails both at once,
//   - a WRONG VALUE fails the value comparison.
func assertResendPayload(t *testing.T, req agentcmd.ResendEmailRequest, wantKeys []string, wantValues map[string]any) {
	t.Helper()

	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal dispatched request: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal dispatched request: %v", err)
	}

	if len(got) != len(wantKeys) {
		t.Errorf("dispatched payload has %d field(s), want %d\n  got:  %s\n  want keys: %v",
			len(got), len(wantKeys), raw, wantKeys)
	}
	for _, k := range wantKeys {
		if _, ok := got[k]; !ok {
			t.Errorf("dispatched payload is missing %q — the agent cannot act on this request\n  got: %s", k, raw)
		}
	}
	for k, want := range wantValues {
		switch w := want.(type) {
		case int64:
			n, ok := got[k].(float64)
			if !ok || int64(n) != w {
				t.Errorf("%s = %v, want %d\n  got: %s", k, got[k], w, raw)
			}
		case string:
			s, ok := got[k].(string)
			if !ok || s != w {
				t.Errorf("%s = %v, want %q\n  got: %s", k, got[k], w, raw)
			}
		default:
			t.Fatalf("assertResendPayload: unsupported want type for %q", k)
		}
	}
}

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

// addRow adds a row with NO recorded Message-ID — the #528 unverified shape,
// and the shape every row of a failed send has.
func (r *fakeResendRepo) addRow(id uuid.UUID, agentSeq int64, bodyStored bool) {
	t := ResendTarget{BodyStored: bodyStored}
	if agentSeq != 0 {
		seq := agentSeq
		t.AgentSeq = &seq
	}
	r.rows[id] = t
}

// addRowWithMessageID adds a row that DOES carry a recorded Message-ID, so the
// dispatch can be confirmed by the agent.
func (r *fakeResendRepo) addRowWithMessageID(id uuid.UUID, agentSeq int64, messageID string) {
	r.addRow(id, agentSeq, true)
	t := r.rows[id]
	m := messageID
	t.MessageID = &m
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
// for GH #520, extended for GH #528. It asserts the literal JSON the agent
// would receive, in both the verified and the unverified shape.
func TestResendEmail_DispatchedPayloadMatchesAgentContract(t *testing.T) {
	tenantID, siteID, logID := uuid.New(), uuid.New(), uuid.New()
	const agentSeq int64 = 4242
	const storedMsgID = "<stored-4242@site.example>"

	repo := &fakeResendRepo{fakeRepo: newFakeRepo(), rows: map[uuid.UUID]ResendTarget{}}
	repo.addRowWithMessageID(logID, agentSeq, storedMsgID)
	// A CURRENT agent (PR #541): it compared the supplied message_id against
	// its own row, they matched, and it says so.
	agent := &fakeResendAgent{result: mustDecodeResendResult(t,
		`{"ok":true,"detail":"resent","message_id":"<new@site>","verified":true}`)}
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

	// message_id must be the CP's RECORDED id for the row (the selector the
	// agent compares against), never the id the agent returns for the new send.
	assertResendPayload(t, agent.lastReq, wantResendKeysVerified, map[string]any{
		"agent_seq":  agentSeq,
		"message_id": storedMsgID,
	})

	// OVER-FIRE GUARD: a site that DID do the comparison and says so must still
	// come back verified, with no warning bolted onto its detail. A change that
	// makes every resend read "unverified" would be as useless as one that makes
	// every resend read "verified".
	if !res.Verified {
		t.Error("an agent that attested verified=true must report Verified=true")
	}
	if strings.Contains(res.Detail, "could not confirm") {
		t.Errorf("a verified resend must not carry the unverified note: %q", res.Detail)
	}
	if meta, ok := resendAuditMeta(logID, res); !ok {
		t.Error("a confirmed resend must be audited")
	} else if meta["verified"] != true {
		t.Errorf("audit metadata verified = %v, want true", meta["verified"])
	} else if meta["legacy_agent"] != false {
		t.Errorf("audit metadata legacy_agent = %v, want false", meta["legacy_agent"])
	}
}

// TestResendEmail_AgentSaysUnverified_IsNotVerified covers the third wire
// state: a current agent that answers the question with "no". It sent the mail
// but did not confirm the row, so the CP must report exactly that — the
// attestation is read, not assumed from the fact that the agent is new enough
// to carry the field.
func TestResendEmail_AgentSaysUnverified_IsNotVerified(t *testing.T) {
	tenantID, siteID, logID := uuid.New(), uuid.New(), uuid.New()

	repo := &fakeResendRepo{fakeRepo: newFakeRepo(), rows: map[uuid.UUID]ResendTarget{}}
	repo.addRowWithMessageID(logID, 77, "<stored-77@site.example>")
	agent := &fakeResendAgent{result: mustDecodeResendResult(t,
		`{"ok":true,"detail":"resent","message_id":"<new@site>","verified":false}`)}
	svc := newResendSvc(repo, agent)

	res, err := svc.ResendEmail(context.Background(), tenantID, siteID, logID)
	if err != nil {
		t.Fatalf("ResendEmail: unexpected error: %v", err)
	}
	if !res.OK {
		t.Fatalf("the send succeeded, so ok must stay true; got detail=%q", res.Detail)
	}
	if res.Verified {
		t.Error("the agent said verified=false; the CP must not report a verified resend")
	}
	if !strings.Contains(res.Detail, "could not confirm") {
		t.Errorf("an unconfirmed resend must say so to the operator, got %q", res.Detail)
	}
	// PR #542 review: an EXPLICIT `verified:false` is a current agent saying it
	// ran and had nothing to compare. That is never a plugin problem, so the
	// note must never send this operator to update anything — even though the
	// CP DID have a Message-ID to send (askedForCheck=true), which is exactly
	// the case the old askedForCheck-keyed wording got wrong.
	if strings.Contains(res.Detail, "too old") || strings.Contains(res.Detail, "Update the plugin") {
		t.Errorf("a current agent's explicit verified=false is not a plugin problem, got %q", res.Detail)
	}
	if meta, ok := resendAuditMeta(logID, res); !ok {
		t.Error("a completed send must still be audited when unverified")
	} else if meta["legacy_agent"] != false {
		t.Errorf("audit metadata legacy_agent = %v, want false (the agent answered explicitly)", meta["legacy_agent"])
	}
}

// TestResendEmail_UnaskedAttestation_FailsClosed is the other direction of the
// same rule. The CP had no Message-ID, so it sent no message_id key and no
// comparison against its record was possible; an agent that claims verified
// anyway is violating the contract, and a claim we never asked for is not
// evidence. Believing it would be the original defect wearing the fix.
func TestResendEmail_UnaskedAttestation_FailsClosed(t *testing.T) {
	tenantID, siteID, logID := uuid.New(), uuid.New(), uuid.New()

	repo := newFakeResendRepo(logID, 88) // no recorded Message-ID
	agent := &fakeResendAgent{result: mustDecodeResendResult(t,
		`{"ok":true,"detail":"resent","message_id":"<new@site>","verified":true}`)}
	svc := newResendSvc(repo, agent)

	res, err := svc.ResendEmail(context.Background(), tenantID, siteID, logID)
	if err != nil {
		t.Fatalf("ResendEmail: unexpected error: %v", err)
	}
	if agent.lastReq.MessageID != "" {
		t.Fatalf("precondition: this row has no Message-ID, so none may be sent; got %q", agent.lastReq.MessageID)
	}
	if res.Verified {
		t.Error("the CP supplied nothing to compare against, so no attestation can be believed")
	}
	if !strings.Contains(res.Detail, "no provider message ID was recorded") {
		t.Errorf("the operator must be told the real cause, got %q", res.Detail)
	}
}

// TestResendEmail_NoMessageID_OmitsKeyAndReportsUnverified pins the other half
// of the #528 decision: a row with no recorded Message-ID is still resent, the
// key is ABSENT rather than empty, and the operator is told it was not
// confirmed. The forbidden outcome is sending unconfirmed and saying nothing.
func TestResendEmail_NoMessageID_OmitsKeyAndReportsUnverified(t *testing.T) {
	tenantID, siteID, logID := uuid.New(), uuid.New(), uuid.New()
	const agentSeq int64 = 99

	repo := newFakeResendRepo(logID, agentSeq) // no message_id — a failed send
	// A current agent, asked for no comparison, answers the question honestly.
	agent := &fakeResendAgent{result: mustDecodeResendResult(t,
		`{"ok":true,"detail":"resent","message_id":"<new@site>","verified":false}`)}
	svc := newResendSvc(repo, agent)

	res, err := svc.ResendEmail(context.Background(), tenantID, siteID, logID)
	if err != nil {
		t.Fatalf("ResendEmail: unexpected error: %v", err)
	}
	if !res.OK {
		t.Fatalf("a row without a Message-ID must still resend, got ok=false detail=%q", res.Detail)
	}
	if agent.calls != 1 {
		t.Fatalf("expected exactly 1 agent dispatch, got %d", agent.calls)
	}

	// Exactly one key. An empty "message_id" would make the agent refuse.
	assertResendPayload(t, agent.lastReq, wantResendKeysUnverified, map[string]any{
		"agent_seq": agentSeq,
	})

	if res.Verified {
		t.Error("a dispatch with no message_id must report Verified=false")
	}
	if !strings.Contains(res.Detail, "could not confirm") {
		t.Errorf("an unverified resend must say so to the operator, got %q", res.Detail)
	}
	// And with the RIGHT cause. Nothing here is fixable by updating the plugin,
	// so the note must not send the operator after one.
	if !strings.Contains(res.Detail, "no provider message ID was recorded") {
		t.Errorf("expected the no-recorded-id cause, got %q", res.Detail)
	}
	if strings.Contains(res.Detail, "too old") {
		t.Errorf("this site's plugin is not the problem; the note misdirects: %q", res.Detail)
	}
	// PR #542 review: this state must say plainly that there is nothing to fix.
	if !strings.Contains(res.Detail, "nothing to fix") {
		t.Errorf("an explicit verified=false with no prior Message-ID must tell the operator there is nothing to fix, got %q", res.Detail)
	}

	// The audit row must record it too, or the log cannot tell the two apart.
	meta, ok := resendAuditMeta(logID, res)
	if !ok {
		t.Fatal("a confirmed send must still be audited even when unverified")
	}
	if meta["verified"] != false {
		t.Errorf("audit metadata verified = %v, want false", meta["verified"])
	}
	// This is a current agent answering honestly, not one too old to attest.
	if meta["legacy_agent"] != false {
		t.Errorf("audit metadata legacy_agent = %v, want false", meta["legacy_agent"])
	}
}

// mustDecodeResendResult builds an agent response by DECODING the literal bytes
// a site would put on the wire, never by filling in the Go struct by hand.
//
// That distinction is the whole point of the tests below. "The agent did not
// send a `verified` key" is a property of the bytes; a hand-built
// agentcmd.ResendEmailResult{} would express it as a Go zero value chosen by
// the test author, which is exactly the reasoning that produced the defect.
func mustDecodeResendResult(t *testing.T, body string) agentcmd.ResendEmailResult {
	t.Helper()
	var out agentcmd.ResendEmailResult
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode agent response %s: %v", body, err)
	}
	return out
}

// ---------------------------------------------------------------------------
// GH #528, PR #542 review — verification is the AGENT's attestation
// ---------------------------------------------------------------------------
//
// The first cut of this fix set Verified from whether the CP had SENT
// message_id, not from whether the site had CHECKED it. A site running a
// compatible older agent reads only agent_seq, ignores the new key, resends
// whatever now sits at that row, and answers ok=true. The CP then recorded a
// verified resend, suppressed the unverified warning and wrote a verified audit
// row, for a comparison that never happened — the same "claiming a check
// happened when it did not" defect this whole issue is about, sitting inside
// the fix for it.
//
// The contract, agreed with the agent half in PR #541: the response carries
// `verified`, set true ONLY on a path where the agent compared a supplied
// message_id against its own row and they matched. Absent means false.

// TestResendEmail_LegacyAgentSilence_IsNotVerified is the regression test for
// that defect. The CP has a Message-ID and sends it; the agent is old enough to
// ignore the key and answers without a `verified` field. Silence is not
// confirmation.
func TestResendEmail_LegacyAgentSilence_IsNotVerified(t *testing.T) {
	tenantID, siteID, logID := uuid.New(), uuid.New(), uuid.New()
	const agentSeq int64 = 4242
	const storedMsgID = "<stored-4242@site.example>"

	repo := &fakeResendRepo{fakeRepo: newFakeRepo(), rows: map[uuid.UUID]ResendTarget{}}
	repo.addRowWithMessageID(logID, agentSeq, storedMsgID)

	// A COMPATIBLE OLDER AGENT: it routes resend_email, reads only agent_seq,
	// never looks at message_id, and returns no `verified` key at all.
	agent := &fakeResendAgent{result: mustDecodeResendResult(t,
		`{"ok":true,"detail":"resent","message_id":"<new@site>"}`)}
	svc := newResendSvc(repo, agent)

	res, err := svc.ResendEmail(context.Background(), tenantID, siteID, logID)
	if err != nil {
		t.Fatalf("ResendEmail: unexpected error: %v", err)
	}
	if !res.OK {
		t.Fatalf("the send itself succeeded, so ok must stay true; got detail=%q", res.Detail)
	}
	// The CP did ask for the comparison — that is what makes the silence
	// meaningful rather than absent.
	if agent.lastReq.MessageID != storedMsgID {
		t.Fatalf("the CP must still send message_id; got %q", agent.lastReq.MessageID)
	}

	if res.Verified {
		t.Error("an agent that returned no `verified` field never compared anything; " +
			"reporting the resend as verified tells the operator a check happened when none did")
	}
	if !strings.Contains(res.Detail, "could not confirm") {
		t.Errorf("an unconfirmed resend must say so to the operator, got %q", res.Detail)
	}
	// And the cause has to be the honest one: the plugin did not answer, which
	// is NOT the same as "no Message-ID was recorded for this entry".
	if !strings.Contains(res.Detail, "too old") {
		t.Errorf("the operator must be told the site's plugin could not check, got %q", res.Detail)
	}
	if strings.Contains(res.Detail, "no provider message ID was recorded") {
		t.Errorf("wrong cause reported: the CP had a Message-ID and sent it, got %q", res.Detail)
	}
	// PR #542 review: an agent old enough to omit `verified` entirely is
	// exactly the state where updating the plugin is the fix — say so.
	if !strings.Contains(res.Detail, "Update the plugin") {
		t.Errorf("a legacy agent's silence must tell the operator to update the plugin, got %q", res.Detail)
	}

	meta, ok := resendAuditMeta(logID, res)
	if !ok {
		t.Fatal("a completed send must still be audited when unverified")
	}
	if meta["verified"] != false {
		t.Errorf("audit metadata verified = %v, want false", meta["verified"])
	}
	if meta["legacy_agent"] != true {
		t.Errorf("audit metadata legacy_agent = %v, want true (the agent never answered `verified`)", meta["legacy_agent"])
	}
}

// TestResendEmail_NoMessageID_LegacyAgentSilence_IsNotAPluginProblem is the
// regression test for a CodeRabbit thread on PR #542: the combination the
// legacyAgent-only branching in this same PR's first pass got wrong.
//
// The row has no recorded Message-ID (askedForCheck=false, the ordinary shape
// of a resend whose original send failed), AND the agent's response happens to
// omit `verified` altogether — the same wire shape as a legacy agent's
// silence. The first pass keyed the operator note on that silence alone
// (legacyAgent := res.Verified == nil) and told this operator to update the
// plugin. That advice cannot possibly help: the CP never had a Message-ID to
// send, so no agent of any version — current or legacy — could have confirmed
// anything. Whether nothing could be checked must be decided before whether
// the agent stayed silent; it wins over legacyAgent, not the other way round.
func TestResendEmail_NoMessageID_LegacyAgentSilence_IsNotAPluginProblem(t *testing.T) {
	tenantID, siteID, logID := uuid.New(), uuid.New(), uuid.New()
	const agentSeq int64 = 202

	repo := newFakeResendRepo(logID, agentSeq) // no message_id — a failed send
	// Same wire shape as TestResendEmail_LegacyAgentSilence_IsNotVerified: no
	// `verified` key at all. The only difference is this row never had a
	// Message-ID to send in the first place.
	agent := &fakeResendAgent{result: mustDecodeResendResult(t,
		`{"ok":true,"detail":"resent","message_id":"<new@site>"}`)}
	svc := newResendSvc(repo, agent)

	res, err := svc.ResendEmail(context.Background(), tenantID, siteID, logID)
	if err != nil {
		t.Fatalf("ResendEmail: unexpected error: %v", err)
	}
	if !res.OK {
		t.Fatalf("a row without a Message-ID must still resend, got ok=false detail=%q", res.Detail)
	}
	if agent.lastReq.MessageID != "" {
		t.Fatalf("precondition: this row has no Message-ID, so none may be sent; got %q", agent.lastReq.MessageID)
	}
	if res.Verified {
		t.Error("the agent returned no `verified` field; this must not report a verified resend")
	}
	if !strings.Contains(res.Detail, "could not confirm") {
		t.Errorf("an unconfirmed resend must say so to the operator, got %q", res.Detail)
	}
	// The crux of the bug: nothing could have been checked, so telling this
	// operator to update the plugin is advice that cannot help.
	if strings.Contains(res.Detail, "too old") || strings.Contains(res.Detail, "Update the plugin") {
		t.Errorf("no Message-ID was ever sent, so this is not a plugin problem, got %q", res.Detail)
	}
	if !strings.Contains(res.Detail, "no provider message ID was recorded") {
		t.Errorf("expected the no-recorded-id cause, got %q", res.Detail)
	}
	if !strings.Contains(res.Detail, "nothing to fix") {
		t.Errorf("the operator must be told plainly there is nothing to fix, got %q", res.Detail)
	}

	meta, ok := resendAuditMeta(logID, res)
	if !ok {
		t.Fatal("a completed send must still be audited when unverified")
	}
	if meta["legacy_agent"] != false {
		t.Errorf("audit metadata legacy_agent = %v, want false: a request that never asked for a "+
			"check cannot use the agent's silence as evidence it is a legacy agent", meta["legacy_agent"])
	}
}

// ---------------------------------------------------------------------------
// GH #528, PR #542 review — the CP never normalises a Message-ID it dispatches
// ---------------------------------------------------------------------------
//
// The agent now compares raw bytes on both sides, untouched, and pins that
// with its own tests (PR #541): test_whitespace_only_supplied_id_is_compared_
// not_discarded, test_supplied_id_padded_with_whitespace_is_a_mismatch, and
// test_identically_padded_ids_on_both_sides_still_resend. The CP used to call
// strings.TrimSpace on the way out, which disagreed with that contract in two
// directions: a row stored as "  <a@x>  " would dispatch as "<a@x>", so a
// byte-comparing agent sees a mismatch and refuses a legitimate resend, and a
// row stored as "   " would trim to "" and the CP would omit the key, so the
// agent skips verification and sends the mail unverified — the exact outcome
// #528 exists to prevent.
//
// The rule: nil, or exactly "", omits the key. Anything else — including a
// whitespace-only or whitespace-padded id — goes out verbatim.

func TestResendEmail_NilMessageID_OmitsKey(t *testing.T) {
	tenantID, siteID, logID := uuid.New(), uuid.New(), uuid.New()
	repo := newFakeResendRepo(logID, 501) // addRow leaves MessageID nil
	agent := &fakeResendAgent{result: mustDecodeResendResult(t,
		`{"ok":true,"detail":"resent","message_id":"<new@site>","verified":false}`)}
	svc := newResendSvc(repo, agent)

	_, err := svc.ResendEmail(context.Background(), tenantID, siteID, logID)
	if err != nil {
		t.Fatalf("ResendEmail: unexpected error: %v", err)
	}
	assertResendPayload(t, agent.lastReq, wantResendKeysUnverified, map[string]any{
		"agent_seq": int64(501),
	})
}

func TestResendEmail_EmptyMessageID_OmitsKey(t *testing.T) {
	tenantID, siteID, logID := uuid.New(), uuid.New(), uuid.New()
	repo := &fakeResendRepo{fakeRepo: newFakeRepo(), rows: map[uuid.UUID]ResendTarget{}}
	repo.addRowWithMessageID(logID, 502, "") // exactly empty, distinct from nil
	agent := &fakeResendAgent{result: mustDecodeResendResult(t,
		`{"ok":true,"detail":"resent","message_id":"<new@site>","verified":false}`)}
	svc := newResendSvc(repo, agent)

	_, err := svc.ResendEmail(context.Background(), tenantID, siteID, logID)
	if err != nil {
		t.Fatalf("ResendEmail: unexpected error: %v", err)
	}
	assertResendPayload(t, agent.lastReq, wantResendKeysUnverified, map[string]any{
		"agent_seq": int64(502),
	})
}

// TestResendEmail_WhitespaceOnlyMessageID_SentVerbatim is the case #528's fix
// mishandled: TrimSpace turns a whitespace-only id into "", which is the
// OMITTED shape, not the verbatim shape a raw-byte-comparing agent expects.
func TestResendEmail_WhitespaceOnlyMessageID_SentVerbatim(t *testing.T) {
	tenantID, siteID, logID := uuid.New(), uuid.New(), uuid.New()
	repo := &fakeResendRepo{fakeRepo: newFakeRepo(), rows: map[uuid.UUID]ResendTarget{}}
	repo.addRowWithMessageID(logID, 503, "   ") // whitespace-only, not empty
	agent := &fakeResendAgent{result: mustDecodeResendResult(t,
		`{"ok":true,"detail":"resent","message_id":"<new@site>","verified":true}`)}
	svc := newResendSvc(repo, agent)

	_, err := svc.ResendEmail(context.Background(), tenantID, siteID, logID)
	if err != nil {
		t.Fatalf("ResendEmail: unexpected error: %v", err)
	}
	assertResendPayload(t, agent.lastReq, wantResendKeysVerified, map[string]any{
		"agent_seq":  int64(503),
		"message_id": "   ",
	})
}

// TestResendEmail_PaddedMessageID_SentWithPadIntact is the other half: a
// stored id with real content and surrounding whitespace must reach the agent
// with both pads intact, or a legitimate resend is refused as a mismatch.
func TestResendEmail_PaddedMessageID_SentWithPadIntact(t *testing.T) {
	tenantID, siteID, logID := uuid.New(), uuid.New(), uuid.New()
	repo := &fakeResendRepo{fakeRepo: newFakeRepo(), rows: map[uuid.UUID]ResendTarget{}}
	repo.addRowWithMessageID(logID, 504, "  <a@x>  ")
	agent := &fakeResendAgent{result: mustDecodeResendResult(t,
		`{"ok":true,"detail":"resent","message_id":"<new@site>","verified":true}`)}
	svc := newResendSvc(repo, agent)

	_, err := svc.ResendEmail(context.Background(), tenantID, siteID, logID)
	if err != nil {
		t.Fatalf("ResendEmail: unexpected error: %v", err)
	}
	assertResendPayload(t, agent.lastReq, wantResendKeysVerified, map[string]any{
		"agent_seq":  int64(504),
		"message_id": "  <a@x>  ",
	})
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
	repo.addRowWithMessageID(okID, 11, "<a-orig@site>")
	repo.addRow(refusedID, 12, true)
	repo.addRow(noSeqID, 0, true)

	// The agent resends the first row (confirming the id it was given) and
	// refuses the second.
	agent := &perSeqResendAgent{results: map[int64]agentcmd.ResendEmailResult{
		11: mustDecodeResendResult(t, `{"ok":true,"detail":"resent","message_id":"<a@site>","verified":true}`),
		12: mustDecodeResendResult(t, `{"ok":false,"detail":"body_not_stored"}`),
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
	// GH #528: the only entry that resent carried a Message-ID, so none of the
	// confirmed sends were unverified.
	if meta["unverified"] != 0 {
		t.Errorf("audit unverified = %v, want 0", meta["unverified"])
	}

	// Each addressable row must be addressed with its OWN selectors — the row
	// with a recorded Message-ID sends it, the row without omits it. A batch
	// that reused one row's message_id for the whole batch would be the #528
	// bug wearing a fix.
	if got := agent.msgIDs; len(got) != 2 || got[0] != "<a-orig@site>" || got[1] != "" {
		t.Errorf("agent saw message_id sequence %q, want [\"<a-orig@site>\" \"\"]", got)
	}
}

// TestBulkResendEmail_UnverifiedCounted proves the bulk audit row distinguishes
// a confirmed resend from an unconfirmed one rather than collapsing both into
// `count`.
func TestBulkResendEmail_UnverifiedCounted(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	withID, withoutID := uuid.New(), uuid.New()

	repo := &fakeResendRepo{fakeRepo: newFakeRepo(), rows: map[uuid.UUID]ResendTarget{}}
	repo.addRowWithMessageID(withID, 31, "<x@site>")
	repo.addRow(withoutID, 32, true)

	// Row 31 carried a Message-ID, so the agent compared it and confirms. Row 32
	// carried none, so there was nothing to compare and the agent says so.
	agent := &perSeqResendAgent{results: map[int64]agentcmd.ResendEmailResult{
		31: mustDecodeResendResult(t, `{"ok":true,"detail":"resent","message_id":"<n1@site>","verified":true}`),
		32: mustDecodeResendResult(t, `{"ok":true,"detail":"resent","message_id":"<n2@site>","verified":false}`),
	}}
	svc := newResendSvc(repo, agent)

	results, err := svc.BulkResendEmail(context.Background(), tenantID, siteID, []uuid.UUID{withID, withoutID})
	if err != nil {
		t.Fatalf("BulkResendEmail: unexpected error: %v", err)
	}
	for _, r := range results {
		wantVerified := r.LogID == withID
		if r.Verified != wantVerified {
			t.Errorf("log %s: Verified=%v, want %v", r.LogID, r.Verified, wantVerified)
		}
	}
	meta, ok := bulkResendAuditMeta(2, results)
	if !ok {
		t.Fatal("a batch with confirmed resends must be audited")
	}
	if meta["count"] != 2 {
		t.Errorf("audit count = %v, want 2", meta["count"])
	}
	if meta["unverified"] != 1 {
		t.Errorf("audit unverified = %v, want 1", meta["unverified"])
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
	msgIDs  []string
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
	f.msgIDs = append(f.msgIDs, req.MessageID)
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
		{
			name:   "#528 row identity mismatch",
			detail: agentcmd.ResendDetailMessageIDMismatch,
			want:   "this entry can't be resent from here",
		},
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
