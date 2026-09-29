package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/govcontext"
)

// ---------------------------------------------------------------------------
// The request rail's unit harness.
//
// railFake is a FAITHFUL fake of the rail's database: a read under a
// principal sees only that principal's sites (the site-scope policy), a
// transaction that returns an error leaves no write behind (rollback), and
// the insert's ON CONFLICT on the one-pending index returns no row. The
// integration tests prove the same things against Postgres as wpmgr_app.
// ---------------------------------------------------------------------------

type railFake struct {
	*fakeStore

	mu     sync.Mutex
	db     map[uuid.UUID]sqlc.Site
	rows   []sqlc.AssistantCachePurgeRequest
	clears map[uuid.UUID]int64 // AI clears per site in the last hour
	ops    []string

	// conflictReadMisses: after an insert conflict, this many dedupe reads
	// find no waiting row (a person decided it in between).
	conflictReadMisses int
	insertCalls        int
	lockErr            error

	txPrincipals   []domain.Principal
	addrPrincipals []domain.Principal
	readPrincipals []domain.Principal
	addrReads      int
}

func newRailFake() *railFake {
	return &railFake{
		fakeStore: &fakeStore{},
		db:        map[uuid.UUID]sqlc.Site{},
		clears:    map[uuid.UUID]int64{},
	}
}

func (f *railFake) op(s string) {
	f.mu.Lock()
	f.ops = append(f.ops, s)
	f.mu.Unlock()
}

// addSite stores a connected site at the floor.
func (f *railFake) addSite(url string) sqlc.Site {
	s := sqlc.Site{
		ID:              uuid.New(),
		Name:            "site " + url,
		Url:             url,
		ConnectionState: "connected",
		AgentVersion:    MinAgentVersionForOriginOnlyPurge,
	}
	f.db[s.ID] = s
	return s
}

func allowedSet(p domain.Principal) map[uuid.UUID]bool {
	m := map[uuid.UUID]bool{}
	for _, id := range p.AllowedSiteIDs {
		m[id] = true
	}
	return m
}

// ListSitesForRead is faithful: only the principal's sites, never archived.
func (f *railFake) ListSitesForRead(_ context.Context, p domain.Principal, limit int32) ([]sqlc.Site, bool, error) {
	f.op("ListSitesForRead")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readPrincipals = append(f.readPrincipals, p)
	if !p.IsSiteConstrained() {
		return nil, false, errors.New("railFake: ListSitesForRead needs a site-constrained principal")
	}
	allowed := allowedSet(p)
	var out []sqlc.Site
	for id, s := range f.db {
		if allowed[id] && s.ConnectionState != "archived" {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.String() < out[j].ID.String() })
	if int32(len(out)) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

func (f *railFake) ListSiteAddressesInScope(_ context.Context, p domain.Principal) ([]siteAddressRow, error) {
	f.op("ListSiteAddressesInScope")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addrPrincipals = append(f.addrPrincipals, p)
	f.addrReads++
	allowed := allowedSet(p)
	var out []siteAddressRow
	for id, s := range f.db {
		if allowed[id] && s.ConnectionState != "archived" {
			out = append(out, siteAddressRow{ID: id, URL: s.Url})
		}
	}
	return out, nil
}

func (f *railFake) RunRequestTx(ctx context.Context, p domain.Principal, fn func(tx pgx.Tx, q requestQueries) error) error {
	f.mu.Lock()
	f.txPrincipals = append(f.txPrincipals, p)
	snapshot := append([]sqlc.AssistantCachePurgeRequest(nil), f.rows...)
	f.mu.Unlock()
	f.op("tx:begin")
	err := fn(nil, &railFakeQueries{f: f, p: p})
	if err != nil {
		f.mu.Lock()
		f.rows = snapshot
		f.mu.Unlock()
		f.op("tx:rollback")
		return err
	}
	f.op("tx:commit")
	return nil
}

func (f *railFake) statusRow(r sqlc.AssistantCachePurgeRequest) requestStatusRow {
	return requestStatusRow{
		ID: r.ID, SiteID: r.SiteID, SiteLabel: r.SiteLabel, Scope: r.Scope, Url: r.Url,
		State: r.State, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt, DecidedAt: r.DecidedAt,
		WithdrawnAt: r.WithdrawnAt, ClaimedAt: r.ClaimedAt, LastAttemptAt: r.LastAttemptAt,
		LastAttemptCode: r.LastAttemptCode, Outcome: r.Outcome, NotSentReason: r.NotSentReason,
		OutcomeAt: r.OutcomeAt, HostingCachesCleared: r.HostingCachesCleared,
		HostingCachesSkipped: r.HostingCachesSkipped, OriginOnlyConfirmed: r.OriginOnlyConfirmed,
		WpmgrCdn: r.WpmgrCdn,
	}
}

func (f *railFake) ReadRequestStatus(_ context.Context, p domain.Principal, grantID, requestID uuid.UUID) (requestStatusRow, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	allowed := allowedSet(p)
	for _, r := range f.rows {
		if r.ID == requestID && r.ProposedByGrantID == grantID && allowed[r.SiteID] && r.TenantID == p.TenantID {
			return f.statusRow(r), true, nil
		}
	}
	return requestStatusRow{}, false, nil
}

func (f *railFake) ListOpenRequestStatus(_ context.Context, p domain.Principal, grantID uuid.UUID, limit int32) ([]requestStatusRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	allowed := allowedSet(p)
	var out []requestStatusRow
	for i := len(f.rows) - 1; i >= 0; i-- {
		r := f.rows[i]
		open := r.State == "pending" || r.State == "approved_undispatched" || (r.State == "dispatched" && r.Outcome == nil)
		if open && r.ProposedByGrantID == grantID && allowed[r.SiteID] && r.TenantID == p.TenantID {
			out = append(out, f.statusRow(r))
		}
		if int32(len(out)) >= limit {
			break
		}
	}
	return out, nil
}

// railFakeQueries models the statements under the transaction's principal.
type railFakeQueries struct {
	f *railFake
	p domain.Principal
}

func (q *railFakeQueries) visible(r sqlc.AssistantCachePurgeRequest) bool {
	return r.TenantID == q.p.TenantID && allowedSet(q.p)[r.SiteID]
}

func (q *railFakeQueries) TakeAssistantRequestXactLock(_ context.Context, arg sqlc.TakeAssistantRequestXactLockParams) error {
	q.f.op("lock:" + arg.LockKey)
	if arg.LockKey == "" || arg.LockID == "" {
		return errors.New("railFake: lock with an empty key")
	}
	return q.f.lockErr
}

func (q *railFakeQueries) ExpireLapsedPendingAssistantCachePurgeRequest(_ context.Context, arg sqlc.ExpireLapsedPendingAssistantCachePurgeRequestParams) ([]uuid.UUID, error) {
	q.f.op("expire")
	q.f.mu.Lock()
	defer q.f.mu.Unlock()
	var out []uuid.UUID
	for i, r := range q.f.rows {
		if q.visible(r) && r.SiteID == arg.SiteID && r.ProposedByGrantID == arg.ProposedByGrantID &&
			r.State == "pending" && !r.ExpiresAt.After(time.Now()) {
			q.f.rows[i].State = "expired"
			out = append(out, r.ID)
		}
	}
	return out, nil
}

func (q *railFakeQueries) CountLivePendingAssistantCachePurgeRequestsForGrant(_ context.Context, arg sqlc.CountLivePendingAssistantCachePurgeRequestsForGrantParams) (int64, error) {
	q.f.mu.Lock()
	defer q.f.mu.Unlock()
	var n int64
	for _, r := range q.f.rows {
		if q.visible(r) && r.ProposedByGrantID == arg.ProposedByGrantID && r.State == "pending" && r.ExpiresAt.After(time.Now()) {
			n++
		}
	}
	return n, nil
}

func (q *railFakeQueries) CountAssistantCachePurgeRequestsForGrantSince(_ context.Context, arg sqlc.CountAssistantCachePurgeRequestsForGrantSinceParams) (int64, error) {
	q.f.mu.Lock()
	defer q.f.mu.Unlock()
	since := time.Now().Add(-time.Duration(arg.WindowSeconds) * time.Second)
	var n int64
	for _, r := range q.f.rows {
		if q.visible(r) && r.ProposedByGrantID == arg.ProposedByGrantID && r.CreatedAt.After(since) {
			n++
		}
	}
	return n, nil
}

func (q *railFakeQueries) CountAssistantCachePurgesOnSiteSince(_ context.Context, arg sqlc.CountAssistantCachePurgesOnSiteSinceParams) (int64, error) {
	q.f.mu.Lock()
	defer q.f.mu.Unlock()
	if !allowedSet(q.p)[arg.SiteID] {
		return 0, nil
	}
	return q.f.clears[arg.SiteID], nil
}

func (q *railFakeQueries) InsertAssistantCachePurgeRequest(_ context.Context, arg sqlc.InsertAssistantCachePurgeRequestParams) (sqlc.AssistantCachePurgeRequest, error) {
	q.f.op("insert")
	q.f.mu.Lock()
	defer q.f.mu.Unlock()
	q.f.insertCalls++
	if !allowedSet(q.p)[arg.SiteID] || arg.TenantID != q.p.TenantID {
		return sqlc.AssistantCachePurgeRequest{}, errors.New("railFake: new row violates row-level security policy")
	}
	if !siteHostPattern.MatchString(arg.SiteHost) {
		return sqlc.AssistantCachePurgeRequest{}, errors.New(`railFake: violates check constraint "site_host_ascii"`)
	}
	if arg.Url != nil && !satisfiesURLBackstop(*arg.Url) {
		return sqlc.AssistantCachePurgeRequest{}, errors.New(`railFake: violates check constraint "url_backstop"`)
	}
	if (arg.Scope == "url") != (arg.Url != nil) {
		return sqlc.AssistantCachePurgeRequest{}, errors.New("railFake: scope and url disagree")
	}
	for _, r := range q.f.rows {
		if r.TenantID == arg.TenantID && r.SiteID == arg.SiteID && r.ProposedByGrantID == arg.ProposedByGrantID && r.State == "pending" {
			return sqlc.AssistantCachePurgeRequest{}, pgx.ErrNoRows
		}
	}
	row := sqlc.AssistantCachePurgeRequest{
		ID: uuid.New(), TenantID: arg.TenantID, SiteID: arg.SiteID, ProposedByGrantID: arg.ProposedByGrantID,
		Scope: arg.Scope, Url: arg.Url, SiteLabel: arg.SiteLabel, SiteHost: arg.SiteHost,
		GrantLabel: arg.GrantLabel, GrantVia: arg.GrantVia, SetupClient: arg.SetupClient,
		DigestNonce: arg.DigestNonce, PresentedDigest: arg.PresentedDigest, State: "pending",
		CreatedAt: time.Now(), ExpiresAt: arg.ExpiresAt,
	}
	q.f.rows = append(q.f.rows, row)
	return row, nil
}

func (q *railFakeQueries) GetPendingAssistantCachePurgeRequestForGrantSite(_ context.Context, arg sqlc.GetPendingAssistantCachePurgeRequestForGrantSiteParams) (sqlc.AssistantCachePurgeRequest, error) {
	q.f.op("dedupe-read")
	q.f.mu.Lock()
	defer q.f.mu.Unlock()
	if q.f.conflictReadMisses > 0 {
		q.f.conflictReadMisses--
		return sqlc.AssistantCachePurgeRequest{}, pgx.ErrNoRows
	}
	for _, r := range q.f.rows {
		if q.visible(r) && r.SiteID == arg.SiteID && r.ProposedByGrantID == arg.ProposedByGrantID && r.State == "pending" {
			return r, nil
		}
	}
	return sqlc.AssistantCachePurgeRequest{}, pgx.ErrNoRows
}

// seedRow puts a request row straight into the fake.
func (f *railFake) seedRow(r sqlc.AssistantCachePurgeRequest) sqlc.AssistantCachePurgeRequest {
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	if r.ExpiresAt.IsZero() {
		r.ExpiresAt = time.Now().Add(24 * time.Hour)
	}
	f.mu.Lock()
	f.rows = append(f.rows, r)
	f.mu.Unlock()
	return r
}

func (f *railFake) rowsFor(grant uuid.UUID) []sqlc.AssistantCachePurgeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []sqlc.AssistantCachePurgeRequest
	for _, r := range f.rows {
		if r.ProposedByGrantID == grant {
			out = append(out, r)
		}
	}
	return out
}

// railRecorder captures every audit event and notes it in the fake's op log,
// so a test can read the order of writes and audit rows. failInTx makes every
// RecordInTx fail.
type railRecorder struct {
	capturingRecorder
	f        *railFake
	failInTx bool
}

func (r *railRecorder) RecordInTx(ctx context.Context, tx pgx.Tx, e audit.Event) (audit.Entry, error) {
	if r.failInTx {
		return audit.Entry{}, errors.New("planted audit failure")
	}
	r.f.op("audit:" + e.Action)
	return r.capturingRecorder.RecordInTx(ctx, tx, e)
}

func (r *railRecorder) RecordOrFail(ctx context.Context, e audit.Event) (audit.Entry, error) {
	r.f.op("audit:" + e.Action)
	return r.capturingRecorder.RecordOrFail(ctx, e)
}

func (r *railRecorder) actions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.Action)
	}
	return out
}

func (r *railRecorder) last(action string) (audit.Event, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.events) - 1; i >= 0; i-- {
		if r.events[i].Action == action {
			return r.events[i], true
		}
	}
	return audit.Event{}, false
}

// railCtxStore is a governed-context store with an org rule set and per-site
// rule sets.
type railCtxStore struct {
	org   []string
	sites map[uuid.UUID][]string
	err   error
}

func (c *railCtxStore) LatestOrgSnapshot(context.Context, uuid.UUID) (govcontext.Snapshot, bool, error) {
	if c.err != nil {
		return govcontext.Snapshot{}, false, c.err
	}
	return govcontext.Snapshot{Restrictions: govcontext.RestrictionSet{ForbiddenTools: c.org}}, len(c.org) > 0, nil
}

func (c *railCtxStore) LatestSiteSnapshot(_ context.Context, _ uuid.UUID, siteID uuid.UUID) (govcontext.Snapshot, bool, error) {
	if c.err != nil {
		return govcontext.Snapshot{}, false, c.err
	}
	rules := c.sites[siteID]
	return govcontext.Snapshot{Restrictions: govcontext.RestrictionSet{ForbiddenTools: rules}}, len(rules) > 0, nil
}

// railEnv is one wired rail: the fake, the recorder, the service, the
// handler, and a connection holding the cache capability.
type railEnv struct {
	t    *testing.T
	f    *railFake
	rec  *railRecorder
	ctx  *railCtxStore
	svc  *Service
	h    *TransportHandler
	auth AuthorizedRequest
}

// newRailEnv wires the rail over the given sites; the connection's scope is
// scope (every site when nil).
func newRailEnv(t *testing.T, f *railFake, scope []uuid.UUID) *railEnv {
	t.Helper()
	if scope == nil {
		for id := range f.db {
			scope = append(scope, id)
		}
	}
	rec := &railRecorder{f: f}
	cs := &railCtxStore{sites: map[uuid.UUID][]string{}}
	svc := NewService(f).withAuditRecorder(rec).WithContextResolver(&govcontext.Resolver{Store: cs})
	if err := svc.SetWriteToolsEnabled(true); err != nil {
		t.Fatalf("switch write tools on: %v", err)
	}
	auth := authWith(NewCapabilitySet(AllCapabilities()), scope...)
	auth.GrantName = "Laptop"
	env := &railEnv{t: t, f: f, rec: rec, ctx: cs, svc: svc, auth: auth}
	env.h = NewTransportHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	return env
}

// railCall is one answered tools/call.
type railCall struct {
	resp jsonrpcResponse
	raw  string
	text string
	code int
	msg  string
	data map[string]any
}

// call drives name through the real authorizeCall and callTool, exactly as
// dispatch does, and resets the per-process limiter first unless keepLimit.
func (e *railEnv) call(name string, args any) railCall {
	e.t.Helper()
	e.svc.requestLimit = newRequestRateLimiter()
	return e.callKeepLimit(name, args)
}

func (e *railEnv) callKeepLimit(name string, args any) railCall {
	e.t.Helper()
	rawArgs, err := json.Marshal(args)
	if err != nil {
		e.t.Fatalf("marshal args: %v", err)
	}
	params, _ := json.Marshal(map[string]any{"name": name, "arguments": json.RawMessage(rawArgs)})
	req := jsonrpcRequest{ID: json.RawMessage(`7`), Method: "tools/call", Params: params}
	ctx := context.Background()
	entry, p, resp, refused := e.h.authorizeCall(ctx, e.auth, req)
	if !refused {
		resp = e.h.callTool(ctx, e.auth, req, entry, p)
	}
	raw, _ := json.Marshal(resp)
	out := railCall{resp: resp, raw: string(raw)}
	if resp.Error != nil {
		out.code = resp.Error.Code
		out.msg = resp.Error.Message
		if len(resp.Error.Data) > 0 {
			_ = json.Unmarshal(resp.Error.Data, &out.data)
		}
		return out
	}
	var env struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || len(env.Result.Content) == 0 {
		e.t.Fatalf("no tool text in %s", raw)
	}
	out.text = env.Result.Content[0].Text
	return out
}

func (c railCall) created(t *testing.T) createdResult {
	t.Helper()
	if c.code != 0 {
		t.Fatalf("want a created request, got %d %q data=%v", c.code, c.msg, c.data)
	}
	var r createdResult
	if err := json.Unmarshal([]byte(c.text), &r); err != nil {
		t.Fatalf("decode created result: %v\n%s", err, c.text)
	}
	return r
}

func (c railCall) wantCode(t *testing.T, code int) {
	t.Helper()
	if c.code != code {
		t.Fatalf("want JSON-RPC code %d, got %d %q (text %q)", code, c.code, c.msg, c.text)
	}
}

func urlReq(site uuid.UUID, u string) map[string]any {
	return map[string]any{"site_id": site.String(), "scope": "url", "url": u}
}

func allReq(site uuid.UUID) map[string]any {
	return map[string]any{"site_id": site.String(), "scope": "all"}
}

// unfence strips the site-text marker, failing when it is absent.
func unfence(t *testing.T, s string) string {
	t.Helper()
	if !strings.HasPrefix(s, siteTextMarker) {
		t.Fatalf("%q is not fenced", s)
	}
	return strings.TrimPrefix(s, siteTextMarker)
}
