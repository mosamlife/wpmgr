package agentcmd

// resend_email_contract.go — CP->agent command contract for resending an email
// the agent already holds in its own local log.
//
// Wire contract (must match apps/agent/includes/commands/class-resend-email-command.php):
//
//	POST {siteURL}/wp-json/wpmgr/v1/command/resend_email
//	Authorization: Bearer <Ed25519 JWT, cmd="resend_email", aud=<siteId>>
//	Body: {"agent_seq": <int>}
//
// The agent looks the row up in its own wpmgr_email_log by that local row id,
// refuses when the row is gone or its body was not captured, and otherwise
// re-sends through its configured provider and returns the new Message-ID.
//
// GH #520: the CP used to send a log_id UUID plus an (unpopulated) copy of the
// message itself. The agent has required agent_seq since the same commit that
// shipped the CP side, so every resend, on every site, failed on the agent's
// first validation branch. The agent's contract wins: it is already implemented
// and tested, one CP deploy repairs the whole fleet without waiting for a
// plugin update, and it keeps customer message bodies off the command channel.
//
// Consequence to keep in view: the agent prunes its log at 14 days / 50k rows
// (class-email-logger.php), so a resend of an older entry legitimately returns
// ResendDetailRowNotFound. That is an honest failure, not a silent empty send.

// ResendEmailRequest is the POST body for the `resend_email` agent command.
//
// One field, deliberately. Everything the agent needs to rebuild the message
// (recipients, from, subject, body, provider) it reads from its own log row —
// the CP names the row and nothing more.
type ResendEmailRequest struct {
	// AgentSeq is the agent-local wpmgr_email_log row id, mirrored into the CP
	// as site_email_log.agent_seq at ingest. It is the only field the agent
	// reads, and it must be a positive integer or the agent refuses.
	AgentSeq int64 `json:"agent_seq"`
}

// Known `detail` values the agent returns with ok=false. These are contract
// strings, not free text: callers map them to operator-facing wording rather
// than showing them raw (a user saw "missing required field: agent_seq" in a
// toast, which is how GH #520 was reported).
const (
	// ResendDetailRowNotFound — the agent's own log no longer holds the row,
	// normally because its 14-day / 50k-row prune has passed over it.
	ResendDetailRowNotFound = "log_row_not_found"
	// ResendDetailBodyNotStored — the row exists but was logged without body
	// capture, so there is nothing to re-send.
	ResendDetailBodyNotStored = "body_not_stored"
	// ResendDetailNoConfig — the agent has no email configuration yet; the CP
	// must push sync_email_config first. Prefix, the agent appends guidance.
	ResendDetailNoConfig = "no email config"
	// ResendDetailMissingSeq — the agent rejected the request shape itself.
	// Reachable only if the CP regresses to a pre-#520 payload.
	ResendDetailMissingSeq = "missing required field: agent_seq"
	// ResendDetailBadSeq — agent_seq was present but not a positive integer.
	ResendDetailBadSeq = "agent_seq must be a positive integer"
)

// ResendEmailResult is the response body for the `resend_email` agent command.
type ResendEmailResult struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	// MessageID is the provider-returned Message-ID header value for the new send.
	MessageID string `json:"message_id,omitempty"`
}
