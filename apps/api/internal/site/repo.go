package site

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// backupManifestReclaimKind names the site-scoped object-storage root that
// DELETE /sites/{id} records for later reclamation (GH #402, m113).
//
// This literal is duplicated from backup.ReclaimKindBackupManifest on purpose:
// importing the backup domain here would couple site deletion to it for a
// single string, and the value's real home is a database column that both sides
// read. tests/contract asserts the two agree, so the duplication cannot drift
// silently.
const backupManifestReclaimKind = "backup_manifest"

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

	// GetSiteByURL returns the (id, connection_state) of any existing site with
	// the given URL inside a tenant (including archived rows). Returns (zero,
	// false, nil) when no row exists, and (row, true, nil) on a hit.
	// Called by MintEnrollmentCode before CreatePending to surface a structured
	// 409 with site_id + connection_state instead of a bare index-violation.
	GetSiteByURL(ctx context.Context, tenantID uuid.UUID, url string) (SiteURLHit, bool, error)

	// MintSiteBoundCode binds a fresh pairing code to an existing site_id.
	MintSiteBoundCode(ctx context.Context, in CreatePairingCodeInput, siteID uuid.UUID, codeHash string, expiresAt time.Time) (PairingCode, error)

	// Transition loads the site (FOR UPDATE), validates from→to via
	// CanTransition, then writes the new state + a site_connection_history row in
	// one tenant-scoped tx. It returns the updated site and the from-state. The
	// applyFn selects which state-write query to run for `to`.
	Transition(ctx context.Context, in TransitionInput) (TransitionResult, error)

	// DeleteCancellable hard-deletes a site inside the caller's tenant tx IFF the
	// site is still in pending_enrollment with no enrolled_at and no agent key.
	// Returns the number of rows deleted (0 or 1). rowsAffected==0 means the row
	// either does not exist or has already raced to a connected/enrolled state;
	// the service must treat 0 as not_cancellable (409).
	DeleteCancellable(ctx context.Context, tenantID, siteID uuid.UUID) (int64, error)

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

	// ListAllSiteIDs returns every non-archived site ID for the tenant in a single
	// lightweight query (SELECT id only). Use this instead of List+cap for fleet
	// adapters that enumerate all sites without a row limit.
	ListAllSiteIDs(ctx context.Context, tenantID uuid.UUID) ([]uuid.UUID, error)

	// GetSiteURL returns the URL and enrolled status of a tenant-scoped site.
	// Used by the screenshot handler to validate enrollment before enqueueing.
	// (url, true, nil) when enrolled; (url, false, nil) when found but not enrolled;
	// ("", false, domain.NotFound) when the site doesn't exist.
	GetSiteURL(ctx context.Context, tenantID, siteID uuid.UUID) (string, bool, error)
}

// SiteRef is the slim (site, tenant, url) projection the timeout sweeper
// iterates. URL is included so the active-verify sweeper can dial the agent
// without a secondary tenant-scoped lookup.
type SiteRef struct {
	ID       uuid.UUID
	TenantID uuid.UUID
	URL      string
}

// SiteURLHit is the minimal (id, connection_state) projection returned by
// GetSiteByURL. Used by MintEnrollmentCode to build a structured 409.
type SiteURLHit struct {
	ID              uuid.UUID
	ConnectionState ConnectionState
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
	// CheckSiteQuota documents the caller's intent that this transition grows
	// the active (non-archived) site count without a fresh INSERT (e.g. Restore,
	// archived -> disconnected un-archive) and therefore re-enforces the M16
	// Phase A hosted-plan site cap.
	//
	// Security review Finding B: the repo no longer TRUSTS this flag to decide
	// whether to run the check — Transition derives that decision itself from
	// the loaded from-state and in.To (any transition FROM archived TO a
	// non-archived state always re-checks the cap, regardless of this field),
	// because a caller-supplied flag is trivially forgettable (BeginReEnrollment
	// didn't set it and bypassed the cap on every archived->pending_enrollment
	// re-enroll). This field is kept only as caller-side documentation/intent
	// and for the existing wiring test; it has no effect on enforcement.
	CheckSiteQuota bool
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

// ScreenshotEnricher enriches site list rows with screenshot data and presigned
// URLs. Implemented by screenshotadapter.Enricher (wired in cmd/wpmgr), or by
// a nil value (no-op when screenshots are unconfigured).
//
// This local interface keeps the site package free of a direct screenshot import.
type ScreenshotEnricher interface {
	// EnrichSites enriches the provided sites slice in-place with screenshot
	// status + presigned URLs. Must be safe to call with an empty or nil slice.
	EnrichSites(ctx context.Context, tenantID uuid.UUID, sites []Site) error
}

// BillingGate gates new-site provisioning behind the M16 Phase A hosted-plan
// site cap. Implemented by *internal/billing.Service (wired in cmd/wpmgr); a
// nil value disables the gate entirely (self-host / back-compat / any test
// that does not wire it) and every call site treats that exactly like an
// unlimited plan. This local interface keeps the site package free of a
// direct billing import.
//
// CheckSiteCreate MUST be called inside the SAME transaction as the site
// row's INSERT (or the archived->active state flip that grows the active
// count), which is why it takes the raw tx rather than a repo-level method.
type BillingGate interface {
	CheckSiteCreate(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error
}

// pgRepo runs every operation inside a transaction scoped by the appropriate
// GUC (tenant, enroll, or agent) so RLS enforces isolation even if a query
// omitted its filter.
type pgRepo struct {
	pool          *db.Pool
	screenshotEnr ScreenshotEnricher // optional; nil disables screenshot enrichment
	billing       BillingGate        // optional; nil disables the site-cap gate
}

// NewRepo builds a Repo backed by the pgx pool with RLS enforcement.
func NewRepo(pool *db.Pool) Repo {
	return &pgRepo{pool: pool}
}

// SetScreenshotEnricher wires the screenshot enricher on a Repo returned by
// NewRepo. It is a no-op if repo is not a *pgRepo. Call once at boot when
// object storage is configured; omit on self-host without screenshots.
func SetScreenshotEnricher(repo Repo, e ScreenshotEnricher) {
	if r, ok := repo.(*pgRepo); ok {
		r.screenshotEnr = e
	}
}

// SetBillingGate wires the M16 Phase A billing gate on a Repo returned by
// NewRepo. It is a no-op if repo is not a *pgRepo.
//
// IMPORTANT (mirrors the SetScreenshotEnricher wiring bug fixed in v0.49.1):
// cmd/wpmgr constructs TWO distinct *pgRepo instances — one held inside
// site.Service (serves Create/Enroll), one passed to
// site.NewConnectionService (serves CreatePending/ConsumeEnrollmentCode/
// Restore). Wiring the gate onto only one of them silently leaves the OTHER
// site-birth paths uncapped. Call this on BOTH: once via
// Service.SetBillingGate, and once directly on the repo instance handed to
// NewConnectionService.
func SetBillingGate(repo Repo, b BillingGate) {
	if r, ok := repo.(*pgRepo); ok {
		r.billing = b
	}
}

func (r *pgRepo) Create(ctx context.Context, in CreateInput) (Site, error) {
	status := in.Status
	if status == "" {
		status = "pending"
	}
	var out Site
	err := r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		if r.billing != nil {
			if err := r.billing.CheckSiteCreate(ctx, tx, in.TenantID); err != nil {
				return err
			}
		}
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
		out = toModelFromGetSiteRow(row)
		return nil
	})
	return out, err
}

func (r *pgRepo) List(ctx context.Context, in ListInput) ([]Site, error) {
	// nil (not empty-non-nil) slices are required so sqlc.narg's IS NULL check
	// treats "no filter" correctly — an empty-but-non-nil []string still binds
	// as a non-NULL empty array, which would match nothing.
	var anyTags, allTags []string
	if len(in.AnyTags) > 0 {
		anyTags = in.AnyTags
	}
	if len(in.AllTags) > 0 {
		allTags = in.AllTags
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
	// Build the optional client_id filter for the DB query.
	var clientIDParam pgtype.UUID
	if in.ClientID != nil {
		clientIDParam = pgtype.UUID{Bytes: [16]byte(*in.ClientID), Valid: true}
	}
	// GH #349 free-text search. nil (SQL NULL) disables the predicate entirely;
	// a blank search must not become a filter that matches nothing.
	var qParam *string
	if q := strings.TrimSpace(in.Query); q != "" {
		qParam = &q
	}
	// GH #349 ordering. The value is bound as a query PARAMETER and compared
	// against fixed literals in the SQL; it is never concatenated into the
	// statement. normalizeListSort is the backstop for callers that bypass
	// Service.List (which is where an unrecognised value becomes a 422).
	sortParam := string(normalizeListSort(in.Sort))

	err := runTx(func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListSites(ctx, sqlc.ListSitesParams{
			TenantID: in.TenantID,
			AnyTags:  anyTags,
			AllTags:  allTags,
			State:    state,
			Limit:    in.Limit,
			Offset:   in.Offset,
			ClientID: clientIDParam,
			Q:        qParam,
			Sort:     sortParam,
		})
		if err != nil {
			return domain.Internal("site_list_failed", "failed to list sites").WithCause(err)
		}
		out = make([]Site, 0, len(rows))
		for _, row := range rows {
			out = append(out, toModelFromListSitesRow(row))
		}
		// Batched per-site latest-backup lookup for the sites-table "Backup"
		// column — ONE query for every listed site (index-only seek per site),
		// inside the same tenant/scope tx so RLS applies. Sites with no backup
		// simply stay nil. (Column2 is sqlc's name for the ANY($2::uuid[]) param.)
		if len(out) > 0 {
			ids := make([]uuid.UUID, len(out))
			for i := range out {
				ids[i] = out[i].ID
			}
			bks, berr := sqlc.New(tx).ListLatestBackupsForSites(ctx, sqlc.ListLatestBackupsForSitesParams{
				TenantID: in.TenantID,
				Column2:  ids,
			})
			if berr != nil {
				return domain.Internal("site_list_backups_failed", "failed to fetch latest backups").WithCause(berr)
			}
			byID := make(map[uuid.UUID]sqlc.ListLatestBackupsForSitesRow, len(bks))
			for _, b := range bks {
				byID[b.SiteID] = b
			}
			for i := range out {
				b, ok := byID[out[i].ID]
				if !ok {
					continue
				}
				out[i].LastBackupStatus = b.Status // raw DB status; toAPI normalizes
				if b.FinishedAt.Valid {
					t := b.FinishedAt.Time
					out[i].LastBackupAt = &t
				} else {
					t := b.CreatedAt
					out[i].LastBackupAt = &t
				}
			}

			// m63 — enrich with client names: one JOIN for all sites that have a
			// client_id set (zero-cost for tenants with no clients).
			clientRows, cerr := sqlc.New(tx).ListClientNamesForSites(ctx, sqlc.ListClientNamesForSitesParams{
				TenantID: in.TenantID,
				Column2:  ids,
			})
			if cerr != nil {
				return domain.Internal("site_list_clients_failed", "failed to fetch client names").WithCause(cerr)
			}
			clientByID := make(map[uuid.UUID]string, len(clientRows))
			for _, cr := range clientRows {
				clientByID[cr.SiteID] = cr.ClientName
			}
			for i := range out {
				if name, ok := clientByID[out[i].ID]; ok {
					out[i].ClientName = name
				}
			}

			// M72 — enrich with screenshot status + presigned URLs. One batched
			// query + presign loop, matching the backup/client enrichment pattern.
			if r.screenshotEnr != nil {
				if err := r.screenshotEnr.EnrichSites(ctx, in.TenantID, out); err != nil {
					// Non-fatal: screenshots are optional; log and continue.
					// The site list is still usable without screenshot URLs.
					_ = err // caller cannot do anything; keep serving
				}
			}

		}
		return nil
	})
	return out, err
}

// Delete removes a site and, in the SAME TRANSACTION, records the
// object-storage work the delete leaves behind (GH #402).
//
// WHY THE RECLAIM RECORD IS WRITTEN HERE AND NOWHERE ELSE.
//
// backup_snapshots.site_id is ON DELETE CASCADE, so the DELETE below destroys
// every snapshot row for this site. Those rows were the only database record
// naming the site's per-snapshot manifest.json objects in storage: both
// deleters of that object need a live snapshot row to build the key, and the
// retention GC's site roster is itself derived from backup_snapshots. Once this
// statement commits, nothing can ever name those objects again, so they leak
// forever. A field report saw 90 orphans from one deleted site.
//
// Chunks need no help and get none. backup_chunks has NO foreign key to sites,
// so the cascade leaves the tenant-wide inventory intact and the ADR-050
// mark-and-sweep recomputes reachability over the surviving snapshots: the
// deleted site's exclusive chunks are already reclaimed, and a chunk still
// shared with a LIVE site is already spared. Nothing here adds any authority to
// delete a chunk.
//
// SAME TRANSACTION IS THE WHOLE POINT. Writing the record in an earlier,
// separate transaction "before the cascade fires" is unsafe: if that commits
// and this delete then rolls back, the database holds a durable instruction to
// delete the manifests of a site that is still live. Insert and delete commit
// or roll back together, gated on the rows-affected check that was already
// here, so the record exists if and only if the site row is really gone.
func (r *pgRepo) Delete(ctx context.Context, tenantID, id uuid.UUID) error {
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)

		// Read the DEFAULT destination's kind before the delete:
		// site_destinations.site_id is also ON DELETE CASCADE, so after the next
		// statement this is gone too. Diagnostic only (never a credential): it
		// lets the reclaim log say plainly where the site's backup payload
		// lived. It says nothing about the manifests this delete is about, which
		// always sit in the control-plane bucket (main.go wires
		// SetIndexPutter(defStore)) whatever the destination is.
		//
		// The query filters is_default deliberately: a new snapshot only ever
		// resolves the site's DEFAULT destination, so a non-default row names a
		// place this site's backups never went. No rows therefore means "no
		// default destination", which is the control-plane bucket, and leaves
		// destKind nil. That is the ordinary case. A REAL error is
		// surfaced rather than swallowed: a failed statement aborts the
		// enclosing Postgres transaction, so ignoring it here would only move
		// the failure to the DELETE below and report it as a confusing
		// "current transaction is aborted".
		var destKind *string
		k, derr := q.GetSiteDefaultDestinationKind(ctx, sqlc.GetSiteDefaultDestinationKindParams{
			TenantID: tenantID,
			SiteID:   pgtype.UUID{Bytes: id, Valid: true},
		})
		switch {
		case derr == nil:
			destKind = &k
		case errors.Is(derr, pgx.ErrNoRows):
			// Legacy control-plane-global bucket; nothing to record.
		default:
			return domain.Internal("site_destination_lookup_failed",
				"failed to resolve the site's backup destination").WithCause(derr)
		}

		n, err := q.DeleteSite(ctx, sqlc.DeleteSiteParams{ID: id, TenantID: tenantID})
		if err != nil {
			return domain.Internal("site_delete_failed", "failed to delete site").WithCause(err)
		}
		if n == 0 {
			return domain.NotFound("site_not_found", "site not found")
		}

		// Only reached when a site row really was removed, so this can never
		// record work for a live site.
		if _, eerr := q.EnqueueSiteObjectReclaim(ctx, sqlc.EnqueueSiteObjectReclaimParams{
			TenantID:        tenantID,
			SiteID:          id,
			Kind:            backupManifestReclaimKind,
			DestinationKind: destKind,
		}); eerr != nil {
			// Deliberately fatal to the transaction. Committing the delete
			// without the record is precisely the reported bug, and it is
			// unrecoverable afterwards; failing the request is recoverable
			// because the caller can simply retry the delete.
			return domain.Internal("site_object_reclaim_enqueue_failed",
				"failed to record the site's object-storage reclamation").WithCause(eerr)
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
		q := sqlc.New(tx)
		row, err := q.SetSiteTags(ctx, sqlc.SetSiteTagsParams{
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
		// M100 (GH #230 "rich tags") binding invariant: every path that writes
		// tag names onto a site upserts those names into the tenant's tag
		// registry in the SAME transaction. Unused (usage 0) registry rows are
		// legitimate; this only ever ADDS names, never removes.
		if len(tags) > 0 {
			if err := q.UpsertTagNames(ctx, sqlc.UpsertTagNamesParams{TenantID: in.TenantID, Names: tags}); err != nil {
				return domain.Internal("site_set_tags_failed", "failed to register tag names").WithCause(err)
			}
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
		q := sqlc.New(tx)
		row, err := q.CreatePairingCode(ctx, sqlc.CreatePairingCodeParams{
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
		// M100 (GH #230 "rich tags") binding invariant: pairing-code minting is
		// an operator-authenticated tx, so it upserts the code's tags into the
		// registry here. The public /enroll consume path (app.enroll GUC) never
		// does this — it has no operator tenant scope to write under.
		if len(tags) > 0 {
			if err := q.UpsertTagNames(ctx, sqlc.UpsertTagNamesParams{TenantID: in.TenantID, Names: tags}); err != nil {
				return domain.Internal("pairing_code_create_failed", "failed to register tag names").WithCause(err)
			}
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
			// A genuinely NEW site is about to be born (the re-enroll/attach
			// branch above reuses an existing row and does not need this check).
			// M16 Phase A: gate it behind the tenant's site cap, in this same
			// enroll-scoped tx, using the tenant resolved from the verified
			// pairing code (pc.TenantID) — this is the public /enroll endpoint,
			// the #1 site-cap bypass risk if left unchecked.
			if r.billing != nil {
				if err := r.billing.CheckSiteCreate(ctx, tx, pc.TenantID); err != nil {
					return err
				}
			}
			// NOTE (m100 follow-up, GH #230): this write does NOT upsert `tags`
			// into the site_tags registry. This whole Enroll flow runs under
			// InEnrollTx (the app.enroll GUC), and site_tags carries no
			// app.enroll RLS policy (only tenant_isolation + agent — see m100
			// migration; deliberately narrow, matching the binding invariant
			// that "the app.enroll enrollment path needs ZERO registry
			// writes"). A net-new tag name introduced here (from the legacy
			// pairing code's stored tags, or a caller-supplied EnrollRequest.Tag
			// not already in pc.Tags) still lands on sites.tags and is fully
			// usable — it renders as a chip and is filterable via
			// ?tags=/?tags_match= — it just won't appear as its own row in the
			// tag registry (GET /api/v1/tags) until something else (a rename,
			// a SetTags call, or a future reconcile sweep) upserts it. A clean
			// fix would upsert here under InAgentTx instead of InEnrollTx (the
			// agent policy IS available), but that's a second transaction for
			// a rare edge case (CreatePairingCode already upserts every tag at
			// MINT time — this only matters for the original create-at-enroll
			// path's own stored pc.Tags, which per the mint-time upsert should
			// already be registered); left as a documented gap rather than
			// adding transaction complexity for it now.
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
			ID:           siteID,
			TenantID:     tenantID,
			WpVersion:    m.WPVersion,
			PhpVersion:   m.PHPVersion,
			ServerInfo:   m.ServerInfo,
			Multisite:    m.Multisite,
			ActiveTheme:  m.ActiveTheme,
			AgentVersion: m.AgentVersion,
			Components:   components,
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

func (r *pgRepo) GetSiteURL(ctx context.Context, tenantID, siteID uuid.UUID) (string, bool, error) {
	s, err := r.Get(ctx, tenantID, siteID)
	if err != nil {
		return "", false, err
	}
	enrolled := s.EnrolledAt != nil
	return s.URL, enrolled, nil
}

func (r *pgRepo) ListAllSiteIDs(ctx context.Context, tenantID uuid.UUID) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListAllSiteIDs(ctx, tenantID)
		if err != nil {
			return domain.Internal("site_list_all_ids_failed", "failed to list all site IDs").WithCause(err)
		}
		ids = rows
		return nil
	})
	return ids, err
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
		ID:              s.ID,
		TenantID:        s.TenantID,
		URL:             s.Url,
		Name:            s.Name,
		Status:          s.Status,
		WPVersion:       s.WpVersion,
		PHPVersion:      s.PhpVersion,
		AgentVersion:    s.AgentVersion,
		AgentPublicKey:  s.AgentPublicKey,
		HealthStatus:    s.HealthStatus,
		ServerInfo:      s.ServerInfo,
		Multisite:       s.Multisite,
		ActiveTheme:     s.ActiveTheme,
		Components:      s.Components,
		Tags:            s.Tags,
		AgeRecipient:    s.AgeRecipient,
		WpTimezone:      s.WpTimezone,
		WpGmtOffset:     float64(s.WpGmtOffset),
		HostProvider:    s.HostProvider,
		HostProviderOrg: s.HostProviderOrg,
		HostProviderIP:  s.HostProviderIp,
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
	if s.ClientID.Valid {
		id := uuid.UUID(s.ClientID.Bytes)
		m.ClientID = &id
	}
	return m
}

// toModelFromGetSiteRow converts the GH #243 GetSite row (the sites.* columns
// plus the joined page_cache_enabled/object_cache_enabled) into a Site. It
// re-uses toModel for the base sites.* fields by re-assembling an sqlc.Site
// from the row (GetSiteRow's join adds two trailing columns sqlc.Site does
// not have, so the two types cannot share Scan destinations directly).
func toModelFromGetSiteRow(row sqlc.GetSiteRow) Site {
	m := toModel(sqlc.Site{
		ID:                    row.ID,
		TenantID:              row.TenantID,
		Url:                   row.Url,
		Name:                  row.Name,
		Status:                row.Status,
		WpVersion:             row.WpVersion,
		PhpVersion:            row.PhpVersion,
		AgentVersion:          row.AgentVersion,
		AgentPublicKey:        row.AgentPublicKey,
		EnrolledAt:            row.EnrolledAt,
		LastSeenAt:            row.LastSeenAt,
		HealthStatus:          row.HealthStatus,
		ServerInfo:            row.ServerInfo,
		Multisite:             row.Multisite,
		ActiveTheme:           row.ActiveTheme,
		Components:            row.Components,
		Tags:                  row.Tags,
		AgeRecipient:          row.AgeRecipient,
		WpTimezone:            row.WpTimezone,
		WpGmtOffset:           row.WpGmtOffset,
		HostProvider:          row.HostProvider,
		HostProviderOrg:       row.HostProviderOrg,
		HostProviderIp:        row.HostProviderIp,
		HostProviderCheckedAt: row.HostProviderCheckedAt,
		ConnectionState:       row.ConnectionState,
		ConnectionGeneration:  row.ConnectionGeneration,
		DisconnectedAt:        row.DisconnectedAt,
		DisconnectedReason:    row.DisconnectedReason,
		ArchivedAt:            row.ArchivedAt,
		MissedHeartbeats:      row.MissedHeartbeats,
		ClientID:              row.ClientID,
		CreatedAt:             row.CreatedAt,
		UpdatedAt:             row.UpdatedAt,
	})
	m.PageCacheEnabled = row.PageCacheEnabled
	m.ObjectCacheEnabled = row.ObjectCacheEnabled
	return m
}

// toModelFromListSitesRow is toModelFromGetSiteRow's ListSites counterpart
// (identical join, separate sqlc row type since sqlc generates one Row struct
// per query).
func toModelFromListSitesRow(row sqlc.ListSitesRow) Site {
	m := toModel(sqlc.Site{
		ID:                    row.ID,
		TenantID:              row.TenantID,
		Url:                   row.Url,
		Name:                  row.Name,
		Status:                row.Status,
		WpVersion:             row.WpVersion,
		PhpVersion:            row.PhpVersion,
		AgentVersion:          row.AgentVersion,
		AgentPublicKey:        row.AgentPublicKey,
		EnrolledAt:            row.EnrolledAt,
		LastSeenAt:            row.LastSeenAt,
		HealthStatus:          row.HealthStatus,
		ServerInfo:            row.ServerInfo,
		Multisite:             row.Multisite,
		ActiveTheme:           row.ActiveTheme,
		Components:            row.Components,
		Tags:                  row.Tags,
		AgeRecipient:          row.AgeRecipient,
		WpTimezone:            row.WpTimezone,
		WpGmtOffset:           row.WpGmtOffset,
		HostProvider:          row.HostProvider,
		HostProviderOrg:       row.HostProviderOrg,
		HostProviderIp:        row.HostProviderIp,
		HostProviderCheckedAt: row.HostProviderCheckedAt,
		ConnectionState:       row.ConnectionState,
		ConnectionGeneration:  row.ConnectionGeneration,
		DisconnectedAt:        row.DisconnectedAt,
		DisconnectedReason:    row.DisconnectedReason,
		ArchivedAt:            row.ArchivedAt,
		MissedHeartbeats:      row.MissedHeartbeats,
		ClientID:              row.ClientID,
		CreatedAt:             row.CreatedAt,
		UpdatedAt:             row.UpdatedAt,
	})
	m.PageCacheEnabled = row.PageCacheEnabled
	m.ObjectCacheEnabled = row.ObjectCacheEnabled
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
