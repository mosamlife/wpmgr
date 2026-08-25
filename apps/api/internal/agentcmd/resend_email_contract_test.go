package agentcmd

import (
	"encoding/json"
	"testing"
)

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
			name: "resent",
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got ResendEmailResult
			if err := json.Unmarshal([]byte(tc.body), &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got != tc.want {
				t.Errorf("decoded %+v, want %+v", got, tc.want)
			}
		})
	}
}
