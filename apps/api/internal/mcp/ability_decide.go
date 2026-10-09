package mcp

import (
	"context"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/aipolicy"
)

// The approval-tier engine's side of a write (ADR-065). This package never
// approves anything: it creates a pending request, then hands the decider
// the request's ids, and nothing the AI sent. The decider lives in the
// approval package and approves inside it, or records why the request waits
// for a person.

// AbilityDecision is what the decider made of one request.
type AbilityDecision struct {
	// Approved is true when the site's setting approved the request and it
	// was handed to the sender.
	Approved bool
	// AskReason is why the request waits for a person; empty when Approved,
	// or when no decision was recorded (the request then waits as created).
	AskReason string
	// ResumesAt is when automatic changes may resume, with
	// over_change_budget. Zero otherwise.
	ResumesAt time.Time
}

// AbilityDecider decides a just-created request. It takes ids only.
type AbilityDecider interface {
	DecideAbilityRequest(ctx context.Context, tenantID, requestID uuid.UUID) (AbilityDecision, error)
}

// SetAbilityDecider wires the decider. Without it every request waits for a
// person, as before.
func (s *Service) SetAbilityDecider(d AbilityDecider) { s.abilityDecider = d }

// SetPublicBaseURL is WPMGR_PUBLIC_BASE_URL, for approval_url.
func (s *Service) SetPublicBaseURL(base string) {
	s.publicBaseURL = strings.TrimRight(strings.TrimSpace(base), "/")
}

// abilityAutoWait bounds how long a write call waits for an automatic change
// to finish before it answers "running".
const abilityAutoWait = 25 * time.Second

// abilityAutoPoll is the interval of that wait.
const abilityAutoPoll = 500 * time.Millisecond

// MessageAutoNotDone is the AI's text for an automatic change that ended
// without making the change.
const msgAutoNotDone = "This change was approved under a setting a person chose for this site, but it did not " +
	"complete. outcome and code say what happened. Call site_ability_request_status with request_id for details. " +
	"Do not send it again without reading the page first."

// approvalURL is the absolute address where a person decides the request.
func (s *Service) approvalURL(requestID string) string {
	if s.publicBaseURL == "" {
		return ""
	}
	return s.publicBaseURL + "/ai/requests?request=" + url.QueryEscape(requestID)
}

// askReasonOf is the stored reason, when it is one of the closed set.
func askReasonOf(r *string) string {
	if r == nil || !aipolicy.AskReason(*r).Known() {
		return ""
	}
	return *r
}

// decideAbility runs the decider on a newly created request and shapes the
// answer. A request that already existed is answered as stored. Any failure
// leaves the request waiting for a person, which is how it was created.
func (s *Service) decideAbility(ctx context.Context, store AbilityRequestStore, auth AuthorizedRequest, res abilityCreatedResult) abilityCreatedResult {
	res.ApprovalURL = s.approvalURL(res.RequestID)
	if res.AskReason != "" {
		res.Message = aipolicy.AskReason(res.AskReason).Message()
	}
	if res.Existing || s.abilityDecider == nil {
		return res
	}
	id, err := uuid.Parse(res.RequestID)
	if err != nil {
		return res
	}
	d, err := s.abilityDecider.DecideAbilityRequest(ctx, auth.TenantID, id)
	if err != nil {
		slog.Default().WarnContext(ctx, "mcp: ability request not decided; it waits for a person",
			slog.String("request_id", res.RequestID), slog.Any("error", err))
		return res
	}
	if !d.Approved {
		if d.AskReason != "" && aipolicy.AskReason(d.AskReason).Known() {
			res.AskReason = d.AskReason
			res.Message = aipolicy.AskReason(d.AskReason).Message()
			if !d.ResumesAt.IsZero() {
				res.AutoResumesAt = d.ResumesAt.UTC().Format(time.RFC3339)
			}
		}
		return res
	}
	return s.awaitAutoRun(ctx, store, auth, id, res)
}

// awaitAutoRun answers for a request the site's setting approved: it waits
// up to abilityAutoWait for the change to finish and reports how it ended,
// or answers "running" with poll_after_seconds.
func (s *Service) awaitAutoRun(ctx context.Context, store AbilityRequestStore, auth AuthorizedRequest, id uuid.UUID, res abilityCreatedResult) abilityCreatedResult {
	res.Approval = aipolicy.ApprovalAuto
	res.AskReason = ""
	res.ApprovalURL = ""
	res.State = "running"
	res.Message = aipolicy.MessageRunningBySetting
	p := connectionScopedPrincipal(auth)
	deadline := s.now().Add(abilityAutoWait)
	for {
		row, found, err := store.ReadAbilityRequestStatus(ctx, p, auth.GrantID, id)
		if err == nil && found {
			st := abilityStatusFromRow(row, s.now())
			if st.State == "done" || st.State == "declined" || st.State == "withdrawn" || st.State == "expired" {
				res.State = st.State
				res.Outcome, res.Code, res.CreatedPostID = st.Outcome, st.Code, st.CreatedPostID
				if st.Outcome != nil && (*st.Outcome == "created" || *st.Outcome == "applied") {
					res.Message = aipolicy.MessageDoneBySetting
				} else {
					res.Message = msgAutoNotDone
				}
				return res
			}
		}
		if !s.now().Add(abilityAutoPoll).Before(deadline) {
			return res
		}
		t := time.NewTimer(abilityAutoPoll)
		select {
		case <-ctx.Done():
			t.Stop()
			return res
		case <-t.C:
		}
	}
}
