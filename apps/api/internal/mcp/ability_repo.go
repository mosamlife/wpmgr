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
	// AbilityCatalogue reads every catalogue entry (any status).
	AbilityCatalogue(ctx context.Context, p domain.Principal) ([]sqlc.AbilityCatalogue, error)
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

// AbilityCatalogue implements AbilityStore.
func (r *Repo) AbilityCatalogue(ctx context.Context, p domain.Principal) ([]sqlc.AbilityCatalogue, error) {
	var out []sqlc.AbilityCatalogue
	err := r.pool.RunTenantTx(ctx, p, func(tx pgx.Tx) error {
		var err error
		out, err = sqlc.New(tx).ListAbilityCatalogue(ctx)
		return err
	})
	return out, err
}
