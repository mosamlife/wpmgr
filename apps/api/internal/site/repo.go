package site

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Repo is the tenant-scoped site persistence interface plus the enrollment and
// agent-auth paths, which (by necessity) run before a tenant scope is known.
type Repo interface {
	Create(ctx context.Context, in CreateInput) (Site, error)
	Get(ctx context.Context, tenantID, id uuid.UUID) (Site, error)
	List(ctx context.Context, in ListInput) ([]Site, error)
	Delete(ctx context.Context, tenantID, id uuid.UUID) error
	SetTags(ctx context.Context, in SetTagsInput) (Site, error)
	SetAgeRecipient(ctx context.Context, tenantID, siteID uuid.UUID, recipient string) (Site, error)

	// Enrollment path (public /enroll; app.enroll GUC).
	CreatePairingCode(ctx context.Context, in CreatePairingCodeInput, codeHash string, expiresAt time.Time) (PairingCode, error)
	Enroll(ctx context.Context, codeHash string, in EnrollInput) (Site, error)

	// Agent-auth path (app.agent GUC): resolve a site by its agent public key.
	GetByAgentKey(ctx context.Context, agentPublicKey string) (Site, error)

	// Agent metadata/heartbeat run in the resolved site's own tenant scope.
	UpdateMetadata(ctx context.Context, tenantID, siteID uuid.UUID, m Metadata, components []byte) (Site, error)
	TouchSeen(ctx context.Context, tenantID, siteID uuid.UUID) error

	// Anti-replay nonce recording (app.agent GUC). Returns false on replay.
	RecordNonce(ctx context.Context, siteID uuid.UUID, nonce string) (bool, error)

	// Health job (app.agent GUC, cross-tenant).
	ListEnrolled(ctx context.Context) ([]EnrolledSite, error)
	MarkUnreachable(ctx context.Context, siteID uuid.UUID) (bool, error)

	// PruneNonces deletes agent_nonces created before the cutoff (maintenance op
	// run cross-tenant under the app.agent GUC). Returns the number of rows
	// deleted. Nonces older than the signature-skew window can never replay, so
	// deleting them is safe and bounds table growth.
	PruneNonces(ctx context.Context, before time.Time) (int64, error)

	// ---- M21 connection lifecycle (ADR-041) ----

	// CreatePending creates a sites row in pending_enrollment (the site-first
	// "Add site" flow) and returns it.
	CreatePending(ctx context.Context, tenantID uuid.UUID, url, name string, tags []string) (Site, error)

	// MintSiteBoundCode binds a fresh pairing code to an existing site_id.
	MintSiteBoundCode(ctx context.Context, in CreatePairingCodeInput, siteID uuid.UUID, codeHash string, expiresAt time.Time) (PairingCode, error)

	// Transition loads the site (FOR UPDATE), validates from→to via
	// CanTransition, then writes the new state + a site_connection_history row in
	// one tenant-scoped tx. It returns the updated site and the from-state. The
	// applyFn selects which state-write query to run for `to`.
	Transition(ctx context.Context, in TransitionInput) (TransitionResult, error)

	// ConsumeSiteBoundCode atomically consumes a code by hash (single-use) and,
	// when the code is site-bound, transitions that site pending_enrollment→
	// connected (storing the agent key, bumping nothing — the generation was
	// already advanced at re-enroll mint time). Runs pre-tenant-scope under the
	// enroll GUC. Returns the resulting site + whether the code was site-bound.
	ConsumeSiteBoundCode(ctx context.Context, codeHash, consumedFromIP string, in EnrollInput) (ConsumeResult, error)

	// Heartbeat bumps last_seen_at and returns the post-update site (so the
	// service can decide on a recovery transition + pending instructions).
	Heartbeat(ctx context.Context, tenantID, siteID uuid.UUID) (Site, error)

	// ListToDegrade / ListToDisconnect are the timeout-sweeper selects
	// (cross-tenant, app.agent GUC).
	ListToDegrade(ctx context.Context, cutoff time.Time) ([]SiteRef, error)
	ListToDisconnect(ctx context.Context, cutoff time.Time) ([]SiteRef, error)

	// ResolveTenant resolves a site's tenant by id (cross-tenant, app.agent GUC).
	ResolveTenant(ctx context.Context, siteID uuid.UUID) (uuid.UUID, error)

	// PairingCodeSiteID peeks a code's bound site_id (enroll GUC) so /enroll can
	// route between the site-first consume and the legacy create-at-enroll flow.
	PairingCodeSiteID(ctx context.Context, codeHash string) (uuid.UUID, bool, error)
}

// SiteRef is the slim (site, tenant) projection the timeout sweeper iterates.
type SiteRef struct {
	ID       uuid.UUID
	TenantID uuid.UUID
}

// TransitionInput drives a single state-machine write. ApplyFn runs the chosen
// state-write query inside the locked tx and returns the updated row.
type TransitionInput struct {
	TenantID uuid.UUID
	SiteID   uuid.UUID
	To       ConnectionState
	Reason   string
	ActorID  uuid.UUID
	Metadata map[string]any
	// RequireFrom, when non-empty, additionally requires the site's current
	// state to be exactly this value (beyond CanTransition). Used where a
	// transition target is reachable from several states but the action is only
	// meaningful from one — e.g. Restore (archived→disconnected) must NOT fire on
	// a connected site even though connected→disconnected is otherwise legal.
	RequireFrom ConnectionState
	// Apply performs the concrete state write (sqlc query) under the same tx as
	// the FOR UPDATE load + the history insert. It receives the tx context, the
	// loaded sqlc tx query handle, and the locked site.
	Apply func(ctx context.Context, q *sqlc.Queries, loaded sqlc.Site) (sqlc.Site, error)
}

// TransitionResult is the outcome of a state transition.
type TransitionResult struct {
	Site Site
	From ConnectionState
}

// ConsumeResult is the outcome of consuming an enrollment code.
type ConsumeResult struct {
	Site      Site
	SiteBound bool // true when a pre-existing site was transitioned (site-first flow)
}

// EnrollInput carries the validated enroll request fields used to create or
// attach a site.
type EnrollInput struct {
	URL            string
	Name           string
	AgentPublicKey string
	WPVersion      string
	PHPVersion     string
	Tags           []string
}

// EnrolledSite is the slim projection the health job iterates over.
type EnrolledSite struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	LastSeenAt   *time.Time
	HealthStatus string
}

// pgRepo runs every operation inside a transaction scoped by the appropriate
// GUC (tenant, enroll, or agent) so RLS enforces isolation even if a query
// omitted its filter.
type pgRepo struct {
	pool *db.Pool
}

// NewRepo builds a Repo backed by the pgx pool with RLS enforcement.
func NewRepo(pool *db.Pool) Repo {
	return &pgRepo{pool: pool}
}

func (r *pgRepo) Create(ctx context.Context, in CreateInput) (Site, error) {
	status := in.Status
	if status == "" {
		status = "pending"
	}
	var out Site
	err := r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).CreateSite(ctx, sqlc.CreateSiteParams{
			TenantID:   in.TenantID,
			Url:        in.URL,
			Name:       in.Name,
			Status:     status,
			WpVersion:  in.WPVersion,
			PhpVersion: in.PHPVersion,
		})
		if err != nil {
			return mapCreateErr(err)
		}
		out = toModel(row)
		return nil
	})
	return out, err
}

func (r *pgRepo) Get(ctx context.Context, tenantID, id uuid.UUID) (Site, error) {
	var out Site
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).GetSite(ctx, sqlc.GetSiteParams{ID: id, TenantID: tenantID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("site_not_found", "site not found")
			}
			return domain.Internal("site_get_failed", "failed to load site").WithCause(err)
		}
		out = toModel(row)
		return nil
	})
	return out, err
}

func (r *pgRepo) List(ctx context.Context, in ListInput) ([]Site, error) {
	var tag *string
	if in.Tag != "" {
		t := in.Tag
		tag = &t
	}
	var state *string
	if in.State != "" {
		st := in.State
		state = &st
	}
	var out []Site
	// FIX 3 (CRITICAL): site-scoped principals must see ONLY their granted sites.
	// Use RunTenantTx (which dispatches to InScopedTenantTx for Scope=="site")
	// when a principal is provided so the RESTRICTIVE RLS policy filters the rows.
	// Fall back to plain InTenantTx (org-scoped, full list) when no principal is
	// provided (backward compat: health-job, agent, test paths).
	runTx := func(fn func(tx pgx.Tx) error) error {
		if in.Principal != nil {
			return r.pool.RunTenantTx(ctx, in.Principal, fn)
		}
		return r.pool.InTenantTx(ctx, in.TenantID, fn)
	}
	err := runTx(func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListSites(ctx, sqlc.ListSitesParams{
			TenantID: in.TenantID,
			Tag:      tag,
			State:    state,
			Limit:    in.Limit,
			Offset:   in.Offset,
		})
		if err != nil {
			return domain.Internal("site_list_failed", "failed to list sites").WithCause(err)
		}
		out = make([]Site, 0, len(rows))
		for _, row := range rows {
			out = append(out, toModel(row))
		}
		return nil
	})
	return out, err
}

func (r *pgRepo) Delete(ctx context.Context, tenantID, id uuid.UUID) error {
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		n, err := sqlc.New(tx).DeleteSite(ctx, sqlc.DeleteSiteParams{ID: id, TenantID: tenantID})
		if err != nil {
			return domain.Internal("site_delete_failed", "failed to delete site").WithCause(err)
		}
		if n == 0 {
			return domain.NotFound("site_not_found", "site not found")
		}
		return nil
	})
}

func (r *pgRepo) SetTags(ctx context.Context, in SetTagsInput) (Site, error) {
	tags := in.Tags
	if tags == nil {
		tags = []string{}
	}
	var out Site
	err := r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).SetSiteTags(ctx, sqlc.SetSiteTagsParams{
			ID:       in.SiteID,
			TenantID: in.TenantID,
			Tags:     tags,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("site_not_found", "site not found")
			}
			return domain.Internal("site_set_tags_failed", "failed to set site tags").WithCause(err)
		}
		out = toModel(row)
		return nil
	})
	return out, err
}

func (r *pgRepo) SetAgeRecipient(ctx context.Context, tenantID, siteID uuid.UUID, recipient string) (Site, error) {
	var out Site
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).SetSiteAgeRecipient(ctx, sqlc.SetSiteAgeRecipientParams{
			ID:           siteID,
			TenantID:     tenantID,
			AgeRecipient: recipient,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("site_not_found", "site not found")
			}
			return domain.Internal("site_set_recipient_failed", "failed to set site age recipient").WithCause(err)
		}
		out = toModel(row)
		return nil
	})
	return out, err
}

func (r *pgRepo) CreatePairingCode(ctx context.Context, in CreatePairingCodeInput, codeHash string, expiresAt time.Time) (PairingCode, error) {
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
		row, err := sqlc.New(tx).CreatePairingCode(ctx, sqlc.CreatePairingCodeParams{
			TenantID:  in.TenantID,
			CodeHash:  codeHash,
			CreatedBy: createdBy,
			SiteName:  in.SiteName,
			Tags:      tags,
			ExpiresAt: expiresAt,
		})
		if err != nil {
			return domain.Internal("pairing_code_create_failed", "failed to create pairing code").WithCause(err)
		}
		out = toPairingCode(row)
		return nil
	})
	return out, err
}

// Enroll validates and consumes a pairing code (by hash) and creates or attaches
// the site, rotating the agent key on re-enrollment. The whole flow runs in a
// single enroll-scoped transaction so a failure rolls everything back (the code
// is not consumed unless the site is created/attached). It returns the resulting
// site. Domain errors are returned for the invalid-code cases.
func (r *pgRepo) Enroll(ctx context.Context, codeHash string, in EnrollInput) (Site, error) {
	var out Site
	err := r.pool.InEnrollTx(ctx, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		pc, err := q.GetPairingCodeByHash(ctx, codeHash)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.Unauthorized("pairing_code_invalid", "invalid pairing code")
			}
			return domain.Internal("pairing_code_lookup_failed", "failed to resolve pairing code").WithCause(err)
		}
		// Attempt cap (defense-in-depth).
		if pc.Attempts >= pairingCodeMaxAttempts {
			return domain.Unauthorized("pairing_code_invalid", "invalid pairing code")
		}
		if pc.ConsumedAt.Valid {
			_, _ = q.IncrementPairingCodeAttempts(ctx, pc.ID)
			return domain.Conflict("pairing_code_consumed", "pairing code has already been used")
		}
		if !pc.ExpiresAt.After(time.Now()) {
			_, _ = q.IncrementPairingCodeAttempts(ctx, pc.ID)
			return domain.Unauthorized("pairing_code_expired", "pairing code has expired")
		}

		name := in.Name
		if name == "" {
			name = pc.SiteName
		}
		if name == "" {
			name = in.URL
		}
		tags := in.Tags
		if len(tags) == 0 {
			tags = pc.Tags
		}
		if tags == nil {
			tags = []string{}
		}

		// Idempotency: re-enrolling the same URL rotates the agent key.
		existing, err := q.GetSiteByURLForEnroll(ctx, sqlc.GetSiteByURLForEnrollParams{TenantID: pc.TenantID, Url: in.URL})
		switch {
		case err == nil:
			row, aerr := q.AttachAgentToSite(ctx, sqlc.AttachAgentToSiteParams{
				ID:             existing.ID,
				TenantID:       pc.TenantID,
				AgentPublicKey: in.AgentPublicKey,
				WpVersion:      in.WPVersion,
				PhpVersion:     in.PHPVersion,
			})
			if aerr != nil {
				return mapEnrollDupKey(aerr)
			}
			out = toModel(row)
		case errors.Is(err, pgx.ErrNoRows):
			row, cerr := q.CreateSiteForEnroll(ctx, sqlc.CreateSiteForEnrollParams{
				TenantID:       pc.TenantID,
				Url:            in.URL,
				Name:           name,
				WpVersion:      in.WPVersion,
				PhpVersion:     in.PHPVersion,
				AgentPublicKey: in.AgentPublicKey,
				Tags:           tags,
			})
			if cerr != nil {
				return mapEnrollDupKey(cerr)
			}
			out = toModel(row)
		default:
			return domain.Internal("site_lookup_failed", "failed to resolve site").WithCause(err)
		}

		// Consume the code exactly once.
		n, err := q.ConsumePairingCode(ctx, pc.ID)
		if err != nil {
			return domain.Internal("pairing_code_consume_failed", "failed to consume pairing code").WithCause(err)
		}
		if n == 0 {
			// Lost a race against a concurrent enroll using the same code.
			return domain.Conflict("pairing_code_consumed", "pairing code has already been used")
		}
		return nil
	})
	return out, err
}

func (r *pgRepo) GetByAgentKey(ctx context.Context, agentPublicKey string) (Site, error) {
	var out Site
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).GetSiteByAgentKey(ctx, agentPublicKey)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.Unauthorized("agent_unknown", "unknown agent")
			}
			return domain.Internal("agent_lookup_failed", "failed to resolve agent").WithCause(err)
		}
		out = toModel(row)
		return nil
	})
	return out, err
}

func (r *pgRepo) UpdateMetadata(ctx context.Context, tenantID, siteID uuid.UUID, m Metadata, components []byte) (Site, error) {
	var out Site
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).UpdateSiteMetadata(ctx, sqlc.UpdateSiteMetadataParams{
			ID:          siteID,
			TenantID:    tenantID,
			WpVersion:   m.WPVersion,
			PhpVersion:  m.PHPVersion,
			ServerInfo:  m.ServerInfo,
			Multisite:   m.Multisite,
			ActiveTheme: m.ActiveTheme,
			Components:  components,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("site_not_found", "site not found")
			}
			return domain.Internal("site_metadata_failed", "failed to update site metadata").WithCause(err)
		}
		out = toModel(row)
		return nil
	})
	return out, err
}

func (r *pgRepo) TouchSeen(ctx context.Context, tenantID, siteID uuid.UUID) error {
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := sqlc.New(tx).TouchSiteSeen(ctx, sqlc.TouchSiteSeenParams{ID: siteID, TenantID: tenantID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("site_not_found", "site not found")
			}
			return domain.Internal("site_touch_failed", "failed to update site liveness").WithCause(err)
		}
		return nil
	})
}

func (r *pgRepo) RecordNonce(ctx context.Context, siteID uuid.UUID, nonce string) (bool, error) {
	var fresh bool
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		n, err := sqlc.New(tx).InsertAgentNonce(ctx, sqlc.InsertAgentNonceParams{SiteID: siteID, Nonce: nonce})
		if err != nil {
			return domain.Internal("nonce_record_failed", "failed to record nonce").WithCause(err)
		}
		fresh = n > 0
		return nil
	})
	return fresh, err
}

func (r *pgRepo) ListEnrolled(ctx context.Context) ([]EnrolledSite, error) {
	var out []EnrolledSite
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListEnrolledSitesAllTenants(ctx)
		if err != nil {
			return domain.Internal("site_list_enrolled_failed", "failed to list enrolled sites").WithCause(err)
		}
		out = make([]EnrolledSite, 0, len(rows))
		for _, row := range rows {
			es := EnrolledSite{ID: row.ID, TenantID: row.TenantID, HealthStatus: row.HealthStatus}
			if row.LastSeenAt.Valid {
				t := row.LastSeenAt.Time
				es.LastSeenAt = &t
			}
			out = append(out, es)
		}
		return nil
	})
	return out, err
}

func (r *pgRepo) PruneNonces(ctx context.Context, before time.Time) (int64, error) {
	var deleted int64
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		n, err := sqlc.New(tx).PruneAgentNonces(ctx, before)
		if err != nil {
			return domain.Internal("nonce_prune_failed", "failed to prune agent nonces").WithCause(err)
		}
		deleted = n
		return nil
	})
	return deleted, err
}

func (r *pgRepo) MarkUnreachable(ctx context.Context, siteID uuid.UUID) (bool, error) {
	var changed bool
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		n, err := sqlc.New(tx).MarkSiteUnreachable(ctx, siteID)
		if err != nil {
			return domain.Internal("site_mark_unreachable_failed", "failed to mark site unreachable").WithCause(err)
		}
		changed = n > 0
		return nil
	})
	return changed, err
}

func mapCreateErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return domain.Conflict("site_url_exists", "a site with this URL already exists for this tenant").WithCause(err)
	}
	return domain.Internal("site_create_failed", "failed to create site").WithCause(err)
}

// mapEnrollDupKey maps a unique-violation during enroll (typically the
// agent_public_key uniqueness) to a clean conflict.
func mapEnrollDupKey(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return domain.Conflict("agent_key_in_use", "this agent key is already bound to another site").WithCause(err)
	}
	return domain.Internal("site_enroll_failed", "failed to enroll site").WithCause(err)
}

func toModel(s sqlc.Site) Site {
	m := Site{
		ID:             s.ID,
		TenantID:       s.TenantID,
		URL:            s.Url,
		Name:           s.Name,
		Status:         s.Status,
		WPVersion:      s.WpVersion,
		PHPVersion:     s.PhpVersion,
		AgentPublicKey: s.AgentPublicKey,
		HealthStatus:   s.HealthStatus,
		ServerInfo:     s.ServerInfo,
		Multisite:      s.Multisite,
		ActiveTheme:    s.ActiveTheme,
		Components:     s.Components,
		Tags:           s.Tags,
		AgeRecipient:   s.AgeRecipient,
		WpTimezone:     s.WpTimezone,
		WpGmtOffset:    float64(s.WpGmtOffset),
		// M21 connection lifecycle.
		ConnectionState:      ConnectionState(s.ConnectionState),
		ConnectionGeneration: s.ConnectionGeneration,
		CreatedAt:            s.CreatedAt,
		UpdatedAt:            s.UpdatedAt,
	}
	if s.EnrolledAt.Valid {
		t := s.EnrolledAt.Time
		m.EnrolledAt = &t
	}
	if s.LastSeenAt.Valid {
		t := s.LastSeenAt.Time
		m.LastSeenAt = &t
	}
	if s.DisconnectedAt.Valid {
		t := s.DisconnectedAt.Time
		m.DisconnectedAt = &t
	}
	if s.DisconnectedReason != nil {
		m.DisconnectedReason = *s.DisconnectedReason
	}
	if s.ArchivedAt.Valid {
		t := s.ArchivedAt.Time
		m.ArchivedAt = &t
	}
	if m.Tags == nil {
		m.Tags = []string{}
	}
	return m
}

func toPairingCode(p sqlc.PairingCode) PairingCode {
	m := PairingCode{
		ID:        p.ID,
		TenantID:  p.TenantID,
		SiteName:  p.SiteName,
		Tags:      p.Tags,
		ExpiresAt: p.ExpiresAt,
		Attempts:  p.Attempts,
		CreatedAt: p.CreatedAt,
	}
	if p.CreatedBy.Valid {
		id := uuid.UUID(p.CreatedBy.Bytes)
		m.CreatedBy = &id
	}
	if p.ConsumedAt.Valid {
		t := p.ConsumedAt.Time
		m.ConsumedAt = &t
	}
	if m.Tags == nil {
		m.Tags = []string{}
	}
	return m
}
