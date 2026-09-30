// AI cache-clear requests, end to end through internal/assistantrequest, as
// wpmgr_app: approve and decline, the queue's session-only digest, and the
// dispatch worker's closes, transients, reservation, send and outcome, with
// the sweeper and the reconciler.
//
// Every call goes through the shipped Service and the shipped River workers'
// Work methods, on the pool startPostgres returns (wpmgr_app: NOSUPERUSER,
// NOBYPASSRLS, asserted from inside the transactions this file reads with).
// The only thing faked is the network send, so the proofs can count sends and
// choose the site's answer. The superuser connection is used only to arrange
// the world (a revoked grant, a backdated approval, an archived site) and
// never to read an answer.
package tests

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/assistantrequest"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/govcontext"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
	"github.com/mosamlife/wpmgr/apps/api/internal/perf"
)

// ---------------------------------------------------------------------------
// Fakes: the send, and a gauge store that can be made to fail.
// ---------------------------------------------------------------------------

type arSend struct {
	SiteID  uuid.UUID
	SiteURL string
	Req     perf.AssistantPurge
}

// arSender records every send. during, if set, runs inside the send, which
// is where a real send is on the network with no transaction open.
type arSender struct {
	mu        sync.Mutex
	sends     []arSend
	res       perf.AssistantPurgeResult
	err       error
	during    func()
	published int
}

func newARSender() *arSender {
	honoured := true
	return &arSender{res: perf.AssistantPurgeResult{
		Agent:    agentcmd.CachePurgeResult{OK: true, OriginOnlyHonoured: &honoured},
		WpmgrCDN: perf.AssistantCDNNotAttempted,
	}}
}

func (f *arSender) SendAssistantPurge(_ context.Context, siteID uuid.UUID, siteURL string, _ perf.CDNCiphertext, req perf.AssistantPurge) (perf.AssistantPurgeResult, error) {
	f.mu.Lock()
	f.sends = append(f.sends, arSend{SiteID: siteID, SiteURL: siteURL, Req: req})
	during := f.during
	f.mu.Unlock()
	if during != nil {
		during()
	}
	return f.res, f.err
}

func (f *arSender) PublishAssistantPurge(context.Context, uuid.UUID, uuid.UUID, string) {
	f.mu.Lock()
	f.published++
	f.mu.Unlock()
}

func (f *arSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sends)
}

// arFailingGauge is the real perf.Repo except that the gauge stamp fails, so
// the outcome transaction it joins must roll back whole.
type arFailingGauge struct{ *perf.Repo }

func (arFailingGauge) MarkCachePurgedTx(context.Context, pgx.Tx, uuid.UUID, uuid.UUID, string) error {
	return errors.New("planted gauge failure")
}

type arEnqueuer struct{ jobs []assistantrequest.DispatchArgs }

func (e *arEnqueuer) EnqueueDispatch(_ context.Context, a assistantrequest.DispatchArgs) error {
	e.jobs = append(e.jobs, a)
	return nil
}

// ---------------------------------------------------------------------------
// The stack
// ---------------------------------------------------------------------------

type arStack struct {
	pool    *db.Pool
	admin   *db.Pool
	mcpRepo *mcp.Repo
	mcpSvc  *mcp.Service
	rec     *audit.Recorder
	sender  *arSender
	svc     *assistantrequest.Service
	tenant  uuid.UUID
	user    uuid.UUID
}

func newARStack(t *testing.T) *arStack {
	t.Helper()
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	t.Cleanup(admin.Close)
	rec := audit.NewRecorder(pool, domain.SystemClock{})
	mcpRepo := mcp.NewRepo(pool)
	mcpSvc := mcp.NewService(mcpRepo).WithAudit(rec).
		WithContextResolver(&govcontext.Resolver{Store: govcontext.NewRepo(pool)})
	st := &arStack{pool: pool, admin: admin, mcpRepo: mcpRepo, mcpSvc: mcpSvc, rec: rec, sender: newARSender()}
	st.svc = st.newService(rec, st.sender, perf.NewRepo(pool), true)
	st.tenant = seedTenant(t, pool, "ar-"+uuid.NewString()[:8])
	st.user = seedUser(t, pool, "approver-"+uuid.NewString()[:8]+"@example.com", "Approver", true)
	return st
}

// newService builds another Service on the same database, the way main.go
// builds the one it serves with.
func (st *arStack) newService(rec *audit.Recorder, sender assistantrequest.PurgeSender, store assistantrequest.SiteCacheStore, on bool) *assistantrequest.Service {
	svc := assistantrequest.NewService(assistantrequest.NewRepo(st.pool), st.mcpRepo, st.mcpSvc, rec, nil)
	svc.SetSender(sender, store)
	svc.SetWriteToolsEnabled(on)
	return svc
}

func (st *arStack) approver() domain.Principal {
	return domain.Principal{Type: domain.PrincipalUser, UserID: st.user, TenantID: st.tenant, Scope: domain.ScopeOrg, Role: "owner"}
}

func (st *arStack) apiKey() domain.Principal {
	return domain.Principal{Type: domain.PrincipalAPIKey, APIKeyID: uuid.New(), TenantID: st.tenant, Scope: domain.ScopeOrg, Role: "admin"}
}

// site seeds a connected site whose agent meets the floor.
func (st *arStack) site(t *testing.T) uuid.UUID {
	t.Helper()
	id := seedSite(t, st.pool, st.tenant, "")
	st.exec(t, `UPDATE sites SET agent_version = $2, connection_state = 'connected' WHERE id = $1`,
		id, mcp.MinAgentVersionForOriginOnlyPurge)
	return id
}

// grant seeds a live connection holding the cache capability, scoped to the
// listed sites.
func (st *arStack) grant(t *testing.T, sites ...uuid.UUID) sqlc.McpGrant {
	t.Helper()
	return arSeedGrant(t, st.mcpRepo, st.tenant, sites, []string{"mcp.sites.read", "mcp.cache.purge"})
}

// pending inserts one waiting request through the creation statement, under
// the connection's site-scoped principal.
func (st *arStack) pending(t *testing.T, site, grant uuid.UUID) sqlc.AssistantCachePurgeRequest {
	t.Helper()
	return acprInsert(t, st.pool, acprSitePrincipal(st.tenant, site), acprParams(st.tenant, site, grant, uuid.NewString()))
}

// approved is pending, then approved through the Service.
func (st *arStack) approved(t *testing.T, site, grant uuid.UUID) sqlc.AssistantCachePurgeRequest {
	t.Helper()
	return st.approve(t, st.pending(t, site, grant))
}

// approvedPage is an approved request to clear one page. A page clear is not
// held by the whole-site cooldown.
func (st *arStack) approvedPage(t *testing.T, site, grant uuid.UUID) sqlc.AssistantCachePurgeRequest {
	t.Helper()
	arg := acprParams(st.tenant, site, grant, uuid.NewString())
	arg.Scope = "url"
	arg.Url = acprStr("https://shop.example.com/sale/")
	return st.approve(t, acprInsert(t, st.pool, acprSitePrincipal(st.tenant, site), arg))
}

func (st *arStack) approve(t *testing.T, row sqlc.AssistantCachePurgeRequest) sqlc.AssistantCachePurgeRequest {
	t.Helper()
	site := row.SiteID
	if _, err := st.svc.Approve(context.Background(), st.approver(), site, row.ID, row.PresentedDigest); err != nil {
		t.Fatalf("approve %s: %v", row.ID, err)
	}
	return row
}

func (st *arStack) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := st.admin.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("arrange (%s): %v", sql, err)
	}
}

func arDispatch(t *testing.T, svc *assistantrequest.Service, row sqlc.AssistantCachePurgeRequest) error {
	t.Helper()
	w := assistantrequest.NewDispatchWorker(svc)
	return w.Work(context.Background(), &river.Job[assistantrequest.DispatchArgs]{Args: assistantrequest.DispatchArgs{
		TenantID: row.TenantID, RequestID: row.ID, SiteID: row.SiteID, GrantID: row.ProposedByGrantID,
	}})
}

func arSweep(t *testing.T, svc *assistantrequest.Service) {
	t.Helper()
	if err := assistantrequest.NewSweepWorker(svc).Work(context.Background(), &river.Job[assistantrequest.SweepArgs]{}); err != nil {
		t.Fatalf("sweep: %v", err)
	}
}

func arReconcile(t *testing.T, svc *assistantrequest.Service) {
	t.Helper()
	if err := assistantrequest.NewReconcileWorker(svc).Work(context.Background(), &river.Job[assistantrequest.ReconcileArgs]{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func arSeedGrant(t *testing.T, repo *mcp.Repo, tenantID uuid.UUID, sites []uuid.UUID, caps []string) sqlc.McpGrant {
	t.Helper()
	ctx := context.Background()
	clientID := "ar-client-" + uuid.NewString()
	secretHash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if n, err := repo.RegisterClient(ctx, sqlc.RegisterMCPOAuthClientParams{
		ClientID:                clientID,
		ClientSecretHash:        &secretHash,
		TokenEndpointAuthMethod: "client_secret_basic",
		RedirectUris:            []string{"https://claude.ai/api/mcp/auth_callback"},
		RegisteredScopes:        mcp.SupportedScopes(),
	}); err != nil || n != 1 {
		t.Fatalf("seed client: n=%d err=%v", n, err)
	}
	g, _, err := repo.CreateGrantWithCode(ctx, domain.Principal{TenantID: tenantID, Scope: domain.ScopeOrg}, sqlc.CreateMCPGrantParams{
		TenantID:      tenantID,
		Name:          "Laptop",
		Status:        "active",
		SiteScopeMode: "list",
		ScopeTagIds:   []uuid.UUID{},
		ScopeSiteIds:  sites,
		ClientID:      &clientID,
		Capabilities:  caps,
		OauthScopes:   []string{"mcp:read", "mcp:cache"},
		ExpiresAt:     time.Now().UTC().Add(90 * 24 * time.Hour),
	}, func(grantID uuid.UUID) sqlc.CreateMCPAuthorizationCodeParams {
		return sqlc.CreateMCPAuthorizationCodeParams{
			TenantID: tenantID, GrantID: grantID, ClientID: clientID,
			CodeHash:            acprHex("code-" + grantID.String()),
			CodeChallenge:       "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM",
			CodeChallengeMethod: "S256",
			RedirectUri:         "https://claude.ai/api/mcp/auth_callback",
			ExpiresAt:           time.Now().UTC().Add(5 * time.Minute),
		}
	}, nil)
	if err != nil {
		t.Fatalf("seed grant: %v", err)
	}
	return g
}

// ---------------------------------------------------------------------------
// Reads, as wpmgr_app under the tenant transaction
// ---------------------------------------------------------------------------

type arState struct {
	State           string
	Outcome         *string
	NotSentReason   *string
	LastAttemptCode *string
	DecidedBy       *uuid.UUID
	PurgeAuditID    *uuid.UUID
}

func (st *arStack) state(t *testing.T, id uuid.UUID) arState {
	t.Helper()
	var s arState
	if err := st.pool.InTenantTx(context.Background(), st.tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (read request)")
		return tx.QueryRow(context.Background(),
			`SELECT state, outcome, not_sent_reason, last_attempt_code, decided_by_user_id, cache_purge_audit_id
			   FROM assistant_cache_purge_requests WHERE id = $1`, id).
			Scan(&s.State, &s.Outcome, &s.NotSentReason, &s.LastAttemptCode, &s.DecidedBy, &s.PurgeAuditID)
	}); err != nil {
		t.Fatalf("read request %s: %v", id, err)
	}
	return s
}

func (st *arStack) countSQL(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := st.pool.InTenantTx(context.Background(), st.tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (count)")
		return tx.QueryRow(context.Background(), sql, args...).Scan(&n)
	}); err != nil {
		t.Fatalf("count (%s): %v", sql, err)
	}
	return n
}

func (st *arStack) purgeRows(t *testing.T, site uuid.UUID) int {
	t.Helper()
	return st.countSQL(t, `SELECT count(*) FROM cache_purge_audit WHERE site_id = $1 AND initiator_grant_id IS NOT NULL`, site)
}

func (st *arStack) auditRows(t *testing.T, action string, target uuid.UUID) []map[string]any {
	t.Helper()
	var out []map[string]any
	if err := st.pool.InTenantTx(context.Background(), st.tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(),
			`SELECT actor_type, actor_id, metadata FROM audit_log WHERE action = $1 AND target_id = $2 ORDER BY created_at, id`,
			action, target.String())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var actorType, actorID string
			var raw []byte
			if err := rows.Scan(&actorType, &actorID, &raw); err != nil {
				return err
			}
			md := map[string]any{}
			if err := json.Unmarshal(raw, &md); err != nil {
				return err
			}
			md["@actor_type"], md["@actor_id"] = actorType, actorID
			out = append(out, md)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read audit rows %s: %v", action, err)
	}
	return out
}

func arStr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func arWantCode(t *testing.T, what string, err error, status int, code string) {
	t.Helper()
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("%s: err = %v, want a domain error %d %s", what, err, status, code)
	}
	if domain.HTTPStatus(err) != status || (code != "" && de.Code != code) {
		t.Fatalf("%s: got %d %s (%v), want %d %s", what, domain.HTTPStatus(err), de.Code, err, status, code)
	}
}

func arWantClosed(t *testing.T, st *arStack, row sqlc.AssistantCachePurgeRequest, reason string) {
	t.Helper()
	got := st.state(t, row.ID)
	if got.State != assistantrequest.StateDispatched || arStr(got.Outcome) != assistantrequest.OutcomeNotSent || arStr(got.NotSentReason) != reason {
		t.Fatalf("request %s: state=%s outcome=%s reason=%s, want dispatched/not_sent/%s",
			row.ID, got.State, arStr(got.Outcome), arStr(got.NotSentReason), reason)
	}
	if got.DecidedBy == nil {
		t.Fatalf("request %s: the approver was dropped when it closed", row.ID)
	}
	if n := len(st.auditRows(t, audit.ActionAssistantRequestNotSent, row.ID)); n != 1 {
		t.Fatalf("request %s: %d not_sent audit rows, want 1", row.ID, n)
	}
}

func arWantWaiting(t *testing.T, st *arStack, row sqlc.AssistantCachePurgeRequest, code string) {
	t.Helper()
	got := st.state(t, row.ID)
	if got.State != assistantrequest.StateApproved || arStr(got.LastAttemptCode) != code {
		t.Fatalf("request %s: state=%s last_attempt_code=%s, want approved_undispatched/%s",
			row.ID, got.State, arStr(got.LastAttemptCode), code)
	}
}

// ---------------------------------------------------------------------------
// APPROVE, DECLINE AND THE QUEUE
// ---------------------------------------------------------------------------

// TestAssistantRequestApproveDeclineAndQueueAsAppRole: only a signed-in person
// decides; a stale digest, a second approval and a failed ledger write change
// nothing; the approval's audit row carries every card fact; the digest is
// shown to a session and never to an API key.
func TestAssistantRequestApproveDeclineAndQueueAsAppRole(t *testing.T) {
	ctx := context.Background()
	st := newARStack(t)
	s1 := st.site(t)
	g1 := st.grant(t, s1)
	g2 := st.grant(t, s1)
	row := st.pending(t, s1, g1.ID)
	other := st.pending(t, s1, g2.ID)

	// An API key never approves or declines.
	_, err := st.svc.Approve(ctx, st.apiKey(), s1, row.ID, row.PresentedDigest)
	arWantCode(t, "approve as an API key", err, 403, assistantrequest.CodeSessionRequired)
	_, err = st.svc.Decline(ctx, st.apiKey(), s1, row.ID)
	arWantCode(t, "decline as an API key", err, 403, assistantrequest.CodeSessionRequired)
	if got := st.state(t, row.ID); got.State != assistantrequest.StatePending {
		t.Fatalf("an API key's refused call changed the row to %s", got.State)
	}

	// A digest the person did not see approves nothing.
	_, err = st.svc.Approve(ctx, st.approver(), s1, row.ID, acprHex("some other card"))
	arWantCode(t, "approve with a stale digest", err, 409, assistantrequest.CodeRequestChanged)
	if got := st.state(t, row.ID); got.State != assistantrequest.StatePending {
		t.Fatalf("a stale digest changed the row to %s", got.State)
	}

	// A ledger failure rolls the approval back.
	noLedger := st.newService(nil, st.sender, perf.NewRepo(st.pool), true)
	_, err = noLedger.Approve(ctx, st.approver(), s1, row.ID, row.PresentedDigest)
	arWantCode(t, "approve with the ledger failing", err, 500, assistantrequest.CodeLedgerFailed)
	if got := st.state(t, row.ID); got.State != assistantrequest.StatePending || got.DecidedBy != nil {
		t.Fatalf("a failed ledger write left the row %s decided by %v", got.State, got.DecidedBy)
	}

	// The honest approval.
	out, err := st.svc.Approve(ctx, st.approver(), s1, row.ID, row.PresentedDigest)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if out.State != assistantrequest.StateApproved {
		t.Fatalf("approve returned state %s", out.State)
	}
	got := st.state(t, row.ID)
	if got.State != assistantrequest.StateApproved || got.DecidedBy == nil || *got.DecidedBy != st.user {
		t.Fatalf("after approve: %+v", got)
	}
	approvals := st.auditRows(t, audit.ActionAssistantRequestApproved, row.ID)
	if len(approvals) != 1 {
		t.Fatalf("%d approval audit rows, want 1", len(approvals))
	}
	md := approvals[0]
	if md["@actor_type"] != audit.ActorUser || md["@actor_id"] != st.user.String() {
		t.Fatalf("approval actor = %v %v", md["@actor_type"], md["@actor_id"])
	}
	for _, k := range []string{"request_id", "site_id", "scope", "presented_digest", "proposed_by_grant_id",
		"grant_label", "grant_via", "site_label", "site_host", "expires_at", "copy_version", "url", "setup_client"} {
		if _, ok := md[k]; !ok {
			t.Errorf("approval metadata has no %q: %v", k, md)
		}
	}
	if md["presented_digest"] != row.PresentedDigest || md["proposed_by_grant_id"] != g1.ID.String() {
		t.Errorf("approval metadata names the wrong card: %v", md)
	}

	// A second approval is refused and writes nothing.
	_, err = st.svc.Approve(ctx, st.approver(), s1, row.ID, row.PresentedDigest)
	arWantCode(t, "approve twice", err, 409, assistantrequest.CodeRequestChanged)
	if n := len(st.auditRows(t, audit.ActionAssistantRequestApproved, row.ID)); n != 1 {
		t.Fatalf("a second approval wrote a second audit row (%d)", n)
	}

	// The wrong site in the path approves nothing.
	s2 := st.site(t)
	_, err = st.svc.Decline(ctx, st.approver(), s2, other.ID)
	arWantCode(t, "decline with another site's id", err, 409, assistantrequest.CodeRequestChanged)

	// Decline names the person.
	if _, err := st.svc.Decline(ctx, st.approver(), s1, other.ID); err != nil {
		t.Fatalf("decline: %v", err)
	}
	if got := st.state(t, other.ID); got.State != assistantrequest.StateRejected || got.DecidedBy == nil || *got.DecidedBy != st.user {
		t.Fatalf("after decline: %+v", got)
	}
	if n := len(st.auditRows(t, audit.ActionAssistantRequestDeclined, other.ID)); n != 1 {
		t.Fatalf("%d decline audit rows, want 1", n)
	}

	// The queue: the digest for a session, never for an API key.
	for _, tc := range []struct {
		name   string
		p      domain.Principal
		digest bool
	}{{"session", st.approver(), true}, {"api key", st.apiKey(), false}} {
		for _, site := range []*uuid.UUID{nil, &s1} {
			q, err := st.svc.List(ctx, tc.p, site, 50, 0)
			if err != nil {
				t.Fatalf("%s: list: %v", tc.name, err)
			}
			if len(q.Requests) != 2 {
				t.Fatalf("%s: list returned %d requests, want 2", tc.name, len(q.Requests))
			}
			for _, r := range q.Requests {
				if (r.PresentedDigest != nil) != tc.digest {
					t.Fatalf("%s (site filter %v): presented_digest present=%v, want %v",
						tc.name, site != nil, r.PresentedDigest != nil, tc.digest)
				}
			}
		}
	}
}

// TestAssistantRequestApproveRefusalsAsAppRole: approve re-derives the
// connection's authority and refuses, naming the reason and writing nothing,
// when the switch is off, the connection is revoked, the capability is gone,
// the site left the scope, the agent is below the floor, or the organisation
// is paused.
func TestAssistantRequestApproveRefusalsAsAppRole(t *testing.T) {
	ctx := context.Background()
	st := newARStack(t)

	type refusal struct {
		name    string
		arrange func(site uuid.UUID, grant sqlc.McpGrant)
		svc     func() *assistantrequest.Service
		code    string
		grant   func(site uuid.UUID) sqlc.McpGrant
	}
	off := st.newService(st.rec, st.sender, perf.NewRepo(st.pool), false)
	cases := []refusal{
		{name: "switched off", svc: func() *assistantrequest.Service { return off }, code: assistantrequest.CodeWriteToolsDisabled},
		{name: "revoked", code: assistantrequest.CodeConnectionInactive,
			arrange: func(_ uuid.UUID, g sqlc.McpGrant) {
				st.exec(t, `UPDATE mcp_grants SET status = 'revoked', revoked_at = now() WHERE id = $1`, g.ID)
			}},
		{name: "capability not held", code: assistantrequest.CodeCapabilityNotHeld,
			grant: func(site uuid.UUID) sqlc.McpGrant {
				return arSeedGrant(t, st.mcpRepo, st.tenant, []uuid.UUID{site}, []string{"mcp.sites.read"})
			}},
		{name: "site left scope", code: assistantrequest.CodeSiteLeftScope,
			grant: func(uuid.UUID) sqlc.McpGrant { return st.grant(t, st.site(t)) }},
		{name: "below the floor", code: assistantrequest.CodeAgentOutdated,
			arrange: func(site uuid.UUID, _ sqlc.McpGrant) {
				st.exec(t, `UPDATE sites SET agent_version = '0.61.152' WHERE id = $1`, site)
			}},
		{name: "paused", code: assistantrequest.CodeAssistantPaused,
			arrange: func(uuid.UUID, sqlc.McpGrant) {
				st.exec(t, `UPDATE tenants SET assistant_enabled_at = COALESCE(assistant_enabled_at, now()), assistant_paused_at = now() WHERE id = $1`, st.tenant)
			}},
	}
	for _, tc := range cases {
		site := st.site(t)
		var g sqlc.McpGrant
		if tc.grant != nil {
			g = tc.grant(site)
		} else {
			g = st.grant(t, site)
		}
		row := st.pending(t, site, g.ID)
		if tc.arrange != nil {
			tc.arrange(site, g)
		}
		svc := st.svc
		if tc.svc != nil {
			svc = tc.svc()
		}
		_, err := svc.Approve(ctx, st.approver(), site, row.ID, row.PresentedDigest)
		arWantCode(t, tc.name, err, 409, tc.code)
		if got := st.state(t, row.ID); got.State != assistantrequest.StatePending || got.DecidedBy != nil {
			t.Fatalf("%s: the refused approval changed the row: %+v", tc.name, got)
		}
		if n := len(st.auditRows(t, audit.ActionAssistantRequestApproved, row.ID)); n != 0 {
			t.Fatalf("%s: a refused approval wrote %d audit rows", tc.name, n)
		}
		// Decline always works.
		if _, err := st.svc.Decline(ctx, st.approver(), site, row.ID); err != nil {
			t.Fatalf("%s: decline after a refused approval: %v", tc.name, err)
		}
	}
}

// ---------------------------------------------------------------------------
// DISPATCH
// ---------------------------------------------------------------------------

// TestAssistantRequestDispatchPurgesOnceAsAppRole: the scan finds the approved
// row, the worker reserves it once, sends once, records purged, stamps the
// gauge in the outcome transaction, and a re-run sends nothing. An approver
// whose account is gone is recorded as such, and the clear still runs.
func TestAssistantRequestDispatchPurgesOnceAsAppRole(t *testing.T) {
	ctx := context.Background()
	st := newARStack(t)
	s1 := st.site(t)
	g := st.grant(t, s1)
	row := st.approved(t, s1, g.ID)

	enq := &arEnqueuer{}
	scan := assistantrequest.NewScanWorker(st.svc)
	scan.SetEnqueuer(enq)
	if err := scan.Work(ctx, &river.Job[assistantrequest.ScanArgs]{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(enq.jobs) != 1 || enq.jobs[0].RequestID != row.ID || enq.jobs[0].GrantID != g.ID || enq.jobs[0].SiteID != s1 {
		t.Fatalf("scan enqueued %+v, want the one approved request", enq.jobs)
	}

	if err := arDispatch(t, st.svc, row); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if st.sender.count() != 1 {
		t.Fatalf("%d sends, want 1", st.sender.count())
	}
	send := st.sender.sends[0]
	if send.SiteID != s1 || send.Req.Scope != "all" || send.Req.URL != "" || send.SiteURL == "" {
		t.Fatalf("send = %+v", send)
	}
	got := st.state(t, row.ID)
	if got.State != assistantrequest.StateDispatched || arStr(got.Outcome) != assistantrequest.OutcomePurged || got.PurgeAuditID == nil {
		t.Fatalf("after dispatch: %+v", got)
	}
	if n := st.countSQL(t, `SELECT count(*) FROM cache_purge_audit WHERE id = $1 AND initiator_grant_id = $2 AND initiator_user_id = $3 AND kind = 'all'`,
		*got.PurgeAuditID, g.ID, st.user); n != 1 {
		t.Fatalf("the attempt record does not name the grant and the approver (%d rows)", n)
	}
	if n := st.countSQL(t, `SELECT count(*) FROM site_cache_stats WHERE site_id = $1 AND last_purged_at IS NOT NULL AND last_purge_kind = 'all'`, s1); n != 1 {
		t.Fatalf("the purge gauge was not stamped")
	}
	if n := len(st.auditRows(t, audit.ActionAssistantRequestDispatched, row.ID)); n != 1 {
		t.Fatalf("%d dispatched audit rows, want 1", n)
	}
	purged := st.auditRows(t, audit.ActionCachePurged, s1)
	if len(purged) != 1 || purged[0]["@actor_type"] != audit.ActorUser || purged[0]["@actor_id"] != st.user.String() ||
		purged[0]["requested_by_grant"] != g.ID.String() || purged[0]["origin_only_confirmed"] != true {
		t.Fatalf("site.cache.purged rows = %v", purged)
	}

	// A re-run finds nothing to do.
	if err := arDispatch(t, st.svc, row); err != nil {
		t.Fatalf("second dispatch: %v", err)
	}
	if st.sender.count() != 1 || st.purgeRows(t, s1) != 1 {
		t.Fatalf("a re-run sent again: sends=%d purge rows=%d", st.sender.count(), st.purgeRows(t, s1))
	}

	// Deleted approver: approved by an account that no longer exists.
	s2 := st.site(t)
	g2 := st.grant(t, s2)
	gone := st.pending(t, s2, g2.ID)
	ghost := domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: st.tenant, Scope: domain.ScopeOrg, Role: "owner"}
	if _, err := st.svc.Approve(ctx, ghost, s2, gone.ID, gone.PresentedDigest); err != nil {
		t.Fatalf("approve as the soon-deleted approver: %v", err)
	}
	if err := arDispatch(t, st.svc, gone); err != nil {
		t.Fatalf("dispatch with a deleted approver: %v", err)
	}
	if st.sender.count() != 2 {
		t.Fatalf("a deleted approver stopped the clear (sends=%d)", st.sender.count())
	}
	if n := st.countSQL(t, `SELECT count(*) FROM cache_purge_audit WHERE site_id = $1 AND initiator_grant_id = $2 AND initiator_user_id IS NULL`, s2, g2.ID); n != 1 {
		t.Fatalf("the attempt record names a user that does not exist (%d rows with NULL)", n)
	}
	disp := st.auditRows(t, audit.ActionAssistantRequestDispatched, gone.ID)
	if len(disp) != 1 || disp[0]["approver_account_deleted"] != true {
		t.Fatalf("dispatched row did not flag the deleted approver: %v", disp)
	}
}

// TestAssistantRequestDispatchVerdictTableAsAppRole: every terminal reason
// closes the row as not_sent with its reason, 0 sends and 0 attempt records.
// The first five are decided before a site principal can exist (closed in a
// tenant transaction); the last two are decided in the site transaction.
func TestAssistantRequestDispatchVerdictTableAsAppRole(t *testing.T) {
	st := newARStack(t)
	type verdict struct {
		name    string
		reason  string
		arrange func(site uuid.UUID, g sqlc.McpGrant)
	}
	cases := []verdict{
		{"revoked, cascade skipped", assistantrequest.ReasonGrantInactive, func(_ uuid.UUID, g sqlc.McpGrant) {
			st.exec(t, `UPDATE mcp_grants SET status = 'revoked', revoked_at = now() WHERE id = $1`, g.ID)
		}},
		{"absolutely expired", assistantrequest.ReasonGrantInactive, func(_ uuid.UUID, g sqlc.McpGrant) {
			st.exec(t, `UPDATE mcp_grants SET created_at = now() - interval '2 days', expires_at = now() - interval '1 minute' WHERE id = $1`, g.ID)
		}},
		{"idle expired", assistantrequest.ReasonGrantInactive, func(_ uuid.UUID, g sqlc.McpGrant) {
			st.exec(t, `UPDATE mcp_grants SET created_at = now() - interval '3 days', idle_expire_after_days = 1, last_used_at = now() - interval '2 days' WHERE id = $1`, g.ID)
		}},
		{"capability removed", assistantrequest.ReasonCapabilityNotHeld, func(_ uuid.UUID, g sqlc.McpGrant) {
			st.exec(t, `UPDATE mcp_grants SET capabilities = ARRAY['mcp.sites.read'] WHERE id = $1`, g.ID)
		}},
		{"site untagged after approval", assistantrequest.ReasonSiteAbsent, func(_ uuid.UUID, g sqlc.McpGrant) {
			st.exec(t, `UPDATE mcp_grants SET scope_site_ids = ARRAY[$2::uuid] WHERE id = $1`, g.ID, st.site(t))
		}},
		{"agent downgraded", assistantrequest.ReasonAgentOutdated, func(site uuid.UUID, _ sqlc.McpGrant) {
			st.exec(t, `UPDATE sites SET agent_version = '0.61.152' WHERE id = $1`, site)
		}},
		{"site archived", assistantrequest.ReasonSiteAbsent, func(site uuid.UUID, _ sqlc.McpGrant) {
			st.exec(t, `UPDATE sites SET connection_state = 'archived', archived_at = now() WHERE id = $1`, site)
		}},
	}
	for _, tc := range cases {
		site := st.site(t)
		g := st.grant(t, site)
		row := st.approved(t, site, g.ID)
		tc.arrange(site, g)
		before := st.sender.count()
		for run := 0; run < 3; run++ {
			if err := arDispatch(t, st.svc, row); err != nil {
				t.Fatalf("%s: dispatch run %d: %v", tc.name, run, err)
			}
		}
		if st.sender.count() != before {
			t.Fatalf("%s: sent %d times", tc.name, st.sender.count()-before)
		}
		if n := st.purgeRows(t, site); n != 0 {
			t.Fatalf("%s: %d attempt records", tc.name, n)
		}
		arWantClosed(t, st, row, tc.reason)
	}
}

// TestAssistantRequestDispatchPausedAndDeletedOrganisationAsAppRole: a pause
// closes before any site principal, and a soft-deleted organisation is caught
// at the reservation's tenants FOR SHARE re-read; both send nothing.
func TestAssistantRequestDispatchPausedAndDeletedOrganisationAsAppRole(t *testing.T) {
	st := newARStack(t)
	site := st.site(t)
	g := st.grant(t, site)
	row := st.approved(t, site, g.ID)
	st.exec(t, `UPDATE tenants SET deleted_at = now() WHERE id = $1`, st.tenant)
	if err := arDispatch(t, st.svc, row); err != nil {
		t.Fatalf("dispatch for a deleted organisation: %v", err)
	}
	if st.sender.count() != 0 || st.purgeRows(t, site) != 0 {
		t.Fatalf("a deleted organisation's clear ran: sends=%d", st.sender.count())
	}
	arWantClosed(t, st, row, assistantrequest.ReasonOrganisationDeleted)

	st.exec(t, `UPDATE tenants SET deleted_at = NULL WHERE id = $1`, st.tenant)
	row2 := st.approved(t, site, g.ID)
	st.exec(t, `UPDATE tenants SET assistant_enabled_at = COALESCE(assistant_enabled_at, now()), assistant_paused_at = now() WHERE id = $1`, st.tenant)
	if err := arDispatch(t, st.svc, row2); err != nil {
		t.Fatalf("dispatch for a paused organisation: %v", err)
	}
	if st.sender.count() != 0 {
		t.Fatalf("a paused organisation's clear ran")
	}
	arWantClosed(t, st, row2, assistantrequest.ReasonAssistantPaused)
}

// TestAssistantRequestDispatchTransientsAsAppRole: every transient reason
// leaves the row approved with its code, 0 sends and 0 attempt records: the
// switch, an offline agent, the whole-site cooldown after a dashboard purge,
// the organisation lifecycle lock held, another clear in flight on the site,
// and a failed intent audit write.
func TestAssistantRequestDispatchTransientsAsAppRole(t *testing.T) {
	ctx := context.Background()
	st := newARStack(t)

	// The switch: the worker runs and records why.
	{
		site := st.site(t)
		row := st.approved(t, site, st.grant(t, site).ID)
		off := st.newService(st.rec, st.sender, perf.NewRepo(st.pool), false)
		if err := arDispatch(t, off, row); err != nil {
			t.Fatalf("switched-off dispatch: %v", err)
		}
		if st.sender.count() != 0 || st.purgeRows(t, site) != 0 {
			t.Fatalf("switched off, yet sends=%d", st.sender.count())
		}
		arWantWaiting(t, st, row, assistantrequest.AttemptWriteToolsDisabled)
	}

	// An offline agent.
	{
		site := st.site(t)
		row := st.approved(t, site, st.grant(t, site).ID)
		st.exec(t, `UPDATE sites SET connection_state = 'disconnected' WHERE id = $1`, site)
		if err := arDispatch(t, st.svc, row); err != nil {
			t.Fatalf("offline dispatch: %v", err)
		}
		arWantWaiting(t, st, row, assistantrequest.AttemptSiteUnreachable)
		st.exec(t, `UPDATE sites SET connection_state = 'connected' WHERE id = $1`, site)
		if err := arDispatch(t, st.svc, row); err != nil {
			t.Fatalf("dispatch after reconnect: %v", err)
		}
		if got := st.state(t, row.ID); arStr(got.Outcome) != assistantrequest.OutcomePurged {
			t.Fatalf("after reconnect: %+v", got)
		}
	}
	sent := st.sender.count()

	// A dashboard whole-site purge a minute ago delays the clear; it does not
	// refuse it.
	{
		site := st.site(t)
		row := st.approved(t, site, st.grant(t, site).ID)
		if err := st.pool.InTenantTx(ctx, st.tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO cache_purge_audit (tenant_id, site_id, kind, initiator_user_id, created_at)
				VALUES ($1, $2, 'all', $3, now() - interval '1 minute')`, st.tenant, site, st.user)
			return err
		}); err != nil {
			t.Fatalf("seed dashboard purge: %v", err)
		}
		if err := arDispatch(t, st.svc, row); err != nil {
			t.Fatalf("dispatch in cooldown: %v", err)
		}
		arWantWaiting(t, st, row, assistantrequest.AttemptSiteCooldown)
	}

	// The organisation lifecycle lock, held by a session.
	{
		site := st.site(t)
		row := st.approved(t, site, st.grant(t, site).ID)
		conn, err := st.admin.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtext($1), hashtext($2))`, "org_lifecycle", st.tenant.String()); err != nil {
			t.Fatalf("hold lifecycle lock: %v", err)
		}
		start := time.Now()
		derr := arDispatch(t, st.svc, row)
		took := time.Since(start)
		_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtext($1), hashtext($2))`, "org_lifecycle", st.tenant.String())
		conn.Release()
		if derr != nil {
			t.Fatalf("dispatch under the lifecycle lock: %v", derr)
		}
		if took > time.Second {
			t.Fatalf("org_busy took %s, want under 1s (a try-lock, never a wait)", took)
		}
		arWantWaiting(t, st, row, assistantrequest.AttemptOrgBusy)
	}

	// A failed intent audit write leaves no attempt record.
	{
		site := st.site(t)
		row := st.approved(t, site, st.grant(t, site).ID)
		noLedger := st.newService(nil, st.sender, perf.NewRepo(st.pool), true)
		if err := arDispatch(t, noLedger, row); err == nil {
			t.Fatalf("a reservation without its audit row reported success")
		}
		if n := st.purgeRows(t, site); n != 0 {
			t.Fatalf("a failed intent audit left %d attempt records", n)
		}
		if got := st.state(t, row.ID); got.State != assistantrequest.StateApproved {
			t.Fatalf("a failed reservation moved the row to %s", got.State)
		}
	}
	if st.sender.count() != sent {
		t.Fatalf("a transient sent anyway (%d sends)", st.sender.count()-sent)
	}

	// Busy: while one clear is on the wire, a second approved clear on the same
	// site waits as site_busy and does not run alongside it.
	{
		site := st.site(t)
		first := st.approvedPage(t, site, st.grant(t, site).ID)
		second := st.approvedPage(t, site, st.grant(t, site).ID)
		var secondErr error
		st.sender.during = func() {
			st.sender.during = nil
			secondErr = arDispatch(t, st.svc, second)
		}
		if err := arDispatch(t, st.svc, first); err != nil {
			t.Fatalf("first dispatch: %v", err)
		}
		if secondErr != nil {
			t.Fatalf("second dispatch: %v", secondErr)
		}
		if st.sender.count() != sent+1 {
			t.Fatalf("two clears ran on one site at once (sends=%d)", st.sender.count()-sent)
		}
		arWantWaiting(t, st, second, assistantrequest.AttemptSiteBusy)
		if got := st.state(t, first.ID); arStr(got.Outcome) != assistantrequest.OutcomePurged {
			t.Fatalf("first: %+v", got)
		}
	}
}

// ---------------------------------------------------------------------------
// OUTCOME, RECONCILER AND SWEEPER
// ---------------------------------------------------------------------------

// TestAssistantRequestOutcomeReconcilerAndSweeperAsAppRole: the outcome
// transaction is all or nothing with the gauge; a late outcome after the
// reconciler writes nothing; the sweeper expires waiting rows and closes
// approved rows past their deadline even when the worker fails every run.
func TestAssistantRequestOutcomeReconcilerAndSweeperAsAppRole(t *testing.T) {
	st := newARStack(t)

	// The gauge joins the outcome transaction.
	{
		site := st.site(t)
		row := st.approved(t, site, st.grant(t, site).ID)
		bad := st.newService(st.rec, st.sender, arFailingGauge{perf.NewRepo(st.pool)}, true)
		if err := arDispatch(t, bad, row); err != nil {
			t.Fatalf("dispatch with a failing gauge: %v", err)
		}
		got := st.state(t, row.ID)
		if got.State != assistantrequest.StateDispatched || got.Outcome != nil {
			t.Fatalf("a failed outcome transaction left %+v, want dispatched with no outcome", got)
		}
		if n := st.countSQL(t, `SELECT count(*) FROM site_cache_stats WHERE site_id = $1 AND last_purged_at IS NOT NULL`, site); n != 0 {
			t.Fatalf("the gauge was stamped outside the outcome transaction")
		}
		if n := len(st.auditRows(t, audit.ActionCachePurged, site)); n != 0 {
			t.Fatalf("site.cache.purged written without its outcome (%d)", n)
		}
		// The reconciler closes it once it is stale.
		st.exec(t, `UPDATE assistant_cache_purge_requests SET claimed_at = now() - interval '11 minutes' WHERE id = $1`, row.ID)
		arReconcile(t, st.svc)
		if got := st.state(t, row.ID); arStr(got.Outcome) != assistantrequest.OutcomeUnknown {
			t.Fatalf("reconciler left %+v", got)
		}
		arReconcile(t, st.svc)
		if n := len(st.auditRows(t, audit.ActionAssistantRequestFailed, row.ID)); n != 1 {
			t.Fatalf("%d failed audit rows after two reconciles, want 1", n)
		}
	}

	// A late outcome, after the reconciler closed the row, writes nothing.
	{
		site := st.site(t)
		row := st.approved(t, site, st.grant(t, site).ID)
		st.sender.during = func() {
			st.sender.during = nil
			st.exec(t, `UPDATE assistant_cache_purge_requests SET claimed_at = now() - interval '11 minutes' WHERE id = $1`, row.ID)
			arReconcile(t, st.svc)
		}
		if err := arDispatch(t, st.svc, row); err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		if got := st.state(t, row.ID); arStr(got.Outcome) != assistantrequest.OutcomeUnknown {
			t.Fatalf("the late outcome overwrote the reconciler: %+v", got)
		}
		if n := len(st.auditRows(t, audit.ActionCachePurged, site)); n != 0 {
			t.Fatalf("a late outcome wrote site.cache.purged")
		}
		if n := len(st.auditRows(t, audit.ActionAssistantRequestFailed, row.ID)); n != 1 {
			t.Fatalf("%d failed rows, want the reconciler's 1", n)
		}
	}

	// Sweeper: a waiting row past its window expires with no decider.
	{
		site := st.site(t)
		row := st.pending(t, site, st.grant(t, site).ID)
		st.exec(t, `UPDATE assistant_cache_purge_requests
			SET created_at = now() - interval '25 hours', expires_at = now() - interval '1 hour' WHERE id = $1`, row.ID)
		arSweep(t, st.svc)
		if got := st.state(t, row.ID); got.State != assistantrequest.StateExpired || got.DecidedBy != nil {
			t.Fatalf("lapsed row: %+v", got)
		}
		if n := len(st.auditRows(t, audit.ActionAssistantRequestExpired, row.ID)); n != 1 {
			t.Fatalf("%d expired audit rows, want 1", n)
		}
	}

	// Sweeper backstop: the worker cannot make progress (switched off on every
	// run), and the sweeper still closes the row at its deadline.
	{
		site := st.site(t)
		row := st.approved(t, site, st.grant(t, site).ID)
		off := st.newService(st.rec, st.sender, perf.NewRepo(st.pool), false)
		for i := 0; i < 3; i++ {
			if err := arDispatch(t, off, row); err != nil {
				t.Fatalf("switched-off run %d: %v", i, err)
			}
		}
		arSweep(t, off)
		arWantWaiting(t, st, row, assistantrequest.AttemptWriteToolsDisabled)
		st.exec(t, `UPDATE assistant_cache_purge_requests SET created_at = now() - interval '2 hours',
			decided_at = now() - interval '61 minutes' WHERE id = $1`, row.ID)
		arSweep(t, off)
		arWantClosed(t, st, row, assistantrequest.ReasonDispatchDeadlinePassed)
		got := st.state(t, row.ID)
		if arStr(got.LastAttemptCode) != assistantrequest.AttemptWriteToolsDisabled {
			t.Fatalf("the sweeper dropped last_attempt_code: %+v", got)
		}
		md := st.auditRows(t, audit.ActionAssistantRequestNotSent, row.ID)
		if md[0]["closed_by"] != "sweeper" {
			t.Fatalf("not_sent row does not name the sweeper: %v", md[0])
		}
		if st.purgeRows(t, site) != 0 {
			t.Fatalf("the backstop left an attempt record")
		}
	}
}
