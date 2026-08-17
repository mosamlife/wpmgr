package email

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// configDTO is the wire representation of Config for GET responses.
// provider_secret_encrypted and webhook_signing_key_enc are NEVER included;
// only the *_set booleans are surfaced.
type configDTO struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	// SiteID is null in the JSON when this is an org-wide default row.
	SiteID *string `json:"site_id,omitempty"`

	Provider       string `json:"provider"`
	FromAddress    string `json:"from_address"`
	FromName       string `json:"from_name"`
	ForceFromEmail bool   `json:"force_from_email"`
	ForceFromName  bool   `json:"force_from_name"`
	ReturnPath     bool   `json:"return_path"`

	// Config holds non-secret provider settings.
	Config map[string]any `json:"config"`

	// SecretSet reports whether an encrypted provider secret is stored.
	SecretSet bool `json:"secret_set"`

	// Inherited is true when this row is the org-wide default returned for a
	// site that has no config of its own. When it is true, secret_set describes
	// the ORG credential, not one belonging to this site.
	Inherited bool `json:"inherited"`

	Mappings           map[string]any `json:"mappings"`
	DefaultConnection  *string        `json:"default_connection,omitempty"`
	FallbackConnection *string        `json:"fallback_connection,omitempty"`

	LogEmails     bool `json:"log_emails"`
	StoreBody     bool `json:"store_body"`
	RetentionDays int  `json:"retention_days"`

	// m61 webhook security fields (read-only; masked).
	// WebhookURL is the fully-qualified webhook URL for this config row.
	// Empty when no route token has been generated yet.
	WebhookURL string `json:"webhook_url,omitempty"`
	// WebhookSigningKeySet is true when a per-row signing key is stored.
	WebhookSigningKeySet bool `json:"webhook_signing_key_set"`
	// SesTopicArns is the SNS TopicArn allowlist (nil / empty = not configured).
	SesTopicArns []string `json:"ses_topic_arns,omitempty"`
	// WebhookRouteToken is only present in the response immediately after a token
	// rotation (PUT …/webhook-config with rotate_token=true). It is the plain
	// token that must be saved — it cannot be retrieved again.
	WebhookRouteToken string `json:"webhook_route_token,omitempty"`

	// m62: named connections list (may be nil/empty).
	Connections []connectionDTO `json:"connections,omitempty"`

	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`
}

// toConfigDTO maps the domain Config to the wire DTO (masked read).
// baseURL is the public-facing base URL (e.g. "https://manage.wpmgr.app")
// used to construct the webhook_url field. Pass "" to omit the URL.
// conns may be nil; when non-nil the connections slice is embedded.
func toConfigDTOWithConnections(c Config, baseURL string, conns []Connection) configDTO {
	dto := toConfigDTO(c, baseURL)
	if len(conns) > 0 {
		dtoConns := make([]connectionDTO, 0, len(conns))
		for _, conn := range conns {
			dtoConns = append(dtoConns, toConnectionDTO(conn))
		}
		dto.Connections = dtoConns
	}
	return dto
}

func toConfigDTO(c Config, baseURL string) configDTO {
	dto := configDTO{
		ID:                   c.ID.String(),
		TenantID:             c.TenantID.String(),
		Provider:             c.Provider,
		FromAddress:          c.FromAddress,
		FromName:             c.FromName,
		ForceFromEmail:       c.ForceFromEmail,
		ForceFromName:        c.ForceFromName,
		ReturnPath:           c.ReturnPath,
		Config:               c.Config,
		SecretSet:            c.SecretSet,
		Inherited:            c.Inherited,
		Mappings:             c.Mappings,
		DefaultConnection:    c.DefaultConnection,
		FallbackConnection:   c.FallbackConnection,
		LogEmails:            c.LogEmails,
		StoreBody:            c.StoreBody,
		RetentionDays:        c.RetentionDays,
		WebhookSigningKeySet: c.WebhookSigningKeySet,
		SesTopicArns:         c.SesTopicArns,
		CreatedAt:            c.CreatedAt.Unix(),
		UpdatedAt:            c.UpdatedAt.Unix(),
	}
	if c.SiteID != nil {
		s := c.SiteID.String()
		dto.SiteID = &s
	}
	if dto.Config == nil {
		dto.Config = map[string]any{}
	}
	if dto.Mappings == nil {
		dto.Mappings = map[string]any{}
	}
	// m61: webhook URL (only when token is configured AND baseURL is supplied).
	if baseURL != "" && c.WebhookRouteTokenHashSet {
		// If a plain token was just generated (post-rotation), include it.
		// Otherwise just show that a URL exists (the plain token is not stored).
		if c.WebhookRouteToken != "" {
			dto.WebhookURL = WebhookURL(baseURL, c.Provider, c.WebhookRouteToken)
			dto.WebhookRouteToken = c.WebhookRouteToken
		} else {
			// Token is configured but we don't hold the plain value — indicate presence.
			dto.WebhookURL = baseURL + "/webhooks/email/" + c.Provider + "/<token>"
		}
	}
	return dto
}

// putConfigBody is the request body for PUT /sites/:siteId/email/config and
// PUT /email/org-config.
type putConfigBody struct {
	Provider       string `json:"provider"`
	FromAddress    string `json:"from_address"`
	FromName       string `json:"from_name"`
	ForceFromEmail bool   `json:"force_from_email"`
	ForceFromName  bool   `json:"force_from_name"`
	ReturnPath     bool   `json:"return_path"`

	// Config holds non-secret provider settings (host/port/region etc.).
	Config map[string]any `json:"config"`

	// Secret is the plaintext provider secret (SMTP password / API key).
	// nil or absent = preserve the existing stored secret.
	// Non-nil = age-encrypt and store; the old ciphertext is overwritten.
	Secret *string `json:"secret,omitempty"`

	Mappings           map[string]any `json:"mappings"`
	DefaultConnection  *string        `json:"default_connection,omitempty"`
	FallbackConnection *string        `json:"fallback_connection,omitempty"`

	LogEmails     *bool `json:"log_emails,omitempty"`
	StoreBody     *bool `json:"store_body,omitempty"`
	RetentionDays *int  `json:"retention_days,omitempty"`
}

// fromPutBody converts a putConfigBody + tenant/site IDs into a UpsertInput.
// Default values for boolean/int fields mirror the DB defaults.
func fromPutBody(body putConfigBody, tenantID uuid.UUID, siteID *uuid.UUID) UpsertInput {
	in := UpsertInput{
		TenantID:           tenantID,
		SiteID:             siteID,
		Provider:           body.Provider,
		FromAddress:        body.FromAddress,
		FromName:           body.FromName,
		ForceFromEmail:     body.ForceFromEmail,
		ForceFromName:      body.ForceFromName,
		ReturnPath:         body.ReturnPath,
		Config:             body.Config,
		SecretRaw:          body.Secret,
		Mappings:           body.Mappings,
		DefaultConnection:  body.DefaultConnection,
		FallbackConnection: body.FallbackConnection,
		LogEmails:          true,
		StoreBody:          false,
		RetentionDays:      14,
	}
	if body.LogEmails != nil {
		in.LogEmails = *body.LogEmails
	}
	if body.StoreBody != nil {
		in.StoreBody = *body.StoreBody
	}
	if body.RetentionDays != nil {
		in.RetentionDays = *body.RetentionDays
	}
	if in.Config == nil {
		in.Config = map[string]any{}
	}
	if in.Mappings == nil {
		in.Mappings = map[string]any{}
	}
	return in
}

// webhookConfigBody is the request body for PUT /sites/:siteId/email/webhook-config
// and PUT /email/org-config/webhook-config.
//
// UI usage: the operator opens the webhook config panel, which shows:
//   - webhook_url (from GET — the current URL, or placeholder if no token yet)
//   - webhook_signing_key_set (bool — masked; never the key)
//   - ses_topic_arns (from GET)
//
// The operator can:
//   - Rotate the token: rotate_token=true → a new webhook_route_token is returned
//     ONCE in the response; the UI must copy it to configure the provider.
//   - Set/rotate the signing key: provide webhook_signing_key (write-only;
//     nil = preserve existing).
//   - Update the SES TopicArn allowlist: provide ses_topic_arns.
type webhookConfigBody struct {
	// RotateToken, when true, generates a new random routeToken and stores its
	// hash.  The new plain token is returned ONCE in the response.
	RotateToken bool `json:"rotate_token"`
	// WebhookSigningKey is the plaintext webhook signing key to store (age-encrypted).
	// nil = preserve existing.
	WebhookSigningKey *string `json:"webhook_signing_key,omitempty"`
	// SesTopicArns is the updated SNS TopicArn allowlist.
	// nil = preserve existing; empty array = clear all ARNs.
	SesTopicArns *[]string `json:"ses_topic_arns,omitempty"`
}

// testSendBody is the request body for POST /sites/:siteId/email/test.
type testSendBody struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// ---------------------------------------------------------------------------
// Phase 3 — email log DTOs
// ---------------------------------------------------------------------------

// logEntryDTO is the wire representation of a log entry. When includeBody is
// false (list view) the body field is always omitted regardless of body_stored.
type logEntryDTO struct {
	ID          string         `json:"id"`
	TenantID    string         `json:"tenant_id"`
	SiteID      string         `json:"site_id"`
	AgentSeq    *int64         `json:"agent_seq,omitempty"`
	MessageID   *string        `json:"message_id,omitempty"`
	ToAddresses []string       `json:"to_addresses"`
	FromAddress string         `json:"from_address"`
	Subject     string         `json:"subject"`
	Provider    string         `json:"provider"`
	Status      string         `json:"status"`
	Response    map[string]any `json:"response"`
	Error       string         `json:"error"`
	Retries     int            `json:"retries"`
	ResentCount int            `json:"resent_count"`
	BodyStored  bool           `json:"body_stored"`
	// Body is only present in the detail response and only when body_stored=true.
	Body *string `json:"body,omitempty"`
	// m62 additions.
	ConnectionKey   string              `json:"connection_key"`
	AttachmentCount int                 `json:"attachment_count"`
	Attachments     []attachmentMetaDTO `json:"attachments,omitempty"` // detail only
	CreatedAt       string              `json:"created_at"`
	UpdatedAt       string              `json:"updated_at"`
}

// attachmentMetaDTO is the wire shape of one attachment.
type attachmentMetaDTO struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
}

// logDetailDTO is the detail response including prev/next navigation.
type logDetailDTO struct {
	Entry  logEntryDTO `json:"entry"`
	PrevID *string     `json:"prev_id,omitempty"`
	NextID *string     `json:"next_id,omitempty"`
}

// emailStatsDayDTO is one day of stats.
type emailStatsDayDTO struct {
	Day             string `json:"day"`
	Total           int64  `json:"total"`
	SentCount       int64  `json:"sent_count"`
	FailedCount     int64  `json:"failed_count"`
	BouncedCount    int64  `json:"bounced_count"`
	ComplainedCount int64  `json:"complained_count"`
}

// emailStatsProviderDTO is one provider's stats.
type emailStatsProviderDTO struct {
	Provider    string `json:"provider"`
	Total       int64  `json:"total"`
	SentCount   int64  `json:"sent_count"`
	FailedCount int64  `json:"failed_count"`
}

// emailStatsDTO is the full stats response.
type emailStatsDTO struct {
	Total           int64                   `json:"total"`
	SentCount       int64                   `json:"sent_count"`
	FailedCount     int64                   `json:"failed_count"`
	BouncedCount    int64                   `json:"bounced_count"`
	ComplainedCount int64                   `json:"complained_count"`
	ProviderCount   int64                   `json:"provider_count"`
	SiteCount       int64                   `json:"site_count,omitempty"`
	ByDay           []emailStatsDayDTO      `json:"by_day"`
	ByProvider      []emailStatsProviderDTO `json:"by_provider"`
}

// ---------------------------------------------------------------------------
// Deliverability DTOs
// ---------------------------------------------------------------------------

// siteDeliveryItemDTO is the wire shape of one site in GET /email/deliverability.
type siteDeliveryItemDTO struct {
	SiteID          string  `json:"site_id"`
	SiteName        string  `json:"site_name"`
	SiteURL         string  `json:"site_url"`
	Provider        string  `json:"provider"`
	Total           int64   `json:"total"`
	SentCount       int64   `json:"sent_count"`
	FailedCount     int64   `json:"failed_count"`
	BouncedCount    int64   `json:"bounced_count"`
	ComplainedCount int64   `json:"complained_count"`
	BounceRate      float64 `json:"bounce_rate"`
	ComplaintRate   float64 `json:"complaint_rate"`
	LastSentAt      *string `json:"last_sent_at"` // RFC3339 or null
	Sparkline       []int64 `json:"sparkline"`    // [] never null
}

// deliverabilityReportDTO is the full response for GET /email/deliverability.
type deliverabilityReportDTO struct {
	WindowDays int                   `json:"window_days"`
	Items      []siteDeliveryItemDTO `json:"items"` // [] never null
}

// toLogEntryDTO maps a LogEntry to its wire DTO. Pass includeBody=true only for
// the detail endpoint; list endpoints always pass false.
func toLogEntryDTO(e LogEntry, includeBody bool) logEntryDTO {
	dto := logEntryDTO{
		ID:              e.ID.String(),
		TenantID:        e.TenantID.String(),
		SiteID:          e.SiteID.String(),
		AgentSeq:        e.AgentSeq,
		MessageID:       e.MessageID,
		ToAddresses:     e.ToAddresses,
		FromAddress:     e.FromAddress,
		Subject:         e.Subject,
		Provider:        e.Provider,
		Status:          e.Status,
		Response:        e.Response,
		Error:           e.Error,
		Retries:         e.Retries,
		ResentCount:     e.ResentCount,
		BodyStored:      e.BodyStored,
		ConnectionKey:   e.ConnectionKey,
		AttachmentCount: e.AttachmentCount,
		CreatedAt:       e.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:       e.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if dto.ToAddresses == nil {
		dto.ToAddresses = []string{}
	}
	if dto.Response == nil {
		dto.Response = map[string]any{}
	}
	if includeBody && e.BodyStored && e.Body != nil {
		dto.Body = e.Body
	}
	// Attachments only in detail view.
	if includeBody && len(e.Attachments) > 0 {
		dtoAtts := make([]attachmentMetaDTO, 0, len(e.Attachments))
		for _, a := range e.Attachments {
			dtoAtts = append(dtoAtts, attachmentMetaDTO{Name: a.Name, SizeBytes: a.SizeBytes})
		}
		dto.Attachments = dtoAtts
	}
	return dto
}

// toLogDetailDTO maps a LogDetail to its wire DTO.
func toLogDetailDTO(d LogDetail) logDetailDTO {
	dto := logDetailDTO{
		Entry: toLogEntryDTO(d.Entry, true),
	}
	if d.PrevID != nil {
		s := d.PrevID.String()
		dto.PrevID = &s
	}
	if d.NextID != nil {
		s := d.NextID.String()
		dto.NextID = &s
	}
	return dto
}

// toEmailStatsDTO maps EmailStats to the wire DTO.
func toEmailStatsDTO(s EmailStats) emailStatsDTO {
	dto := emailStatsDTO{
		Total:           s.Total,
		SentCount:       s.SentCount,
		FailedCount:     s.FailedCount,
		BouncedCount:    s.BouncedCount,
		ComplainedCount: s.ComplainedCount,
		ProviderCount:   s.ProviderCount,
		SiteCount:       s.SiteCount,
		ByDay:           make([]emailStatsDayDTO, 0, len(s.ByDay)),
		ByProvider:      make([]emailStatsProviderDTO, 0, len(s.ByProvider)),
	}
	for _, d := range s.ByDay {
		dto.ByDay = append(dto.ByDay, emailStatsDayDTO{
			Day:             d.Day.UTC().Format("2006-01-02"),
			Total:           d.Total,
			SentCount:       d.SentCount,
			FailedCount:     d.FailedCount,
			BouncedCount:    d.BouncedCount,
			ComplainedCount: d.ComplainedCount,
		})
	}
	for _, p := range s.ByProvider {
		dto.ByProvider = append(dto.ByProvider, emailStatsProviderDTO{
			Provider:    p.Provider,
			Total:       p.Total,
			SentCount:   p.SentCount,
			FailedCount: p.FailedCount,
		})
	}
	return dto
}

// toDeliverabilityReportDTO maps a DeliverabilityReport to its wire DTO.
// All slices are always non-nil ([]).
func toDeliverabilityReportDTO(r DeliverabilityReport) deliverabilityReportDTO {
	items := make([]siteDeliveryItemDTO, 0, len(r.Items))
	for _, item := range r.Items {
		dto := siteDeliveryItemDTO{
			SiteID:          item.SiteID.String(),
			SiteName:        item.SiteName,
			SiteURL:         item.SiteURL,
			Provider:        item.Provider,
			Total:           item.Total,
			SentCount:       item.SentCount,
			FailedCount:     item.FailedCount,
			BouncedCount:    item.BouncedCount,
			ComplainedCount: item.ComplainedCount,
			BounceRate:      item.BounceRate,
			ComplaintRate:   item.ComplaintRate,
			Sparkline:       item.Sparkline,
		}
		if item.LastSentAt != nil {
			s := item.LastSentAt.UTC().Format(time.RFC3339)
			dto.LastSentAt = &s
		}
		if dto.Sparkline == nil {
			dto.Sparkline = []int64{}
		}
		items = append(items, dto)
	}
	return deliverabilityReportDTO{
		WindowDays: r.WindowDays,
		Items:      items,
	}
}

// parseLogListFilter extracts the log list query parameters from a Gin context.
func parseLogListFilter(c *gin.Context) LogListFilter {
	f := LogListFilter{
		Cursor: c.Query("cursor"),
		Status: c.Query("status"),
		Q:      c.Query("q"),
		Limit:  50,
	}
	if s := c.Query("limit"); s != "" {
		if n, err := parseInt(s); err == nil {
			f.Limit = n
		}
	}
	if s := c.Query("from"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			f.From = t
		} else if t, err := time.Parse("2006-01-02", s); err == nil {
			f.From = t.UTC()
		}
	}
	if s := c.Query("to"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			f.To = t
		} else if t, err := time.Parse("2006-01-02", s); err == nil {
			f.To = t.Add(24*time.Hour - time.Nanosecond).UTC()
		}
	}
	return f
}

// parseStatRange extracts the from/to date range for stats endpoints.
// Returns zero times when not provided (the service/repo applies sentinel defaults).
func parseStatRange(c *gin.Context) (time.Time, time.Time) {
	var from, to time.Time
	if s := c.Query("from"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			from = t
		} else if t, err := time.Parse("2006-01-02", s); err == nil {
			from = t.UTC()
		}
	}
	if s := c.Query("to"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			to = t
		} else if t, err := time.Parse("2006-01-02", s); err == nil {
			to = t.Add(24*time.Hour - time.Nanosecond).UTC()
		}
	}
	return from, to
}

func parseInt(s string) (int, error) {
	n, err := strconv.Atoi(s)
	return n, err
}

// ---------------------------------------------------------------------------
// Phase 4a — suppression DTOs
// ---------------------------------------------------------------------------

// suppressionDTO is the wire representation of a Suppression entry.
type suppressionDTO struct {
	ID       string  `json:"id"`
	TenantID string  `json:"tenant_id"`
	SiteID   *string `json:"site_id,omitempty"`
	// Email is the masked plaintext email (if stored). Nil when PII opt-in is off.
	Email           *string `json:"email,omitempty"`
	Reason          string  `json:"reason"`
	Provider        string  `json:"provider"`
	EventAt         *string `json:"event_at,omitempty"`
	SourceMessageID *string `json:"source_message_id,omitempty"`
	CreatedAt       string  `json:"created_at"`
}

// toSuppressionDTO maps a domain Suppression to its wire DTO.
func toSuppressionDTO(s Suppression) suppressionDTO {
	dto := suppressionDTO{
		ID:              s.ID.String(),
		TenantID:        s.TenantID.String(),
		Email:           s.Email,
		Reason:          s.Reason,
		Provider:        s.Provider,
		SourceMessageID: s.SourceMessageID,
		CreatedAt:       s.CreatedAt.UTC().Format(time.RFC3339),
	}
	if s.SiteID != nil {
		str := s.SiteID.String()
		dto.SiteID = &str
	}
	if s.EventAt != nil {
		str := s.EventAt.UTC().Format(time.RFC3339)
		dto.EventAt = &str
	}
	return dto
}

// addSuppressionBody is the request body for manual suppression add.
type addSuppressionBody struct {
	Email  string `json:"email"`
	Reason string `json:"reason"` // manual | unsubscribe
}

// ---------------------------------------------------------------------------
// m62 — connection DTOs
// ---------------------------------------------------------------------------

// connectionDTO is the wire representation of a named Connection. The secret is
// never returned — only secret_set (bool) is surfaced.
type connectionDTO struct {
	ID            string         `json:"id"`
	TenantID      string         `json:"tenant_id"`
	ConfigID      string         `json:"config_id"`
	ConnectionKey string         `json:"connection_key"`
	Provider      string         `json:"provider"`
	FromAddress   string         `json:"from_address"`
	FromName      string         `json:"from_name"`
	Config        map[string]any `json:"config"`
	SecretSet     bool           `json:"secret_set"`
	CreatedAt     int64          `json:"created_at"`
	UpdatedAt     int64          `json:"updated_at"`
}

// toConnectionDTO maps a domain Connection to its wire DTO.
func toConnectionDTO(c Connection) connectionDTO {
	dto := connectionDTO{
		ID:            c.ID.String(),
		TenantID:      c.TenantID.String(),
		ConfigID:      c.ConfigID.String(),
		ConnectionKey: c.ConnectionKey,
		Provider:      c.Provider,
		FromAddress:   c.FromAddress,
		FromName:      c.FromName,
		Config:        c.Config,
		SecretSet:     c.SecretSet,
		CreatedAt:     c.CreatedAt.Unix(),
		UpdatedAt:     c.UpdatedAt.Unix(),
	}
	if dto.Config == nil {
		dto.Config = map[string]any{}
	}
	return dto
}

// putConnectionBody is the request body for PUT /sites/:siteId/email/connections/:key.
type putConnectionBody struct {
	Provider    string         `json:"provider"`
	Config      map[string]any `json:"config"`
	Secret      *string        `json:"secret,omitempty"`
	FromAddress string         `json:"from_address"`
	FromName    string         `json:"from_name"`
}

// ---------------------------------------------------------------------------
// m62 — notify settings DTOs
// ---------------------------------------------------------------------------

// notifySettingsDTO is the wire representation of NotifySettings.
type notifySettingsDTO struct {
	TenantID                 string              `json:"tenant_id"`
	Enabled                  bool                `json:"enabled"`
	Recipients               []string            `json:"recipients"`
	AlertOnFailure           bool                `json:"alert_on_failure"`
	AlertThrottleMinutes     int                 `json:"alert_throttle_minutes"`
	DigestEnabled            bool                `json:"digest_enabled"`
	DigestCadence            string              `json:"digest_cadence"`
	DigestDay                int                 `json:"digest_day"`
	DigestHour               int                 `json:"digest_hour"`
	Timezone                 string              `json:"timezone"`
	NextDigestAt             *string             `json:"next_digest_at,omitempty"`
	InstanceMailerConfigured bool                `json:"instance_mailer_configured"`
	FailureDetection         failureDetectionDTO `json:"failure_detection"`
	CreatedAt                int64               `json:"created_at"`
	UpdatedAt                int64               `json:"updated_at"`
}

// failureDetectionDTO is the wire representation of FailureDetectionCoverage
// (GH #381 phase 2).
type failureDetectionDTO struct {
	SitesTotal      int    `json:"sites_total"`
	SitesCovered    int    `json:"sites_covered"`
	MinAgentVersion string `json:"min_agent_version"`
}

// toNotifySettingsDTO maps domain NotifySettings to the wire DTO.
func toNotifySettingsDTO(s NotifySettings) notifySettingsDTO {
	dto := notifySettingsDTO{
		TenantID:                 s.TenantID.String(),
		Enabled:                  s.Enabled,
		Recipients:               s.Recipients,
		AlertOnFailure:           s.AlertOnFailure,
		AlertThrottleMinutes:     s.AlertThrottleMinutes,
		DigestEnabled:            s.DigestEnabled,
		DigestCadence:            s.DigestCadence,
		DigestDay:                s.DigestDay,
		DigestHour:               s.DigestHour,
		Timezone:                 s.Timezone,
		InstanceMailerConfigured: s.InstanceMailerConfigured,
		FailureDetection: failureDetectionDTO{
			SitesTotal:      s.FailureDetection.SitesTotal,
			SitesCovered:    s.FailureDetection.SitesCovered,
			MinAgentVersion: s.FailureDetection.MinAgentVersion,
		},
		CreatedAt: s.CreatedAt.Unix(),
		UpdatedAt: s.UpdatedAt.Unix(),
	}
	if dto.Recipients == nil {
		dto.Recipients = []string{}
	}
	if s.NextDigestAt != nil {
		str := s.NextDigestAt.UTC().Format(time.RFC3339)
		dto.NextDigestAt = &str
	}
	return dto
}

// notifySettingsUpdateBody is the request body for PUT /email/notify-settings.
type notifySettingsUpdateBody struct {
	Enabled              bool     `json:"enabled"`
	Recipients           []string `json:"recipients"`
	AlertOnFailure       bool     `json:"alert_on_failure"`
	AlertThrottleMinutes int      `json:"alert_throttle_minutes"`
	DigestEnabled        bool     `json:"digest_enabled"`
	DigestCadence        string   `json:"digest_cadence"`
	DigestDay            int      `json:"digest_day"`
	DigestHour           int      `json:"digest_hour"`
	Timezone             string   `json:"timezone"`
}

// parseSuppressionFilter extracts suppression list query params from Gin context.
func parseSuppressionFilter(c *gin.Context) SuppressionFilter {
	f := SuppressionFilter{
		Cursor: c.Query("cursor"),
		Reason: c.Query("reason"),
		Limit:  50,
	}
	if s := c.Query("limit"); s != "" {
		if n, err := parseInt(s); err == nil {
			f.Limit = n
		}
	}
	return f
}
