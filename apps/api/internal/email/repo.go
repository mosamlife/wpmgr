package email

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// m62 — multi-connection CRUD  (Area 2)
// ---------------------------------------------------------------------------

// ListConnections returns all named connections for a config row. Ordered by
// created_at ASC, id ASC (stable insertion order). Runs under scopedTenantTx.
func (r *Repo) ListConnections(ctx context.Context, tenantID, configID uuid.UUID) ([]Connection, error) {
	var out []Connection
	err := r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).ListEmailConnections(ctx, sqlc.ListEmailConnectionsParams{
			ConfigID: configID,
			TenantID: tenantID,
		})
		if qerr != nil {
			return domain.Internal("email_list_connections", "failed to list connections").WithCause(qerr)
		}
		for _, row := range rows {
			out = append(out, connectionFromRow(row))
		}
		return nil
	})
	return out, err
}

// GetConnection returns a single named connection by key. Returns ErrNotFound
// when absent. Runs under scopedTenantTx.
func (r *Repo) GetConnection(ctx context.Context, tenantID, configID uuid.UUID, key string) (Connection, error) {
	var c Connection
	err := r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).GetEmailConnection(ctx, sqlc.GetEmailConnectionParams{
			ConfigID:      configID,
			ConnectionKey: key,
			TenantID:      tenantID,
		})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return qerr
		}
		c = connectionFromRow(row)
		return nil
	})
	return c, err
}

// UpsertConnection creates or updates a named connection. Uses the nil-sentinel
// pattern: when in.SecretCiphertext is nil the existing ciphertext is preserved.
// Runs under scopedTenantTx.
func (r *Repo) UpsertConnection(ctx context.Context, in ConnectionUpsertInput, secretCiphertext []byte, setSecret bool) (Connection, error) {
	cfgJSON, err := jsonMarshal(in.Config)
	if err != nil {
		return Connection{}, err
	}
	var c Connection
	dbErr := r.scopedTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).UpsertEmailConnection(ctx, sqlc.UpsertEmailConnectionParams{
			TenantID:                in.TenantID,
			ConfigID:                in.ConfigID,
			ConnectionKey:           in.ConnectionKey,
			Provider:                in.Provider,
			FromAddress:             in.FromAddress,
			FromName:                in.FromName,
			Config:                  cfgJSON,
			SetSecret:               setSecret,
			ProviderSecretEncrypted: secretCiphertext,
		})
		if qerr != nil {
			return qerr
		}
		c = connectionFromRow(row)
		return nil
	})
	return c, dbErr
}

// DeleteConnection deletes a named connection by key. Runs under scopedTenantTx.
func (r *Repo) DeleteConnection(ctx context.Context, tenantID, configID uuid.UUID, key string) error {
	return r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		return sqlc.New(tx).DeleteEmailConnection(ctx, sqlc.DeleteEmailConnectionParams{
			ConfigID:      configID,
			ConnectionKey: key,
			TenantID:      tenantID,
		})
	})
}

// GetConnectionSecretCiphertexts fetches (connection_key, provider_secret_encrypted)
// for all connections under a config row. Used by buildAgentConfigReq to decrypt and
// build the connections registry. Runs under scopedTenantTx.
func (r *Repo) GetConnectionSecretCiphertexts(ctx context.Context, tenantID, configID uuid.UUID) ([]ConnectionSecretRow, error) {
	var out []ConnectionSecretRow
	err := r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).GetConnectionSecretCiphertexts(ctx, sqlc.GetConnectionSecretCiphertextsParams{
			ConfigID: configID,
			TenantID: tenantID,
		})
		if qerr != nil {
			return qerr
		}
		for _, row := range rows {
			out = append(out, ConnectionSecretRow{
				ConnectionKey:           row.ConnectionKey,
				ProviderSecretEncrypted: row.ProviderSecretEncrypted,
			})
		}
		return nil
	})
	return out, err
}

// connectionFromRow maps a sqlc SiteEmailConnection to the domain Connection.
// The provider_secret_encrypted column is NEVER copied — only SecretSet (bool).
func connectionFromRow(row sqlc.SiteEmailConnection) Connection {
	c := Connection{
		ID:            row.ID,
		TenantID:      row.TenantID,
		ConfigID:      row.ConfigID,
		ConnectionKey: row.ConnectionKey,
		Provider:      row.Provider,
		FromAddress:   row.FromAddress,
		FromName:      row.FromName,
		SecretSet:     len(row.ProviderSecretEncrypted) > 0,
		CreatedAt:     row.CreatedAt,
		UpdatedAt:     row.UpdatedAt,
	}
	if len(row.Config) > 0 {
		_ = json.Unmarshal(row.Config, &c.Config)
	}
	if c.Config == nil {
		c.Config = map[string]any{}
	}
	return c
}

// ---------------------------------------------------------------------------
// m62 — Org-propagation  (Area 1)
// ---------------------------------------------------------------------------

// ListEmailInheritingSites returns enrolled sites that have no per-site email
// config row (i.e. inherit the org default). Runs under InAgentTx.
func (r *Repo) ListEmailInheritingSites(ctx context.Context, tenantID uuid.UUID) ([]InheritingSite, error) {
	var out []InheritingSite
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).ListEmailInheritingSites(ctx, tenantID)
		if qerr != nil {
			return qerr
		}
		for _, row := range rows {
			out = append(out, InheritingSite{ID: row.ID, URL: row.Url})
		}
		return nil
	})
	return out, err
}

// GetSiteRef fetches a site's URL and name for use in notification emails.
// Runs under InAgentTx.
func (r *Repo) GetSiteRef(ctx context.Context, tenantID, siteID uuid.UUID) (SiteRef, error) {
	var ref SiteRef
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).GetSiteRef(ctx, sqlc.GetSiteRefParams{
			ID:       siteID,
			TenantID: tenantID,
		})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return qerr
		}
		ref = SiteRef{ID: row.ID, URL: row.Url, Name: row.Name}
		return nil
	})
	return ref, err
}

// ---------------------------------------------------------------------------
// m62 — Notify settings (Area 4)
// ---------------------------------------------------------------------------

// GetNotifySettings returns the org-level notify settings row. Returns
// ErrNotFound when no row exists (service returns defaults). Runs under InTenantTx.
func (r *Repo) GetNotifySettings(ctx context.Context, tenantID uuid.UUID) (NotifySettings, error) {
	var out NotifySettings
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).GetNotifySettings(ctx, tenantID)
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return qerr
		}
		out = notifySettingsFromRow(row)
		return nil
	})
	return out, err
}

// ListConnectedSiteEmailCoverage returns, for every site in this tenant
// currently in the 'connected' connection_state, the two raw facts needed to
// compute failure-detection coverage (GH #381): its reported agent_version,
// and whether WPMgr is actively routing its mail (per-site
// site_email_config.provider, falling back to the org-wide default row when
// no per-site row exists — mirrors the agent's own
// EmailConfig::is_configured()). Both the version-gate comparison and the
// routed-OR-new-enough predicate happen in the service layer.
//
// Unlike GetNotifySettings/UpsertNotifySettings, sites and site_email_config
// are site-keyed tables carrying the m112 RESTRICTIVE site_scope policies, so
// this runs under scopedTenantTx like every other site-keyed query in this
// package: a site-scoped collaborator's coverage is correctly narrowed to the
// sites they can see, and an org-scoped principal sees the whole tenant
// fleet.
func (r *Repo) ListConnectedSiteEmailCoverage(ctx context.Context, tenantID uuid.UUID) ([]ConnectedSiteEmailFact, error) {
	var out []ConnectedSiteEmailFact
	err := r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).ListConnectedSiteEmailCoverage(ctx, tenantID)
		if qerr != nil {
			return qerr
		}
		for _, row := range rows {
			out = append(out, ConnectedSiteEmailFact{AgentVersion: row.AgentVersion, Routed: row.Routed})
		}
		return nil
	})
	return out, err
}

// UpsertNotifySettings creates or updates the notify settings row.
// Runs under InTenantTx.
func (r *Repo) UpsertNotifySettings(ctx context.Context, in NotifySettings) (NotifySettings, error) {
	recipientsJSON, err := json.Marshal(in.Recipients)
	if err != nil {
		return NotifySettings{}, fmt.Errorf("marshal recipients: %w", err)
	}
	var nextDigestAtPG pgtype.Timestamptz
	if in.NextDigestAt != nil {
		nextDigestAtPG = pgtype.Timestamptz{Time: *in.NextDigestAt, Valid: true}
	}
	var out NotifySettings
	dbErr := r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).UpsertNotifySettings(ctx, sqlc.UpsertNotifySettingsParams{
			TenantID:             in.TenantID,
			Enabled:              in.Enabled,
			Recipients:           recipientsJSON,
			AlertOnFailure:       in.AlertOnFailure,
			AlertThrottleMinutes: int32(in.AlertThrottleMinutes),
			DigestEnabled:        in.DigestEnabled,
			DigestCadence:        in.DigestCadence,
			DigestDay:            int32(in.DigestDay),
			DigestHour:           int32(in.DigestHour),
			Timezone:             in.Timezone,
			NextDigestAt:         nextDigestAtPG,
		})
		if qerr != nil {
			return qerr
		}
		out = notifySettingsFromRow(row)
		return nil
	})
	return out, dbErr
}

// ---------------------------------------------------------------------------
// m62 — Alert state (Area 4)
// ---------------------------------------------------------------------------

// AccumulateAlertFailures upserts an email_alert_state row and increments
// failures_since_alert by n. Runs under InAgentTx.
func (r *Repo) AccumulateAlertFailures(ctx context.Context, tenantID, siteID uuid.UUID, n int64) error {
	return r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		return sqlc.New(tx).AccumulateAlertFailures(ctx, sqlc.AccumulateAlertFailuresParams{
			TenantID: tenantID,
			SiteID:   siteID,
			Delta:    n,
		})
	})
}

// ClaimAlertSlot tries to claim an alert slot for a site and, when the claim
// succeeds, invokes onClaim in the SAME transaction as the claim UPDATE
// before committing — typically an EnqueueTx call. This is the transactional
// outbox pattern (mirrors internal/vuln's ClaimUnnotifiedFindings +
// AlertMailer.EnqueueTx): if onClaim returns an error, the WHOLE transaction
// rolls back, so the failures_since_alert reset and last_alert_at bump are
// undone together with the failed send. A genuine enqueue failure therefore
// never leaves the site silently marked as recently-alerted — the original
// GH #381 bug, reintroduced by claiming and enqueueing as two independent
// steps that could disagree.
//
// Returns the claimed state (nil = throttled, not an error — pgx.ErrNoRows),
// or the onClaim error, propagated unwrapped after the rollback. onClaim may
// be nil (claim with no side effect, e.g. tests exercising the throttle gate
// alone). Runs under InAgentTx.
func (r *Repo) ClaimAlertSlot(ctx context.Context, tenantID, siteID uuid.UUID, minFailures int64, throttleMinutes int, onClaim func(tx pgx.Tx) error) (*AlertState, error) {
	var state *AlertState
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).ClaimAlertSlot(ctx, sqlc.ClaimAlertSlotParams{
			TenantID:        tenantID,
			SiteID:          siteID,
			MinFailures:     minFailures,
			ThrottleMinutes: int32(throttleMinutes),
		})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return nil // throttled — not an error
			}
			return qerr
		}
		s := AlertState{
			TenantID:           row.TenantID,
			SiteID:             row.SiteID,
			FailuresSinceAlert: row.FailuresSinceAlert,
		}
		if row.LastAlertAt.Valid {
			t := row.LastAlertAt.Time
			s.LastAlertAt = &t
		}
		if onClaim != nil {
			if cbErr := onClaim(tx); cbErr != nil {
				return cbErr // rolls back the claim together with the failed side effect
			}
		}
		state = &s
		return nil
	})
	if err != nil {
		return nil, err
	}
	return state, nil
}

// ---------------------------------------------------------------------------
// m62 — Digest scheduling (Area 4)
// ---------------------------------------------------------------------------

// ListDueDigests returns notify-settings rows where next_digest_at <= now()
// and digest is enabled. Used by the DigestWorker. Runs under InAgentTx.
func (r *Repo) ListDueDigests(ctx context.Context, limit int32) ([]NotifySettings, error) {
	var out []NotifySettings
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).ListDueDigests(ctx, limit)
		if qerr != nil {
			return qerr
		}
		for _, row := range rows {
			out = append(out, notifySettingsFromRow(row))
		}
		return nil
	})
	return out, err
}

// ClaimAdvanceDigest atomically advances next_digest_at to newNextAt for a
// tenant. Returns the updated row when the claim succeeds (row still due at
// call time), ErrNotFound when already claimed by another worker instance.
// Runs under InAgentTx.
func (r *Repo) ClaimAdvanceDigest(ctx context.Context, tenantID uuid.UUID, newNextAt time.Time) (NotifySettings, error) {
	var out NotifySettings
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).ClaimAdvanceDigest(ctx, sqlc.ClaimAdvanceDigestParams{
			TenantID:        tenantID,
			NewNextDigestAt: pgtype.Timestamptz{Time: newNextAt, Valid: true},
		})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return ErrNotFound // already claimed
			}
			return qerr
		}
		out = notifySettingsFromRow(row)
		return nil
	})
	return out, err
}

// ---------------------------------------------------------------------------
// m62 — Digest analytics (Area 4)
// ---------------------------------------------------------------------------

// GetFleetStatsBySite returns per-site aggregate stats for the given [from, to]
// window, ordered by failed_count DESC. Runs under InAgentTx.
func (r *Repo) GetFleetStatsBySite(ctx context.Context, tenantID uuid.UUID, from, to time.Time, limit int32) ([]SiteStatsRow, error) {
	rangeFrom, rangeTo := resolveRange(from, to)
	var out []SiteStatsRow
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).GetFleetStatsBySite(ctx, sqlc.GetFleetStatsBySiteParams{
			TenantID:  tenantID,
			RangeFrom: rangeFrom,
			RangeTo:   rangeTo,
			RowLimit:  limit,
		})
		if qerr != nil {
			return qerr
		}
		for _, row := range rows {
			out = append(out, SiteStatsRow{
				SiteID:       row.SiteID,
				Total:        row.Total,
				SentCount:    row.SentCount,
				FailedCount:  row.FailedCount,
				BouncedCount: row.BouncedCount,
			})
		}
		return nil
	})
	return out, err
}

// TopFailureSamples returns the top N failure rows (subject + error) across
// all sites for the given [from, to] window. Runs under InAgentTx.
func (r *Repo) TopFailureSamples(ctx context.Context, tenantID uuid.UUID, from, to time.Time, limit int32) ([]FailureSample, error) {
	rangeFrom, rangeTo := resolveRange(from, to)
	var out []FailureSample
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).TopFailureSamples(ctx, sqlc.TopFailureSamplesParams{
			TenantID:  tenantID,
			RangeFrom: rangeFrom,
			RangeTo:   rangeTo,
			RowLimit:  limit,
		})
		if qerr != nil {
			return qerr
		}
		for _, row := range rows {
			out = append(out, FailureSample{SiteID: row.SiteID, Subject: row.Subject, Error: row.Error})
		}
		return nil
	})
	return out, err
}

// TopFailureSamplesBySite returns the top N failure rows for a specific site.
// Runs under InAgentTx.
func (r *Repo) TopFailureSamplesBySite(ctx context.Context, tenantID, siteID uuid.UUID, from, to time.Time, limit int32) ([]FailureSample, error) {
	rangeFrom, rangeTo := resolveRange(from, to)
	var out []FailureSample
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).TopFailureSamplesBySite(ctx, sqlc.TopFailureSamplesBySiteParams{
			TenantID:  tenantID,
			SiteID:    siteID,
			RangeFrom: rangeFrom,
			RangeTo:   rangeTo,
			RowLimit:  limit,
		})
		if qerr != nil {
			return qerr
		}
		for _, row := range rows {
			out = append(out, FailureSample{SiteID: row.SiteID, Subject: row.Subject, Error: row.Error})
		}
		return nil
	})
	return out, err
}

// ---------------------------------------------------------------------------
// notify settings helpers
// ---------------------------------------------------------------------------

func notifySettingsFromRow(row sqlc.EmailNotifySetting) NotifySettings {
	s := NotifySettings{
		TenantID:             row.TenantID,
		Enabled:              row.Enabled,
		AlertOnFailure:       row.AlertOnFailure,
		AlertThrottleMinutes: int(row.AlertThrottleMinutes),
		DigestEnabled:        row.DigestEnabled,
		DigestCadence:        row.DigestCadence,
		DigestDay:            int(row.DigestDay),
		DigestHour:           int(row.DigestHour),
		Timezone:             row.Timezone,
		CreatedAt:            row.CreatedAt,
		UpdatedAt:            row.UpdatedAt,
	}
	if row.NextDigestAt.Valid {
		t := row.NextDigestAt.Time
		s.NextDigestAt = &t
	}
	// Recipients is stored as a JSON array ([]byte) in the DB.
	if len(row.Recipients) > 0 {
		_ = json.Unmarshal(row.Recipients, &s.Recipients)
	}
	if s.Recipients == nil {
		s.Recipients = []string{}
	}
	return s
}

// epochStart / farFuture are sentinel bounds used when no date range is supplied.
// Using time.Time{} (zero) as epoch-start and a year far in the future as upper
// bound avoids NULL handling in SQL while keeping queries simple.
var (
	epochStart = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	farFuture  = time.Date(2099, 12, 31, 23, 59, 59, 0, time.UTC)
	// cursorIDMax is the string representation of the max UUID (all f's), used as
	// the initial cursor so the first page gets all rows.
	cursorIDMax = uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
)

// ErrNotFound is returned when no config row exists yet for the given key.
var ErrNotFound = errors.New("email: not found")

// ErrSuppressionRefused is returned when a suppression delete targeted a row
// the caller can read but may not remove. It exists because that outcome is
// otherwise indistinguishable from success: RLS refuses the DELETE by matching
// no rows, which is not an error in Postgres. See Repo.DeleteSuppression.
var ErrSuppressionRefused = errors.New("email: suppression entry exists but the delete was refused")

// Repo is the persistence layer for per-site email config. Every operator
// read/write runs under scopedTenantTx, which is plain InTenantTx
// (app.tenant_id GUC) for org-scoped principals and workers, and
// InScopedTenantTx (which additionally sets app.site_scope and
// app.allowed_site_ids, activating the m112 RESTRICTIVE policies) for a
// site-scoped collaborator. The provider_secret_encrypted column is NEVER
// returned to callers; only the SecretSet bool is surfaced (mirrors the perf
// repo's CDN credentials pattern).
type Repo struct {
	pool *db.Pool
}

// NewRepo wires a Repo with the shared pgx pool.
func NewRepo(pool *db.Pool) *Repo { return &Repo{pool: pool} }

// scopedTenantTx is the transaction wrapper every operator-path email query
// runs under. It exists because the m112 RESTRICTIVE policies are inert unless
// something actually sets app.site_scope, and nothing in this package ever did:
// every method here called InTenantTx directly, which sets app.tenant_id and
// nothing else. A policy no transaction activates is decoration.
//
// The dispatch is deliberately narrower than db.Pool.RunTenantTx. RunTenantTx
// routes org-scoped principals to InTenantTxAsUser, which sets an additional
// app.user_id GUC; adopting that here would change the transaction every
// existing org-member email read and write runs in, for no security gain, in
// the same change that adds new policies. Instead ONLY a site-scoped principal
// is routed anywhere new. For everyone else, and for every path with no
// principal in context at all (River workers, the digest job, the agent
// ingest, tests), this is InTenantTx exactly as before, so org-scoped
// behaviour is unchanged to the byte.
//
// The tenant equality check is not ceremony. A background job can carry a
// principal in a context it inherited while operating on some other tenant's
// rows; scoping that transaction to the inherited principal's site allowlist
// would filter the wrong tenant's data. When the principal is not the owner of
// the tenant being queried, it tells us nothing and is ignored.
//
// The "is this principal restricted" half of the condition is
// domain.IsSiteConstrained, reached through Principal.IsSiteConstrained — the
// same single predicate as db.RunTenantTx and every HTTP site gate. This
// package must not restate it: #513 found this file deciding the RLS dispatch
// on a bare `Scope == ScopeSite`, so a principal carrying an allowlist without
// the label was constrained at the gate and tenant-wide over every email row.
// Which helper actually runs is proved behaviourally by
// repo_site_scope_integration_test.go against real policies, and the structural
// guard in repo_tx_dispatch_test.go refuses a new method that skips this
// wrapper.
func (r *Repo) scopedTenantTx(ctx context.Context, tenantID uuid.UUID, fn func(tx pgx.Tx) error) error {
	if p, ok := domain.PrincipalFromContext(ctx); ok &&
		p.IsSiteConstrained() &&
		p.TenantID == tenantID {
		return r.pool.InScopedTenantTx(ctx, tenantID, p.UserID, p.AllowedSiteIDs, fn)
	}
	return r.pool.InTenantTx(ctx, tenantID, fn)
}

// GetSiteConfig returns the per-site config row (without the encrypted secret).
// Returns ErrNotFound when no row exists yet.
func (r *Repo) GetSiteConfig(ctx context.Context, tenantID, siteID uuid.UUID) (Config, error) {
	var cfg Config
	err := r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		sid := pgtype.UUID{Bytes: siteID, Valid: true}
		row, qerr := sqlc.New(tx).GetSiteEmailConfig(ctx, sqlc.GetSiteEmailConfigParams{
			TenantID: tenantID,
			SiteID:   sid,
		})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return qerr
		}
		cfg = configFromRow(row)
		return nil
	})
	return cfg, err
}

// GetOrgConfig returns the org-wide default config row (site_id IS NULL).
// Returns ErrNotFound when no org-wide default is configured.
func (r *Repo) GetOrgConfig(ctx context.Context, tenantID uuid.UUID) (Config, error) {
	var cfg Config
	err := r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).GetOrgEmailConfig(ctx, tenantID)
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return qerr
		}
		cfg = configFromRow(row)
		return nil
	})
	return cfg, err
}

// GetSecretCiphertext returns the age-encrypted secret for a site config row.
// Returns (nil, nil) when no secret is stored. Never decrypts — that is the
// service's responsibility.
func (r *Repo) GetSecretCiphertext(ctx context.Context, tenantID, siteID uuid.UUID) ([]byte, error) {
	var ct []byte
	err := r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		sid := pgtype.UUID{Bytes: siteID, Valid: true}
		row, qerr := sqlc.New(tx).GetSiteEmailConfig(ctx, sqlc.GetSiteEmailConfigParams{
			TenantID: tenantID,
			SiteID:   sid,
		})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return nil // not found = no secret
			}
			return qerr
		}
		ct = row.ProviderSecretEncrypted
		return nil
	})
	return ct, err
}

// GetOrgSecretCiphertext returns the age-encrypted secret for the org-wide row.
func (r *Repo) GetOrgSecretCiphertext(ctx context.Context, tenantID uuid.UUID) ([]byte, error) {
	var ct []byte
	err := r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).GetOrgEmailConfig(ctx, tenantID)
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return nil
			}
			return qerr
		}
		ct = row.ProviderSecretEncrypted
		return nil
	})
	return ct, err
}

// GetConfigByRouteTokenHash looks up a config row by the SHA-256 of its route
// token. Used by the webhook dispatcher to resolve a config row without knowing
// the tenant. Runs under InAgentTx (no tenant GUC available at lookup time).
func (r *Repo) GetConfigByRouteTokenHash(ctx context.Context, tokenHash []byte) (Config, error) {
	var cfg Config
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).GetEmailConfigByRouteTokenHash(ctx, tokenHash)
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return qerr
		}
		cfg = configFromRow(row)
		return nil
	})
	return cfg, err
}

// GetConfigByRouteTokenHashWithSecret looks up a config row by route token hash
// AND returns the encrypted signing key so the caller can decrypt it.
// Returns (Config, signingKeyCiphertext, error). Runs under InAgentTx.
func (r *Repo) GetConfigByRouteTokenHashWithSecret(ctx context.Context, tokenHash []byte) (Config, []byte, error) {
	var cfg Config
	var signingKeyCT []byte
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).GetEmailConfigByRouteTokenHash(ctx, tokenHash)
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return qerr
		}
		cfg = configFromRow(row)
		signingKeyCT = row.WebhookSigningKeyEnc
		return nil
	})
	return cfg, signingKeyCT, err
}

// SetWebhookFields writes the webhook security columns on a config row.
// Runs under scopedTenantTx (operator path).
func (r *Repo) SetWebhookFields(ctx context.Context, tenantID, configID uuid.UUID, tokenHash, signingKeyCT []byte, setSigningKey bool, sesTopicArns []string) (Config, error) {
	var cfg Config
	err := r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).SetEmailConfigWebhookFields(ctx, sqlc.SetEmailConfigWebhookFieldsParams{
			TokenHash:     tokenHash,
			SesTopicArns:  sesTopicArns,
			SetSigningKey: setSigningKey,
			SigningKeyEnc: signingKeyCT,
			TenantID:      tenantID,
			ID:            configID,
		})
		if qerr != nil {
			return qerr
		}
		cfg = configFromRow(row)
		return nil
	})
	return cfg, err
}

// UpsertSiteConfig creates or updates the per-site config row. When
// in.SecretCiphertext is nil the existing ciphertext is preserved (nil-sentinel).
func (r *Repo) UpsertSiteConfig(ctx context.Context, in upsertRepoInput) (Config, error) {
	cfgJSON, err := jsonMarshal(in.Config)
	if err != nil {
		return Config{}, err
	}
	mappingsJSON, err := jsonMarshal(in.Mappings)
	if err != nil {
		return Config{}, err
	}

	var cfg Config
	dbErr := r.scopedTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		sid := pgtype.UUID{Bytes: *in.SiteID, Valid: true}
		row, qerr := sqlc.New(tx).UpsertSiteEmailConfigByTenantSite(ctx,
			sqlc.UpsertSiteEmailConfigByTenantSiteParams{
				TenantID:                in.TenantID,
				SiteID:                  sid,
				Provider:                in.Provider,
				FromAddress:             in.FromAddress,
				FromName:                in.FromName,
				ForceFromEmail:          in.ForceFromEmail,
				ForceFromName:           in.ForceFromName,
				ReturnPath:              in.ReturnPath,
				Config:                  cfgJSON,
				SetSecret:               in.SetSecret,
				ProviderSecretEncrypted: in.SecretCiphertext,
				Mappings:                mappingsJSON,
				DefaultConnection:       in.DefaultConnection,
				FallbackConnection:      in.FallbackConnection,
				LogEmails:               in.LogEmails,
				StoreBody:               in.StoreBody,
				RetentionDays:           int32(in.RetentionDays),
			})
		if qerr != nil {
			return qerr
		}
		cfg = configFromRow(row)
		return nil
	})
	return cfg, dbErr
}

// UpsertOrgConfig creates or updates the org-wide default config row.
func (r *Repo) UpsertOrgConfig(ctx context.Context, in upsertRepoInput) (Config, error) {
	cfgJSON, err := jsonMarshal(in.Config)
	if err != nil {
		return Config{}, err
	}
	mappingsJSON, err := jsonMarshal(in.Mappings)
	if err != nil {
		return Config{}, err
	}

	var cfg Config
	dbErr := r.scopedTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).UpsertOrgEmailConfig(ctx,
			sqlc.UpsertOrgEmailConfigParams{
				TenantID:                in.TenantID,
				Provider:                in.Provider,
				FromAddress:             in.FromAddress,
				FromName:                in.FromName,
				ForceFromEmail:          in.ForceFromEmail,
				ForceFromName:           in.ForceFromName,
				ReturnPath:              in.ReturnPath,
				Config:                  cfgJSON,
				SetSecret:               in.SetSecret,
				ProviderSecretEncrypted: in.SecretCiphertext,
				Mappings:                mappingsJSON,
				DefaultConnection:       in.DefaultConnection,
				FallbackConnection:      in.FallbackConnection,
				LogEmails:               in.LogEmails,
				StoreBody:               in.StoreBody,
				RetentionDays:           int32(in.RetentionDays),
			})
		if qerr != nil {
			return qerr
		}
		cfg = configFromRow(row)
		return nil
	})
	return cfg, dbErr
}

// ListSiteConfigs returns all per-site config rows for a tenant (excludes the
// org-wide default). Used by the portfolio overview.
func (r *Repo) ListSiteConfigs(ctx context.Context, tenantID uuid.UUID, limit, offset int32) ([]Config, error) {
	var out []Config
	err := r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).ListSiteEmailConfigs(ctx, sqlc.ListSiteEmailConfigsParams{
			TenantID:  tenantID,
			RowLimit:  limit,
			RowOffset: offset,
		})
		if qerr != nil {
			return qerr
		}
		for _, row := range rows {
			out = append(out, configFromRow(row))
		}
		return nil
	})
	return out, err
}

// ---------------------------------------------------------------------------
// internal helpers
// ---------------------------------------------------------------------------

// upsertRepoInput is the internal wire type between service and repo.
// It carries the already-encrypted ciphertext (or nil = preserve existing).
type upsertRepoInput struct {
	TenantID uuid.UUID
	SiteID   *uuid.UUID // nil = org-wide default

	Provider       string
	FromAddress    string
	FromName       string
	ForceFromEmail bool
	ForceFromName  bool
	ReturnPath     bool

	Config   map[string]any
	Mappings map[string]any

	// SetSecret flags whether SecretCiphertext should be written.
	SetSecret        bool
	SecretCiphertext []byte

	DefaultConnection  *string
	FallbackConnection *string

	LogEmails     bool
	StoreBody     bool
	RetentionDays int
}

// configFromRow maps a sqlc SiteEmailConfig row to the domain Config. The
// provider_secret_encrypted and webhook_signing_key_enc columns are NEVER
// copied — only SecretSet and WebhookSigningKeySet bools are surfaced.
func configFromRow(row sqlc.SiteEmailConfig) Config {
	cfg := Config{
		ID:             row.ID,
		TenantID:       row.TenantID,
		Provider:       row.Provider,
		FromAddress:    row.FromAddress,
		FromName:       row.FromName,
		ForceFromEmail: row.ForceFromEmail,
		ForceFromName:  row.ForceFromName,
		ReturnPath:     row.ReturnPath,
		SecretSet:      len(row.ProviderSecretEncrypted) > 0,
		LogEmails:      row.LogEmails,
		StoreBody:      row.StoreBody,
		RetentionDays:  int(row.RetentionDays),
		// m61: webhook security masked reads.
		WebhookSigningKeySet:     len(row.WebhookSigningKeyEnc) > 0,
		WebhookRouteTokenHashSet: len(row.WebhookRouteTokenHash) > 0,
		SesTopicArns:             row.SesTopicArns,
		CreatedAt:                row.CreatedAt,
		UpdatedAt:                row.UpdatedAt,
	}

	if row.SiteID.Valid {
		id := uuid.UUID(row.SiteID.Bytes)
		cfg.SiteID = &id
	}

	if len(row.Config) > 0 {
		_ = json.Unmarshal(row.Config, &cfg.Config)
	}
	if cfg.Config == nil {
		cfg.Config = map[string]any{}
	}

	if len(row.Mappings) > 0 {
		_ = json.Unmarshal(row.Mappings, &cfg.Mappings)
	}
	if cfg.Mappings == nil {
		cfg.Mappings = map[string]any{}
	}

	if row.DefaultConnection != nil {
		cfg.DefaultConnection = row.DefaultConnection
	}
	if row.FallbackConnection != nil {
		cfg.FallbackConnection = row.FallbackConnection
	}

	return cfg
}

func jsonMarshal(v map[string]any) ([]byte, error) {
	if v == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(v)
}

// ---------------------------------------------------------------------------
// Email log (Phase 3)
// ---------------------------------------------------------------------------

// IngestLogBatch idempotently upserts a batch of agent-pushed log entries.
// Runs under InAgentTx (app.agent='on') so the agent RLS policy allows the
// INSERT/UPDATE. The batch is bounded to maxIngestBatch entries.
// Returns the maximum agent_seq accepted.
func (r *Repo) IngestLogBatch(ctx context.Context, tenantID, siteID uuid.UUID, entries []IngestEntry) (int64, error) {
	if len(entries) == 0 {
		return 0, nil
	}
	var maxSeq int64
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		for _, e := range entries {
			respJSON, merr := json.Marshal(e.Response)
			if merr != nil || len(respJSON) == 0 {
				respJSON = []byte("{}")
			}
			agentSeq := &e.AgentSeq
			var messageID *string
			if e.MessageID != "" {
				s := e.MessageID
				messageID = &s
			}
			body := ""
			if e.Body != nil {
				body = *e.Body
			}
			createdAt := e.CreatedAt
			if createdAt.IsZero() {
				createdAt = time.Now().UTC()
			}
			// m62: marshal attachments; nil/empty → '[]'.
			attJSON, mErr := json.Marshal(e.Attachments)
			if mErr != nil || len(attJSON) == 0 {
				attJSON = []byte("[]")
			}
			_, qerr := q.IngestEmailLogEntry(ctx, sqlc.IngestEmailLogEntryParams{
				TenantID:      tenantID,
				SiteID:        siteID,
				AgentSeq:      agentSeq,
				MessageID:     messageID,
				ToAddresses:   e.ToAddresses,
				FromAddress:   e.FromAddress,
				Subject:       e.Subject,
				Provider:      e.Provider,
				Status:        e.Status,
				Response:      respJSON,
				Error:         e.Error,
				Retries:       int32(e.Retries),
				ResentCount:   int32(e.ResentCount),
				BodyStored:    e.BodyStored,
				Body:          body,
				ConnectionKey: e.ConnectionKey,
				Attachments:   attJSON,
				CreatedAt:     createdAt,
			})
			if qerr != nil {
				return domain.Internal("email_ingest_entry", "failed to upsert email log entry").WithCause(qerr)
			}
			if e.AgentSeq > maxSeq {
				maxSeq = e.AgentSeq
			}
		}
		return nil
	})
	return maxSeq, err
}

// ListSiteLog returns a keyset-paginated list of log entries for a single site.
// Body is never included in list results.
func (r *Repo) ListSiteLog(ctx context.Context, tenantID, siteID uuid.UUID, f LogListFilter) (LogListPage, error) {
	limit := clampLimit(f.Limit, 50, 200)
	cursorTs, cursorID := parseCursor(f.Cursor)
	rangeFrom, rangeTo := resolveRange(f.From, f.To)

	var page LogListPage
	err := r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).ListSiteEmailLog(ctx, sqlc.ListSiteEmailLogParams{
			TenantID:     tenantID,
			SiteID:       siteID,
			CursorTs:     cursorTs,
			CursorID:     cursorID,
			FilterStatus: f.Status,
			RangeFrom:    rangeFrom,
			RangeTo:      rangeTo,
			SearchQ:      f.Q,
			RowLimit:     int32(limit + 1),
		})
		if qerr != nil {
			return domain.Internal("email_list_log", "failed to list email log").WithCause(qerr)
		}
		for _, row := range rows {
			page.Entries = append(page.Entries, logListRowToEntry(row))
		}
		if len(page.Entries) > limit {
			last := page.Entries[limit-1]
			page.NextCursor = encodeCursor(last.CreatedAt, last.ID)
			page.Entries = page.Entries[:limit]
		}
		return nil
	})
	return page, err
}

// GetLogEntry returns a single email log entry including body (if stored).
func (r *Repo) GetLogEntry(ctx context.Context, tenantID, siteID, id uuid.UUID) (LogDetail, error) {
	var detail LogDetail
	err := r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		row, qerr := q.GetEmailLog(ctx, sqlc.GetEmailLogParams{
			TenantID: tenantID,
			SiteID:   siteID,
			ID:       id,
		})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return domain.Internal("email_get_log", "failed to fetch email log entry").WithCause(qerr)
		}
		detail.Entry = logRowToEntry(row)

		// Prev (older row).
		prevID, perr := q.GetEmailLogPrev(ctx, sqlc.GetEmailLogPrevParams{
			TenantID: tenantID,
			SiteID:   siteID,
			ThisTs:   row.CreatedAt,
			ThisID:   row.ID,
		})
		if perr == nil {
			detail.PrevID = &prevID
		}

		// Next (newer row).
		nextID, nerr := q.GetEmailLogNext(ctx, sqlc.GetEmailLogNextParams{
			TenantID: tenantID,
			SiteID:   siteID,
			ThisTs:   row.CreatedAt,
			ThisID:   row.ID,
		})
		if nerr == nil {
			detail.NextID = &nextID
		}
		return nil
	})
	return detail, err
}

// ListFleetLog returns a keyset-paginated cross-site log list for a tenant.
// Body is never included in list results.
func (r *Repo) ListFleetLog(ctx context.Context, tenantID uuid.UUID, f LogListFilter) (LogListPage, error) {
	limit := clampLimit(f.Limit, 50, 200)
	cursorTs, cursorID := parseCursor(f.Cursor)
	rangeFrom, rangeTo := resolveRange(f.From, f.To)

	var page LogListPage
	err := r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).ListFleetEmailLog(ctx, sqlc.ListFleetEmailLogParams{
			TenantID:     tenantID,
			CursorTs:     cursorTs,
			CursorID:     cursorID,
			FilterStatus: f.Status,
			RangeFrom:    rangeFrom,
			RangeTo:      rangeTo,
			SearchQ:      f.Q,
			RowLimit:     int32(limit + 1),
		})
		if qerr != nil {
			return domain.Internal("email_list_fleet_log", "failed to list fleet email log").WithCause(qerr)
		}
		for _, row := range rows {
			page.Entries = append(page.Entries, fleetLogRowToEntry(row))
		}
		if len(page.Entries) > limit {
			last := page.Entries[limit-1]
			page.NextCursor = encodeCursor(last.CreatedAt, last.ID)
			page.Entries = page.Entries[:limit]
		}
		return nil
	})
	return page, err
}

// GetSiteStats returns the email stats (summary + per-day + per-provider) for a site.
func (r *Repo) GetSiteStats(ctx context.Context, tenantID, siteID uuid.UUID, from, to time.Time) (EmailStats, error) {
	rangeFrom, rangeTo := resolveRange(from, to)
	var stats EmailStats
	err := r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		sumRow, serr := q.GetEmailStats(ctx, sqlc.GetEmailStatsParams{
			TenantID:  tenantID,
			SiteID:    siteID,
			RangeFrom: rangeFrom,
			RangeTo:   rangeTo,
		})
		if serr != nil {
			return domain.Internal("email_get_stats", "failed to get email stats").WithCause(serr)
		}
		stats.Total = sumRow.Total
		stats.SentCount = sumRow.SentCount
		stats.FailedCount = sumRow.FailedCount
		stats.BouncedCount = sumRow.BouncedCount
		stats.ComplainedCount = sumRow.ComplainedCount
		stats.ProviderCount = sumRow.ProviderCount

		dayRows, derr := q.GetEmailStatsByDay(ctx, sqlc.GetEmailStatsByDayParams{
			TenantID:  tenantID,
			SiteID:    siteID,
			RangeFrom: rangeFrom,
			RangeTo:   rangeTo,
		})
		if derr != nil {
			return domain.Internal("email_get_stats_by_day", "failed to get email stats by day").WithCause(derr)
		}
		for _, d := range dayRows {
			stats.ByDay = append(stats.ByDay, StatsByDay{
				Day:             d.Day,
				Total:           d.Total,
				SentCount:       d.SentCount,
				FailedCount:     d.FailedCount,
				BouncedCount:    d.BouncedCount,
				ComplainedCount: d.ComplainedCount,
			})
		}

		provRows, prerr := q.GetEmailStatsByProvider(ctx, sqlc.GetEmailStatsByProviderParams{
			TenantID:  tenantID,
			SiteID:    siteID,
			RangeFrom: rangeFrom,
			RangeTo:   rangeTo,
		})
		if prerr != nil {
			return domain.Internal("email_get_stats_by_provider", "failed to get email stats by provider").WithCause(prerr)
		}
		for _, p := range provRows {
			stats.ByProvider = append(stats.ByProvider, StatsByProvider{
				Provider:    p.Provider,
				Total:       p.Total,
				SentCount:   p.SentCount,
				FailedCount: p.FailedCount,
			})
		}
		return nil
	})
	return stats, err
}

// GetFleetStats returns the fleet-wide email stats for a tenant.
func (r *Repo) GetFleetStats(ctx context.Context, tenantID uuid.UUID, from, to time.Time) (EmailStats, error) {
	rangeFrom, rangeTo := resolveRange(from, to)
	var stats EmailStats
	err := r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		sumRow, serr := q.GetFleetEmailStats(ctx, sqlc.GetFleetEmailStatsParams{
			TenantID:  tenantID,
			RangeFrom: rangeFrom,
			RangeTo:   rangeTo,
		})
		if serr != nil {
			return domain.Internal("email_get_fleet_stats", "failed to get fleet email stats").WithCause(serr)
		}
		stats.Total = sumRow.Total
		stats.SentCount = sumRow.SentCount
		stats.FailedCount = sumRow.FailedCount
		stats.BouncedCount = sumRow.BouncedCount
		stats.ComplainedCount = sumRow.ComplainedCount
		stats.ProviderCount = sumRow.ProviderCount
		stats.SiteCount = sumRow.SiteCount

		dayRows, derr := q.GetFleetEmailStatsByDay(ctx, sqlc.GetFleetEmailStatsByDayParams{
			TenantID:  tenantID,
			RangeFrom: rangeFrom,
			RangeTo:   rangeTo,
		})
		if derr != nil {
			return domain.Internal("email_get_fleet_stats_by_day", "failed to get fleet email stats by day").WithCause(derr)
		}
		for _, d := range dayRows {
			stats.ByDay = append(stats.ByDay, StatsByDay{
				Day:             d.Day,
				Total:           d.Total,
				SentCount:       d.SentCount,
				FailedCount:     d.FailedCount,
				BouncedCount:    d.BouncedCount,
				ComplainedCount: d.ComplainedCount,
			})
		}
		return nil
	})
	return stats, err
}

// ---------------------------------------------------------------------------
// GetFleetDelivery — deliverability dashboard (GET /email/deliverability)
// ---------------------------------------------------------------------------

// GetFleetDelivery returns per-site deliverability aggregates + sparklines for
// the given window (days). Runs under scopedTenantTx.
func (r *Repo) GetFleetDelivery(ctx context.Context, tenantID uuid.UUID, windowDays int) (DeliverabilityReport, error) {
	if windowDays < 1 {
		windowDays = 30
	}
	if windowDays > 365 {
		windowDays = 365
	}
	now := time.Now().UTC()
	rangeFrom := now.AddDate(0, 0, -windowDays)
	rangeTo := now

	var report DeliverabilityReport
	report.WindowDays = windowDays

	err := r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)

		// Fetch per-site aggregates (includes joined name/url/provider).
		siteRows, serr := q.GetFleetDeliveryPerSite(ctx, sqlc.GetFleetDeliveryPerSiteParams{
			TenantID:  tenantID,
			RangeFrom: rangeFrom,
			RangeTo:   rangeTo,
		})
		if serr != nil {
			return domain.Internal("email_get_fleet_delivery", "failed to get fleet delivery stats").WithCause(serr)
		}

		// Fetch sparkline rows (daily sent per site, oldest→newest).
		dailyRows, derr := q.GetFleetDeliveryDailyBySite(ctx, sqlc.GetFleetDeliveryDailyBySiteParams{
			TenantID:  tenantID,
			RangeFrom: rangeFrom,
			RangeTo:   rangeTo,
		})
		if derr != nil {
			return domain.Internal("email_get_fleet_delivery_sparkline", "failed to get fleet delivery sparkline").WithCause(derr)
		}

		// Build sparkline map: siteID → []int64 (sent counts oldest→newest).
		type dayCount struct {
			day   time.Time
			count int64
		}
		sparkMap := make(map[uuid.UUID][]dayCount)
		for _, d := range dailyRows {
			sparkMap[d.SiteID] = append(sparkMap[d.SiteID], dayCount{day: d.Day, count: d.SentCount})
		}

		report.Items = make([]SiteDeliveryItem, 0, len(siteRows))
		for _, row := range siteRows {
			var bounceRate, complaintRate float64
			if row.Total > 0 {
				bounceRate = float64(row.BouncedCount) / float64(row.Total) * 100
				complaintRate = float64(row.ComplainedCount) / float64(row.Total) * 100
			}

			// Extract last_sent_at from the interface{} value sqlc produced.
			var lastSentAt *time.Time
			if row.LastSentAt != nil {
				switch v := row.LastSentAt.(type) {
				case time.Time:
					if !v.IsZero() {
						t := v.UTC()
						lastSentAt = &t
					}
				}
			}

			// Build sparkline: convert the per-site day slices to []int64.
			sparks := sparkMap[row.SiteID]
			sparkline := make([]int64, 0, len(sparks))
			for _, s := range sparks {
				sparkline = append(sparkline, s.count)
			}

			report.Items = append(report.Items, SiteDeliveryItem{
				SiteID:          row.SiteID,
				SiteName:        row.SiteName,
				SiteURL:         row.SiteUrl,
				Provider:        row.Provider,
				Total:           row.Total,
				SentCount:       row.SentCount,
				FailedCount:     row.FailedCount,
				BouncedCount:    row.BouncedCount,
				ComplainedCount: row.ComplainedCount,
				BounceRate:      bounceRate,
				ComplaintRate:   complaintRate,
				LastSentAt:      lastSentAt,
				Sparkline:       sparkline,
			})
		}
		return nil
	})
	return report, err
}

// ---------------------------------------------------------------------------
// Suppression (Phase 4a)
// ---------------------------------------------------------------------------

// UpsertSuppression upserts a suppression entry. When in.SiteID is nil the
// fleet-wide variant is used (different partial-index conflict target).
// Runs under InAgentTx (webhook path) or scopedTenantTx (operator manual-add).
func (r *Repo) UpsertSuppression(ctx context.Context, in UpsertSuppressionInput) (Suppression, error) {
	hash := suppressionHash(in.Email)
	var emailPtr *string
	if in.StorePlaintext {
		e := strings.ToLower(strings.TrimSpace(in.Email))
		emailPtr = &e
	}
	var eventAt pgtype.Timestamptz
	if in.EventAt != nil {
		eventAt = pgtype.Timestamptz{Time: *in.EventAt, Valid: true}
	}

	var row sqlc.EmailSuppression
	var err error

	if in.SiteID == nil {
		// Fleet-wide row.
		err = r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
			var qerr error
			row, qerr = sqlc.New(tx).UpsertEmailSuppressionFleet(ctx, sqlc.UpsertEmailSuppressionFleetParams{
				TenantID:        in.TenantID,
				EmailHash:       hash,
				Email:           emailPtr,
				Reason:          in.Reason,
				Provider:        in.Provider,
				EventAt:         eventAt,
				SourceMessageID: in.SourceMessageID,
			})
			return qerr
		})
	} else {
		sid := pgtype.UUID{Bytes: *in.SiteID, Valid: true}
		err = r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
			var qerr error
			row, qerr = sqlc.New(tx).UpsertEmailSuppression(ctx, sqlc.UpsertEmailSuppressionParams{
				TenantID:        in.TenantID,
				SiteID:          sid,
				EmailHash:       hash,
				Email:           emailPtr,
				Reason:          in.Reason,
				Provider:        in.Provider,
				EventAt:         eventAt,
				SourceMessageID: in.SourceMessageID,
			})
			return qerr
		})
	}
	if err != nil {
		return Suppression{}, err
	}
	return suppressionFromRow(row), nil
}

// UpsertSuppressionTenantTx upserts a suppression entry using scopedTenantTx
// (operator manual-add path). Same logic but runs under tenant context.
func (r *Repo) UpsertSuppressionTenantTx(ctx context.Context, in UpsertSuppressionInput) (Suppression, error) {
	hash := suppressionHash(in.Email)
	var emailPtr *string
	if in.StorePlaintext {
		e := strings.ToLower(strings.TrimSpace(in.Email))
		emailPtr = &e
	}
	var eventAt pgtype.Timestamptz
	if in.EventAt != nil {
		eventAt = pgtype.Timestamptz{Time: *in.EventAt, Valid: true}
	}

	var row sqlc.EmailSuppression
	var err error

	if in.SiteID == nil {
		err = r.scopedTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
			var qerr error
			row, qerr = sqlc.New(tx).UpsertEmailSuppressionFleet(ctx, sqlc.UpsertEmailSuppressionFleetParams{
				TenantID:        in.TenantID,
				EmailHash:       hash,
				Email:           emailPtr,
				Reason:          in.Reason,
				Provider:        in.Provider,
				EventAt:         eventAt,
				SourceMessageID: in.SourceMessageID,
			})
			return qerr
		})
	} else {
		sid := pgtype.UUID{Bytes: *in.SiteID, Valid: true}
		err = r.scopedTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
			var qerr error
			row, qerr = sqlc.New(tx).UpsertEmailSuppression(ctx, sqlc.UpsertEmailSuppressionParams{
				TenantID:        in.TenantID,
				SiteID:          sid,
				EmailHash:       hash,
				Email:           emailPtr,
				Reason:          in.Reason,
				Provider:        in.Provider,
				EventAt:         eventAt,
				SourceMessageID: in.SourceMessageID,
			})
			return qerr
		})
	}
	if err != nil {
		return Suppression{}, err
	}
	return suppressionFromRow(row), nil
}

// GetSuppression fetches a single suppression row by id.
func (r *Repo) GetSuppression(ctx context.Context, tenantID, id uuid.UUID) (Suppression, error) {
	var s Suppression
	err := r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).GetEmailSuppression(ctx, sqlc.GetEmailSuppressionParams{
			ID:       id,
			TenantID: tenantID,
		})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return qerr
		}
		s = suppressionFromRow(row)
		return nil
	})
	return s, err
}

// IsSuppressed returns true when email is suppressed for the given tenant/site
// (including fleet-wide entries). Runs under scopedTenantTx.
func (r *Repo) IsSuppressed(ctx context.Context, tenantID, siteID uuid.UUID, email string) (bool, error) {
	hash := suppressionHash(email)
	var suppressed bool
	err := r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		var qerr error
		suppressed, qerr = sqlc.New(tx).IsSuppressed(ctx, sqlc.IsSuppressedParams{
			TenantID:  tenantID,
			EmailHash: hash,
			SiteID:    pgtype.UUID{Bytes: siteID, Valid: true},
		})
		return qerr
	})
	return suppressed, err
}

// ListSiteSuppression returns a keyset-paginated suppression list for a site
// (including fleet-wide entries).
func (r *Repo) ListSiteSuppression(ctx context.Context, tenantID, siteID uuid.UUID, f SuppressionFilter) (SuppressionPage, error) {
	limit := clampLimit(f.Limit, 50, 200)
	cursorTs, cursorID := parseCursor(f.Cursor)

	var page SuppressionPage
	err := r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).ListEmailSuppression(ctx, sqlc.ListEmailSuppressionParams{
			TenantID:     tenantID,
			SiteID:       pgtype.UUID{Bytes: siteID, Valid: true},
			IncludeFleet: true,
			CursorTs:     cursorTs,
			CursorID:     cursorID,
			FilterReason: f.Reason,
			RowLimit:     int32(limit + 1),
		})
		if qerr != nil {
			return qerr
		}
		for _, row := range rows {
			page.Entries = append(page.Entries, suppressionFromRow(row))
		}
		if len(page.Entries) > limit {
			last := page.Entries[limit-1]
			page.NextCursor = encodeCursor(last.CreatedAt, last.ID)
			page.Entries = page.Entries[:limit]
		}
		return nil
	})
	return page, err
}

// ListFleetSuppression returns a keyset-paginated fleet-scope suppression list.
func (r *Repo) ListFleetSuppression(ctx context.Context, tenantID uuid.UUID, f SuppressionFilter) (SuppressionPage, error) {
	limit := clampLimit(f.Limit, 50, 200)
	cursorTs, cursorID := parseCursor(f.Cursor)

	var page SuppressionPage
	err := r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).ListFleetEmailSuppression(ctx, sqlc.ListFleetEmailSuppressionParams{
			TenantID:     tenantID,
			CursorTs:     cursorTs,
			CursorID:     cursorID,
			FilterReason: f.Reason,
			RowLimit:     int32(limit + 1),
		})
		if qerr != nil {
			return qerr
		}
		for _, row := range rows {
			page.Entries = append(page.Entries, suppressionFromRow(row))
		}
		if len(page.Entries) > limit {
			last := page.Entries[limit-1]
			page.NextCursor = encodeCursor(last.CreatedAt, last.ID)
			page.Entries = page.Entries[:limit]
		}
		return nil
	})
	return page, err
}

// DeleteSuppression deletes a suppression entry by id. Runs under
// scopedTenantTx. Returns ErrSuppressionRefused when the row exists and is
// visible but the delete was refused, and ErrNotFound when there is no such row.
//
// WHY THIS IS NOT A PLAIN EXEC (GH #380).
//
// The route resolves a suppression entry by id with tenant scope only; there is
// no site check on the id, and there deliberately is none on the READ, because
// a fleet-wide entry (site_id IS NULL) has to stay visible to every site or the
// pre-send check would start mailing addresses the organisation suppressed.
// m112 then refuses the DELETE of that same row for a site-scoped principal.
//
// Postgres expresses that refusal as zero rows affected, not as an error. So
// the honest-looking code path was a lie: the delete removed nothing, the
// handler returned 204, and the caller was told a fleet-wide suppression entry
// had been lifted while it was still in force. A caller acting on that answer
// (a support tool marking the address deliverable, an operator moving on)
// believes something about the system that is not true.
//
// The two outcomes are told apart by re-reading the row inside the SAME
// transaction, so the answer describes one consistent snapshot rather than a
// second, later state of the world:
//
//	deleted 1        -> success.
//	deleted 0, row visible     -> the policy refused it. ErrSuppressionRefused,
//	                              which the service maps to 403 with an
//	                              explanation the operator can act on.
//	deleted 0, row not visible -> there is nothing here for this principal.
//	                              ErrNotFound -> 404.
func (r *Repo) DeleteSuppression(ctx context.Context, tenantID, id uuid.UUID) error {
	return r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		rows, qerr := q.DeleteEmailSuppression(ctx, sqlc.DeleteEmailSuppressionParams{
			ID:       id,
			TenantID: tenantID,
		})
		if qerr != nil {
			return qerr
		}
		if rows > 0 {
			return nil
		}
		if _, gerr := q.GetEmailSuppression(ctx, sqlc.GetEmailSuppressionParams{
			ID:       id,
			TenantID: tenantID,
		}); gerr != nil {
			if errors.Is(gerr, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return gerr
		}
		// Readable, and still here: the delete was refused, not a no-op.
		return ErrSuppressionRefused
	})
}

// ListSuppressionDeltas returns suppression entries created after the cursor
// for the given tenant+site (for the agent suppression-fetch endpoint).
// Runs under InAgentTx.
func (r *Repo) ListSuppressionDeltas(ctx context.Context, tenantID, siteID uuid.UUID, sinceCursor string, limit int) (SuppressionDeltaPage, error) {
	lim := clampLimit(limit, 200, 1000)
	sinceTs, sinceID := parseCursor(sinceCursor)
	// For the delta (ascending) cursor we use epoch-start sentinel on the first call.
	if sinceCursor == "" {
		sinceTs = epochStart
		sinceID = uuid.Nil
	}

	var page SuppressionDeltaPage
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).ListEmailSuppressionDeltas(ctx, sqlc.ListEmailSuppressionDeltasParams{
			TenantID: tenantID,
			SiteID:   pgtype.UUID{Bytes: siteID, Valid: true},
			SinceTs:  sinceTs,
			SinceID:  sinceID,
			RowLimit: int32(lim + 1),
		})
		if qerr != nil {
			return qerr
		}
		for _, row := range rows {
			page.Entries = append(page.Entries, suppressionFromRow(row))
		}
		if len(page.Entries) > lim {
			last := page.Entries[lim-1]
			// Delta cursor is (created_at ASC, id ASC) — same encodeCursor format.
			page.NextCursor = encodeCursor(last.CreatedAt, last.ID)
			page.Entries = page.Entries[:lim]
		}
		return nil
	})
	return page, err
}

// InsertWebhookEventDedup inserts a dedup sentinel. Returns (inserted=false)
// when the event is a duplicate. Runs under InAgentTx.
func (r *Repo) InsertWebhookEventDedup(ctx context.Context, in WebhookEventInput, suppressionID *uuid.UUID) (bool, error) {
	var tenantPG pgtype.UUID
	if in.TenantID != nil {
		tenantPG = pgtype.UUID{Bytes: *in.TenantID, Valid: true}
	}
	var sitePG pgtype.UUID
	if in.SiteID != nil {
		sitePG = pgtype.UUID{Bytes: *in.SiteID, Valid: true}
	}
	var supPG pgtype.UUID
	if suppressionID != nil {
		supPG = pgtype.UUID{Bytes: *suppressionID, Valid: true}
	}
	// m61: store email_hash (SHA-256) rather than plaintext email (SHOULD-FIX #2).
	// in.EmailHash is pre-computed by the webhook handler; fall back to computing
	// it here if the caller did not set it (backwards-compat for the fakeRepo path).
	emailHash := in.EmailHash
	if len(emailHash) == 0 && in.Email != "" {
		emailHash = suppressionHash(in.Email)
	}

	var inserted bool
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		_, qerr := sqlc.New(tx).InsertWebhookEventDedup(ctx, sqlc.InsertWebhookEventDedupParams{
			ProviderEventID: in.ProviderEventID,
			Provider:        in.Provider,
			TenantID:        tenantPG,
			SiteID:          sitePG,
			EmailHash:       emailHash,
			EventType:       in.EventType,
			SuppressionID:   supPG,
		})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				// ON CONFLICT DO NOTHING → duplicate; not an error.
				inserted = false
				return nil
			}
			return qerr
		}
		inserted = true
		return nil
	})
	return inserted, err
}

// MarkEmailLogBounced updates the status of a log entry to 'bounced' or
// 'complained' by message_id + tenant_id + site_id.
// m61 SHOULD-FIX #3: site_id now scopes the update so a forged/colliding
// message_id from another site in the same tenant cannot flip a different
// site's log row. Runs under InAgentTx.
func (r *Repo) MarkEmailLogBounced(ctx context.Context, tenantID, siteID uuid.UUID, messageID, status string) error {
	mid := &messageID
	return r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		return sqlc.New(tx).MarkEmailLogBounced(ctx, sqlc.MarkEmailLogBouncedParams{
			MessageID: mid,
			TenantID:  tenantID,
			SiteID:    siteID,
			Status:    status,
		})
	})
}

// PruneWebhookDedup deletes dedup rows older than cutoffTs. Cross-tenant (InAgentTx).
func (r *Repo) PruneWebhookDedup(ctx context.Context, cutoffTs time.Time) (int64, error) {
	var deleted int64
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		n, qerr := sqlc.New(tx).PruneWebhookEventDedup(ctx, cutoffTs)
		deleted = n
		return qerr
	})
	return deleted, err
}

// ---------------------------------------------------------------------------
// Log actions (Phase 4a)
// ---------------------------------------------------------------------------

// GetResendTarget fetches everything a resend dispatch needs to decide whether
// the command can succeed, and to name the row to the agent: the agent-local
// row id (agent_seq), the body_stored flag, and the recorded Message-ID that
// confirms agent_seq still names the message the operator chose (GH #528).
//
// GH #520: the resend gate used to read body_stored alone, because the CP
// believed it was sending the body itself. The agent addresses its own log by
// agent_seq, so agent_seq is now a precondition, not an afterthought — without
// it there is nothing to ask the agent for.
//
// This reads through the existing GetEmailLog query rather than a narrower one.
// That row carries the stored body; it stays in this process and never reaches
// the command channel, which is the point of the agent-side contract.
func (r *Repo) GetResendTarget(ctx context.Context, tenantID, siteID, id uuid.UUID) (ResendTarget, error) {
	var target ResendTarget
	err := r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).GetEmailLog(ctx, sqlc.GetEmailLogParams{
			TenantID: tenantID,
			SiteID:   siteID,
			ID:       id,
		})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return qerr
		}
		target.AgentSeq = row.AgentSeq
		target.BodyStored = row.BodyStored
		// GH #528: the recorded Message-ID is the confirmation selector the
		// agent checks its own row against. Nullable, and legitimately null for
		// failed sends — the service handles the absence, it does not refuse.
		target.MessageID = row.MessageID
		return nil
	})
	return target, err
}

// IncrEmailLogResentCount increments resent_count on a log entry. Runs under scopedTenantTx.
func (r *Repo) IncrEmailLogResentCount(ctx context.Context, tenantID, siteID, id uuid.UUID) error {
	return r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		return sqlc.New(tx).IncrEmailLogResentCount(ctx, sqlc.IncrEmailLogResentCountParams{
			ID:       id,
			TenantID: tenantID,
			SiteID:   siteID,
		})
	})
}

// DeleteEmailLogsBulk bulk-deletes log entries by id list. Runs under scopedTenantTx.
// Returns the number of rows deleted.
func (r *Repo) DeleteEmailLogsBulk(ctx context.Context, tenantID, siteID uuid.UUID, ids []uuid.UUID) (int64, error) {
	var deleted int64
	err := r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		n, qerr := sqlc.New(tx).DeleteEmailLogsBulk(ctx, sqlc.DeleteEmailLogsBulkParams{
			TenantID: tenantID,
			SiteID:   siteID,
			Ids:      ids,
		})
		deleted = n
		return qerr
	})
	return deleted, err
}

// ---------------------------------------------------------------------------
// suppression helpers
// ---------------------------------------------------------------------------

// suppressionHash returns the SHA-256 hash of the lower-cased, trimmed email.
// This is used as email_hash in the suppression table so raw emails are never
// required at rest.
func suppressionHash(email string) []byte {
	norm := strings.ToLower(strings.TrimSpace(email))
	sum := sha256.Sum256([]byte(norm))
	return sum[:]
}

// suppressionFromRow maps a sqlc EmailSuppression row to the domain type.
func suppressionFromRow(row sqlc.EmailSuppression) Suppression {
	s := Suppression{
		ID:              row.ID,
		TenantID:        row.TenantID,
		EmailHash:       row.EmailHash,
		Email:           row.Email,
		Reason:          row.Reason,
		Provider:        row.Provider,
		SourceMessageID: row.SourceMessageID,
		CreatedAt:       row.CreatedAt,
	}
	if row.SiteID.Valid {
		id := uuid.UUID(row.SiteID.Bytes)
		s.SiteID = &id
	}
	if row.EventAt.Valid {
		t := row.EventAt.Time
		s.EventAt = &t
	}
	return s
}

// DeleteLogsOlderThan deletes one batch of rows older than cutoffTs across all
// tenants (runs under InAgentTx). Returns the number of rows deleted.
func (r *Repo) DeleteLogsOlderThan(ctx context.Context, cutoffTs time.Time, batchSize int64) (int64, error) {
	var deleted int64
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		n, qerr := sqlc.New(tx).DeleteEmailLogsOlderThan(ctx, sqlc.DeleteEmailLogsOlderThanParams{
			CutoffTs:  cutoffTs,
			BatchSize: batchSize,
		})
		if qerr != nil {
			return domain.Internal("email_delete_logs", "failed to delete old email logs").WithCause(qerr)
		}
		deleted = n
		return nil
	})
	return deleted, err
}

// ---------------------------------------------------------------------------
// repo helpers
// ---------------------------------------------------------------------------

// clampLimit bounds limit to [1, max] with a default when 0 or negative.
func clampLimit(limit, defaultVal, maxVal int) int {
	if limit <= 0 {
		return defaultVal
	}
	if limit > maxVal {
		return maxVal
	}
	return limit
}

// resolveRange returns effective from/to bounds. Zero values are replaced with
// epochStart / farFuture so the SQL range predicates always have concrete values.
func resolveRange(from, to time.Time) (time.Time, time.Time) {
	if from.IsZero() {
		from = epochStart
	}
	if to.IsZero() {
		to = farFuture
	}
	return from, to
}

// parseCursor decodes an opaque cursor string into the (created_at, id) pair
// used by the composite keyset predicate. On any parse failure it returns
// (farFuture, cursorIDMax) so the first page is returned.
//
// Cursor format: "<unix-nano-int64>_<uuid>". This is internal — the frontend
// must not construct cursors manually.
func parseCursor(cursor string) (time.Time, uuid.UUID) {
	if cursor == "" {
		return farFuture, cursorIDMax
	}
	// Find the separator between the timestamp and UUID parts.
	sep := len(cursor) - 37 // UUID is always 36 chars + 1 underscore separator
	if sep <= 0 {
		return farFuture, cursorIDMax
	}
	tsStr := cursor[:sep]
	uuidStr := cursor[sep+1:]

	var nanos int64
	for _, ch := range tsStr {
		if ch < '0' || ch > '9' {
			return farFuture, cursorIDMax
		}
		nanos = nanos*10 + int64(ch-'0')
	}
	id, err := uuid.Parse(uuidStr)
	if err != nil {
		return farFuture, cursorIDMax
	}
	return time.Unix(0, nanos).UTC(), id
}

// encodeCursor encodes a (created_at, id) pair into the opaque cursor string.
func encodeCursor(t time.Time, id uuid.UUID) string {
	return fmt.Sprintf("%d_%s", t.UnixNano(), id.String())
}

// logListRowToEntry maps a ListSiteEmailLogRow to a LogEntry (no body).
func logListRowToEntry(row sqlc.ListSiteEmailLogRow) LogEntry {
	e := LogEntry{
		ID:              row.ID,
		TenantID:        row.TenantID,
		SiteID:          row.SiteID,
		AgentSeq:        row.AgentSeq,
		MessageID:       row.MessageID,
		ToAddresses:     row.ToAddresses,
		FromAddress:     row.FromAddress,
		Subject:         row.Subject,
		Provider:        row.Provider,
		Status:          row.Status,
		Error:           row.Error,
		Retries:         int(row.Retries),
		ResentCount:     int(row.ResentCount),
		BodyStored:      row.BodyStored,
		ConnectionKey:   row.ConnectionKey,
		AttachmentCount: int(row.AttachmentCount),
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}
	if len(row.Response) > 0 {
		_ = json.Unmarshal(row.Response, &e.Response)
	}
	if e.Response == nil {
		e.Response = map[string]any{}
	}
	if e.ToAddresses == nil {
		e.ToAddresses = []string{}
	}
	return e
}

// fleetLogRowToEntry maps a ListFleetEmailLogRow to a LogEntry (no body).
func fleetLogRowToEntry(row sqlc.ListFleetEmailLogRow) LogEntry {
	e := LogEntry{
		ID:              row.ID,
		TenantID:        row.TenantID,
		SiteID:          row.SiteID,
		AgentSeq:        row.AgentSeq,
		MessageID:       row.MessageID,
		ToAddresses:     row.ToAddresses,
		FromAddress:     row.FromAddress,
		Subject:         row.Subject,
		Provider:        row.Provider,
		Status:          row.Status,
		Error:           row.Error,
		Retries:         int(row.Retries),
		ResentCount:     int(row.ResentCount),
		BodyStored:      row.BodyStored,
		ConnectionKey:   row.ConnectionKey,
		AttachmentCount: int(row.AttachmentCount),
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}
	if len(row.Response) > 0 {
		_ = json.Unmarshal(row.Response, &e.Response)
	}
	if e.Response == nil {
		e.Response = map[string]any{}
	}
	if e.ToAddresses == nil {
		e.ToAddresses = []string{}
	}
	return e
}

// logRowToEntry maps a full SiteEmailLog row (including body) to a LogEntry.
func logRowToEntry(row sqlc.SiteEmailLog) LogEntry {
	e := LogEntry{
		ID:            row.ID,
		TenantID:      row.TenantID,
		SiteID:        row.SiteID,
		AgentSeq:      row.AgentSeq,
		MessageID:     row.MessageID,
		ToAddresses:   row.ToAddresses,
		FromAddress:   row.FromAddress,
		Subject:       row.Subject,
		Provider:      row.Provider,
		Status:        row.Status,
		Error:         row.Error,
		Retries:       int(row.Retries),
		ResentCount:   int(row.ResentCount),
		BodyStored:    row.BodyStored,
		Body:          row.Body,
		ConnectionKey: row.ConnectionKey,
		CreatedAt:     row.CreatedAt,
		UpdatedAt:     row.UpdatedAt,
	}
	if len(row.Response) > 0 {
		_ = json.Unmarshal(row.Response, &e.Response)
	}
	if e.Response == nil {
		e.Response = map[string]any{}
	}
	if e.ToAddresses == nil {
		e.ToAddresses = []string{}
	}
	// m62: unmarshal attachments JSON (detail view only).
	if len(row.Attachments) > 0 {
		_ = json.Unmarshal(row.Attachments, &e.Attachments)
	}
	if e.Attachments == nil {
		e.Attachments = []AttachmentMeta{}
	}
	// AttachmentCount populated from the slice length (detail row has full data).
	e.AttachmentCount = len(e.Attachments)
	return e
}
