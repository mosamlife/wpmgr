// The AI request cascade inside a connection revoke, end to end through
// mcp.Service as wpmgr_app, with no assistantrequest wiring at all: no
// write-tools switch, no request service, no worker. The Service is built the
// way cmd/wpmgr/main.go builds it (mcp.NewService(mcp.NewRepo(pool)).WithAudit
// .WithContextResolver), so a revoke that closes requests here closes them in
// production whatever else is switched on or off.
//
// The world is arranged through the shipped statements on the app pool
// (insert, approve, the worker's reservation), never through the superuser.
// Every read asserts it runs as wpmgr_app.
package tests

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/govcontext"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// rcWorld is one organisation with a connection that has made four requests
// in four states, and a second connection whose waiting request must survive.
type rcWorld struct {
	pool     *db.Pool
	tenant   uuid.UUID
	approver uuid.UUID
	grant    sqlc.McpGrant
	waiting  []sqlc.AssistantCachePurgeRequest // pending
	approved sqlc.AssistantCachePurgeRequest   // approved_undispatched
	reserved sqlc.AssistantCachePurgeRequest   // dispatched, no outcome yet
	auditRes uuid.UUID                         // the reserved row's cache_purge_audit id
	other    sqlc.AssistantCachePurgeRequest   // another connection's pending row
}

func rcSeed(t *testing.T, pool *db.Pool) *rcWorld {
	t.Helper()
	ctx := context.Background()
	w := &rcWorld{pool: pool}
	w.tenant = seedTenant(t, pool, "rc-"+uuid.NewString()[:8])
	w.approver = seedUser(t, pool, "rc-approver-"+uuid.NewString()[:8]+"@example.com", "Approver", true)
	s1 := seedSite(t, pool, w.tenant, "")
	s2 := seedSite(t, pool, w.tenant, "")
	s3 := seedSite(t, pool, w.tenant, "")
	s4 := seedSite(t, pool, w.tenant, "")
	mcpRepo := mcp.NewRepo(pool)
	caps := []string{"mcp.sites.read", "mcp.cache.purge"}
	w.grant = arSeedGrant(t, mcpRepo, w.tenant, []uuid.UUID{s1, s2, s3, s4}, caps)
	otherGrant := arSeedGrant(t, mcpRepo, w.tenant, []uuid.UUID{s1}, caps)

	conn := acprSitePrincipal(w.tenant, s1, s2, s3, s4)
	w.waiting = []sqlc.AssistantCachePurgeRequest{
		acprInsert(t, pool, conn, acprParams(w.tenant, s1, w.grant.ID, "rc-w1-"+uuid.NewString())),
		acprInsert(t, pool, conn, acprParams(w.tenant, s2, w.grant.ID, "rc-w2-"+uuid.NewString())),
	}
	w.approved = acprApprove(t, pool,
		acprInsert(t, pool, conn, acprParams(w.tenant, s3, w.grant.ID, "rc-ap-"+uuid.NewString())), w.approver)
	w.reserved = acprApprove(t, pool,
		acprInsert(t, pool, conn, acprParams(w.tenant, s4, w.grant.ID, "rc-res-"+uuid.NewString())), w.approver)
	w.other = acprInsert(t, pool, acprSitePrincipal(w.tenant, s1),
		acprParams(w.tenant, s1, otherGrant.ID, "rc-other-"+uuid.NewString()))

	// The worker's reservation, through its own statements under the site's
	// principal: the clear "has started".
	if err := pool.RunTenantTx(ctx, acprSitePrincipal(w.tenant, s4), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (reservation)")
		q := sqlc.New(tx)
		a, err := q.InsertAssistantCachePurgeAudit(ctx, sqlc.InsertAssistantCachePurgeAuditParams{
			TenantID: w.tenant, SiteID: s4, Kind: "all", ApproverUserID: w.approver,
			InitiatorGrantID: w.grant.ID, TargetUrls: []string{}, UrlsCount: 0})
		if err != nil {
			return err
		}
		n, err := q.ReserveAssistantCachePurgeRequest(ctx, sqlc.ReserveAssistantCachePurgeRequestParams{
			CachePurgeAuditID: a.ID, TenantID: w.tenant, ID: w.reserved.ID, DeadlineSeconds: 3600})
		if err != nil {
			return err
		}
		if n != 1 {
			t.Fatalf("reservation changed %d rows, want 1", n)
		}
		w.auditRes = a.ID
		return nil
	}); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	return w
}

// rcService is mcp.Service as main.go builds it, over the given store.
func rcService(pool *db.Pool, store mcp.Store) *mcp.Service {
	return mcp.NewService(store).WithAudit(audit.NewRecorder(pool, domain.SystemClock{})).
		WithContextResolver(&govcontext.Resolver{Store: govcontext.NewRepo(pool)})
}

type rcRow struct {
	State       string
	Outcome     *string
	Reason      *string
	DecidedBy   *uuid.UUID
	WithdrawnAt bool
	PurgeAudit  *uuid.UUID
}

func (w *rcWorld) row(t *testing.T, id uuid.UUID) rcRow {
	t.Helper()
	var r rcRow
	if err := w.pool.InTenantTx(context.Background(), w.tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (read request)")
		return tx.QueryRow(context.Background(),
			`SELECT state, outcome, not_sent_reason, decided_by_user_id, withdrawn_at IS NOT NULL, cache_purge_audit_id
			   FROM assistant_cache_purge_requests WHERE tenant_id = $1 AND id = $2`, w.tenant, id).
			Scan(&r.State, &r.Outcome, &r.Reason, &r.DecidedBy, &r.WithdrawnAt, &r.PurgeAudit)
	}); err != nil {
		t.Fatalf("read request %s: %v", id, err)
	}
	return r
}

type rcAudit struct {
	Action, ActorType, ActorID string
	Metadata                   map[string]any
}

// auditFor returns every audit row naming target, in chain order.
func (w *rcWorld) auditFor(t *testing.T, target uuid.UUID) []rcAudit {
	t.Helper()
	var out []rcAudit
	if err := w.pool.InTenantTx(context.Background(), w.tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (read audit)")
		rows, err := tx.Query(context.Background(),
			`SELECT action, actor_type, actor_id, metadata FROM audit_log
			  WHERE tenant_id = $1 AND target_id = $2 ORDER BY created_at, id`, w.tenant, target.String())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a rcAudit
			var raw []byte
			if err := rows.Scan(&a.Action, &a.ActorType, &a.ActorID, &raw); err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &a.Metadata); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read audit for %s: %v", target, err)
	}
	return out
}

func (w *rcWorld) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := w.pool.InTenantTx(context.Background(), w.tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (count)")
		return tx.QueryRow(context.Background(), sql, args...).Scan(&n)
	}); err != nil {
		t.Fatalf("count (%s): %v", sql, err)
	}
	return n
}

func rcStr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// TestRevokeWithdrawsWithNoAssistantWiringAsAppRole: a revoke, by a person or
// by an org-scoped API key, withdraws the connection's waiting requests,
// closes its approved requests that have not been reserved, leaves a reserved
// one to finish, and records one audit row per closed request naming the
// revoker. Another connection's request is untouched, and a repeated revoke
// writes no request rows.
func TestRevokeWithdrawsWithNoAssistantWiringAsAppRole(t *testing.T) {
	pool := startPostgres(t)

	revokers := []struct {
		name string
		p    func(t *testing.T, w *rcWorld) domain.Principal
		want string
	}{
		{"user", func(t *testing.T, w *rcWorld) domain.Principal {
			u := seedUser(t, pool, "rc-revoker-"+uuid.NewString()[:8]+"@example.com", "Revoker", true)
			return domain.Principal{Type: domain.PrincipalUser, UserID: u, TenantID: w.tenant,
				Scope: domain.ScopeOrg, Role: "owner", AuthModel: domain.AuthModelRole}
		}, audit.ActorUser},
		// apikey.PrincipalFor's shape for an org-scoped key: APIKeyID set,
		// UserID left nil.
		{"api key", func(_ *testing.T, w *rcWorld) domain.Principal {
			return domain.Principal{Type: domain.PrincipalAPIKey, APIKeyID: uuid.New(), TenantID: w.tenant,
				Scope: domain.ScopeOrg, Role: "admin", AuthModel: domain.AuthModelRole}
		}, audit.ActorAPIKey},
	}

	for _, rv := range revokers {
		t.Run(rv.name, func(t *testing.T) {
			ctx := context.Background()
			w := rcSeed(t, pool)
			p := rv.p(t, w)
			actorType, actorID := audit.ActorFor(p)
			if actorType != rv.want || actorID == uuid.Nil.String() {
				t.Fatalf("revoker fixture resolves to %s/%s, want %s with a real id", actorType, actorID, rv.want)
			}
			svc := rcService(pool, mcp.NewRepo(pool))

			out, err := svc.RevokeConnection(ctx, p, w.grant.ID)
			if err != nil {
				t.Fatalf("revoke: %v", err)
			}
			if out.GrantsRevoked != 1 {
				t.Fatalf("revoke outcome %+v, want the grant revoked now", out)
			}

			// Waiting rows: withdrawn, naming nobody.
			for _, r := range w.waiting {
				got := w.row(t, r.ID)
				if got.State != "withdrawn" || got.DecidedBy != nil || !got.WithdrawnAt {
					t.Errorf("waiting request %s: state=%s decided_by=%v withdrawn_at set=%t, want withdrawn/nil/true",
						r.ID, got.State, got.DecidedBy, got.WithdrawnAt)
				}
			}
			// Approved, not reserved: closed as not sent, approver kept.
			if got := w.row(t, w.approved.ID); got.State != "dispatched" || rcStr(got.Outcome) != "not_sent" ||
				rcStr(got.Reason) != "grant_inactive" || got.DecidedBy == nil || *got.DecidedBy != w.approver {
				t.Errorf("approved request: state=%s outcome=%s reason=%s decided_by=%v, want dispatched/not_sent/grant_inactive/%s",
					got.State, rcStr(got.Outcome), rcStr(got.Reason), got.DecidedBy, w.approver)
			}
			// Reserved: left alone, to finish.
			if got := w.row(t, w.reserved.ID); got.State != "dispatched" || got.Outcome != nil || got.Reason != nil ||
				got.PurgeAudit == nil || *got.PurgeAudit != w.auditRes {
				t.Errorf("reserved request: state=%s outcome=%s reason=%s purge_audit=%v, want dispatched with no outcome and audit %s",
					got.State, rcStr(got.Outcome), rcStr(got.Reason), got.PurgeAudit, w.auditRes)
			}
			// Another connection's request: untouched.
			if got := w.row(t, w.other.ID); got.State != "pending" {
				t.Errorf("another connection's request moved to %s", got.State)
			}

			// One audit row per closed request, naming the revoker.
			wantRow := func(id uuid.UUID, action string, md map[string]string) {
				t.Helper()
				rows := w.auditFor(t, id)
				if len(rows) != 1 {
					t.Errorf("request %s: %d audit rows, want exactly 1 %s: %+v", id, len(rows), action, rows)
					return
				}
				a := rows[0]
				if a.Action != action || a.ActorType != actorType || a.ActorID != actorID {
					t.Errorf("request %s: %s by %s/%s, want %s by %s/%s",
						id, a.Action, a.ActorType, a.ActorID, action, actorType, actorID)
				}
				for k, v := range md {
					if got, _ := a.Metadata[k].(string); got != v {
						t.Errorf("request %s: metadata %s = %v, want %q", id, k, a.Metadata[k], v)
					}
				}
			}
			for _, r := range w.waiting {
				wantRow(r.ID, audit.ActionAssistantRequestWithdrawn, map[string]string{
					"reason": "connection_revoked", "proposed_by_grant_id": w.grant.ID.String()})
			}
			wantRow(w.approved.ID, audit.ActionAssistantRequestNotSent, map[string]string{
				"reason": "grant_inactive", "closed_by": "connection_revoked", "proposed_by_grant_id": w.grant.ID.String()})
			for _, id := range []uuid.UUID{w.reserved.ID, w.other.ID} {
				if rows := w.auditFor(t, id); len(rows) != 0 {
					t.Errorf("request %s was not closed and still got audit rows: %+v", id, rows)
				}
			}
			revokeRows := w.auditFor(t, w.grant.ID)
			if len(revokeRows) != 1 || revokeRows[0].Action != audit.ActionMCPGrantRevoked ||
				revokeRows[0].ActorType != actorType || revokeRows[0].ActorID != actorID {
				t.Errorf("revoke audit rows %+v, want one %s by %s/%s", revokeRows, audit.ActionMCPGrantRevoked, actorType, actorID)
			}
			if n := w.count(t, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND actor_id = $2`,
				w.tenant, uuid.Nil.String()); n != 0 {
				t.Errorf("%d audit rows name the nil uuid as their actor", n)
			}

			// Idempotent: a repeat succeeds and writes no request rows.
			const reqRows = `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = ANY($2)`
			actions := []string{audit.ActionAssistantRequestWithdrawn, audit.ActionAssistantRequestNotSent}
			before := w.count(t, reqRows, w.tenant, actions)
			if before != len(w.waiting)+1 {
				t.Fatalf("%d request audit rows after the revoke, want %d", before, len(w.waiting)+1)
			}
			again, err := svc.RevokeConnection(ctx, p, w.grant.ID)
			if err != nil || !again.AlreadyRevoked {
				t.Fatalf("repeat revoke: %+v err=%v, want an idempotent success", again, err)
			}
			if after := w.count(t, reqRows, w.tenant, actions); after != before {
				t.Fatalf("a repeat revoke wrote %d more request audit rows", after-before)
			}
		})
	}
}

// rcFailingCascade is the real repo whose cascade does its work and then
// fails, so the revoke transaction holds changed request rows when it rolls
// back.
type rcFailingCascade struct {
	*mcp.Repo
	t   *testing.T
	err error
}

func (f rcFailingCascade) CloseAssistantRequestsForGrantTx(ctx context.Context, tx pgx.Tx, tenantID, grantID uuid.UUID) ([]uuid.UUID, []uuid.UUID, error) {
	mcpAssertAndReportRole(f.t, tx, "revoke transaction (cascade)")
	withdrawn, notSent, err := f.Repo.CloseAssistantRequestsForGrantTx(ctx, tx, tenantID, grantID)
	if err != nil {
		return nil, nil, err
	}
	if len(withdrawn) == 0 || len(notSent) == 0 {
		f.t.Fatalf("the cascade closed %v and %v before the planted failure; this proof needs both to be non-empty", withdrawn, notSent)
	}
	return withdrawn, notSent, f.err
}

// TestRevokeFailsWhenItsCascadeFailsAsAppRole: the cascade shares the revoke's
// transaction, so its failure fails the revoke and rolls everything back. The
// connection stays active, and no request moves, and nothing is audited.
func TestRevokeFailsWhenItsCascadeFailsAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	w := rcSeed(t, pool)
	planted := errors.New("planted cascade failure")
	svc := rcService(pool, rcFailingCascade{Repo: mcp.NewRepo(pool), t: t, err: planted})
	p := domain.Principal{Type: domain.PrincipalAPIKey, APIKeyID: uuid.New(), TenantID: w.tenant,
		Scope: domain.ScopeOrg, Role: "admin", AuthModel: domain.AuthModelRole}

	out, err := svc.RevokeConnection(ctx, p, w.grant.ID)
	if err == nil {
		t.Fatalf("the revoke succeeded (%+v) although its cascade failed", out)
	}
	if !errors.Is(err, planted) {
		t.Fatalf("revoke error %v does not carry the cascade's failure", err)
	}

	if n := w.count(t, `SELECT count(*) FROM mcp_grants WHERE tenant_id = $1 AND id = $2 AND status = 'active'`,
		w.tenant, w.grant.ID); n != 1 {
		t.Fatalf("the grant is not active after a failed revoke (%d active rows)", n)
	}
	for _, r := range w.waiting {
		if got := w.row(t, r.ID); got.State != "pending" {
			t.Errorf("waiting request %s is %s after a failed revoke, want pending", r.ID, got.State)
		}
	}
	if got := w.row(t, w.approved.ID); got.State != "approved_undispatched" || got.Outcome != nil {
		t.Errorf("approved request is %s/%s after a failed revoke, want approved_undispatched", got.State, rcStr(got.Outcome))
	}
	if n := w.count(t, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = ANY($2)`, w.tenant,
		[]string{audit.ActionAssistantRequestWithdrawn, audit.ActionAssistantRequestNotSent, audit.ActionMCPGrantRevoked}); n != 0 {
		t.Fatalf("a failed revoke left %d audit rows", n)
	}
}
