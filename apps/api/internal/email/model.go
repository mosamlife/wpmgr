// Package email is the per-site Email / SMTP Management control-plane domain.
// It owns the site_email_config / site_email_log / email_suppression tables and
// the CP-side config + secrets layer. The agent side (Phase 2) handles the actual
// SMTP/API sends; the CP pushes the decrypted config via signed commands and
// receives log entries back for fleet-wide dashboarding.
//
// Architecture: modelled on internal/perf/ — handler / service / repo / model /
// dto. Phase 1 covers config CRUD + secrets + provider catalog + test-send route.
// Phase 3 wires log ingest. Phase 4 wires webhooks + suppression.
package email

import (
	"time"

	"github.com/google/uuid"
)

// Config is the per-site (or org-wide) outgoing email configuration. The
// provider_secret_encrypted column is NEVER surfaced past the repo boundary;
// only SecretSet (a bool) is included here.
type Config struct {
	ID       uuid.UUID
	TenantID uuid.UUID
	// SiteID is nil for the org-wide default row.
	SiteID *uuid.UUID

	Provider       string
	FromAddress    string
	FromName       string
	ForceFromEmail bool
	ForceFromName  bool
	ReturnPath     bool

	// Config holds non-secret provider settings:
	//   SMTP: host, port, encryption, auth, username, auto_tls
	//   SES:  region, return_path
	//   SendGrid: (none — API key is the only field, stored in secret)
	//   Mailgun: domain_name, region (us|eu)
	//   Postmark: message_stream, track_opens, track_links
	Config map[string]any

	// SecretSet reports whether provider_secret_encrypted is non-null in the DB.
	// The actual ciphertext is never returned — the service decrypts it only when
	// building a sync_email_config command push.
	SecretSet bool

	Mappings           map[string]any
	DefaultConnection  *string
	FallbackConnection *string

	LogEmails     bool
	StoreBody     bool
	RetentionDays int

	// m61: webhook security fields.
	// WebhookRouteToken is the PLAIN token (never stored at rest; populated only
	// when the service just generated it for a response; otherwise empty).
	WebhookRouteToken string
	// WebhookSigningKeySet is true when webhook_signing_key_enc is non-null.
	WebhookSigningKeySet bool
	// SesTopicArns is the SNS TopicArn allowlist (nil = SES not configured).
	SesTopicArns []string
	// WebhookRouteTokenHashSet is true when webhook_route_token_hash is non-null.
	WebhookRouteTokenHashSet bool

	// Inherited is true when this value is the org-wide row returned for a site
	// that has no config row of its own. It is set only by GetConfig, which
	// rewrites SiteID to the queried site; without this flag the caller cannot
	// tell an inherited row from a per-site one, and SecretSet then describes a
	// credential the site does not own (GH #380).
	Inherited bool

	CreatedAt time.Time
	UpdatedAt time.Time
}

// UpsertInput carries the fields for a config create-or-update. SecretRaw, when
// non-nil, is the plaintext secret that the service will age-encrypt. A nil
// SecretRaw preserves the existing ciphertext (nil-sentinel pattern).
type UpsertInput struct {
	TenantID uuid.UUID
	// SiteID is nil when upserting the org-wide default row.
	SiteID *uuid.UUID

	Provider       string
	FromAddress    string
	FromName       string
	ForceFromEmail bool
	ForceFromName  bool
	ReturnPath     bool

	// Config holds non-secret provider settings as a raw JSON-marshalable map.
	Config map[string]any

	// SecretRaw is the plaintext provider secret (SMTP password / API key).
	// nil = preserve existing; non-nil = age-encrypt and store.
	SecretRaw *string

	Mappings           map[string]any
	DefaultConnection  *string
	FallbackConnection *string

	LogEmails     bool
	StoreBody     bool
	RetentionDays int
}

// UpsertWebhookInput carries the fields for a webhook-security update (m61).
// All fields are optional; nil = preserve existing value (nil-sentinel).
type UpsertWebhookInput struct {
	TenantID uuid.UUID
	// ConfigID is the surrogate PK of the config row to update.
	ConfigID uuid.UUID
	// RotateToken, when true, causes the service to generate a fresh random
	// routeToken, hash it, and store the hash (returning the new plain token).
	RotateToken bool
	// SigningKeyRaw is the plaintext webhook signing key to age-encrypt + store.
	// nil = preserve existing encrypted key.
	SigningKeyRaw *string
	// SesTopicArns is the new SNS TopicArn allowlist. nil = preserve existing.
	SesTopicArns *[]string
}

// WebhookConfigResult is returned after a successful UpsertWebhookConfig call.
type WebhookConfigResult struct {
	// Config is the updated config row (masked: no secrets).
	Config Config
	// WebhookURL is the public URL for this config row's webhook endpoint.
	// Only non-empty when the config row has a route token.
	WebhookURL string
}

// WebhookResolvedConfig carries the decrypted signing material for a config
// row resolved by routeToken during webhook dispatch. It is only used inside
// the webhook handler and is never serialised.
type WebhookResolvedConfig struct {
	// Config is the full domain config (metadata + masked fields).
	Config Config
	// SigningKeyPlain is the decrypted webhook signing key (provider-specific format).
	SigningKeyPlain string
}

// TestSendInput is the request for POST /sites/:siteId/email/test.
type TestSendInput struct {
	To      string
	Subject string
	Body    string
}

// TestSendResult is the response from the test-send dispatch.
type TestSendResult struct {
	OK     bool
	Detail string
}

// ---------------------------------------------------------------------------
// Email log domain types (Phase 3)
// ---------------------------------------------------------------------------

// maxIngestBatch is the maximum number of entries the agent may push in a
// single ingest request. Requests exceeding this are rejected 400.
const maxIngestBatch = 500

// maxIngestAttachments is the maximum number of attachment metadata entries
// accepted per log entry on ingest. Entries beyond this cap are silently dropped.
const maxIngestAttachments = 100

// ---------------------------------------------------------------------------
// m62 — Multi-connection domain types (Area 2)
// ---------------------------------------------------------------------------

// Connection represents one named email connection in site_email_connection.
// The provider_secret_encrypted column is NEVER surfaced past the repo boundary;
// only SecretSet (bool) is included here.
type Connection struct {
	ID            uuid.UUID
	TenantID      uuid.UUID
	ConfigID      uuid.UUID
	ConnectionKey string
	Provider      string
	FromAddress   string
	FromName      string
	Config        map[string]any
	SecretSet     bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// ConnectionUpsertInput carries fields for a connection create-or-update.
// SecretRaw is the plaintext secret; nil preserves the existing ciphertext.
type ConnectionUpsertInput struct {
	TenantID      uuid.UUID
	ConfigID      uuid.UUID
	ConnectionKey string
	Provider      string
	FromAddress   string
	FromName      string
	Config        map[string]any
	SecretRaw     *string // nil = preserve existing
}

// AttachmentMeta is one attachment entry in the log.
// Canonical wire shape: [{"name":"<basename>","size_bytes":<int64>}]
type AttachmentMeta struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
}

// ---------------------------------------------------------------------------
// m62 — Notify settings domain types (Area 4)
// ---------------------------------------------------------------------------

// NotifySettings is the org-level alert + digest configuration.
type NotifySettings struct {
	TenantID             uuid.UUID
	Enabled              bool
	Recipients           []string
	AlertOnFailure       bool
	AlertThrottleMinutes int
	DigestEnabled        bool
	DigestCadence        string // daily | weekly | monthly
	DigestDay            int    // unused for daily; 0-6 (Sun-Sat) for weekly; 1-28 for monthly
	DigestHour           int
	Timezone             string
	NextDigestAt         *time.Time
	CreatedAt            time.Time
	UpdatedAt            time.Time
	// InstanceMailerConfigured is populated by the service from mailer.Service.Enabled.
	// It is NOT stored in DB — injected on GET.
	InstanceMailerConfigured bool
	// FailureDetection is populated by the service from a live count of the
	// tenant's connected sites (GH #381 phase 2). It is NOT stored in DB —
	// injected on GET. Nil means the coverage query itself failed (PR #447
	// bot review finding 2): the rest of the settings are still valid and
	// must still be returned, but the caller must render coverage as ABSENT,
	// never as a false "sites_covered: 0" built from an error.
	FailureDetection *FailureDetectionCoverage
}

// FailureDetectionCoverage summarizes, for a tenant, how many of its
// currently-connected sites can detect and report an email delivery failure
// (GH #381). A site is covered by EITHER of two independent paths:
//
//  1. Routed: WPMgr is the mail transport the site actively uses
//     (MailRouter::intercept() -> EmailLogger::write() on the agent). This
//     path predates GH #381 phase 1 entirely, consults no agent-version gate,
//     and works on any currently-shipped agent.
//  2. Unrouted-but-new-enough: the site does not route its mail through
//     WPMgr, but its agent_version is >= MinAgentVersionUnrouted, so it can
//     still capture and report a failure on mail WPMgr never saw pass
//     through it (GH #381 phase 1's widening).
//
// A site below MinAgentVersionUnrouted that is ALSO not routed cannot trigger
// a per-failure alert. A site that IS routed is covered regardless of its
// agent version. Computed fresh on every GET, never persisted.
type FailureDetectionCoverage struct {
	// SitesTotal is the tenant's currently-connected site count.
	SitesTotal int
	// SitesCovered is, of SitesTotal, how many are covered via EITHER path
	// above (routed OR agent_version >= MinAgentVersionUnrouted).
	SitesCovered int
	// SitesRouted is, of SitesTotal, how many have WPMgr actively configured
	// as their mail transport (path 1 above) and are therefore covered
	// regardless of agent version. Surfaced separately so the interface can
	// explain a high sites_covered count on a fleet that is otherwise below
	// MinAgentVersionUnrouted, instead of that number reading as unexplained.
	SitesRouted int
	// MinAgentVersionUnrouted is the gate applied to path 2 only (sites NOT
	// routed through WPMgr). It says nothing about routed sites, which are
	// covered independent of this value.
	// (MinAgentVersionForFailureDetection at call time).
	MinAgentVersionUnrouted string
}

// ConnectedSiteEmailFact is one connected site's raw facts needed to compute
// FailureDetectionCoverage, as returned by
// repository.ListConnectedSiteEmailCoverage. Routed=true short-circuits the
// coverage predicate regardless of AgentVersion (see FailureDetectionCoverage
// doc comment); the version-gate comparison in Go only matters when
// Routed=false.
type ConnectedSiteEmailFact struct {
	AgentVersion string
	Routed       bool
}

// NotifySettingsUpsertInput is the PUT body for notify settings.
type NotifySettingsUpsertInput struct {
	TenantID             uuid.UUID
	Enabled              bool
	Recipients           []string
	AlertOnFailure       bool
	AlertThrottleMinutes int
	DigestEnabled        bool
	DigestCadence        string
	DigestDay            int
	DigestHour           int
	Timezone             string
}

// ---------------------------------------------------------------------------
// m62 — Propagation types (Area 1)
// ---------------------------------------------------------------------------

// InheritingSite is one site returned by ListEmailInheritingSites.
type InheritingSite struct {
	ID  uuid.UUID
	URL string
}

// SiteRef holds a site's display info for use in notification emails.
type SiteRef struct {
	ID   uuid.UUID
	URL  string
	Name string
}

// ConnectionSecretRow is a lightweight pair returned by GetConnectionSecretCiphertexts.
// It avoids surfacing the sqlc type through the repository interface.
type ConnectionSecretRow struct {
	ConnectionKey           string
	ProviderSecretEncrypted []byte
}

// PropagateResult is returned after a PropagateOrgConfig fan-out.
type PropagateResult struct {
	Synced int
	Failed int
	Total  int
}

// ---------------------------------------------------------------------------
// m62 — Alert / digest data types
// ---------------------------------------------------------------------------

// AlertState is the runtime snapshot returned by ClaimAlertSlot.
// It is NOT stored separately — it maps the email_alert_state row.
type AlertState struct {
	TenantID           uuid.UUID
	SiteID             uuid.UUID
	LastAlertAt        *time.Time
	FailuresSinceAlert int64
}

// SiteStatsRow is one row from GetFleetStatsBySite (for the digest).
type SiteStatsRow struct {
	SiteID       uuid.UUID
	Total        int64
	SentCount    int64
	FailedCount  int64
	BouncedCount int64
}

// FailureSample is one entry from TopFailureSamples (for the digest).
type FailureSample struct {
	SiteID  uuid.UUID
	Subject string
	Error   string
}

// LogEntry is one email log row — the domain representation of site_email_log.
// Body is only populated on the detail view (GetLogEntry); list views omit it
// to avoid leaking potentially sensitive body content in bulk responses.
type LogEntry struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	SiteID      uuid.UUID
	AgentSeq    *int64
	MessageID   *string
	ToAddresses []string
	FromAddress string
	Subject     string
	Provider    string
	Status      string
	Response    map[string]any
	Error       string
	Retries     int
	ResentCount int
	BodyStored  bool
	// Body is non-nil only in the detail view and only when BodyStored=true.
	Body *string
	// m62 additions.
	ConnectionKey   string
	AttachmentCount int
	Attachments     []AttachmentMeta // only populated in detail view
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// LogListFilter holds the operator-supplied filters for a log list request.
type LogListFilter struct {
	// Cursor is the opaque keyset cursor returned by a previous list call.
	// Empty string = first page (far-future sentinel values used internally).
	Cursor string
	// Limit is the maximum number of rows to return (default 50, max 200).
	Limit int
	// Status filters by the status column (sent|failed|pending|…). Empty = all.
	Status string
	// From/To are the date-range bounds (inclusive). Zero = unbounded.
	From time.Time
	To   time.Time
	// Q is a free-text search against subject/from/to. Empty = no search.
	Q string
}

// LogListPage is the paginated result for a log list request.
type LogListPage struct {
	Entries    []LogEntry
	NextCursor string
}

// LogDetail extends LogEntry with prev/next navigation IDs.
type LogDetail struct {
	Entry  LogEntry
	PrevID *uuid.UUID
	NextID *uuid.UUID
}

// ResendTarget is the slice of a log row a resend needs, and nothing else.
//
// AgentSeq is the agent-local wpmgr_email_log row id and the only thing the
// resend_email command carries. It is a pointer because the column is nullable,
// though IngestEmailLogEntry — the only production INSERT — always sets it
// (repo.go IngestLogBatch takes &e.AgentSeq unconditionally). A nil here means
// a row that arrived some other way, and a resend for it cannot be addressed.
type ResendTarget struct {
	AgentSeq   *int64
	BodyStored bool
	// MessageID is the provider Message-ID recorded for this row, or nil when
	// none was ever captured. Sent to the agent as the #528 confirmation
	// selector when present. Nil is common and expected — see
	// agentcmd.ResendEmailRequest.MessageID.
	MessageID *string
}

// IngestEntry is one entry from the agent's ingest push.
type IngestEntry struct {
	AgentSeq    int64
	MessageID   string
	ToAddresses []string
	FromAddress string
	Subject     string
	Provider    string
	Status      string
	Response    map[string]any
	Error       string
	Retries     int
	ResentCount int
	BodyStored  bool
	Body        *string
	// m62 additions — coerced to '' / [] on ingest if absent.
	ConnectionKey string
	Attachments   []AttachmentMeta
	CreatedAt     time.Time
}

// IngestResult is the response returned to the agent after ingest.
type IngestResult struct {
	// AckedThrough is the maximum agent_seq accepted in this batch.
	AckedThrough int64
}

// EmailStats holds the summary counts + per-day + per-provider breakdowns.
type EmailStats struct {
	Total           int64
	SentCount       int64
	FailedCount     int64
	BouncedCount    int64
	ComplainedCount int64
	ProviderCount   int64
	// SiteCount is only populated for fleet stats.
	SiteCount  int64
	ByDay      []StatsByDay
	ByProvider []StatsByProvider
}

// ---------------------------------------------------------------------------
// Deliverability report types (GET /email/deliverability)
// ---------------------------------------------------------------------------

// SiteDeliveryItem is one site's row in the deliverability report.
type SiteDeliveryItem struct {
	SiteID          uuid.UUID
	SiteName        string
	SiteURL         string
	Provider        string
	Total           int64
	SentCount       int64
	FailedCount     int64
	BouncedCount    int64
	ComplainedCount int64
	BounceRate      float64    // bounced/total*100, 0 when total=0
	ComplaintRate   float64    // complained/total*100, 0 when total=0
	LastSentAt      *time.Time // nil when no sent email in window
	Sparkline       []int64    // daily sent counts across the window, oldest→newest
}

// DeliverabilityReport is the full response for GET /email/deliverability.
type DeliverabilityReport struct {
	WindowDays int
	Items      []SiteDeliveryItem
}

// StatsByDay is one day's aggregate.
type StatsByDay struct {
	Day             time.Time
	Total           int64
	SentCount       int64
	FailedCount     int64
	BouncedCount    int64
	ComplainedCount int64
}

// StatsByProvider is one provider's aggregate.
type StatsByProvider struct {
	Provider    string
	Total       int64
	SentCount   int64
	FailedCount int64
}

// ---------------------------------------------------------------------------
// Suppression types (Phase 4a)
// ---------------------------------------------------------------------------

// Suppression represents one email_suppression row.
type Suppression struct {
	ID       uuid.UUID
	TenantID uuid.UUID
	// SiteID is nil for fleet-wide suppression.
	SiteID    *uuid.UUID
	EmailHash []byte
	// Email may be nil when PII storage is off (only hash stored).
	Email           *string
	Reason          string // hard_bounce | complaint | unsubscribe | manual
	Provider        string
	EventAt         *time.Time
	SourceMessageID *string
	CreatedAt       time.Time
}

// SuppressionFilter holds the operator-supplied filters for a suppression list request.
type SuppressionFilter struct {
	Cursor string
	Limit  int
	Reason string
}

// SuppressionPage is the paginated result for a suppression list request.
type SuppressionPage struct {
	Entries    []Suppression
	NextCursor string
}

// UpsertSuppressionInput carries the data needed to upsert a suppression entry.
// Used by both the webhook fanout (reason=hard_bounce/complaint) and the
// operator manual-add route (reason=manual/unsubscribe).
type UpsertSuppressionInput struct {
	TenantID        uuid.UUID
	SiteID          *uuid.UUID // nil = fleet-wide
	Email           string     // normalised (lower-cased) email address
	Reason          string
	Provider        string
	EventAt         *time.Time
	SourceMessageID *string
	// StorePlaintext controls whether the raw email is stored in the email column
	// in addition to the hash. Default: false (hash only).
	StorePlaintext bool
}

// WebhookEventInput is the fanout-resolved event used to write suppression + dedup rows.
type WebhookEventInput struct {
	Provider        string
	ProviderEventID string
	// TenantID is the resolved tenant.  In the new (m61) trust model this is set
	// from the routeToken resolution, NOT from event metadata.  It may still be nil
	// for legacy/un-migrated paths (in which case the event is orphaned).
	TenantID  *uuid.UUID
	SiteID    *uuid.UUID
	Email     string
	EmailHash []byte // sha-256 of lower-cased email; set by the webhook handler
	EventType string // hard_bounce | complaint
	// MetaTenantID is the wpmgr_tenant from event metadata (used for the intra-tenant
	// mismatch assertion: must equal TenantID when both are present).
	MetaTenantID *uuid.UUID
}

// ---------------------------------------------------------------------------
// Log-action domain types (Phase 4a)
// ---------------------------------------------------------------------------

// ResendInput is the request for POST /sites/:siteId/email/log/:logId/resend.
type ResendInput struct {
	TenantID uuid.UUID
	SiteID   uuid.UUID
	LogID    uuid.UUID
}

// ResendResult is the response from a single-entry resend dispatch.
type ResendResult struct {
	OK        bool
	Detail    string
	MessageID string
	// Verified is the SITE's confirmation that it resent the same message the
	// operator selected (GH #528) — agentcmd.ResendEmailResult.IsVerified(),
	// never anything the CP inferred from its own request.
	//
	// False has two causes, both unconfirmed sends: the row carried no recorded
	// Message-ID to compare against, or the site's plugin is too old to answer
	// the question. It is never silently false — Detail names the cause and the
	// audit row records the flag.
	Verified bool
	// LegacyAgent distinguishes those two causes (PR #542 review, second
	// pass): true only when the CP actually had a Message-ID to send AND the
	// agent's response omitted `verified` altogether (too old to know the
	// field exists — updating the plugin is the fix). False both when a
	// current agent answered `verified: false` out loud (it ran and had
	// nothing to compare) and when the CP never sent a Message-ID at all — in
	// that case the agent's silence proves nothing about its version, so
	// recording legacy_agent=true would state an intention rather than a
	// fact. Meaningless when Verified is true.
	LegacyAgent bool
}

// BulkResendInput is the request for POST /sites/:siteId/email/log/resend (bulk).
type BulkResendInput struct {
	TenantID uuid.UUID
	SiteID   uuid.UUID
	LogIDs   []uuid.UUID
}

// BulkResendResult is the per-entry result of a bulk resend.
type BulkResendResult struct {
	LogID  uuid.UUID
	OK     bool
	Detail string
	// Verified mirrors ResendResult.Verified for this entry. Per-entry, not
	// per-batch: one batch routinely mixes rows that could be confirmed with
	// rows that could not.
	Verified bool
	// LegacyAgent mirrors ResendResult.LegacyAgent for this entry — see there.
	LegacyAgent bool
}

// BulkDeleteLogsInput is the request for DELETE /sites/:siteId/email/log (bulk).
type BulkDeleteLogsInput struct {
	TenantID uuid.UUID
	SiteID   uuid.UUID
	LogIDs   []uuid.UUID
}

// SuppressionDeltaPage is the response for the agent suppression-fetch endpoint.
type SuppressionDeltaPage struct {
	Entries    []Suppression
	NextCursor string
}
