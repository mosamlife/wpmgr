package assistantrequest

import (
	"time"

	"github.com/google/uuid"
)

// RequestDTO is one request on the wire. Every text field that came from a
// site or an AI connection (site_label, site_host, grant_label, url,
// site_reported_text) is text to render as a text node, never markup, a link
// or a tooltip. presented_digest is present only for a signed-in person;
// digest_nonce is never returned.
type RequestDTO struct {
	ID                     uuid.UUID  `json:"id"`
	SiteID                 uuid.UUID  `json:"site_id"`
	Scope                  string     `json:"scope"`
	URL                    *string    `json:"url"`
	SiteLabel              string     `json:"site_label"`
	SiteHost               string     `json:"site_host"`
	GrantLabel             string     `json:"grant_label"`
	GrantVia               string     `json:"grant_via"`
	SetupClient            *string    `json:"setup_client"`
	PresentedDigest        *string    `json:"presented_digest,omitempty"`
	State                  string     `json:"state"`
	CreatedAt              time.Time  `json:"created_at"`
	ExpiresAt              time.Time  `json:"expires_at"`
	DecidedAt              *time.Time `json:"decided_at"`
	DecidedByUserID        *uuid.UUID `json:"decided_by_user_id"`
	DecidedByName          *string    `json:"decided_by_name"`
	DecidedByAccountDelete bool       `json:"decided_by_account_deleted"`
	WithdrawnAt            *time.Time `json:"withdrawn_at"`
	ClaimedAt              *time.Time `json:"claimed_at"`
	DispatchAttempts       int32      `json:"dispatch_attempts"`
	LastAttemptAt          *time.Time `json:"last_attempt_at"`
	LastAttemptCode        *string    `json:"last_attempt_code"`
	Outcome                *string    `json:"outcome"`
	NotSentReason          *string    `json:"not_sent_reason"`
	OutcomeAt              *time.Time `json:"outcome_at"`
	HostingCachesCleared   []string   `json:"hosting_caches_cleared"`
	HostingCachesSkipped   []string   `json:"hosting_caches_skipped"`
	OriginOnlyConfirmed    *bool      `json:"origin_only_confirmed"`
	WpmgrCDN               *string    `json:"wpmgr_cdn"`
	SiteReportedText       *string    `json:"site_reported_text"`
}

// ListResponse is a page of the queue.
type ListResponse struct {
	Requests     []RequestDTO `json:"requests"`
	PendingCount int64        `json:"pending_count"`
	Limit        int32        `json:"limit"`
	Offset       int32        `json:"offset"`
}

// ApproveBody is the approve request body.
type ApproveBody struct {
	PresentedDigest string `json:"presented_digest"`
}

func toDTO(r Request) RequestDTO {
	return RequestDTO{
		ID:                     r.ID,
		SiteID:                 r.SiteID,
		Scope:                  r.Scope,
		URL:                    r.URL,
		SiteLabel:              r.SiteLabel,
		SiteHost:               r.SiteHost,
		GrantLabel:             r.GrantLabel,
		GrantVia:               r.GrantVia,
		SetupClient:            r.SetupClient,
		PresentedDigest:        r.PresentedDigest,
		State:                  r.State,
		CreatedAt:              r.CreatedAt,
		ExpiresAt:              r.ExpiresAt,
		DecidedAt:              r.DecidedAt,
		DecidedByUserID:        r.DecidedByUserID,
		DecidedByName:          r.DecidedByName,
		DecidedByAccountDelete: r.DecidedByDeleted,
		WithdrawnAt:            r.WithdrawnAt,
		ClaimedAt:              r.ClaimedAt,
		DispatchAttempts:       r.DispatchAttempts,
		LastAttemptAt:          r.LastAttemptAt,
		LastAttemptCode:        r.LastAttemptCode,
		Outcome:                r.Outcome,
		NotSentReason:          r.NotSentReason,
		OutcomeAt:              r.OutcomeAt,
		HostingCachesCleared:   r.HostingCachesCleared,
		HostingCachesSkipped:   r.HostingCachesSkipped,
		OriginOnlyConfirmed:    r.OriginOnlyConfirmed,
		WpmgrCDN:               r.WpmgrCDN,
		SiteReportedText:       r.SiteReportedText,
	}
}
