package agentcmd

// resend_email_contract.go — CP->agent command contract for resending an email
// the agent already holds in its own local log.
//
// Wire contract (must match apps/agent/includes/commands/class-resend-email-command.php):
//
//	POST {siteURL}/wp-json/wpmgr/v1/command/resend_email
//	Authorization: Bearer <Ed25519 JWT, cmd="resend_email", aud=<siteId>>
//	Body: {"agent_seq": <int>, "message_id": <string, optional>}
//
// The agent looks the row up in its own wpmgr_email_log by that local row id,
// refuses when the row is gone or its body was not captured, and otherwise
// re-sends through its configured provider and returns the new Message-ID.
//
// GH #528: agent_seq alone is not a safe selector across a site database
// restore. It is a MySQL AUTO_INCREMENT on the site's own table
// (apps/agent/includes/class-schema.php), so a restore rolls the counter back
// and later sends re-use ids the CP has already bound to different messages.
// wpmgr performs restores itself, so this is reachable in normal operation:
// CP row X holds agent_seq 42 = "Invoice for Alice"; after a restore the site's
// own row 42 is "Password reset for Bob"; the operator sees Alice and clicks
// Resend; the agent sends Bob's message to Bob. An email cannot be recalled.
//
// message_id is the confirmation field. It is a ROW SELECTOR, not message
// content: the provider's Message-ID header for the send the CP has on record.
// It carries no recipients, no subject and no body, so it does not reopen what
// #520 settled — stored message bodies stay off the command channel.
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
// Two fields, both selectors. Everything the agent needs to rebuild the message
// (recipients, from, subject, body, provider) it reads from its own log row —
// the CP names the row, confirms which row it meant, and nothing more.
type ResendEmailRequest struct {
	// AgentSeq is the agent-local wpmgr_email_log row id, mirrored into the CP
	// as site_email_log.agent_seq at ingest. It is the field the agent
	// addresses by, and it must be a positive integer or the agent refuses.
	AgentSeq int64 `json:"agent_seq"`

	// MessageID is the provider Message-ID the CP has recorded for that row.
	// When present the agent compares it against its own row's message_id and
	// refuses on a mismatch (ResendDetailMessageIDMismatch) rather than sending
	// a message the operator did not choose — see GH #528.
	//
	// omitempty is load-bearing. The CP's message_id is NULL for every send
	// that FAILED at the time (all five agent provider handlers return an empty
	// message id on their failure branch) and for a success whose provider did
	// not surface a Message-ID header. Failed sends are precisely the rows an
	// operator most wants to resend, so the CP does not refuse them; it omits
	// the key, and the omission is what tells the agent no comparison is
	// possible. An empty string would instead read as "compare against empty"
	// and would refuse every one of those rows.
	MessageID string `json:"message_id,omitempty"`
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
	// ResendDetailMessageIDMismatch — GH #528. The CP sent a message_id and the
	// agent's row at that agent_seq carries a different one, so the local id no
	// longer names the message the operator chose. The usual cause is a site
	// database restore rolling the AUTO_INCREMENT counter back. Refusing is
	// correct: the alternative is sending someone else's mail.
	ResendDetailMessageIDMismatch = "message_id_mismatch"
)

// ResendEmailResult is the response body for the `resend_email` agent command.
type ResendEmailResult struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	// MessageID is the provider-returned Message-ID header value for the new send.
	MessageID string `json:"message_id,omitempty"`
}
