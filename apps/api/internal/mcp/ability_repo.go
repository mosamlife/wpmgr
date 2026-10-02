package mcp

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// abilityInventoryReadLimit bounds one site's inventory read. The agent
// reports at most 500 abilities; the refresh stores at most that many.
const abilityInventoryReadLimit = 500

// AbilityStore is the ability tools' database access. Every method runs in
// the transaction RunTenantTx picks for p, which for a SingleSitePrincipal is
// the scoped tenant transaction: the RESTRICTIVE site-scope policy applies.
type AbilityStore interface {
	// SiteAbilities reads one site's last refresh record and its inventory,
	// keyed by site id, in ONE transaction. run is nil when the site has never
	// been refreshed.
	SiteAbilities(ctx context.Context, p domain.Principal, siteID uuid.UUID) (run *sqlc.SiteAbilityInventoryRun, rows []sqlc.SiteAbilityInventory, err error)
	// AbilityCatalogue reads every catalogue entry (any status) and, in the
	// SAME transaction, the entries switched off for p's tenant (m160): a
	// vendor read caught changing a site is off for its reporting tenant at
	// once, whatever the fleet-wide state.
	AbilityCatalogue(ctx context.Context, p domain.Principal) ([]sqlc.AbilityCatalogue, map[uuid.UUID]bool, error)
}

// SiteAbilities implements AbilityStore.
func (r *Repo) SiteAbilities(ctx context.Context, p domain.Principal, siteID uuid.UUID) (*sqlc.SiteAbilityInventoryRun, []sqlc.SiteAbilityInventory, error) {
	var run *sqlc.SiteAbilityInventoryRun
	var rows []sqlc.SiteAbilityInventory
	err := r.pool.RunTenantTx(ctx, p, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		got, err := q.GetSiteAbilityInventoryRun(ctx, sqlc.GetSiteAbilityInventoryRunParams{TenantID: p.TenantID, SiteID: siteID})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return nil
		case err != nil:
			return err
		}
		run = &got
		rows, err = q.ListSiteAbilityInventory(ctx, sqlc.ListSiteAbilityInventoryParams{
			TenantID: p.TenantID, SiteID: siteID, AfterName: "", RowLimit: abilityInventoryReadLimit,
		})
		return err
	})
	return run, rows, err
}

// RecordAbilityReadSideEffect implements AbilitySideEffectRecorder: m160's
// definer, in the site's own tenant transaction (the definer takes the tenant
// from app.tenant_id), short and READ COMMITTED (the pool default), as the
// application role. It returns the qualified-tenant count.
func (r *Repo) RecordAbilityReadSideEffect(ctx context.Context, tenantID, entryID, siteID uuid.UUID) (int32, error) {
	var n int32
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		n, err = sqlc.New(tx).RecordAbilityReadSideEffect(ctx, sqlc.RecordAbilityReadSideEffectParams{
			EntryID: entryID, SiteID: siteID, TenantID: tenantID,
		})
		return err
	})
	return n, err
}

var _ AbilitySideEffectRecorder = (*Repo)(nil)

// AbilityCatalogue implements AbilityStore.
func (r *Repo) AbilityCatalogue(ctx context.Context, p domain.Principal) ([]sqlc.AbilityCatalogue, map[uuid.UUID]bool, error) {
	var out []sqlc.AbilityCatalogue
	var off map[uuid.UUID]bool
	err := r.pool.RunTenantTx(ctx, p, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		var err error
		if out, err = q.ListAbilityCatalogue(ctx); err != nil {
			return err
		}
		ids, err := q.ListAbilityTenantDisabledEntryIDs(ctx, p.TenantID)
		if err != nil {
			return err
		}
		off = make(map[uuid.UUID]bool, len(ids))
		for _, id := range ids {
			off[id] = true
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return out, off, nil
}
