package site

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// CreatePending creates a sites row in pending_enrollment (site-first flow).
func (r *pgRepo) CreatePending(ctx context.Context, tenantID uuid.UUID, url, name string, tags []string) (Site, error) {
	if tags == nil {
		tags = []string{}
	}
	var out Site
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).CreatePendingSite(ctx, sqlc.CreatePendingSiteParams{
			TenantID: tenantID,
			Url:      url,
			Name:     name,
			Tags:     tags,
		})
		if err != nil {
			return mapCreateErr(err)
		}
		out = toModel(row)
		return nil
	})
	return out, err
}

// MintSiteBoundCode binds a fresh pairing code to an existing site_id.
func (r *pgRepo) MintSiteBoundCode(ctx context.Context, in CreatePairingCodeInput, siteID uuid.UUID, codeHash string, expiresAt time.Time) (PairingCode, error) {
	tags := in.Tags
	if tags == nil {
		tags = []string{}
	}
	var createdBy pgtype.UUID
	if in.CreatedBy != uuid.Nil {
		createdBy = pgtype.UUID{Bytes: in.CreatedBy, Valid: true}
	}
	var out PairingCode
	err := r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).CreateSiteBoundPairingCode(ctx, sqlc.CreateSiteBoundPairingCodeParams{
			TenantID:  in.TenantID,
			CodeHash:  codeHash,
			CreatedBy: createdBy,
			SiteName:  in.SiteName,
			Tags:      tags,
			ExpiresAt: expiresAt,
			SiteID:    pgtype.UUID{Bytes: siteID, Valid: true},
		})
		if err != nil {
			return domain.Internal("pairing_code_create_failed", "failed to create pairing code").WithCause(err)
		}
		out = toPairingCode(row)
		return nil
	})
	return out, err
}

// Transition runs the load-validate-write-history sequence in one tenant tx.
// The site is loaded FOR UPDATE so concurrent transitions on the same row
// serialize. from→to is validated via CanTransition before any write; an
// illegal move returns a conflict (the DB CHECK is only the backstop). The
// caller's Apply runs the concrete state-write sqlc query.
func (r *pgRepo) Transition(ctx context.Context, in TransitionInput) (TransitionResult, error) {
	var out TransitionResult
	err := r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		loaded, err := q.GetSiteForTransition(ctx, sqlc.GetSiteForTransitionParams{ID: in.SiteID, TenantID: in.TenantID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("site_not_found", "site not found")
			}
			return domain.Internal("site_load_failed", "failed to load site for transition").WithCause(err)
		}
		from := ConnectionState(loaded.ConnectionState)
		if in.RequireFrom != "" && from != in.RequireFrom {
			return domain.Conflict("illegal_transition",
				"site is "+string(from)+", expected "+string(in.RequireFrom))
		}
		if !CanTransition(from, in.To) {
			return domain.Conflict("illegal_transition",
				"cannot transition site from "+string(from)+" to "+string(in.To))
		}
		// Idempotent self-transition: no state write, no history row.
		if from == in.To {
			out = TransitionResult{Site: toModel(loaded), From: from}
			return nil
		}
		updated, err := in.Apply(ctx, q, loaded)
		if err != nil {
			return mapTransitionErr(err)
		}
		if err := insertHistory(ctx, q, in.TenantID, in.SiteID, from, in.To, in.Reason, in.ActorID, updated.ConnectionGeneration, in.Metadata); err != nil {
			return err
		}
		out = TransitionResult{Site: toModel(updated), From: from}
		return nil
	})
	return out, err
}

// ConsumeSiteBoundCode atomically consumes a code and, when site-bound,
// transitions the bound site pending_enrollment→connected. Runs under the
// enroll GUC (pre-tenant-scope) so the code hash is the bootstrap. The legacy
// site_id IS NULL path is handled by the existing Enroll method, NOT here.
func (r *pgRepo) ConsumeSiteBoundCode(ctx context.Context, codeHash, consumedFromIP string, in EnrollInput) (ConsumeResult, error) {
	var out ConsumeResult
	err := r.pool.InEnrollTx(ctx, func(tx pgx.Tx) error {
		q := sqlc.New(tx)

		var ip *netip.Addr
		if consumedFromIP != "" {
			if addr, perr := netip.ParseAddr(consumedFromIP); perr == nil {
				ip = &addr
			}
		}

		consumed, err := q.ConsumeSiteBoundPairingCode(ctx, sqlc.ConsumeSiteBoundPairingCodeParams{
			CodeHash:       codeHash,
			ConsumedFromIp: ip,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Either unknown, already consumed, or expired — a single race-safe
				// UPDATE cannot distinguish, so disambiguate with a follow-up lookup
				// (best-effort; defaults to the generic invalid-code response).
				return classifyConsumeMiss(ctx, q, codeHash)
			}
			return domain.Internal("pairing_code_consume_failed", "failed to consume pairing code").WithCause(err)
		}

		// Legacy tenant-scoped code (site_id NULL): not our path. The caller falls
		// back to the create-at-enroll flow. We must NOT have consumed it here, so
		// this branch is unreachable in practice (the handler routes site-bound vs
		// legacy by inspecting the code first). Guard defensively.
		if !consumed.SiteID.Valid {
			return domain.Conflict("pairing_code_not_site_bound", "pairing code is not bound to a site")
		}

		siteID := uuid.UUID(consumed.SiteID.Bytes)
		tenantID := consumed.TenantID

		// Transition the bound site → connected, storing the agent key. The
		// generation was advanced at re-enroll mint time; we do not bump it again.
		row, err := q.AttachAgentAndConnect(ctx, sqlc.AttachAgentAndConnectParams{
			ID:             siteID,
			TenantID:       tenantID,
			AgentPublicKey: in.AgentPublicKey,
			WpVersion:      in.WPVersion,
			PhpVersion:     in.PHPVersion,
		})
		if err != nil {
			return mapEnrollDupKey(err)
		}
		from := StatePendingEnrollment // the bound site was pending_enrollment
		if err := insertHistory(ctx, q, tenantID, siteID, from, StateConnected, "enrolled", uuid.Nil, row.ConnectionGeneration, map[string]any{"url": row.Url}); err != nil {
			return err
		}
		out = ConsumeResult{Site: toModel(row), SiteBound: true}
		return nil
	})
	return out, err
}

// PairingCodeSiteID peeks a presented code's bound site_id WITHOUT consuming it
// (enroll GUC), so /enroll can route between the site-first consume and the
// legacy create-at-enroll flow. Returns (uuid.Nil, false, nil) for a legacy
// tenant-scoped (NULL site_id) code, and a NotFound only when the code hash is
// unknown (the consume path then produces the precise invalid/expired error).
func (r *pgRepo) PairingCodeSiteID(ctx context.Context, codeHash string) (uuid.UUID, bool, error) {
	var siteID uuid.UUID
	var bound bool
	err := r.pool.InEnrollTx(ctx, func(tx pgx.Tx) error {
		id, err := sqlc.New(tx).PeekPairingCodeSiteID(ctx, codeHash)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.Unauthorized("pairing_code_invalid", "invalid pairing code")
			}
			return domain.Internal("pairing_code_lookup_failed", "failed to resolve pairing code").WithCause(err)
		}
		if id.Valid {
			siteID = uuid.UUID(id.Bytes)
			bound = true
		}
		return nil
	})
	return siteID, bound, err
}

// Heartbeat bumps last_seen_at and returns the post-update site.
func (r *pgRepo) Heartbeat(ctx context.Context, tenantID, siteID uuid.UUID) (Site, error) {
	var out Site
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).TouchSiteHeartbeat(ctx, sqlc.TouchSiteHeartbeatParams{ID: siteID, TenantID: tenantID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("site_not_found", "site not found")
			}
			return domain.Internal("site_heartbeat_failed", "failed to record heartbeat").WithCause(err)
		}
		out = toModel(row)
		return nil
	})
	return out, err
}

func (r *pgRepo) ListToDegrade(ctx context.Context, cutoff time.Time) ([]SiteRef, error) {
	var out []SiteRef
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListSitesToDegrade(ctx, pgtype.Timestamptz{Time: cutoff, Valid: true})
		if err != nil {
			return domain.Internal("sweep_degrade_list_failed", "failed to list sites to degrade").WithCause(err)
		}
		out = make([]SiteRef, 0, len(rows))
		for _, row := range rows {
			out = append(out, SiteRef{ID: row.ID, TenantID: row.TenantID})
		}
		return nil
	})
	return out, err
}

func (r *pgRepo) ListToDisconnect(ctx context.Context, cutoff time.Time) ([]SiteRef, error) {
	var out []SiteRef
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListSitesToDisconnect(ctx, pgtype.Timestamptz{Time: cutoff, Valid: true})
		if err != nil {
			return domain.Internal("sweep_disconnect_list_failed", "failed to list sites to disconnect").WithCause(err)
		}
		out = make([]SiteRef, 0, len(rows))
		for _, row := range rows {
			out = append(out, SiteRef{ID: row.ID, TenantID: row.TenantID})
		}
		return nil
	})
	return out, err
}

// ResolveTenant resolves a site's tenant by id (cross-tenant, app.agent GUC).
func (r *pgRepo) ResolveTenant(ctx context.Context, siteID uuid.UUID) (uuid.UUID, error) {
	var tenantID uuid.UUID
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		id, err := sqlc.New(tx).GetSiteTenant(ctx, siteID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("site_not_found", "site not found")
			}
			return domain.Internal("site_tenant_lookup_failed", "failed to resolve site tenant").WithCause(err)
		}
		tenantID = id
		return nil
	})
	return tenantID, err
}

// insertHistory appends a site_connection_history row inside the caller's tx.
func insertHistory(ctx context.Context, q *sqlc.Queries, tenantID, siteID uuid.UUID, from, to ConnectionState, reason string, actorID uuid.UUID, generation int32, meta map[string]any) error {
	var reasonPtr *string
	if reason != "" {
		reasonPtr = &reason
	}
	var actor pgtype.UUID
	if actorID != uuid.Nil {
		actor = pgtype.UUID{Bytes: actorID, Valid: true}
	}
	metaJSON := []byte("{}")
	if len(meta) > 0 {
		if b, err := json.Marshal(meta); err == nil {
			metaJSON = b
		}
	}
	_, err := q.InsertConnectionHistory(ctx, sqlc.InsertConnectionHistoryParams{
		TenantID:    tenantID,
		SiteID:      siteID,
		FromState:   string(from),
		ToState:     string(to),
		Reason:      reasonPtr,
		ActorUserID: actor,
		Generation:  generation,
		Metadata:    metaJSON,
	})
	if err != nil {
		return domain.Internal("conn_history_insert_failed", "failed to record connection history").WithCause(err)
	}
	return nil
}

// classifyConsumeMiss disambiguates why a site-bound consume matched no row:
// already consumed (409) vs expired/unknown (401). Best-effort under the enroll
// GUC; on any lookup error it returns the generic invalid-code response.
func classifyConsumeMiss(ctx context.Context, q *sqlc.Queries, codeHash string) error {
	pc, err := q.GetPairingCodeByHash(ctx, codeHash)
	if err != nil {
		return domain.Unauthorized("pairing_code_invalid", "invalid pairing code")
	}
	if pc.ConsumedAt.Valid {
		return domain.Conflict("pairing_code_consumed", "pairing code has already been used")
	}
	if !pc.ExpiresAt.After(time.Now()) {
		return domain.Unauthorized("pairing_code_expired", "pairing code has expired")
	}
	return domain.Unauthorized("pairing_code_invalid", "invalid pairing code")
}

// mapTransitionErr maps a CHECK-constraint violation (the DB backstop) to a
// clean conflict; everything else is an internal error.
func mapTransitionErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "23514" || pgErr.Code == "23505") {
		return domain.Conflict("illegal_transition", "connection-state transition rejected").WithCause(err)
	}
	return domain.Internal("site_transition_failed", "failed to apply connection-state transition").WithCause(err)
}
