package abilities

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// stampCandidates are the rows the boot stamp fills: admitted WPMgr entries
// whose entry_sha256 is still NULL (the migration seeds).
func stampCandidates(rows []sqlc.AbilityCatalogue) []sqlc.AbilityCatalogue {
	var out []sqlc.AbilityCatalogue
	for _, r := range rows {
		if r.Source == "wpmgr" && r.Status == "admitted" && r.EntrySha256 == nil {
			out = append(out, r)
		}
	}
	return out
}

// StampOwnEntryHashes runs at boot. For each admitted source=wpmgr entry
// with a NULL hash it computes the canonical entry bytes (EntryBytes) and
// stores their hash through stamp_wpmgr_ability_entry_hash (m157), which
// only moves NULL to a hash on a wpmgr row and audits it with no human
// actor. One failure does not stop the others; each is logged.
func StampOwnEntryHashes(ctx context.Context, pool *db.Pool, logger *slog.Logger) (stamped int, err error) {
	var rows []sqlc.AbilityCatalogue
	if err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		var err error
		rows, err = sqlc.New(tx).ListAbilityCatalogue(ctx)
		return err
	}); err != nil {
		return 0, fmt.Errorf("read ability catalogue: %w", err)
	}
	for _, r := range stampCandidates(rows) {
		_, sum, err := EntryBytes(r)
		if err != nil {
			logger.ErrorContext(ctx, "ability catalogue: stamp skipped", slog.String("name", r.Name), slog.Any("error", err))
			continue
		}
		err = pool.InAgentTx(ctx, func(tx pgx.Tx) error {
			_, err := sqlc.New(tx).StampWpmgrAbilityEntryHash(ctx, sqlc.StampWpmgrAbilityEntryHashParams{
				EntryID: r.EntryID, EntrySha256: sum,
			})
			return err
		})
		if err != nil {
			logger.WarnContext(ctx, "ability catalogue: stamp refused", slog.String("name", r.Name), slog.Any("error", err))
			continue
		}
		stamped++
		logger.InfoContext(ctx, "ability catalogue: entry hash stamped", slog.String("name", r.Name), slog.String("entry_sha256", sum))
	}
	return stamped, nil
}
