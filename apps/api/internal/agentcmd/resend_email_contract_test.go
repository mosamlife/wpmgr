package agentcmd

import (
	"encoding/json"
	"strings"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

// TestResendEmailRequest_WireShape pins the bytes that go on the command
// channel for a resend.
//
// GH #520: this struct used to marshal to
//
//	{"log_id":"<uuid>","to_addresses":null,"from_address":"","subject":"","body":""}
//
// while apps/agent/includes/commands/class-resend-email-command.php rejects any
// params array without agent_seq on the first line of execute(). Neither side's
// tests looked at the other's, so the mismatch shipped and every resend failed.
//
// Exact equality, not a field check: an extra field here is message content
// travelling on the command channel to a reader that ignores it.
func TestResendEmailRequest_WireShape(t *testing.T) {
	raw, err := json.Marshal(ResendEmailRequest{AgentSeq: 4242})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"agent_seq":4242}`
	if string(raw) != want {
		t.Errorf("resend_email request body = %s, want %s", raw, want)
	}
}

// ---------------------------------------------------------------------------
// The RESPONSE half of the contract
// ---------------------------------------------------------------------------
//
// The request half above is pinned to exact bytes because a mismatch there
// broke every resend on every site (GH #520). The response half now carries a
// SECURITY-RELEVANT field and gets the same treatment.
//
// `verified` is the agent's attestation that it compared the message_id the CP
// supplied against its own row and they matched (GH #528 / PR #541). Everything
// the CP tells the operator about whether the right message went out rests on
// it, so a rename, a drop or a type change on either side has to be a test
// failure — not a field that quietly decodes to false and makes every resend
// read "unconfirmed", and not one that quietly decodes to true.

// wantResendResponseKeys is the exact set of JSON keys the agent's resend_email
// handler returns on the confirmed-success path. Keep it in lockstep with
// class-resend-email-command.php::execute().
var wantResendResponseKeys = []string{"ok", "detail", "message_id", "verified"}

// assertResendResponseShape asserts a literal agent response against the pinned
// key set, then decodes it with UNKNOWN FIELDS REFUSED, which is what proves
// the CP actually reads every key the agent sends under exactly these names.
//
// Exact in all four directions, mirroring assertResendPayload on the request
// side:
//   - a MISSING key fails the per-key loop,
//   - an EXTRA key fails the length check and the strict decode,
//   - a RENAMED key fails both at once — and a rename of `verified` is the one
//     that matters, because a key the CP does not read leaves the field at its
//     legacy default forever,
//   - a WRONG VALUE fails the field comparison below.
//
// The strictness lives HERE, in the contract test, not in the client: the real
// decode path must stay tolerant of keys a future agent adds. That tolerance is
// proven separately by TestResendEmailResult_UnknownKeyIsTolerated.
func assertResendResponseShape(t *testing.T, body string, want ResendEmailResult) {
	t.Helper()

	var keys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &keys); err != nil {
		t.Fatalf("agent response is not a JSON object: %v\n  got: %s", err, body)
	}
	if len(keys) != len(wantResendResponseKeys) {
		t.Errorf("agent response has %d field(s), want %d\n  got: %s\n  want keys: %v",
			len(keys), len(wantResendResponseKeys), body, wantResendResponseKeys)
	}
	for _, k := range wantResendResponseKeys {
		if _, ok := keys[k]; !ok {
			t.Errorf("agent response is missing %q — the CP cannot read what it is not sent\n  got: %s", k, body)
		}
	}

	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	var got ResendEmailResult
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("the CP does not read every key the agent sends: %v\n  got: %s", err, body)
	}

	if got.OK != want.OK {
		t.Errorf("ok = %v, want %v\n  got: %s", got.OK, want.OK, body)
	}
	if got.Detail != want.Detail {
		t.Errorf("detail = %q, want %q\n  got: %s", got.Detail, want.Detail, body)
	}
	if got.MessageID != want.MessageID {
		t.Errorf("message_id = %q, want %q\n  got: %s", got.MessageID, want.MessageID, body)
	}
	// Presence AND value. Collapsing the two is how "the agent said nothing"
	// and "the agent said no" became the same thing.
	if (got.Verified == nil) != (want.Verified == nil) {
		t.Errorf("verified present = %v, want %v\n  got: %s", got.Verified != nil, want.Verified != nil, body)
	}
	if got.IsVerified() != want.IsVerified() {
		t.Errorf("IsVerified() = %v, want %v\n  got: %s", got.IsVerified(), want.IsVerified(), body)
	}
}

// TestResendEmailResult_WireShape pins the exact response of a current agent
// that compared the id it was given and matched it.
func TestResendEmailResult_WireShape(t *testing.T) {
	assertResendResponseShape(t,
		`{"ok":true,"detail":"resent","message_id":"<abc@site>","verified":true}`,
		ResendEmailResult{OK: true, Detail: "resent", MessageID: "<abc@site>", Verified: boolPtr(true)},
	)
}

// TestResendEmailResult_LegacyAgentOmitsVerified is the safety property of the
// whole change: an agent too old to know the field exists says nothing, and
// silence must decode to "not verified", distinguishably so.
func TestResendEmailResult_LegacyAgentOmitsVerified(t *testing.T) {
	var got ResendEmailResult
	if err := json.Unmarshal([]byte(`{"ok":true,"detail":"resent","message_id":"<abc@site>"}`), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Verified != nil {
		t.Errorf("an absent key must stay absent, not become a value: %v", *got.Verified)
	}
	if got.IsVerified() {
		t.Error("an agent that said nothing about verification must never read as verified")
	}
}

// TestResendEmailResult_RenamedVerifiedKeyIsCaught proves the rename direction.
// The strict contract decode rejects it outright, and the CP's real (lenient)
// decode falls back to unverified rather than to verified — so even the failure
// mode of this guard is the safe one.
func TestResendEmailResult_RenamedVerifiedKeyIsCaught(t *testing.T) {
	const renamed = `{"ok":true,"detail":"resent","message_id":"<abc@site>","is_verified":true}`

	dec := json.NewDecoder(strings.NewReader(renamed))
	dec.DisallowUnknownFields()
	var strict ResendEmailResult
	if err := dec.Decode(&strict); err == nil {
		t.Error("a renamed verification key must be caught by the contract decode, not absorbed")
	}

	var lenient ResendEmailResult
	if err := json.Unmarshal([]byte(renamed), &lenient); err != nil {
		t.Fatalf("the real decode path must not hard-fail on an unread key: %v", err)
	}
	if lenient.IsVerified() {
		t.Error("a key the CP does not read must not produce a verified resend")
	}
}

// TestResendEmailResult_WrongTypeFailsClosed covers the wrong-value direction.
// A non-boolean `verified` is a decode error, which client.post() returns as an
// error and Client.ResendEmail turns into ok=false — the resend is reported as
// failed rather than as verified.
func TestResendEmailResult_WrongTypeFailsClosed(t *testing.T) {
	var got ResendEmailResult
	err := json.Unmarshal([]byte(`{"ok":true,"detail":"resent","message_id":"<a@site>","verified":"true"}`), &got)
	if err == nil {
		t.Fatal("a non-boolean verified must not decode; a string \"true\" is not an attestation")
	}
	if got.IsVerified() {
		t.Error("a failed decode must leave the result unverified")
	}
}

// TestResendEmailResult_UnknownKeyIsTolerated is the over-fire guard. The real
// decode path must keep accepting a response from an agent NEWER than this
// control plane, and an unread key must not move `verified` in either direction.
func TestResendEmailResult_UnknownKeyIsTolerated(t *testing.T) {
	var got ResendEmailResult
	body := `{"ok":true,"detail":"resent","message_id":"<a@site>","verified":true,"queued_at":"2026-08-26T00:00:00Z"}`
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("a future agent key must not break the CP: %v", err)
	}
	if !got.IsVerified() {
		t.Error("an unknown extra key must not suppress a real attestation")
	}
}

// TestResendEmailResult_DecodesAgentResponses decodes the exact bodies the
// agent's execute() returns, so a rename on either side is a test failure
// rather than a silently empty result.
func TestResendEmailResult_DecodesAgentResponses(t *testing.T) {
	cases := []struct {
		name string
		body string
		want ResendEmailResult
	}{
		{
			name: "resent and confirmed",
			body: `{"ok":true,"detail":"resent","message_id":"<abc@site>","verified":true}`,
			want: ResendEmailResult{OK: true, Detail: "resent", MessageID: "<abc@site>", Verified: boolPtr(true)},
		},
		{
			name: "resent without a confirmation to give",
			body: `{"ok":true,"detail":"resent","message_id":"<abc@site>","verified":false}`,
			want: ResendEmailResult{OK: true, Detail: "resent", MessageID: "<abc@site>", Verified: boolPtr(false)},
		},
		{
			name: "resent by an agent too old to confirm",
			body: `{"ok":true,"detail":"resent","message_id":"<abc@site>"}`,
			want: ResendEmailResult{OK: true, Detail: "resent", MessageID: "<abc@site>"},
		},
		{
			name: "pruned from the agent log",
			body: `{"ok":false,"detail":"log_row_not_found","message_id":""}`,
			want: ResendEmailResult{OK: false, Detail: ResendDetailRowNotFound},
		},
		{
			name: "body was never captured",
			body: `{"ok":false,"detail":"body_not_stored","message_id":""}`,
			want: ResendEmailResult{OK: false, Detail: ResendDetailBodyNotStored},
		},
		{
			name: "the #520 rejection",
			body: `{"ok":false,"detail":"missing required field: agent_seq","message_id":""}`,
			want: ResendEmailResult{OK: false, Detail: ResendDetailMissingSeq},
		},
		{
			name: "the #528 refusal",
			body: `{"ok":false,"detail":"message_id_mismatch","message_id":"","verified":false}`,
			want: ResendEmailResult{OK: false, Detail: ResendDetailMessageIDMismatch, Verified: boolPtr(false)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got ResendEmailResult
			if err := json.Unmarshal([]byte(tc.body), &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got.OK != tc.want.OK || got.Detail != tc.want.Detail || got.MessageID != tc.want.MessageID {
				t.Errorf("decoded ok=%v detail=%q message_id=%q, want ok=%v detail=%q message_id=%q",
					got.OK, got.Detail, got.MessageID, tc.want.OK, tc.want.Detail, tc.want.MessageID)
			}
			if (got.Verified == nil) != (tc.want.Verified == nil) {
				t.Errorf("verified present = %v, want %v", got.Verified != nil, tc.want.Verified != nil)
			}
			if got.IsVerified() != tc.want.IsVerified() {
				t.Errorf("IsVerified() = %v, want %v", got.IsVerified(), tc.want.IsVerified())
			}
		})
	}
}
