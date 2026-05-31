// Package repo is the persistence layer for the Media Optimizer domain. It
// follows the internal/scan repo style: operator-facing reads/writes run under
// InTenantTx (app.tenant_id GUC); agent-callback writes run under InAgentTx
// (app.agent GUC) because the agent's identity precedes any tenant scope, yet
// the tenant is known from the verified Ed25519 identity and re-asserted on the
// row. updated_at is set by app code (no DB trigger — ADR-043 §7).
package repo

import (
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
)

// Repo bundles the three media tables behind one handle. It is split across
// asset_repo.go / job_repo.go / variant_repo.go for readability; all share the
// same pool.
type Repo struct {
	pool *db.Pool
}

// NewRepo wires a Repo with the shared pgx pool.
func NewRepo(pool *db.Pool) *Repo {
	return &Repo{pool: pool}
}
