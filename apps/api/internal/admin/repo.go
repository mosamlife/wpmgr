package admin

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

// Repo provides superadmin data access. All queries run on the bare pool without
// RLS tenant scope — the users table has no RLS, and the admin area is gated by
// the requireSuperadmin middleware.
type Repo struct {
	pool *db.Pool
	q    *sqlc.Queries
}

// NewRepo builds an admin Repo over the pgx pool.
func NewRepo(pool *db.Pool) *Repo {
	return &Repo{pool: pool, q: sqlc.New(pool.Pool)}
}

// AdminUser is the view model for the superadmin user list.
type AdminUser struct {
	ID            uuid.UUID
	Email         string
	Name          string
	Status        string
	EmailVerified bool
	CreatedAt     time.Time
	LastLoginAt   *time.Time
	IsSuperadmin  bool
	OrgCount      int64
}

// AdminStats holds instance-wide counts.
type AdminStats struct {
	Users int64
	Orgs  int64
	Sites int64
}

// asBool safely converts an interface{} column from a computed boolean expression
// (e.g. `email_verified_at IS NOT NULL AS email_verified`) to a Go bool.
func asBool(v interface{}) bool {
	if v == nil {
		return false
	}
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}

// ListUsers returns all users across the instance, optionally filtered by search.
// An empty search string matches all users. The org_count column is a LEFT JOIN
// onto memberships, which has FORCE row-level security, so the query MUST run
// under app.agent='on' (the memberships_agent SELECT policy) via InAgentTx — on
// the bare pool the unset app.tenant_id GUC would hide every membership and make
// org_count always 0.
func (r *Repo) ListUsers(ctx context.Context, search string, limit, offset int32) ([]AdminUser, error) {
	var out []AdminUser
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).AdminListUsers(ctx, sqlc.AdminListUsersParams{
			Column1: search,
			Limit:   limit,
			Offset:  offset,
		})
		if err != nil {
			return err
		}
		out = make([]AdminUser, 0, len(rows))
		for _, row := range rows {
			u := AdminUser{
				ID:            row.ID,
				Email:         row.Email,
				Name:          row.Name,
				Status:        row.Status,
				EmailVerified: asBool(row.EmailVerified),
				CreatedAt:     row.CreatedAt,
				IsSuperadmin:  row.IsSuperadmin,
				OrgCount:      row.OrgCount,
			}
			if row.LastLoginAt.Valid {
				t := row.LastLoginAt.Time
				u.LastLoginAt = &t
			}
			out = append(out, u)
		}
		return nil
	})
	if err != nil {
		return nil, domain.Internal("admin_list_users_failed", "failed to list users").WithCause(err)
	}
	return out, nil
}

// GetUser loads a single user by ID for the superadmin view.
func (r *Repo) GetUser(ctx context.Context, id uuid.UUID) (AdminUser, error) {
	row, err := r.q.AdminGetUserByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AdminUser{}, domain.NotFound("user_not_found", "user not found")
		}
		return AdminUser{}, domain.Internal("admin_get_user_failed", "failed to load user").WithCause(err)
	}
	u := AdminUser{
		ID:            row.ID,
		Email:         row.Email,
		Name:          row.Name,
		Status:        row.Status,
		EmailVerified: asBool(row.EmailVerified),
		CreatedAt:     row.CreatedAt,
		IsSuperadmin:  row.IsSuperadmin,
	}
	if row.LastLoginAt.Valid {
		t := row.LastLoginAt.Time
		u.LastLoginAt = &t
	}
	return u, nil
}

// DeleteUser permanently deletes a user by ID. Returns NotFound when no row
// was deleted.
func (r *Repo) DeleteUser(ctx context.Context, id uuid.UUID) error {
	n, err := r.q.AdminDeleteUser(ctx, id)
	if err != nil {
		return domain.Internal("admin_delete_user_failed", "failed to delete user").WithCause(err)
	}
	if n == 0 {
		return domain.NotFound("user_not_found", "user not found")
	}
	return nil
}

// OrphanTenant is a tenant that a user is the sole member of — deleting that
// user would leave the org memberless. SiteCount distinguishes a truly empty
// org (safe to remove) from one that still owns sites (kept + flagged).
type OrphanTenant struct {
	ID        uuid.UUID
	Name      string
	SiteCount int64
}

// SoleTenants returns the tenants in which userID is the ONLY member, with each
// tenant's name + site count. It runs under InAgentTx (memberships_agent +
// sites_agent) so the cross-tenant counts are visible. Call this BEFORE deleting
// the user — afterwards the membership rows are gone and the orphans cannot be
// reconstructed.
func (r *Repo) SoleTenants(ctx context.Context, userID uuid.UUID) ([]OrphanTenant, error) {
	var out []OrphanTenant
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).AdminUserSoleTenants(ctx, userID)
		if err != nil {
			return err
		}
		out = make([]OrphanTenant, 0, len(rows))
		for _, row := range rows {
			out = append(out, OrphanTenant{ID: row.TenantID, Name: row.TenantName, SiteCount: row.SiteCount})
		}
		return nil
	})
	if err != nil {
		return nil, domain.Internal("admin_sole_tenants_failed", "failed to inspect orphaned orgs").WithCause(err)
	}
	return out, nil
}

// DeleteEmptyTenant removes a tenant only if it has no memberships and no sites,
// returning true when a row was actually deleted. It delegates to the
// admin_delete_empty_tenant SECURITY DEFINER function (owner privileges) because
// a tenant's ON DELETE CASCADE reaches audit_log, which wpmgr_app may not delete
// (the trail is insert-only) — a direct DELETE fails 42501. The function re-checks
// emptiness inside the statement and pins app.agent='on' itself, so a tenant that
// gained a member or site between SoleTenants and this call is left intact. The
// InAgentTx wrapper is retained for transactional consistency with the caller.
func (r *Repo) DeleteEmptyTenant(ctx context.Context, tenantID uuid.UUID) (bool, error) {
	var deleted bool
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		ok, err := sqlc.New(tx).AdminDeleteEmptyTenant(ctx, tenantID)
		if err != nil {
			return err
		}
		deleted = ok
		return nil
	})
	if err != nil {
		return false, domain.Internal("admin_delete_tenant_failed", "failed to delete orphaned org").WithCause(err)
	}
	return deleted, nil
}

// TenancyRef is one tenant reference in the site-tenancy diagnostic. Role is the
// membership/share role or the source table label; Count is the row count.
type TenancyRef struct {
	TenantID   uuid.UUID `json:"tenant_id"`
	TenantName string    `json:"tenant_name"`
	Role       string    `json:"role,omitempty"`
	Count      int64     `json:"count,omitempty"`
}

// SiteTenancyReport compares where a site + its perf data live against a user's
// org memberships — for diagnosing a tenant/ownership split.
type SiteTenancyReport struct {
	SiteID         uuid.UUID    `json:"site_id"`
	SiteFound      bool         `json:"site_found"`
	SiteTenantID   uuid.UUID    `json:"site_tenant_id"`
	SiteTenantName string       `json:"site_tenant_name"`
	SiteURL        string       `json:"site_url"`
	DataTenants    []TenancyRef `json:"data_tenants"`
	Memberships    []TenancyRef `json:"your_memberships"`
	SiteShares     []TenancyRef `json:"site_shares"`
}

// SiteTenancy returns where a site (and its rucss/cache-stats/config rows) live vs
// the requesting user's orgs. Runs under InAgentTx so the *_agent RLS policies
// expose rows cross-tenant. Read-only.
func (r *Repo) SiteTenancy(ctx context.Context, userID, siteID uuid.UUID) (SiteTenancyReport, error) {
	rep := SiteTenancyReport{SiteID: siteID}
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		// site owner
		serr := tx.QueryRow(ctx,
			`SELECT s.tenant_id, t.name, s.url FROM sites s JOIN tenants t ON t.id = s.tenant_id WHERE s.id = $1`,
			siteID,
		).Scan(&rep.SiteTenantID, &rep.SiteTenantName, &rep.SiteURL)
		if serr == nil {
			rep.SiteFound = true
		} else if !errors.Is(serr, pgx.ErrNoRows) {
			return serr
		}

		// which tenant owns each perf table's rows for this site
		dataQ := []struct{ label, sql string }{
			{"rucss_results", `SELECT r.tenant_id, t.name, count(*) FROM rucss_results r JOIN tenants t ON t.id = r.tenant_id WHERE r.site_id = $1 GROUP BY r.tenant_id, t.name`},
			{"site_cache_stats", `SELECT c.tenant_id, t.name, count(*) FROM site_cache_stats c JOIN tenants t ON t.id = c.tenant_id WHERE c.site_id = $1 GROUP BY c.tenant_id, t.name`},
			{"site_perf_config", `SELECT pc.tenant_id, t.name, count(*) FROM site_perf_config pc JOIN tenants t ON t.id = pc.tenant_id WHERE pc.site_id = $1 GROUP BY pc.tenant_id, t.name`},
		}
		for _, dq := range dataQ {
			rows, qerr := tx.Query(ctx, dq.sql, siteID)
			if qerr != nil {
				return qerr
			}
			for rows.Next() {
				ref := TenancyRef{Role: dq.label}
				if scanErr := rows.Scan(&ref.TenantID, &ref.TenantName, &ref.Count); scanErr != nil {
					rows.Close()
					return scanErr
				}
				rep.DataTenants = append(rep.DataTenants, ref)
			}
			rows.Close()
			if rows.Err() != nil {
				return rows.Err()
			}
		}

		// the requesting user's org memberships
		mem, merr := collectRefs(ctx, tx,
			`SELECT m.tenant_id, t.name, m.role FROM memberships m JOIN tenants t ON t.id = m.tenant_id WHERE m.user_id = $1`,
			userID)
		if merr != nil {
			return merr
		}
		rep.Memberships = mem
		// any per-site share of this site
		shares, sherr := collectRefs(ctx, tx,
			`SELECT sh.tenant_id, t.name, sh.role FROM site_shares sh JOIN tenants t ON t.id = sh.tenant_id WHERE sh.site_id = $1`,
			siteID)
		if sherr != nil {
			return sherr
		}
		rep.SiteShares = shares
		return nil
	})
	if err != nil {
		return SiteTenancyReport{}, domain.Internal("admin_site_tenancy_failed", "failed to inspect site tenancy").WithCause(err)
	}
	return rep, nil
}

// GrantSelfOwnerMembership adds userID as an 'owner' member of the tenant that
// owns siteID, idempotently. Returns the tenant id/name and whether a row was
// actually inserted. Used to re-attach a superadmin to a site's org after a
// recovery left their account in a different org. The site-tenant lookup runs
// cross-tenant under InAgentTx; the INSERT runs under InTenantTx(siteTenant) so
// the memberships tenant_isolation WITH CHECK (tenant_id = app.tenant_id) passes.
func (r *Repo) GrantSelfOwnerMembership(ctx context.Context, userID, siteID uuid.UUID) (uuid.UUID, string, bool, error) {
	var tenantID uuid.UUID
	var tenantName string
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT s.tenant_id, t.name FROM sites s JOIN tenants t ON t.id = s.tenant_id WHERE s.id = $1`,
			siteID,
		).Scan(&tenantID, &tenantName)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, "", false, domain.NotFound("site_not_found", "site not found")
		}
		return uuid.Nil, "", false, domain.Internal("admin_resolve_site_tenant_failed", "failed to resolve site tenant").WithCause(err)
	}

	var added bool
	err = r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		ct, ierr := tx.Exec(ctx,
			`INSERT INTO memberships (user_id, tenant_id, role)
			 SELECT $1, $2, 'owner'
			 WHERE NOT EXISTS (SELECT 1 FROM memberships WHERE user_id = $1 AND tenant_id = $2)`,
			userID, tenantID,
		)
		if ierr != nil {
			return ierr
		}
		added = ct.RowsAffected() > 0
		return nil
	})
	if err != nil {
		return uuid.Nil, "", false, domain.Internal("admin_grant_membership_failed", "failed to add membership").WithCause(err)
	}
	return tenantID, tenantName, added, nil
}

// AccountUserMembership is one org membership within the accounts-tenancy
// diagnostic report.
type AccountUserMembership struct {
	TenantID   uuid.UUID `json:"tenant_id"`
	TenantName string    `json:"tenant_name"`
	Role       string    `json:"role"`
}

// AccountUser represents one user in the accounts-tenancy diagnostic, with all
// their org memberships inline.
type AccountUser struct {
	ID           uuid.UUID               `json:"id"`
	Email        string                  `json:"email"`
	IsSuperadmin bool                    `json:"is_superadmin"`
	Memberships  []AccountUserMembership `json:"memberships"`
}

// AccountOrg is a summary row for one org in the accounts-tenancy diagnostic.
type AccountOrg struct {
	TenantID    uuid.UUID `json:"tenant_id"`
	TenantName  string    `json:"tenant_name"`
	SiteCount   int64     `json:"site_count"`
	MemberCount int64     `json:"member_count"`
}

// AccountsTenancyReport is the full response for GET /admin/accounts-tenancy.
type AccountsTenancyReport struct {
	Users []AccountUser `json:"users"`
	Orgs  []AccountOrg  `json:"orgs"`
}

// AccountsTenancy returns every user whose email ILIKE %emailSubstr%, with
// their memberships, plus a full org census (every tenant with site + member
// counts). Everything runs under InAgentTx (memberships_agent + sites_agent)
// so cross-tenant rows are visible. Read-only.
func (r *Repo) AccountsTenancy(ctx context.Context, emailSubstr string) (AccountsTenancyReport, error) {
	var rep AccountsTenancyReport
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		// 1. Matching users (ILIKE %substr%)
		userRows, err := tx.Query(ctx,
			`SELECT id, email, is_superadmin FROM users
			 WHERE email ILIKE '%' || $1 || '%'
			 ORDER BY email`,
			emailSubstr,
		)
		if err != nil {
			return err
		}
		defer userRows.Close()
		userByID := map[uuid.UUID]*AccountUser{}
		for userRows.Next() {
			var u AccountUser
			if err := userRows.Scan(&u.ID, &u.Email, &u.IsSuperadmin); err != nil {
				return err
			}
			u.Memberships = []AccountUserMembership{}
			rep.Users = append(rep.Users, u)
			// keep pointer into the slice element for membership decoration below
		}
		if err := userRows.Err(); err != nil {
			return err
		}
		userRows.Close()

		// Build a stable lookup by re-indexing into the slice (pointer-stable after
		// we stop appending).
		for i := range rep.Users {
			userByID[rep.Users[i].ID] = &rep.Users[i]
		}

		// 2. Memberships for all matching users in one query (IN list built from IDs)
		if len(rep.Users) > 0 {
			ids := make([]uuid.UUID, 0, len(rep.Users))
			for _, u := range rep.Users {
				ids = append(ids, u.ID)
			}
			memRows, merr := tx.Query(ctx,
				`SELECT m.user_id, m.tenant_id, t.name, m.role
				 FROM memberships m
				 JOIN tenants t ON t.id = m.tenant_id
				 WHERE m.user_id = ANY($1)
				 ORDER BY t.name`,
				ids,
			)
			if merr != nil {
				return merr
			}
			defer memRows.Close()
			for memRows.Next() {
				var userID uuid.UUID
				var mem AccountUserMembership
				if err := memRows.Scan(&userID, &mem.TenantID, &mem.TenantName, &mem.Role); err != nil {
					return err
				}
				if u, ok := userByID[userID]; ok {
					u.Memberships = append(u.Memberships, mem)
				}
			}
			if err := memRows.Err(); err != nil {
				return err
			}
			memRows.Close()
		}

		// 3. Full org census — every tenant with site + member counts
		orgRows, oerr := tx.Query(ctx,
			`SELECT t.id,
			        t.name,
			        COUNT(DISTINCT s.id)::bigint  AS site_count,
			        COUNT(DISTINCT m.user_id)::bigint AS member_count
			 FROM tenants t
			 LEFT JOIN sites s       ON s.tenant_id = t.id
			 LEFT JOIN memberships m ON m.tenant_id = t.id
			 GROUP BY t.id, t.name
			 ORDER BY t.name`,
		)
		if oerr != nil {
			return oerr
		}
		defer orgRows.Close()
		for orgRows.Next() {
			var o AccountOrg
			if err := orgRows.Scan(&o.TenantID, &o.TenantName, &o.SiteCount, &o.MemberCount); err != nil {
				return err
			}
			rep.Orgs = append(rep.Orgs, o)
		}
		return orgRows.Err()
	})
	if err != nil {
		return AccountsTenancyReport{}, domain.Internal("admin_accounts_tenancy_failed", "failed to load accounts tenancy").WithCause(err)
	}
	// Guarantee non-nil slices for clean JSON serialisation.
	if rep.Users == nil {
		rep.Users = []AccountUser{}
	}
	if rep.Orgs == nil {
		rep.Orgs = []AccountOrg{}
	}
	return rep, nil
}

// AdminUserSite is one site record in the per-user sites list for the superadmin view.
type AdminUserSite struct {
	SiteID          uuid.UUID  `json:"site_id"`
	URL             string     `json:"url"`
	Name            string     `json:"name"`
	ConnectionState string     `json:"connection_state"`
	EnrolledAt      *time.Time `json:"enrolled_at"`
	SiteCreatedAt   time.Time  `json:"site_created_at"`
	TenantID        uuid.UUID  `json:"tenant_id"`
	TenantName      string     `json:"tenant_name"`
	MemberRole      string     `json:"member_role"`
}

// ListSitesByUser returns every site reachable via the membership chain for
// targetUserID. The query joins memberships → tenants → sites and runs under
// InAgentTx so the memberships_agent + sites_agent RLS policies expose
// cross-tenant rows. No LIMIT is applied here; the caller (Service) imposes a
// pragmatic cap.
func (r *Repo) ListSitesByUser(ctx context.Context, userID uuid.UUID) ([]AdminUserSite, error) {
	var out []AdminUserSite
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT
			    s.id              AS site_id,
			    s.url,
			    s.name,
			    s.connection_state,
			    s.enrolled_at,
			    s.created_at      AS site_created_at,
			    s.tenant_id,
			    t.name            AS tenant_name,
			    m.role            AS member_role
			FROM memberships m
			JOIN tenants t  ON t.id = m.tenant_id
			JOIN sites s    ON s.tenant_id = m.tenant_id
			WHERE m.user_id = $1
			ORDER BY s.created_at DESC, s.id DESC`,
			userID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		out = make([]AdminUserSite, 0)
		for rows.Next() {
			var site AdminUserSite
			var enrolledAt *time.Time
			if err := rows.Scan(
				&site.SiteID,
				&site.URL,
				&site.Name,
				&site.ConnectionState,
				&enrolledAt,
				&site.SiteCreatedAt,
				&site.TenantID,
				&site.TenantName,
				&site.MemberRole,
			); err != nil {
				return err
			}
			site.EnrolledAt = enrolledAt
			out = append(out, site)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, domain.Internal("admin_list_sites_by_user_failed", "failed to list sites for user").WithCause(err)
	}
	return out, nil
}

// collectRefs runs a (tenant_id, name, role) query and returns the refs.
func collectRefs(ctx context.Context, tx pgx.Tx, sql string, arg uuid.UUID) ([]TenancyRef, error) {
	rows, err := tx.Query(ctx, sql, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TenancyRef
	for rows.Next() {
		var ref TenancyRef
		if err := rows.Scan(&ref.TenantID, &ref.TenantName, &ref.Role); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// SetStatus updates a user's status and returns the updated view model.
func (r *Repo) SetStatus(ctx context.Context, id uuid.UUID, status string) (AdminUser, error) {
	row, err := r.q.AdminSetUserStatus(ctx, sqlc.AdminSetUserStatusParams{ID: id, Status: status})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AdminUser{}, domain.NotFound("user_not_found", "user not found")
		}
		return AdminUser{}, domain.Internal("admin_set_status_failed", "failed to update status").WithCause(err)
	}
	u := AdminUser{
		ID:            row.ID,
		Email:         row.Email,
		Name:          row.Name,
		Status:        row.Status,
		EmailVerified: asBool(row.EmailVerified),
		CreatedAt:     row.CreatedAt,
		IsSuperadmin:  row.IsSuperadmin,
	}
	if row.LastLoginAt.Valid {
		t := row.LastLoginAt.Time
		u.LastLoginAt = &t
	}
	return u, nil
}

// Stats returns instance-wide counts for users, orgs, and sites. The sites
// table has FORCE row-level security, so the count must run under app.agent='on'
// (the sites_agent policy) via InAgentTx — on the bare pool the unset
// app.tenant_id GUC would make the sites count always 0. users + tenants have no
// RLS, so their counts are unaffected by the agent scope.
func (r *Repo) Stats(ctx context.Context) (AdminStats, error) {
	var out AdminStats
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).AdminInstanceStats(ctx)
		if err != nil {
			return err
		}
		out = AdminStats{Users: row.UserCount, Orgs: row.OrgCount, Sites: row.SiteCount}
		return nil
	})
	if err != nil {
		return AdminStats{}, domain.Internal("admin_stats_failed", "failed to load stats").WithCause(err)
	}
	return out, nil
}

// SystemAuditEvent is one row of the tenant-INDEPENDENT audit trail.
//
// TenantID/TenantName are denormalized snapshots, not references: the whole
// reason this table exists is to outlive the organisation an event concerned.
// Both are empty for an event that never had an organisation at all, which is
// the case for the auth events of accounts with no membership.
type SystemAuditEvent struct {
	ID         uuid.UUID
	OccurredAt time.Time
	ActorType  string
	ActorID    *uuid.UUID
	Action     string
	TenantID   *uuid.UUID
	TenantName string
	Metadata   []byte
}

// ListSystemAuditEvents returns a page of the tenant-independent audit trail,
// newest first, plus the total row count for paging.
//
// system_audit_log carries no RLS (it is not tenant-scoped data, see its
// schema.sql comment), so this reads on the bare pool. The gate is the
// superadmin middleware on the route, and it is the only gate: these rows span
// every account on the install.
func (r *Repo) ListSystemAuditEvents(ctx context.Context, limit, offset int32) ([]SystemAuditEvent, int64, error) {
	rows, err := r.q.ListSystemAuditEvents(ctx, sqlc.ListSystemAuditEventsParams{
		RowLimit:  limit,
		RowOffset: offset,
	})
	if err != nil {
		return nil, 0, domain.Internal("system_audit_list_failed", "failed to load system audit log").WithCause(err)
	}
	total, err := r.q.CountSystemAuditEvents(ctx)
	if err != nil {
		return nil, 0, domain.Internal("system_audit_count_failed", "failed to count system audit log").WithCause(err)
	}

	out := make([]SystemAuditEvent, 0, len(rows))
	for _, row := range rows {
		ev := SystemAuditEvent{
			ID:         row.ID,
			OccurredAt: row.OccurredAt,
			ActorType:  row.ActorType,
			Action:     row.Action,
			TenantName: row.TenantName,
			Metadata:   row.Metadata,
		}
		if row.ActorID.Valid {
			id := uuid.UUID(row.ActorID.Bytes)
			ev.ActorID = &id
		}
		// The nil UUID is how a tenantless event is stored (the column is NOT
		// NULL and has no FK). Surfaced as absent rather than as a zero id that
		// a reader could mistake for a real organisation.
		if row.TenantID != uuid.Nil {
			id := row.TenantID
			ev.TenantID = &id
		}
		out = append(out, ev)
	}
	return out, total, nil
}
