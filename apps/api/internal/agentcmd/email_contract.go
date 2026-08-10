package agentcmd

// email_contract.go — CP->agent command contract for per-site email management
// (m59, Phase 1 foundation). The wp-agent-engineer mirrors these shapes in the
// agent's command handlers when implementing Phase 2.
//
// Wire commands (POST {site_url}/wp-json/wpmgr/v1/command/{cmd},
// Authorization: Bearer <minted EdDSA JWT>, aud=<siteId>):
//
//   sync_email_config  — push the full per-site email config (provider, from
//                        address, connection details) including the DECRYPTED
//                        provider secret so the agent can store it in its own
//                        keystore. The CP decrypts the age ciphertext in-memory;
//                        the plaintext secret travels in the signed JWT-protected
//                        body (HTTPS + Ed25519 signature). The agent stores the
//                        secret in its local WP options table encrypted with its
//                        own key. Response: {ok, detail}.
//
//   send_test_email    — ask the agent to send a test email using its current
//                        email config (sync_email_config MUST be called first).
//                        Response: {ok, detail, message_id?}.
//
// Phase 2 (wp-agent-engineer) MUST implement both command handlers.

// EmailConnectionWire is one named connection in the connections registry sent
// to the agent on each sync_email_config push. The agent replaces its full
// connections registry with the contents of this map (replace-all semantics).
// An absent or empty map means "no named connections; clear the registry".
type EmailConnectionWire struct {
	// Provider is the provider slug: smtp | ses | sendgrid | mailgun | postmark.
	Provider string `json:"provider"`
	// Config holds non-secret provider settings (same shape as EmailConfigRequest.Config).
	Config map[string]any `json:"config"`
	// Secret is the DECRYPTED plaintext per-connection secret. Same trust
	// boundary and same three-state contract as EmailConfigRequest.Secret:
	// omitted means "keep the stored one", a non-empty value replaces it, and
	// ClearSecret removes it.
	Secret *string `json:"secret,omitempty"`
	// ClearSecret asks the agent to delete this connection's stored secret.
	// See EmailConfigRequest.ClearSecret for the full contract.
	ClearSecret bool `json:"clear_secret,omitempty"`
	// FromAddress is an optional per-connection sender address override.
	// When non-empty, outgoing mail routes through this connection using this address.
	FromAddress string `json:"from_address,omitempty"`
	// FromName is an optional per-connection sender name override.
	FromName string `json:"from_name,omitempty"`
}

// EmailConfigRequest is the POST body for `sync_email_config`.
// It carries the full per-site email config including the DECRYPTED provider
// secret — the signing + HTTPS transport is the security boundary.
type EmailConfigRequest struct {
	// Provider is the provider slug: smtp | ses | sendgrid | mailgun | postmark.
	Provider string `json:"provider"`

	// FromAddress is the From: email address.
	FromAddress string `json:"from_address"`
	// FromName is the From: display name.
	FromName string `json:"from_name"`
	// ForceFromEmail when true overrides WP's generated From address.
	ForceFromEmail bool `json:"force_from_email"`
	// ForceFromName when true overrides WP's generated display name.
	ForceFromName bool `json:"force_from_name"`
	// ReturnPath when true sets the Return-Path / bounce address.
	ReturnPath bool `json:"return_path"`

	// Config holds non-secret provider settings. The shape depends on the
	// provider (see catalog.go for field definitions):
	//   smtp:      host, port, encryption, auth, username, auto_tls
	//   ses:       access_key, region, return_path
	//   sendgrid:  (none — secret is the sole configuration)
	//   mailgun:   domain_name, region
	//   postmark:  message_stream, track_opens, track_links
	Config map[string]any `json:"config"`

	// Secret is the DECRYPTED provider secret (SMTP password / API key / AWS
	// secret access key).
	//
	// GH #380 — THE SECRET WIRE CONTRACT. The three states a push can be in are
	// three distinct values on the wire, because collapsing "could not resolve"
	// and "delete it" into one empty string is what destroyed working
	// credentials on routine config pushes:
	//
	//	Secret omitted, ClearSecret false  keep the stored credential
	//	Secret non-empty                   replace the stored credential
	//	ClearSecret true                   delete the stored credential
	//
	// The CP never sends a non-nil empty Secret; an explicit clear is always
	// expressed as ClearSecret. The two are never both set (see pushSecret in
	// internal/email), but if they somehow both arrive the agent takes the
	// non-empty Secret: it is the newer credential and it is bound to the
	// settings travelling beside it.
	//
	// This mirrors the nil-sentinel the database column already speaks (see
	// site_email.sql, where a nil secret preserves the stored ciphertext) plus
	// the explicit revoke the column expresses as NULL.
	//
	// SECURITY: this field travels in the signed JWT-protected body over HTTPS.
	// The CP decrypts from age ciphertext in-memory and never logs this value.
	Secret *string `json:"secret,omitempty"`

	// ClearSecret asks the agent to delete the credential it has stored for
	// this site. It is the only way the control plane can revoke a credential
	// it already pushed, and it is sent whenever the config being pushed is no
	// longer the one the stored credential was issued for: an explicit clear
	// from the operator, a provider switch, or an endpoint the credential's
	// owner did not choose.
	ClearSecret bool `json:"clear_secret,omitempty"`

	// Mappings is a JSON object mapping From-email addresses to connection keys
	// for per-sender routing. Values are connection key strings (not arrays).
	// Old agents' is_array() check fails safely → falls to primary row.
	Mappings map[string]any `json:"mappings,omitempty"`

	// m62 — multi-connection fields (additive; omitempty — old agents drop them).

	// Connections is the full named-connection registry for this site.
	// Replace-all semantics: absent/empty clears the agent registry.
	// Key is the operator-chosen slug (^[a-z0-9][a-z0-9_-]{0,31}$, 'default' reserved).
	Connections map[string]EmailConnectionWire `json:"connections,omitempty"`

	// DefaultConnection is the connection key to use when mappings has no match.
	// "" or "default" means use the primary config row (the existing behaviour).
	DefaultConnection string `json:"default_connection,omitempty"`

	// FallbackConnection is the connection key to try when DefaultConnection fails.
	// "" means no fallback (disable_fallback semantics apply for test sends).
	FallbackConnection string `json:"fallback_connection,omitempty"`

	// LogEmails when true the agent buffers each send to its local WP table.
	LogEmails bool `json:"log_emails"`
	// StoreBody when true the agent includes the full message body in the log.
	StoreBody bool `json:"store_body"`
	// RetentionDays is the maximum age (in days) of log entries the agent keeps.
	RetentionDays int `json:"retention_days"`
}

// EmailConfigResult is the response body for `sync_email_config`.
type EmailConfigResult struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// SendTestEmailRequest is the POST body for `send_test_email`.
type SendTestEmailRequest struct {
	// To is the recipient address for the test message. Required.
	To string `json:"to"`
	// Subject is the email subject line (defaults to "Test Email from WPMgr" on
	// the agent if empty).
	Subject string `json:"subject,omitempty"`
	// Body is the plain-text email body (defaults to a stock message if empty).
	Body string `json:"body,omitempty"`
	// Connection is the connection key to route the test send through.
	// "" means use the primary config row (disable_fallback=true for all test sends).
	Connection string `json:"connection,omitempty"`
}

// SendTestEmailResult is the response body for `send_test_email`.
type SendTestEmailResult struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	// MessageID is the provider-returned Message-ID header value (if available).
	MessageID string `json:"message_id,omitempty"`
}
