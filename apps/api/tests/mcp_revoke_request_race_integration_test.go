// A connection revoke and a cache-clear request created by the same
// connection, interleaved deterministically as wpmgr_app. The request arrives
// over HTTP with a real bearer token (so Authenticate has already passed when
// the revoke starts), and the revoke runs through mcp.Service.RevokeConnection
// as production builds it.
//
// The interleave: a test transaction holds the connection's request lock;
// the tool call authenticates and queues on that lock inside its creation
// transaction; the revoke starts; the test transaction releases. Whatever
// order the two then run in, no waiting request of a revoked connection may
// be left behind.
package tests

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/govcontext"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// rrWaiters counts transactions queued on the connection's request lock.
func rrWaiters(t *testing.T, admin interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, grantID uuid.UUID) int {
	t.Helper()
	var n int
	if err := admin.QueryRow(context.Background(), `
		SELECT count(*) FROM pg_locks
		 WHERE locktype = 'advisory' AND NOT granted AND objsubid = 2
		   AND classid::bigint = (hashtext('assistant_request_grant')::bigint & 4294967295)
		   AND objid::bigint   = (hashtext($1)::bigint & 4294967295)`, grantID.String()).Scan(&n); err != nil {
		t.Fatalf("count lock waiters: %v", err)
	}
	return n
}

func TestRevokeSerialisesWithCacheClearCreationAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	repo := mcp.NewRepo(pool)
	rec := audit.NewRecorder(pool, domain.SystemClock{})
	newSvc := func() *mcp.Service {
		return mcp.NewService(repo).WithAudit(rec).
			WithContextResolver(&govcontext.Resolver{Store: govcontext.NewRepo(pool)})
	}

	r := uuid.NewString()[:8]
	tenant := seedTenant(t, pool, "rr-"+r)
	site := seedSite(t, pool, tenant, "https://rr-"+r+".test")
	admin := connectAdmin(t, pool)
	defer admin.Close()
	if _, err := admin.Exec(ctx, `UPDATE sites SET connection_state = 'connected', agent_version = $2 WHERE id = $1`,
		site, mcp.MinAgentVersionForOriginOnlyPurge); err != nil {
		t.Fatalf("arrange site: %v", err)
	}
	grant, bearer := cpeGrant(t, repo, tenant, []uuid.UUID{site})
	revoker := seedUser(t, pool, "rr-revoker-"+r+"@example.com", "Revoker", true)
	revokerP := domain.Principal{Type: domain.PrincipalUser, UserID: revoker, TenantID: tenant,
		Scope: domain.ScopeOrg, Role: "owner", AuthModel: domain.AuthModelRole}

	createSvc := newSvc()
	if err := createSvc.SetWriteToolsEnabled(true); err != nil {
		t.Fatalf("switch on: %v", err)
	}
	eng := mountLikeProduction(t, createSvc, domain.Principal{TenantID: tenant, Scope: domain.ScopeOrg})

	// 1. Hold the connection's request lock, through the shipped statement.
	held := make(chan struct{})
	release := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
			if err := sqlc.New(tx).TakeAssistantRequestXactLock(ctx, sqlc.TakeAssistantRequestXactLockParams{
				LockKey: "assistant_request_grant", LockID: grant.ID.String(),
			}); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	select {
	case <-held:
	case <-time.After(20 * time.Second):
		t.Fatal("the holder never took the lock")
	}

	// 2. The tool call authenticates and queues on the lock.
	created := make(chan cpeRPC, 1)
	go func() {
		created <- cpeCall(t, eng, bearer, mcp.ToolSiteCachePurgeRequest,
			map[string]any{"site_id": site.String(), "scope": "all"})
	}()
	waitFor := func(what string, want int, alsoDone <-chan struct{}) {
		t.Helper()
		for i := 0; i < 400; i++ {
			if rrWaiters(t, admin, grant.ID) >= want {
				return
			}
			if alsoDone != nil {
				select {
				case <-alsoDone:
					return
				default:
				}
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatalf("%s: fewer than %d waiters on the request lock after 10s", what, want)
	}
	waitFor("creation queued", 1, nil)

	// 3. The revoke starts while the creation is queued.
	revokeDone := make(chan struct{})
	var revokeErr error
	var revokeOut mcp.RevokeOutcome
	go func() {
		defer close(revokeDone)
		revokeOut, revokeErr = newSvc().RevokeConnection(ctx, revokerP, grant.ID)
	}()
	// With the fix the revoke queues behind the creation; without it, it
	// finishes on its own. Either way, go on once one has happened.
	waitFor("revoke queued", 2, revokeDone)

	// 4. Release, and let both finish.
	close(release)
	if err := <-holderDone; err != nil {
		t.Fatalf("holder: %v", err)
	}
	select {
	case <-revokeDone:
	case <-time.After(30 * time.Second):
		t.Fatal("the revoke did not finish")
	}
	if revokeErr != nil {
		t.Fatalf("revoke: %v", revokeErr)
	}
	if revokeOut.GrantsRevoked != 1 {
		t.Fatalf("revoke: grants revoked = %d, want 1", revokeOut.GrantsRevoked)
	}
	var res cpeRPC
	select {
	case res = <-created:
	case <-time.After(30 * time.Second):
		t.Fatal("the tool call did not finish")
	}
	t.Logf("tool call answered http=%d code=%d text=%s", res.http, res.code, res.text)

	// 5. No waiting request of the revoked connection survives.
	var pending, withdrawn int
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (read requests)")
		return tx.QueryRow(ctx, `
			SELECT count(*) FILTER (WHERE state = 'pending'),
			       count(*) FILTER (WHERE state = 'withdrawn')
			  FROM assistant_cache_purge_requests
			 WHERE tenant_id = $1 AND proposed_by_grant_id = $2`, tenant, grant.ID).Scan(&pending, &withdrawn)
	}); err != nil {
		t.Fatalf("read requests: %v", err)
	}
	if pending != 0 {
		t.Fatalf("a revoked connection still has %d waiting request(s) (withdrawn: %d)", pending, withdrawn)
	}
	// The creation took the lock first, so its row exists and was withdrawn
	// by the revoke, with the revoke's audit row.
	if withdrawn != 1 {
		t.Fatalf("withdrawn rows = %d, want 1 (the request created while the revoke waited)", withdrawn)
	}
}
