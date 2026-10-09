package abilityrequest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/aipolicy"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// The policy engine's side of the write rail (ADR-065): after a request is
// created pending, Decide either approves it under the site's setting, inside
// the approval package, or records why it waits for a person. It takes ids
// only, so nothing the AI sent reaches a decision.
//
// The order of one decision:
//
//  1. Read the request and every stored input, read-only, with no user in
//     the transaction.
//  2. Check each person the approval would stand for (the site's setter and
//     the connection's setter), each in their own read, before any lock.
//     Those reads carry the person's own id, so they never run inside the
//     approving transaction.
//  3. Open the approving transaction (organisation-wide, no user, no site
//     allowlist), take the tenant's policy lock, and re-read what step 2
//     checked. If any of it changed, start again from step 1, at most
//     twice; a third change leaves the request waiting with not_checked.
//  4. Count the connection's automatic changes, evaluate, then approve by
//     compare-and-set, or record the reason it waits, with the audit row in
//     the same transaction.
//
// The database re-checks the approval in the same statement (the request's
// backstop) and again when the change is sent (the reservation).

// policyTenantLockKey is the transaction lock every automatic approval in a
// tenant takes, from either request table, so the budget counts are exact.
// Taken before the site dispatch lock, never while holding it.
const policyTenantLockKey = aipolicy.PolicyTenantLockKey

// decideAttempts bounds the restarts when the setting moves under Decide.
const decideAttempts = 3

// PolicySnapshot is what one decision reads about one request.
type PolicySnapshot struct {
	// Request is the request row as stored.
	Request sqlc.AssistantAbilityRequest
	// StoredClass is the catalogue entry's change_class, or for a REST
	// write the route row's.
	StoredClass aipolicy.Class
	// AIDraft is true when the site holds a done, not-undone page creation
	// whose created post is the request's target.
	AIDraft bool
	// SiteMode, SiteModeSource, SiteModeVersion, SiteSetter and SiteSetAt
	// are sites.ai_mode*.
	SiteMode        aipolicy.Mode
	SiteModeSource  string
	SiteModeVersion int64
	SiteSetter      uuid.UUID
	SiteSetAt       time.Time
	// ConnectionAuto and ConnectionSetter are mcp_grants.ai_auto*.
	ConnectionAuto   aipolicy.ConnectionAuto
	ConnectionSetter uuid.UUID
	// TooOld is true when the request was created longer ago than a
	// decision by setting may follow it (unsettledAge), by the database's
	// clock. Such a request waits for a person with not_checked.
	TooOld bool
}

// sameSetting reports whether every value a setter check relied on is
// unchanged.
func (a PolicySnapshot) sameSetting(b PolicySnapshot) bool {
	return a.SiteMode == b.SiteMode &&
		a.SiteModeSource == b.SiteModeSource &&
		a.SiteModeVersion == b.SiteModeVersion &&
		a.SiteSetter == b.SiteSetter &&
		a.SiteSetAt.Equal(b.SiteSetAt) &&
		a.ConnectionAuto == b.ConnectionAuto &&
		a.ConnectionSetter == b.ConnectionSetter &&
		a.StoredClass == b.StoredClass
}

// undecided reports whether the request still waits for its first decision.
func (a PolicySnapshot) undecided() bool {
	return a.Request.State == "pending" && !a.Request.PolicyCheckedAt.Valid
}

// PolicyApproval is the compare-and-set that approves a request under the
// site's setting. The statement must also require the request to be
// pending, unchecked and unexpired, the site's mode, its source, version,
// setter and set time to be these, the mode to allow the class, and the
// connection to run by the site's setting. The approval records each of
// them, as the request's backstop requires.
type PolicyApproval struct {
	TenantID, RequestID, SiteID uuid.UUID
	SiteMode                    aipolicy.Mode
	ModeSource                  string
	ModeVersion                 int64
	SetterUserID                uuid.UUID
	SetterSetAt                 time.Time
	BaseClass, Class            aipolicy.Class
	DispatchWindowSeconds       int32
}

// PolicyAsk records why a request waits. The statement must require the
// request to be pending and unchecked.
type PolicyAsk struct {
	TenantID, RequestID uuid.UUID
	BaseClass, Class    aipolicy.Class
	Reason              aipolicy.AskReason
}

// PolicyStore is the database side of Decide.
type PolicyStore interface {
	// Snapshot reads the request and its stored inputs, read-only, in a
	// tenant transaction with no user.
	Snapshot(ctx context.Context, tenantID, requestID uuid.UUID) (PolicySnapshot, error)
	// Approving runs fn in the approving transaction: a tenant transaction
	// with no user and no site allowlist, holding the tenant's policy lock.
	Approving(ctx context.Context, tenantID uuid.UUID, fn func(tx PolicyTx) error) error
}

// PolicyTx is what Decide does inside the approving transaction.
type PolicyTx interface {
	Snapshot(ctx context.Context, tenantID, requestID uuid.UUID) (PolicySnapshot, error)
	// DraftUsage counts the connection's approvals by setting in the draft
	// classes over the window, and whether siteID is among their sites.
	DraftUsage(ctx context.Context, tenantID, grantID, siteID uuid.UUID) (aipolicy.Usage, error)
	// ApproveByPolicy is the compare-and-set; pgx.ErrNoRows when it
	// touched no row.
	ApproveByPolicy(ctx context.Context, a PolicyApproval) (sqlc.AssistantAbilityRequest, error)
	// RecordAsk writes the reason; pgx.ErrNoRows when it touched no row.
	RecordAsk(ctx context.Context, a PolicyAsk) error
	// Audit writes one audit row in this transaction.
	Audit(ctx context.Context, e audit.Event) error
}

// SetterResolver builds the person's current principal in the tenant and
// their account's status. Production is middleware.Authenticator's
// ResolveSetter, the session authenticator's own code path.
type SetterResolver interface {
	ResolveSetter(ctx context.Context, tenantID, userID uuid.UUID) aipolicy.Setter
}

// SetPolicy wires the engine. Until both are set, Decide decides nothing and
// every request waits for a person, as before.
func (s *Service) SetPolicy(store PolicyStore, setters SetterResolver) {
	s.policy, s.setters = store, setters
}

// SetDispatchEnqueuer wires the enqueuer an approval by setting uses to send
// the change at once. Without it the periodic scan sends it.
func (s *Service) SetDispatchEnqueuer(e Enqueuer) { s.dispatchEnqueuer = e }

// DecideResult is the outcome of Decide.
type DecideResult struct {
	// Decided is false when the engine is not wired, or the request had
	// already been decided; the request then waits as it is.
	Decided   bool
	Outcome   aipolicy.Outcome
	Ask       aipolicy.AskReason
	Class     aipolicy.Class
	ResumesAt time.Time
	// Approved is the approved row, set with OutcomeAutoBySetting.
	Approved sqlc.AssistantAbilityRequest
}

// errSettingMoved restarts a decision whose inputs changed under the lock.
var errSettingMoved = errors.New("the setting changed while the request was being decided")

// errAlreadyDecided stops a decision whose request was decided elsewhere.
var errAlreadyDecided = errors.New("the request was already decided")

// Decide decides one request that was just created pending. It never
// returns an approval it did not commit. A decision that errors records
// nothing: the request stays pending and waits for a person, and nothing
// approves it automatically later.
func (s *Service) Decide(ctx context.Context, tenantID, requestID uuid.UUID) (DecideResult, error) {
	if s.policy == nil || s.setters == nil || !s.enabled {
		return DecideResult{}, nil
	}
	for attempt := 1; attempt <= decideAttempts; attempt++ {
		snap, err := s.policy.Snapshot(ctx, tenantID, requestID)
		if errors.Is(err, pgx.ErrNoRows) {
			return DecideResult{}, nil
		}
		if err != nil {
			return DecideResult{}, fmt.Errorf("read the request's policy inputs: %w", err)
		}
		if !snap.undecided() {
			return DecideResult{}, nil
		}
		siteCheck, connCheck := s.checkSetters(ctx, tenantID, snap)
		res, err := s.decideLocked(ctx, tenantID, requestID, snap, siteCheck, connCheck, attempt == decideAttempts)
		if errors.Is(err, errSettingMoved) {
			continue
		}
		if errors.Is(err, errAlreadyDecided) {
			return DecideResult{}, nil
		}
		if err == nil && res.Outcome == aipolicy.OutcomeAutoBySetting {
			s.enqueueApproved(ctx, res.Approved)
		}
		return res, err
	}
	return DecideResult{}, errors.New("decide: attempts exhausted")
}

// enqueueApproved sends an approved request at once. A failure is logged
// only: the periodic scan sends every approved request that is due.
func (s *Service) enqueueApproved(ctx context.Context, row sqlc.AssistantAbilityRequest) {
	if s.dispatchEnqueuer == nil {
		return
	}
	if err := s.dispatchEnqueuer.EnqueueDispatch(ctx, DispatchArgs{
		TenantID: row.TenantID, RequestID: row.ID, SiteID: row.SiteID, GrantID: row.ProposedByGrantID,
	}); err != nil {
		s.logger.WarnContext(ctx, "ability request: approved by setting; the scan will send it",
			slog.String("request_id", row.ID.String()), slog.Any("error", err))
	}
}

// checkSetters runs the setter checks before any lock. A person who is not
// relied on is not looked up.
func (s *Service) checkSetters(ctx context.Context, tenantID uuid.UUID, snap PolicySnapshot) (site, conn aipolicy.SetterCheck) {
	if snap.SiteMode == aipolicy.ModeAIDrafts || snap.SiteMode == aipolicy.ModeFull {
		setter := s.setters.ResolveSetter(ctx, tenantID, snap.SiteSetter)
		site = aipolicy.CheckSiteSetter(setter, tenantID, snap.Request.SiteID, snap.SiteMode, snap.Request.OperatorPermission)
	}
	if snap.ConnectionAuto == aipolicy.AutoSiteSetting {
		setter := s.setters.ResolveSetter(ctx, tenantID, snap.ConnectionSetter)
		conn = aipolicy.CheckConnectionSetter(setter, tenantID)
	}
	return site, conn
}

// decideLocked is steps 3 and 4. last is true on the final attempt, where a
// moved setting asks with not_checked instead of restarting.
func (s *Service) decideLocked(ctx context.Context, tenantID, requestID uuid.UUID, checked PolicySnapshot,
	siteCheck, connCheck aipolicy.SetterCheck, last bool) (DecideResult, error) {
	var res DecideResult
	err := s.policy.Approving(ctx, tenantID, func(tx PolicyTx) error {
		cur, err := tx.Snapshot(ctx, tenantID, requestID)
		if err != nil {
			return err
		}
		if !cur.undecided() {
			return errAlreadyDecided
		}
		classing := aipolicy.Classify(classFacts(cur))
		var d aipolicy.Decision
		switch {
		case cur.TooOld:
			// Never approved by a setting long after it was made.
			d = aipolicy.Decision{Outcome: aipolicy.OutcomeAsk, Class: classing.Class, Ask: aipolicy.AskNotChecked}
		case !cur.sameSetting(checked):
			if !last {
				return errSettingMoved
			}
			d = aipolicy.Decision{Outcome: aipolicy.OutcomeAsk, Class: classing.Class, Ask: aipolicy.AskNotChecked}
		default:
			in := aipolicy.Inputs{
				Class:            classing,
				SiteMode:         cur.SiteMode,
				SiteSetter:       siteCheck,
				ConnectionAuto:   cur.ConnectionAuto,
				ConnectionSetter: connCheck,
			}
			if classing.Ask == "" && aipolicy.Allows(cur.SiteMode, classing.Class) &&
				aipolicy.BucketOf(classing.Class) == aipolicy.BucketDraft {
				in.Usage, err = tx.DraftUsage(ctx, tenantID, cur.Request.ProposedByGrantID, cur.Request.SiteID)
				if err != nil {
					return fmt.Errorf("count the connection's automatic changes: %w", err)
				}
				in.Usage.Checked = true
			}
			d = aipolicy.Evaluate(in)
		}
		res = DecideResult{Decided: true, Outcome: d.Outcome, Ask: d.Ask, Class: d.Class, ResumesAt: d.ResumesAt}
		md := policyMetadata(cur, classing, d, siteCheck, connCheck)
		if d.Outcome == aipolicy.OutcomeAutoBySetting {
			row, err := tx.ApproveByPolicy(ctx, PolicyApproval{
				TenantID: tenantID, RequestID: requestID, SiteID: cur.Request.SiteID,
				SiteMode: cur.SiteMode, ModeSource: cur.SiteModeSource, ModeVersion: cur.SiteModeVersion,
				SetterUserID: cur.SiteSetter, SetterSetAt: cur.SiteSetAt,
				BaseClass: cur.StoredClass, Class: d.Class, DispatchWindowSeconds: dispatchWindowSeconds,
			})
			if errors.Is(err, pgx.ErrNoRows) {
				// The setting or the request moved between the read and
				// the compare-and-set; decide again on what is there now.
				return errSettingMoved
			}
			if err != nil {
				return err
			}
			res.Approved = row
			return tx.Audit(ctx, audit.Event{
				TenantID: tenantID, ActorType: audit.ActorPolicy, ActorID: cur.Request.SiteID.String(),
				Action: audit.ActionAbilityRequestApproved, TargetType: audit.TargetTypeAssistantAbilityRequest,
				TargetID: requestID.String(), Metadata: md,
			})
		}
		err = tx.RecordAsk(ctx, PolicyAsk{
			TenantID: tenantID, RequestID: requestID, BaseClass: cur.StoredClass, Class: d.Class, Reason: d.Ask,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return errAlreadyDecided
		}
		if err != nil {
			return err
		}
		return tx.Audit(ctx, audit.Event{
			TenantID: tenantID, ActorType: audit.ActorPolicy, ActorID: cur.Request.SiteID.String(),
			Action: audit.ActionAbilityRequestAsked, TargetType: audit.TargetTypeAssistantAbilityRequest,
			TargetID: requestID.String(), Metadata: md,
		})
	})
	if err != nil {
		if !errors.Is(err, errSettingMoved) && !errors.Is(err, errAlreadyDecided) {
			s.logger.ErrorContext(ctx, "ability request: policy decision not recorded; the request waits for a person",
				slog.String("request_id", requestID.String()), slog.Any("error", err))
		}
		return DecideResult{}, err
	}
	return res, nil
}

// classFacts builds the classing input from the stored request only.
func classFacts(snap PolicySnapshot) aipolicy.ClassFacts {
	return aipolicy.ClassFacts{
		Stored:       snap.StoredClass,
		Undo:         undoFact(snap.Request),
		TargetStatus: snap.Request.CheckedTargetStatus,
		AIDraft:      snap.AIDraft,
	}
}

// undoFact reads the checked precheck's Undo fact from the card facts the
// control plane wrote at creation. A request through a REST route carries
// the boolean undo_exact; anything missing or unreadable is UndoUnknown,
// which asks. A request with no route (page creation) has its own Undo.
func undoFact(r sqlc.AssistantAbilityRequest) aipolicy.Undo {
	if r.RouteID == nil {
		return aipolicy.UndoByAbility
	}
	var facts struct {
		UndoExact *bool `json:"undo_exact"`
	}
	if err := json.Unmarshal(r.CardFacts, &facts); err != nil || facts.UndoExact == nil {
		return aipolicy.UndoUnknown
	}
	if *facts.UndoExact {
		return aipolicy.UndoExact
	}
	return aipolicy.UndoNotExact
}

// policyMetadata is the decision's audit metadata: today's decision
// metadata plus what the decision relied on and each setter check.
func policyMetadata(snap PolicySnapshot, c aipolicy.Classification, d aipolicy.Decision,
	site, conn aipolicy.SetterCheck) map[string]any {
	md := decisionMetadata(snap.Request, false)
	md["approval_source"] = "policy"
	md["site_mode"] = string(snap.SiteMode)
	md["mode_version"] = snap.SiteModeVersion
	md["base_change_class"] = string(snap.StoredClass)
	md["change_class"] = string(d.Class)
	md["outcome"] = string(d.Outcome)
	if d.Ask != "" {
		md["ask_reason"] = string(d.Ask)
	}
	if snap.SiteSetter != uuid.Nil {
		md["site_setter_user_id"] = snap.SiteSetter.String()
		md["site_setter_set_at"] = snap.SiteSetAt.UTC().Format(time.RFC3339Nano)
	}
	md["connection_auto"] = string(snap.ConnectionAuto)
	if snap.ConnectionSetter != uuid.Nil {
		md["connection_setter_user_id"] = snap.ConnectionSetter.String()
	}
	md["site_setter_check"] = setterCheckMetadata(site)
	md["connection_setter_check"] = setterCheckMetadata(conn)
	return md
}

func setterCheckMetadata(c aipolicy.SetterCheck) map[string]any {
	if c.Rule == "" {
		return map[string]any{"applied": false}
	}
	m := map[string]any{"applied": true, "rule": string(c.Rule), "ok": c.OK}
	if c.Failed != "" {
		m["failed"] = c.Failed
	}
	return m
}

// DecideAbilityRequest is mcp.AbilityDecider: Decide, answered in the MCP
// package's terms. It takes ids only.
func (s *Service) DecideAbilityRequest(ctx context.Context, tenantID, requestID uuid.UUID) (mcp.AbilityDecision, error) {
	res, err := s.Decide(ctx, tenantID, requestID)
	if err != nil || !res.Decided {
		return mcp.AbilityDecision{}, err
	}
	if res.Outcome == aipolicy.OutcomeAutoBySetting {
		return mcp.AbilityDecision{Approved: true}, nil
	}
	return mcp.AbilityDecision{AskReason: string(res.Ask), ResumesAt: res.ResumesAt}, nil
}

var _ mcp.AbilityDecider = (*Service)(nil)
