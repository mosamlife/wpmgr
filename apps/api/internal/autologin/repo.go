package autologin

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Repo is the Postgres persistence for autologin: tenant-scoped mint + policy
// upsert/read (app.tenant_id RLS) and cross-tenant consume + policy read for
// the agent path (app.agent RLS).
type Repo interface {
	// Mint path (app.tenant_id).
	InsertToken(ctx context.Context, in InsertTokenInput) error
	GetOrCreatePolicy(ctx context.Context, tenantID, siteID uuid.UUID) (Policy, error)

	// Consume path (app.agent — cross-tenant, before any tenant scope is known).
	// ConsumeToken is the single atomic arbiter for nonce consumption; both
	// the Redis-hit and Redis-miss paths in the service drive it so PG is the
	// authoritative source of truth even when Redis is available.
	ConsumeToken(ctx context.Context, nonceID string, siteID uuid.UUID, consumedFromIP string) (ConsumedRow, bool, error)
	GetPolicyForAgent(ctx context.Context, siteID uuid.UUID) (Policy, bool, error)
}

// InsertTokenInput is the validated mint INSERT input.
type InsertTokenInput struct {
	NonceID            string
	TenantID           uuid.UUID
	SiteID             uuid.UUID
	InitiatorUserID    uuid.UUID
	TargetWPUserLogin  string
	InitiatorIP        string
	InitiatorUserAgent string
	ExpiresAt          time.Time
}

// ConsumedRow is what the atomic consume returns when it wins.
type ConsumedRow struct {
	NonceID           string
	TenantID          uuid.UUID
	SiteID            uuid.UUID
	InitiatorUserID   uuid.UUID
	TargetWPUserLogin string
}

type pgRepo struct {
	pool *db.Pool
}

// NewRepo builds a Repo backed by the pgx pool with RLS enforcement.
func NewRepo(pool *db.Pool) Repo { return &pgRepo{pool: pool} }

func (r *pgRepo) InsertToken(ctx context.Context, in InsertTokenInput) error {
	return r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		_, err := sqlc.New(tx).InsertAutologinToken(ctx, sqlc.InsertAutologinTokenParams{
			ID:                 in.NonceID,
			TenantID:           in.TenantID,
			SiteID:             in.SiteID,
			InitiatorUserID:    in.InitiatorUserID,
			TargetWpUserLogin:  in.TargetWPUserLogin,
			InitiatorIp:        parseIP(in.InitiatorIP),
			InitiatorUserAgent: in.InitiatorUserAgent,
			ExpiresAt:          in.ExpiresAt,
		})
		if err != nil {
			return domain.Internal("autologin_insert_failed", "failed to persist autologin nonce").WithCause(err)
		}
		return nil
	})
}

func (r *pgRepo) GetOrCreatePolicy(ctx context.Context, tenantID, siteID uuid.UUID) (Policy, error) {
	var out Policy
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		row, err := q.UpsertAutologinPolicyDefault(ctx, sqlc.UpsertAutologinPolicyDefaultParams{
			SiteID: siteID, TenantID: tenantID,
		})
		if err != nil {
			return domain.Internal("autologin_policy_failed", "failed to load or create autologin policy").WithCause(err)
		}
		out = policyFromRow(row)
		return nil
	})
	return out, err
}

func (r *pgRepo) ConsumeToken(ctx context.Context, nonceID string, siteID uuid.UUID, consumedFromIP string) (ConsumedRow, bool, error) {
	var out ConsumedRow
	var found bool
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).ConsumeAutologinToken(ctx, sqlc.ConsumeAutologinTokenParams{
			ID:             nonceID,
			ConsumedFromIp: parseIP(consumedFromIP),
			SiteID:         siteID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // lost the race / never existed / already consumed / expired
			}
			return domain.Internal("autologin_consume_failed", "failed to consume autologin nonce").WithCause(err)
		}
		out = ConsumedRow{
			NonceID:           row.ID,
			TenantID:          row.TenantID,
			SiteID:            row.SiteID,
			InitiatorUserID:   row.InitiatorUserID,
			TargetWPUserLogin: row.TargetWpUserLogin,
		}
		found = true
		return nil
	})
	return out, found, err
}

func (r *pgRepo) GetPolicyForAgent(ctx context.Context, siteID uuid.UUID) (Policy, bool, error) {
	var out Policy
	var found bool
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).GetAutologinPolicyForAgent(ctx, siteID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return domain.Internal("autologin_policy_lookup_failed", "failed to load autologin policy").WithCause(err)
		}
		out = policyFromRow(row)
		found = true
		return nil
	})
	return out, found, err
}

func policyFromRow(row sqlc.AutologinPolicy) Policy {
	roles := row.AllowedWpRoles
	if roles == nil {
		roles = []string{}
	}
	return Policy{
		SiteID:               row.SiteID,
		TenantID:             row.TenantID,
		Enabled:              row.Enabled,
		AllowedWPRoles:       roles,
		Require2FAStepUp:     row.Require2faStepUp,
		MaxSessionAgeMinutes: row.MaxSessionAgeMinutes,
		UpdatedAt:            row.UpdatedAt,
	}
}
