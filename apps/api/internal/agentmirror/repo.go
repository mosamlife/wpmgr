package agentmirror

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
)

// maxDetailLen bounds AttemptInput.Detail before it is written. Deliberately
// enforced in Go, NOT as a DDL length CHECK: a CHECK would make an
// over-length string fail the entire stamp write, losing the outcome, which
// is the exact failure mode this feature exists to remove. Truncation here is
// unconditional, so the write can never be rejected on this account.
const maxDetailLen = 200

// Repo is the data-access layer for the single-row agent_mirror_state
// sentinel (m109). No RLS applies to this table (see the migration's header
// comment): it carries no tenant_id, no PII and no secrets, and every column
// is a property of this install's own release channel, exactly like its
// structural sibling wordfence_vuln_feed_meta. Every method here therefore
// uses the bare pool, with no InTenantTx/InAgentTx wrapper: access control at
// the HTTP layer (requireSuperadmin) is the real gate on the write path; the
// read path is exposed on the existing tenant-scoped fleet rollup precisely
// because every value here is already shared by every tenant on the install.
type Repo struct {
	pool *db.Pool
}

// NewRepo builds a Repo.
func NewRepo(pool *db.Pool) *Repo {
	return &Repo{pool: pool}
}

// AttemptInput is what the caller (agentupstream.MirrorWorker, after every
// attempted run) hands to RecordAttempt.
type AttemptInput struct {
	Trigger Trigger
	Outcome Outcome
	// Detail is curated and non-secret; see the package doc on
	// State.LastAttemptDetail. Truncated to maxDetailLen here.
	Detail string
	// Version is the agent version this attempt examined, when one was
	// determined. Empty is valid (e.g. a 304, or a refusal before the
	// manifest was read) and leaves the corresponding stored version
	// untouched rather than blanking it.
	Version string
	// LastRequestAt is this process's view of when it last issued an ACTUAL
	// upstream HTTP request (agentupstream.Mirror.LastRequestAt()). The zero
	// Time means "leave the persisted value untouched"; this happens when no
	// mirror instance exists at all (object storage never configured), and is
	// always safe: it is simply a no-op write to that one column.
	LastRequestAt time.Time
}

// RecordAttempt persists the outcome of one mirror attempt. Called exactly
// once per attempted run, never for the case where mirroring is disabled
// entirely, since stamping an attempt for a run that did nothing would be the
// same lie in miniature that this feature exists to remove.
//
// last_success_at/last_success_outcome/last_success_version advance ONLY when
// in.Outcome.IsSuccess(); last_mirrored_at/last_mirrored_version advance ONLY
// when in.Outcome is OutcomeMirrored. Both are single-statement CASE
// expressions so the whole record is one atomic UPDATE.
func (r *Repo) RecordAttempt(ctx context.Context, in AttemptInput) error {
	at := time.Now().UTC()
	isSuccess := in.Outcome.IsSuccess()
	isMirrored := in.Outcome == OutcomeMirrored
	detail := truncateDetail(in.Detail)

	var lastRequestAt *time.Time
	if !in.LastRequestAt.IsZero() {
		t := in.LastRequestAt.UTC()
		lastRequestAt = &t
	}

	_, err := r.pool.Exec(ctx, `
		UPDATE agent_mirror_state SET
			last_attempt_at       = $1,
			last_attempt_outcome  = $2,
			last_attempt_detail   = NULLIF($3, ''),
			last_attempt_trigger  = $4,
			last_request_at       = COALESCE($5, last_request_at),
			last_success_at       = CASE WHEN $6 THEN $1 ELSE last_success_at END,
			last_success_outcome  = CASE WHEN $6 THEN $2 ELSE last_success_outcome END,
			last_success_version  = CASE WHEN $6 AND $7 <> '' THEN $7 ELSE last_success_version END,
			last_mirrored_at      = CASE WHEN $8 THEN $1 ELSE last_mirrored_at END,
			last_mirrored_version = CASE WHEN $8 AND $7 <> '' THEN $7 ELSE last_mirrored_version END,
			updated_at            = now()
		WHERE id = 1`,
		at, string(in.Outcome), detail, string(in.Trigger),
		lastRequestAt, isSuccess, in.Version, isMirrored,
	)
	if err != nil {
		return fmt.Errorf("record agent mirror attempt: %w", err)
	}
	return nil
}

// Load reads the singleton sentinel row. The migration seeds row id=1, so
// this only ever fails on a genuine connectivity problem, never on "no row
// yet"; callers (the fleet handler, the manual-check rate-limit pre-flight)
// both treat a Load failure as "degrade to the zero State", never as a reason
// to fail the caller's own request; see their doc comments.
func (r *Repo) Load(ctx context.Context) (State, error) {
	var s State
	var lastRequestAt, lastAttemptAt, lastSuccessAt, lastMirroredAt pgtype.Timestamptz
	var attemptOutcome, attemptDetail, attemptTrigger pgtype.Text
	var successOutcome, successVersion, mirroredVersion pgtype.Text

	err := r.pool.QueryRow(ctx, `
		SELECT last_request_at,
		       last_attempt_at, last_attempt_outcome, last_attempt_detail, last_attempt_trigger,
		       last_success_at, last_success_outcome, last_success_version,
		       last_mirrored_at, last_mirrored_version
		FROM agent_mirror_state WHERE id = 1`,
	).Scan(
		&lastRequestAt,
		&lastAttemptAt, &attemptOutcome, &attemptDetail, &attemptTrigger,
		&lastSuccessAt, &successOutcome, &successVersion,
		&lastMirroredAt, &mirroredVersion,
	)
	if err != nil {
		return State{}, fmt.Errorf("load agent mirror state: %w", err)
	}

	if lastRequestAt.Valid {
		t := lastRequestAt.Time
		s.LastRequestAt = &t
	}
	if lastAttemptAt.Valid {
		t := lastAttemptAt.Time
		s.LastAttemptAt = &t
	}
	s.LastAttemptOutcome = Outcome(attemptOutcome.String)
	s.LastAttemptDetail = attemptDetail.String
	s.LastAttemptTrigger = Trigger(attemptTrigger.String)
	if lastSuccessAt.Valid {
		t := lastSuccessAt.Time
		s.LastSuccessAt = &t
	}
	s.LastSuccessOutcome = Outcome(successOutcome.String)
	s.LastSuccessVersion = successVersion.String
	if lastMirroredAt.Valid {
		t := lastMirroredAt.Time
		s.LastMirroredAt = &t
	}
	s.LastMirroredVersion = mirroredVersion.String

	return s, nil
}

func truncateDetail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxDetailLen {
		return s[:maxDetailLen]
	}
	return s
}
