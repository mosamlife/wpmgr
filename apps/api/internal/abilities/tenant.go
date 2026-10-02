package abilities

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// ---------------------------------------------------------------------------
// The per-tenant re-enable (m160, owner ruling 2026-10-02). A vendor read
// caught changing a site or calling out is switched off for the reporting
// tenant at once. A tenant admin or owner, or a superadmin, switches it back
// on for that tenant here. The fleet-wide switch is the superadmin catalogue
// route's, not this one's.
// ---------------------------------------------------------------------------

// ActionAbilityTenantReenabled is the audit action for the re-enable.
const ActionAbilityTenantReenabled = "ability.tenant_reenabled"

// SuperadminChecker answers users.is_superadmin (admingate.Store satisfies it).
type SuperadminChecker interface {
	IsSuperadmin(ctx context.Context, userID uuid.UUID) (bool, error)
}

// TenantReenableStore switches one entry back on for p's tenant and audits it
// in the same transaction. It reports false when the entry was not switched
// off for the tenant (nothing written).
type TenantReenableStore interface {
	ReenableForTenant(ctx context.Context, p domain.Principal, entryID uuid.UUID, superadmin bool) (bool, error)
}

// TenantHandler serves the tenant-facing ability routes.
type TenantHandler struct {
	store TenantReenableStore
	sa    SuperadminChecker
}

// NewTenantHandler builds the handler. Both arguments are required.
func NewTenantHandler(store TenantReenableStore, sa SuperadminChecker) *TenantHandler {
	return &TenantHandler{store: store, sa: sa}
}

// Register mounts, on the authenticated /api/v1 group (RequireAuth and
// RequireTenant already applied):
//
//	POST /ai/abilities/:entryId/reenable
//
// The role check is in the handler, not RequirePermission, because a
// superadmin passes whatever their role. JSON only (the CSRF guard for a
// cookie-authenticated change).
func (h *TenantHandler) Register(r *gin.RouterGroup) {
	r.POST("/ai/abilities/:entryId/reenable", httpx.RequireJSONBody(), h.reenable)
}

var (
	errReenableSession = domain.Forbidden("session_required",
		"only a signed-in person can turn a tool back on")
	errReenableRole = domain.Forbidden("insufficient_role",
		"only an admin or owner of this account can turn this tool back on")
	errReenableNotOff = domain.NotFound("ability_not_disabled_for_account",
		"this tool is not switched off for your account")
)

// authorizeReenable admits a signed-in person who is a full-organisation
// admin or owner of the active tenant, or a superadmin. It fails closed on a
// superadmin lookup error.
func (h *TenantHandler) authorizeReenable(ctx context.Context, p domain.Principal) (superadmin bool, err error) {
	if p.Type != domain.PrincipalUser || p.UserID == uuid.Nil || p.TenantID == uuid.Nil {
		return false, errReenableSession
	}
	if !p.IsSiteConstrained() && authz.Role(p.Role).AtLeast(authz.RoleAdmin) {
		return false, nil
	}
	if h.sa == nil {
		return false, errReenableRole
	}
	isSA, saErr := h.sa.IsSuperadmin(ctx, p.UserID)
	if saErr != nil || !isSA {
		return false, errReenableRole
	}
	return true, nil
}

type reenableResponse struct {
	EntryID   uuid.UUID `json:"entry_id"`
	Reenabled bool      `json:"reenabled"`
}

func (h *TenantHandler) reenable(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	entryID, err := uuid.Parse(c.Param("entryId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_entryId", "entryId is not a valid UUID"))
		return
	}
	superadmin, err := h.authorizeReenable(c.Request.Context(), p)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	done, err := h.store.ReenableForTenant(c.Request.Context(), p, entryID, superadmin)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	if !done {
		httpx.Error(c, errReenableNotOff)
		return
	}
	c.JSON(http.StatusOK, reenableResponse{EntryID: entryID, Reenabled: true})
}

// TenantRepo is the production TenantReenableStore.
type TenantRepo struct {
	pool *db.Pool
	rec  *audit.Recorder
}

// NewTenantRepo builds it. rec is required: the re-enable is never unaudited.
func NewTenantRepo(pool *db.Pool, rec *audit.Recorder) *TenantRepo {
	return &TenantRepo{pool: pool, rec: rec}
}

// ReenableForTenant implements TenantReenableStore: the update and its audit
// row in one tenant transaction as the user, so either both land or neither.
func (r *TenantRepo) ReenableForTenant(ctx context.Context, p domain.Principal, entryID uuid.UUID, superadmin bool) (bool, error) {
	if r.rec == nil {
		return false, domain.Internal("audit_unavailable", "audit recorder not configured")
	}
	var done bool
	err := r.pool.InTenantTxAsUser(ctx, p.TenantID, p.UserID, func(tx pgx.Tx) error {
		n, err := sqlc.New(tx).ReenableAbilityForTenant(ctx, sqlc.ReenableAbilityForTenantParams{
			UserID: p.UserID, TenantID: p.TenantID, EntryID: entryID,
		})
		if err != nil || n == 0 {
			return err
		}
		done = true
		_, err = r.rec.RecordInTx(ctx, tx, audit.Event{
			TenantID:   p.TenantID,
			ActorType:  audit.ActorUser,
			ActorID:    p.UserID.String(),
			Action:     ActionAbilityTenantReenabled,
			TargetType: "ability_catalogue_entry",
			TargetID:   entryID.String(),
			Metadata:   map[string]any{"as_superadmin": superadmin, "role": p.Role},
		})
		return err
	})
	if err != nil {
		return false, err
	}
	return done, nil
}

var _ TenantReenableStore = (*TenantRepo)(nil)
