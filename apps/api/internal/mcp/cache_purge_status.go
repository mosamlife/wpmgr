package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// ---------------------------------------------------------------------------
// site_cache_purge_request_status: what became of a request THIS connection
// made. It reads only rows with proposed_by_grant_id = auth.GrantID on sites
// still in auth.Sites, under connectionScopedPrincipal; anything else answers
// -32007, byte-identical to an absent site. Every site-originated field is
// fenced, and none of the approver's identity, the digest, the nonce, the
// grant label, the setup client, the agent's detail or the site-reported text
// is ever read, let alone returned.
// ---------------------------------------------------------------------------

// statusListLimit is list mode's bound.
const statusListLimit = 20

// The model-facing states. Each is derived from the stored state and outcome.
const (
	statusWaiting      = "waiting_for_approval"
	statusApproved     = "approved_not_started"
	statusRunning      = "running"
	statusDone         = "done"
	statusDeclined     = "declined"
	statusWithdrawn    = "withdrawn"
	statusExpired      = "expired"
	statusPollAfterSec = 30
)

// Closed sets. A stored value outside them is dropped, never echoed, so the
// strings the model reads are our own constants.
var (
	statusWaitingReasons = closedSet("site_unreachable", "site_cooldown", "site_hourly_cap",
		"site_busy", "org_busy", "context_unavailable", "write_tools_disabled")
	statusOutcomes = closedSet("purged", "site_reported_failure", "agent_failed",
		"outcome_unknown", "not_sent")
	statusNotSentReasons = closedSet("grant_inactive", "assistant_paused", "organisation_deleted",
		"capability_not_held", "site_absent", "forbidden_by_context", "agent_outdated",
		"dispatch_deadline_passed", "transport_pre_send")
	statusCDN = closedSet("not_attempted", "cleared", "failed", "not_configured")
)

func closedSet(vals ...string) map[string]string {
	m := make(map[string]string, len(vals))
	for _, v := range vals {
		m[v] = v
	}
	return m
}

// fromClosed returns OUR constant for v, or "" when v is not in the set.
func fromClosed(set map[string]string, v *string) string {
	if v == nil {
		return ""
	}
	return set[*v]
}

// requestStatus is one request as the model reads it.
type requestStatus struct {
	RequestID            string   `json:"request_id"`
	SiteID               string   `json:"site_id"`
	SiteName             string   `json:"site_name"`
	Scope                string   `json:"scope"`
	URL                  *string  `json:"url"`
	State                string   `json:"state"`
	WaitingReason        string   `json:"waiting_reason,omitempty"`
	Outcome              string   `json:"outcome,omitempty"`
	NotSentReason        string   `json:"not_sent_reason,omitempty"`
	HostingCachesCleared []string `json:"hosting_caches_cleared,omitempty"`
	HostingCachesSkipped []string `json:"hosting_caches_skipped,omitempty"`
	OriginOnlyConfirmed  *bool    `json:"origin_only_confirmed,omitempty"`
	WpmgrCDN             string   `json:"wpmgr_cdn,omitempty"`
	DecidedAt            string   `json:"decided_at,omitempty"`
	FinishedAt           string   `json:"finished_at,omitempty"`
	PollAfterSeconds     int      `json:"poll_after_seconds,omitempty"`
}

func tsString(ts pgtype.Timestamptz) string {
	if !ts.Valid {
		return ""
	}
	return ts.Time.UTC().Format(time.RFC3339)
}

// hostingSlugs keeps only slugs in the closed reach-table set, emitting our
// own constant for each.
func hostingSlugs(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	known := closedSet(HostingCacheSlugs()...)
	out := make([]string, 0, len(in))
	for _, s := range in {
		if v, ok := known[s]; ok {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// statusFromRow is the model-facing projection of one row.
func statusFromRow(r requestStatusRow, now time.Time) requestStatus {
	out := requestStatus{
		RequestID: r.ID.String(),
		SiteID:    r.SiteID.String(),
		SiteName:  fenceSiteText(r.SiteLabel),
		Scope:     r.Scope,
		URL:       fencedURL(r.Url),
	}
	if r.Scope != scopeAll && r.Scope != scopeURL {
		out.Scope = ""
	}
	switch r.State {
	case "pending":
		if !r.ExpiresAt.After(now) {
			out.State = statusExpired
			out.FinishedAt = r.ExpiresAt.UTC().Format(time.RFC3339)
		} else {
			out.State = statusWaiting
			out.PollAfterSeconds = statusPollAfterSec
		}
	case "approved_undispatched":
		out.State = statusApproved
		out.WaitingReason = fromClosed(statusWaitingReasons, r.LastAttemptCode)
		out.DecidedAt = tsString(r.DecidedAt)
		out.PollAfterSeconds = statusPollAfterSec
	case "dispatched":
		out.DecidedAt = tsString(r.DecidedAt)
		if r.Outcome == nil {
			out.State = statusRunning
			out.PollAfterSeconds = statusPollAfterSec
			break
		}
		out.State = statusDone
		out.Outcome = fromClosed(statusOutcomes, r.Outcome)
		out.NotSentReason = fromClosed(statusNotSentReasons, r.NotSentReason)
		out.HostingCachesCleared = hostingSlugs(r.HostingCachesCleared)
		out.HostingCachesSkipped = hostingSlugs(r.HostingCachesSkipped)
		out.OriginOnlyConfirmed = r.OriginOnlyConfirmed
		out.WpmgrCDN = fromClosed(statusCDN, r.WpmgrCdn)
		out.FinishedAt = tsString(r.OutcomeAt)
	case "rejected":
		out.State = statusDeclined
		out.DecidedAt = tsString(r.DecidedAt)
		out.FinishedAt = tsString(r.DecidedAt)
	case "withdrawn":
		out.State = statusWithdrawn
		out.FinishedAt = tsString(r.WithdrawnAt)
	case "expired":
		out.State = statusExpired
		out.FinishedAt = r.ExpiresAt.UTC().Format(time.RFC3339)
	default:
		// A state the CHECK does not admit. Say nothing we cannot stand
		// behind.
		out.State = ""
	}
	return out
}

// decodeStatusArgs reads {request_id} or {}.
func decodeStatusArgs(raw json.RawMessage) (requestIDText string, list bool, ref *toolRefusal) {
	schema := cachePurgeRequestStatusSchema
	obj, ref := decodeStrictObject(raw, map[string]bool{"request_id": true}, msgArgStatusUnknown, schema)
	if ref != nil {
		return "", false, ref
	}
	s, ok, ref := stringArg(obj, "request_id", schema)
	if ref != nil {
		return "", false, ref
	}
	if !ok {
		return "", true, nil
	}
	if _, valid := parseStrictUUID(s); !valid {
		return "", false, argRefusal(reasonInvalidArguments, "request_id", s, msgArgStatusID, schema)
	}
	return s, false, nil
}

// siteCachePurgeRequestStatus is the status tool's entry point.
func (s *Service) siteCachePurgeRequestStatus(ctx context.Context, auth AuthorizedRequest, raw json.RawMessage) (string, error) {
	rs, err := s.requestRail()
	if err != nil {
		return "", railUnavailable(err)
	}
	idText, list, ref := decodeStatusArgs(raw)
	if ref != nil {
		return "", ref
	}
	if auth.Sites.IsEmpty() {
		return "", scopeEmptyRefusal()
	}
	p := connectionScopedPrincipal(auth)
	now := s.now()
	if list {
		rows, err := rs.ListOpenRequestStatus(ctx, p, auth.GrantID, statusListLimit)
		if err != nil {
			return "", fmt.Errorf("list open cache requests: %w", err)
		}
		out := struct {
			Requests []requestStatus `json:"requests"`
			Count    int             `json:"count"`
			Limit    int             `json:"limit"`
		}{Requests: make([]requestStatus, 0, len(rows)), Limit: statusListLimit}
		for _, r := range rows {
			out.Requests = append(out.Requests, statusFromRow(r, now))
		}
		out.Count = len(out.Requests)
		b, err := json.Marshal(out)
		if err != nil {
			return "", fmt.Errorf("encode cache request list: %w", err)
		}
		return string(b), nil
	}
	id, _ := parseStrictUUID(idText)
	row, found, err := rs.ReadRequestStatus(ctx, p, auth.GrantID, id)
	if err != nil {
		return "", fmt.Errorf("read cache request status: %w", err)
	}
	if !found {
		return "", absentRefusal(reasonRequestAbsent, map[string]any{"request_id": id.String()})
	}
	b, err := json.Marshal(statusFromRow(row, now))
	if err != nil {
		return "", fmt.Errorf("encode cache request status: %w", err)
	}
	return string(b), nil
}
