package site

import (
	"context"
	"net/url"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// connService is the concrete ConnectionService (ADR-041). It is the single
// owner of every connection-state transition: each mutating method loads the
// current state, validates via CanTransition (in the repo's locked tx), writes
// the new state + a site_connection_history row + (for operator actions) a
// hash-chained audit entry, then publishes the SSE event AFTER commit.
type connService struct {
	repo      Repo
	validator *domain.Validator
	audit     *audit.Recorder
	pub       EventPublisher
	clock     domain.Clock
	minter    RevokeTokenMinter // may be nil (no CP signing key)
}

// NewConnectionService builds the concrete ConnectionService. Mirrors the
// existing site.Service dependency shape (repo + validator + clock) plus the
// audit recorder, the SSE EventPublisher (ADR-038), and the revoke-token minter
// (ADR-031 reuse). pub may be a no-op in environments without the SSE bus (nil
// disables publishing); minter may be nil when the CP has no signing key (in
// which case revoke falls back to an unsigned instruction — legacy behaviour).
func NewConnectionService(repo Repo, v *domain.Validator, rec *audit.Recorder, pub EventPublisher, clock domain.Clock, minter RevokeTokenMinter) ConnectionService {
	return &connService{repo: repo, validator: v, audit: rec, pub: pub, clock: clock, minter: minter}
}

// publishStateChange is the standard post-commit SSE emit for a transition. It
// is best-effort: a publish failure is swallowed (the client reconciles on
// connect, ADR-038 §4) so it never fails the request that already committed.
func (s *connService) publishStateChange(ctx context.Context, tenantID, siteID uuid.UUID, evType string, from, to ConnectionState, st Site, extra map[string]any) {
	if s.pub == nil {
		return
	}
	data := map[string]any{
		"from": string(from),
		"to":   string(to),
		"site": siteSummary(st),
	}
	for k, v := range extra {
		data[k] = v
	}
	_ = s.pub.Publish(ctx, ConnectionEvent{
		Type:     evType,
		TenantID: tenantID,
		SiteID:   siteID,
		TS:       s.clock.Now().UTC(),
		Data:     data,
	})
}

// recordAudit appends a hash-chained audit entry for an operator/agent action.
// Best-effort (the audit recorder itself is the point only for security events;
// a lifecycle transition that committed should not be rolled back by an audit
// write failure).
func (s *connService) recordAudit(ctx context.Context, tenantID, siteID uuid.UUID, actorType string, actorID uuid.UUID, action string, meta map[string]any) {
	if s.audit == nil {
		return
	}
	id := ""
	if actorID != uuid.Nil {
		id = actorID.String()
	}
	_, _ = s.audit.Record(ctx, audit.Event{
		TenantID:   tenantID,
		ActorType:  actorType,
		ActorID:    id,
		Action:     action,
		TargetType: "site",
		TargetID:   siteID.String(),
		Metadata:   meta,
	})
}

// ---- MintEnrollmentCode (site-first "Add site") --------------------------

func (s *connService) MintEnrollmentCode(ctx context.Context, in MintEnrollmentInput) (EnrollmentCode, error) {
	if in.TenantID == uuid.Nil {
		return EnrollmentCode{}, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	if u, err := url.Parse(in.URL); err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") {
		return EnrollmentCode{}, domain.Validation("site_url_scheme", "url must be an http or https URL")
	}
	name := in.Name
	if name == "" {
		name = in.URL
	}
	tags := normalizeTags(in.Tags)

	// 1. Create the pending_enrollment site row first.
	st, err := s.repo.CreatePending(ctx, in.TenantID, in.URL, name, tags)
	if err != nil {
		return EnrollmentCode{}, err
	}

	// 2. Mint a site-bound code.
	plaintext, err := generatePairingCode()
	if err != nil {
		return EnrollmentCode{}, domain.Internal("pairing_code_gen_failed", "failed to generate pairing code").WithCause(err)
	}
	expiresAt := s.clock.Now().Add(pairingCodeTTL)
	if _, err := s.repo.MintSiteBoundCode(ctx, CreatePairingCodeInput{
		TenantID:  in.TenantID,
		CreatedBy: in.CreatedBy,
		SiteName:  name,
		Tags:      tags,
	}, st.ID, hashPairingCode(plaintext), expiresAt); err != nil {
		return EnrollmentCode{}, err
	}

	// 3. Audit + publish site.created so the dashboard renders the pending row.
	s.recordAudit(ctx, in.TenantID, st.ID, audit.ActorUser, in.CreatedBy, audit.ActionSiteCreate, map[string]any{"url": st.URL})
	s.publishCreated(ctx, in.TenantID, st)

	return EnrollmentCode{SiteID: st.ID, Plaintext: plaintext, ExpiresAt: expiresAt}, nil
}

func (s *connService) publishCreated(ctx context.Context, tenantID uuid.UUID, st Site) {
	if s.pub == nil {
		return
	}
	_ = s.pub.Publish(ctx, ConnectionEvent{
		Type:     EventSiteCreated,
		TenantID: tenantID,
		SiteID:   st.ID,
		TS:       s.clock.Now().UTC(),
		Data:     map[string]any{"site": siteSummary(st)},
	})
}

// ---- ConsumeEnrollmentCode (agent /enroll, site-bound) -------------------

func (s *connService) ConsumeEnrollmentCode(ctx context.Context, in ConsumeEnrollmentInput) (Site, error) {
	res, err := s.repo.ConsumeSiteBoundCode(ctx, in.CodeHash, in.ConsumedFromIP, EnrollInput{
		AgentPublicKey: in.AgentPublicKey,
		WPVersion:      in.Meta.WPVersion,
		PHPVersion:     in.Meta.PHPVersion,
	})
	if err != nil {
		return Site{}, err
	}
	// Audit + publish the enroll/connect after the consume committed.
	s.recordAudit(ctx, res.Site.TenantID, res.Site.ID, audit.ActorSystem, uuid.Nil, audit.ActionSiteEnrolled, map[string]any{
		"url":        res.Site.URL,
		"generation": res.Site.ConnectionGeneration,
	})
	s.recordAudit(ctx, res.Site.TenantID, res.Site.ID, audit.ActorSystem, uuid.Nil, audit.ActionSiteConnected, nil)
	s.publishStateChange(ctx, res.Site.TenantID, res.Site.ID, EventSiteStateChanged, StatePendingEnrollment, StateConnected, res.Site, map[string]any{"enrolled": true})
	return res.Site, nil
}

// ---- RecordHeartbeat -----------------------------------------------------

func (s *connService) RecordHeartbeat(ctx context.Context, in HeartbeatInput) (HeartbeatResult, error) {
	st, err := s.repo.Heartbeat(ctx, in.TenantID, in.SiteID)
	if err != nil {
		return HeartbeatResult{}, err
	}

	// A revoked site that is still heartbeating gets the revoke instruction
	// (derived-from-state; see the package doc on the instruction model). The
	// instruction is accompanied by a short-lived SIGNED token (aud=site_id,
	// cmd="revoke") which the agent MUST verify before wiping its keys +
	// self-deactivating (ADR-040 addendum / Phase 6 finding B) — so a MITM on the
	// TLS-only heartbeat response cannot forge a destructive teardown. The agent
	// can still authenticate this heartbeat to RECEIVE the token because revoke
	// no longer nulls agent_public_key (Phase 6 finding C).
	var result HeartbeatResult
	if st.ConnectionState == StateRevoked {
		result.Instructions = []string{"revoke"}
		if s.minter != nil {
			if token, _, merr := s.minter.Mint(s.clock.Now().UTC(), in.SiteID.String(), "revoke"); merr == nil {
				result.RevokeToken = token
			}
		}
		return result, nil
	}

	// Recovery: a heartbeat from a degraded/disconnected site transitions it back
	// to connected (the heartbeat handler is the ONLY recovery writer, ADR-039).
	if st.ConnectionState == StateDegraded || st.ConnectionState == StateDisconnected {
		from := st.ConnectionState
		res, terr := s.transition(ctx, in.TenantID, in.SiteID, StateConnected, "heartbeat", uuid.Nil, nil, applyConnected)
		if terr == nil {
			s.recordAudit(ctx, in.TenantID, in.SiteID, audit.ActorSystem, uuid.Nil, audit.ActionSiteConnected, map[string]any{"recovered_from": string(from)})
			s.publishStateChange(ctx, in.TenantID, in.SiteID, EventSiteStateChanged, res.From, StateConnected, res.Site, map[string]any{"recovered": true})
		}
		// A recovery failure must not fail the heartbeat itself (liveness already
		// recorded); fall through and return no instructions.
	}
	return result, nil
}

// ---- Sweeper transitions (MarkDegraded / MarkDisconnected) ---------------

// MarkDegraded resolves the site's tenant then runs the connected→degraded
// sweeper transition. The sweeper itself already holds the tenant and calls
// MarkDegradedTenant directly (no extra lookup); this interface entry point is
// the tenant-less convenience the contract declares.
func (s *connService) MarkDegraded(ctx context.Context, siteID uuid.UUID) error {
	tenantID, err := s.repo.ResolveTenant(ctx, siteID)
	if err != nil {
		return err
	}
	return s.MarkDegradedTenant(ctx, tenantID, siteID)
}

// MarkDisconnected resolves the tenant then runs the degraded→disconnected
// transition. See MarkDegraded on the tenant-less convenience contract.
func (s *connService) MarkDisconnected(ctx context.Context, siteID uuid.UUID, reason string) error {
	tenantID, err := s.repo.ResolveTenant(ctx, siteID)
	if err != nil {
		return err
	}
	return s.MarkDisconnectedTenant(ctx, tenantID, siteID, reason)
}

// MarkDegradedTenant is the tenant-aware sweeper transition (connected→degraded).
func (s *connService) MarkDegradedTenant(ctx context.Context, tenantID, siteID uuid.UUID) error {
	res, err := s.transition(ctx, tenantID, siteID, StateDegraded, "heartbeat_timeout", uuid.Nil, nil, applyDegraded)
	if err != nil {
		return err
	}
	if res.From == StateDegraded {
		return nil // idempotent no-op (already degraded); no event
	}
	s.recordAudit(ctx, tenantID, siteID, audit.ActorSystem, uuid.Nil, audit.ActionSiteDegraded, nil)
	s.publishStateChange(ctx, tenantID, siteID, EventSiteStateChanged, res.From, StateDegraded, res.Site, nil)
	return nil
}

// MarkDisconnectedTenant is the tenant-aware sweeper/last-will transition.
func (s *connService) MarkDisconnectedTenant(ctx context.Context, tenantID, siteID uuid.UUID, reason string) error {
	res, err := s.transition(ctx, tenantID, siteID, StateDisconnected, reason, uuid.Nil,
		map[string]any{"reason": reason}, applyDisconnected(reason))
	if err != nil {
		return err
	}
	if res.From == StateDisconnected {
		return nil
	}
	s.recordAudit(ctx, tenantID, siteID, audit.ActorSystem, uuid.Nil, audit.ActionSiteDisconnected, map[string]any{"reason": reason})
	s.publishStateChange(ctx, tenantID, siteID, EventSiteDisconnected, res.From, StateDisconnected, res.Site, map[string]any{"reason": reason})
	return nil
}

// ---- RecordLastWill (signed agent disconnect) ----------------------------

// RecordLastWill handles a signed agent disconnect by resolving the tenant then
// transitioning connected/degraded→disconnected (ADR-040). The agent endpoint
// already verified the signature + bound it to this site before calling, so the
// tenant resolution here is a trusted lookup of an already-authenticated site.
func (s *connService) RecordLastWill(ctx context.Context, siteID uuid.UUID, reason string) error {
	tenantID, err := s.repo.ResolveTenant(ctx, siteID)
	if err != nil {
		return err
	}
	return s.RecordLastWillTenant(ctx, tenantID, siteID, reason)
}

// RecordLastWillTenant handles a signed agent disconnect: connected/degraded→
// disconnected with the supplied reason (ADR-040). It does NOT archive.
func (s *connService) RecordLastWillTenant(ctx context.Context, tenantID, siteID uuid.UUID, reason string) error {
	if reason == "" {
		reason = "user_initiated"
	}
	return s.MarkDisconnectedTenant(ctx, tenantID, siteID, reason)
}

// ---- Operator actions (Revoke / Archive / Restore / BeginReEnrollment) ---

func (s *connService) Revoke(ctx context.Context, in ActorSiteInput) (Site, error) {
	if in.TenantID == uuid.Nil {
		return Site{}, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	res, err := s.transition(ctx, in.TenantID, in.SiteID, StateRevoked, in.Reason, in.ActorID,
		map[string]any{"reason": in.Reason}, applyRevoked(in.Reason))
	if err != nil {
		return Site{}, err
	}
	s.recordAudit(ctx, in.TenantID, in.SiteID, audit.ActorUser, in.ActorID, audit.ActionSiteRevoked, map[string]any{"reason": in.Reason})
	s.publishStateChange(ctx, in.TenantID, in.SiteID, EventSiteRevoked, res.From, StateRevoked, res.Site, map[string]any{"reason": in.Reason})
	return res.Site, nil
}

func (s *connService) Archive(ctx context.Context, in ActorSiteInput) error {
	if in.TenantID == uuid.Nil {
		return domain.Forbidden("tenant_required", "a tenant context is required")
	}
	res, err := s.transition(ctx, in.TenantID, in.SiteID, StateArchived, in.Reason, in.ActorID,
		map[string]any{"reason": in.Reason}, applyArchived)
	if err != nil {
		return err
	}
	s.recordAudit(ctx, in.TenantID, in.SiteID, audit.ActorUser, in.ActorID, audit.ActionSiteArchived, map[string]any{"reason": in.Reason})
	s.publishStateChange(ctx, in.TenantID, in.SiteID, EventSiteArchived, res.From, StateArchived, res.Site, nil)
	return nil
}

func (s *connService) Restore(ctx context.Context, in ActorSiteInput) (Site, error) {
	if in.TenantID == uuid.Nil {
		return Site{}, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	res, err := s.repo.Transition(ctx, TransitionInput{
		TenantID:    in.TenantID,
		SiteID:      in.SiteID,
		To:          StateDisconnected,
		Reason:      in.Reason,
		ActorID:     in.ActorID,
		Metadata:    map[string]any{"restored": true},
		RequireFrom: StateArchived, // restore is only meaningful from archived
		Apply:       applyRestored,
	})
	if err != nil {
		return Site{}, err
	}
	s.recordAudit(ctx, in.TenantID, in.SiteID, audit.ActorUser, in.ActorID, audit.ActionSiteRestored, nil)
	s.publishStateChange(ctx, in.TenantID, in.SiteID, EventSiteRestored, res.From, StateDisconnected, res.Site, nil)
	return res.Site, nil
}

func (s *connService) BeginReEnrollment(ctx context.Context, in ActorSiteInput) (EnrollmentCode, error) {
	if in.TenantID == uuid.Nil {
		return EnrollmentCode{}, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	// Move the existing site back to pending_enrollment (bumps generation).
	res, err := s.transition(ctx, in.TenantID, in.SiteID, StatePendingEnrollment, in.Reason, in.ActorID,
		map[string]any{"re_enroll": true}, applyBeginReEnroll)
	if err != nil {
		return EnrollmentCode{}, err
	}

	// Mint a fresh site-bound code for the same site_id.
	plaintext, err := generatePairingCode()
	if err != nil {
		return EnrollmentCode{}, domain.Internal("pairing_code_gen_failed", "failed to generate pairing code").WithCause(err)
	}
	expiresAt := s.clock.Now().Add(pairingCodeTTL)
	if _, err := s.repo.MintSiteBoundCode(ctx, CreatePairingCodeInput{
		TenantID:  in.TenantID,
		CreatedBy: in.ActorID,
		SiteName:  res.Site.Name,
		Tags:      res.Site.Tags,
	}, in.SiteID, hashPairingCode(plaintext), expiresAt); err != nil {
		return EnrollmentCode{}, err
	}

	s.recordAudit(ctx, in.TenantID, in.SiteID, audit.ActorUser, in.ActorID, audit.ActionSiteReEnrolled, map[string]any{
		"generation": res.Site.ConnectionGeneration,
	})
	s.publishStateChange(ctx, in.TenantID, in.SiteID, EventSiteStateChanged, res.From, StatePendingEnrollment, res.Site, map[string]any{"re_enroll": true})
	return EnrollmentCode{SiteID: in.SiteID, Plaintext: plaintext, ExpiresAt: expiresAt}, nil
}

// ---- transition plumbing -------------------------------------------------

// transition is the shared load→validate→write→history wrapper around the repo.
func (s *connService) transition(ctx context.Context, tenantID, siteID uuid.UUID, to ConnectionState, reason string, actorID uuid.UUID, meta map[string]any, apply func(ctx context.Context, q *sqlc.Queries, loaded sqlc.Site) (sqlc.Site, error)) (TransitionResult, error) {
	return s.repo.Transition(ctx, TransitionInput{
		TenantID: tenantID,
		SiteID:   siteID,
		To:       to,
		Reason:   reason,
		ActorID:  actorID,
		Metadata: meta,
		Apply:    apply,
	})
}

// Apply functions — one per target state. Each runs the concrete sqlc write
// inside the repo's locked transition tx (under the tx context).

func applyConnected(ctx context.Context, q *sqlc.Queries, l sqlc.Site) (sqlc.Site, error) {
	return q.MarkSiteConnected(ctx, sqlc.MarkSiteConnectedParams{ID: l.ID, TenantID: l.TenantID})
}

func applyDegraded(ctx context.Context, q *sqlc.Queries, l sqlc.Site) (sqlc.Site, error) {
	return q.MarkSiteDegraded(ctx, sqlc.MarkSiteDegradedParams{ID: l.ID, TenantID: l.TenantID})
}

func applyDisconnected(reason string) func(ctx context.Context, q *sqlc.Queries, l sqlc.Site) (sqlc.Site, error) {
	return func(ctx context.Context, q *sqlc.Queries, l sqlc.Site) (sqlc.Site, error) {
		var rp *string
		if reason != "" {
			rp = &reason
		}
		return q.MarkSiteDisconnected(ctx, sqlc.MarkSiteDisconnectedParams{ID: l.ID, TenantID: l.TenantID, DisconnectedReason: rp})
	}
}

func applyRevoked(reason string) func(ctx context.Context, q *sqlc.Queries, l sqlc.Site) (sqlc.Site, error) {
	return func(ctx context.Context, q *sqlc.Queries, l sqlc.Site) (sqlc.Site, error) {
		var rp *string
		if reason != "" {
			rp = &reason
		}
		return q.MarkSiteRevoked(ctx, sqlc.MarkSiteRevokedParams{ID: l.ID, TenantID: l.TenantID, DisconnectedReason: rp})
	}
}

func applyArchived(ctx context.Context, q *sqlc.Queries, l sqlc.Site) (sqlc.Site, error) {
	return q.ArchiveSite(ctx, sqlc.ArchiveSiteParams{ID: l.ID, TenantID: l.TenantID})
}

func applyRestored(ctx context.Context, q *sqlc.Queries, l sqlc.Site) (sqlc.Site, error) {
	return q.RestoreSite(ctx, sqlc.RestoreSiteParams{ID: l.ID, TenantID: l.TenantID})
}

func applyBeginReEnroll(ctx context.Context, q *sqlc.Queries, l sqlc.Site) (sqlc.Site, error) {
	return q.BeginSiteReEnrollment(ctx, sqlc.BeginSiteReEnrollmentParams{ID: l.ID, TenantID: l.TenantID})
}

// siteSummary is the compact site projection embedded in SSE event data. It
// carries exactly what the dashboard row needs to update in place without a
// refetch (ADR-038), and no secrets (never the agent key).
func siteSummary(s Site) map[string]any {
	m := map[string]any{
		"id":                    s.ID.String(),
		"tenant_id":             s.TenantID.String(),
		"url":                   s.URL,
		"name":                  s.Name,
		"connection_state":      string(s.ConnectionState),
		"connection_generation": s.ConnectionGeneration,
		"health_status":         s.HealthStatus,
		"status":                s.Status,
		"enrolled":              s.EnrolledAt != nil,
	}
	if s.LastSeenAt != nil {
		m["last_seen_at"] = s.LastSeenAt.UTC().Format(time.RFC3339)
	}
	if s.DisconnectedReason != "" {
		m["disconnected_reason"] = s.DisconnectedReason
	}
	return m
}
