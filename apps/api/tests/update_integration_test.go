package tests

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/httpclient"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	"github.com/mosamlife/wpmgr/apps/api/internal/update"
)

// fakeAgent is a stand-in WordPress agent. It records the commands it receives
// and lets a test control the update/rollback responses and the homepage health
// (so we can exercise the auto-rollback path without a real WP).
type fakeAgent struct {
	srv *httptest.Server

	mu            sync.Mutex
	updateCalls   int32
	rollbackCalls int32
	homepageCalls int32
	dryRunSeen    bool
	snapshotSeen  bool
	// expectAud is the enrollment site_id the agent verifies the JWT aud against
	// (set by the test after enrollment). authErr records any aud/cmd mismatch
	// observed on a command so the test can assert the worker bound them.
	expectAud string
	updateAud string
	updateCmd string
	rbAud     string
	rbCmd     string

	// homepageStatus is returned for GET / (the health probe). 0 ⇒ 200.
	homepageStatus int
	homepageBody   string
	// pingStatus/pingBody override the signed ping-command response (GH #291
	// Phase 4 agent-first check) INDEPENDENTLY of homepageStatus/homepageBody.
	// pingConfigured=false (the default) mirrors homepageStatus/homepageBody
	// exactly, matching this fake agent's behaviour before the ping route
	// existed here (an unmatched path fell through to the "/" handler), so
	// every test that never calls setPing is unaffected. A test calls setPing
	// to decouple the two, e.g. to simulate a page-cached homepage (stale 200)
	// sitting in front of a genuinely fatal backend (ping 500), which is the
	// exact scenario this phase fixes.
	pingConfigured bool
	pingStatus     int
	pingBody       string
	// pingFailRemaining, when > 0, overrides pingStatus/pingBody for exactly
	// that many remaining ping calls (each call decrements it by one), then
	// the agent answers a plain healthy 200. Used to simulate a TRANSIENT
	// agent-side failure (e.g. the synchronous DB-migration-on-activation
	// window GH #127 documents) that clears within a few requests, as opposed
	// to setPing's persistent override.
	pingFailRemaining int32
	pingFailStatus    int
	pingFailBody      string
	pingCalls         int32
	// updateResp/rollbackResp override the command responses.
	updateResp   agentcmd.UpdateResponse
	rollbackResp agentcmd.RollbackResponse

	// --- GH #328: simulated per-site update lock (see enableSiteLock) ---
	//
	// siteLockHold, when > 0, makes the update handler behave like a real
	// agent holding WP_Upgrader::create_lock for approximately this long per
	// request: it holds an internal mutex for the duration and refuses any
	// OVERLAPPING request with an ItemSiteBusy result instead of the
	// scripted updateResp. concurrentPeak is the high-water mark of
	// SIMULTANEOUS lock holders observed (must never exceed 1 if the lock is
	// doing its job); lockWon counts requests that actually acquired it.
	siteLockHold   time.Duration
	lockMu         sync.Mutex
	concurrentNow  int32
	concurrentPeak int32
	lockWon        int32
	busyRefusals   int32
}

// enableSiteLock turns on the simulated per-site update lock: the update
// handler now holds an internal mutex for `hold` per request and answers any
// overlapping request with a single ItemSiteBusy result (ok=false), instead
// of the scripted updateResp. Used by the GH #328 serialisation tests to
// prove the control plane's per-site gate plus the agent's own lock actually
// keep concurrent applies against one site down to exactly one at a time.
func (fa *fakeAgent) enableSiteLock(hold time.Duration) {
	fa.mu.Lock()
	fa.siteLockHold = hold
	fa.mu.Unlock()
}

func newFakeAgent(t *testing.T) *fakeAgent {
	t.Helper()
	fa := &fakeAgent{
		updateResp: agentcmd.UpdateResponse{OK: true, Results: []agentcmd.ItemResult{{
			Status: agentcmd.ItemSucceeded, FromVersion: "1.0.0", ToVersion: "1.1.0", SnapshotID: "snap-1",
		}}},
		rollbackResp: agentcmd.RollbackResponse{OK: true, RestoredVersion: "1.0.0"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/wp-json/wpmgr/v1/command/update", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fa.updateCalls, 1)
		var req agentcmd.UpdateRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		aud, cmd := bearerAudCmd(r)
		fa.mu.Lock()
		fa.dryRunSeen = req.DryRun
		fa.snapshotSeen = req.Snapshot
		fa.updateAud = aud
		fa.updateCmd = cmd
		expect := fa.expectAud
		resp := fa.updateResp
		lockHold := fa.siteLockHold
		fa.mu.Unlock()
		// Mirror the real agent: require a Bearer token whose aud == this site's
		// enrollment id and cmd == the dispatched command. Reject otherwise.
		if r.Header.Get("Authorization") == "" || (expect != "" && aud != expect) || cmd != "update" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if lockHold > 0 {
			// Simulated site lock (GH #328): only ONE request may hold it at a
			// time; an overlapping request is refused as busy exactly like a
			// real agent whose WP_Upgrader::create_lock failed.
			if !fa.lockMu.TryLock() {
				atomic.AddInt32(&fa.busyRefusals, 1)
				var item agentcmd.UpdateItem
				if len(req.Items) > 0 {
					item = req.Items[0]
				}
				writeJSON(w, agentcmd.UpdateResponse{OK: false, Results: []agentcmd.ItemResult{{
					Type: item.Type, Slug: item.Slug, Status: agentcmd.ItemSiteBusy,
					Log: "another update is running on this site",
				}}})
				return
			}
			atomic.AddInt32(&fa.lockWon, 1)
			n := atomic.AddInt32(&fa.concurrentNow, 1)
			for {
				old := atomic.LoadInt32(&fa.concurrentPeak)
				if n <= old || atomic.CompareAndSwapInt32(&fa.concurrentPeak, old, n) {
					break
				}
			}
			time.Sleep(lockHold)
			atomic.AddInt32(&fa.concurrentNow, -1)
			fa.lockMu.Unlock()
		}
		writeJSON(w, resp)
	})
	mux.HandleFunc("/wp-json/wpmgr/v1/command/rollback", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fa.rollbackCalls, 1)
		aud, cmd := bearerAudCmd(r)
		fa.mu.Lock()
		fa.rbAud = aud
		fa.rbCmd = cmd
		expect := fa.expectAud
		resp := fa.rollbackResp
		fa.mu.Unlock()
		if r.Header.Get("Authorization") == "" || (expect != "" && aud != expect) || cmd != "rollback" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		writeJSON(w, resp)
	})
	mux.HandleFunc("/wp-json/wpmgr/v1/command/ping", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fa.pingCalls, 1)
		fa.mu.Lock()
		status, body := fa.pingStatus, fa.pingBody
		if !fa.pingConfigured {
			status, body = fa.homepageStatus, fa.homepageBody
		}
		if fa.pingFailRemaining > 0 {
			// A transient failure overrides everything else above for exactly
			// this many remaining calls, then clears (see setPingTransientFailure).
			fa.pingFailRemaining--
			status, body = fa.pingFailStatus, fa.pingFailBody
		}
		expect := fa.expectAud
		fa.mu.Unlock()
		aud, cmd := bearerAudCmd(r)
		if r.Header.Get("Authorization") == "" || (expect != "" && aud != expect) || cmd != "ping" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		if body != "" {
			_, _ = w.Write([]byte(body))
		} else if status >= 200 && status < 300 {
			// A plain 200 with no body override IS a valid, agent-shaped OK
			// ping response, matching the real agent's ping command handler.
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fa.homepageCalls, 1)
		fa.mu.Lock()
		status := fa.homepageStatus
		body := fa.homepageBody
		fa.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		if body != "" {
			_, _ = w.Write([]byte(body))
		}
	})
	fa.srv = httptest.NewServer(mux)
	t.Cleanup(fa.srv.Close)
	return fa
}

func (fa *fakeAgent) url() string { return fa.srv.URL }

func (fa *fakeAgent) setHomepage(status int, body string) {
	fa.mu.Lock()
	fa.homepageStatus = status
	fa.homepageBody = body
	fa.mu.Unlock()
}

// setPing decouples the signed ping-command response from the public
// homepage response (GH #291 Phase 4). Until called, ping mirrors
// homepageStatus/homepageBody exactly.
func (fa *fakeAgent) setPing(status int, body string) {
	fa.mu.Lock()
	fa.pingConfigured = true
	fa.pingStatus = status
	fa.pingBody = body
	fa.mu.Unlock()
}

// setPingTransientFailure makes the next n ping calls answer with status/body,
// then reverts to a plain healthy 200 for every call after that. Used to
// simulate a transient agent-side failure (e.g. a synchronous DB migration
// on activation, GH #127) that clears within the retry window, as opposed to
// setPing's persistent override.
func (fa *fakeAgent) setPingTransientFailure(n int, status int, body string) {
	fa.mu.Lock()
	fa.pingConfigured = true
	fa.pingFailRemaining = int32(n)
	fa.pingFailStatus = status
	fa.pingFailBody = body
	fa.mu.Unlock()
}

// setExpectAud sets the enrollment site_id the fake agent verifies the command
// JWT's aud claim against (mirrors the real agent binding to its own site_id).
func (fa *fakeAgent) setExpectAud(aud string) {
	fa.mu.Lock()
	fa.expectAud = aud
	fa.mu.Unlock()
}

// bearerAudCmd extracts the aud and cmd claims from the request's Bearer JWT
// (no signature verification needed here — we only assert the worker bound the
// claims; jwt_test.go proves the signature/verify contract).
func bearerAudCmd(r *http.Request) (aud, cmd string) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return "", ""
	}
	parts := strings.Split(strings.TrimPrefix(h, prefix), ".")
	if len(parts) != 3 {
		return "", ""
	}
	p := parts[1]
	if m := len(p) % 4; m != 0 {
		p += strings.Repeat("=", 4-m)
	}
	raw, err := base64.URLEncoding.DecodeString(p)
	if err != nil {
		return "", ""
	}
	var claims struct {
		Aud string `json:"aud"`
		Cmd string `json:"cmd"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return "", ""
	}
	return claims.Aud, claims.Cmd
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// svcSiteLookup adapts the site service to update.SiteLookup (mirrors the
// production adapter in cmd/wpmgr/siteadapter.go).
type svcSiteLookup struct{ svc *site.Service }

func (l *svcSiteLookup) GetSiteInfo(ctx context.Context, tenantID, siteID uuid.UUID) (update.SiteInfo, error) {
	s, err := l.svc.Get(ctx, tenantID, siteID)
	if err != nil {
		return update.SiteInfo{}, err
	}
	return toUpdateSiteInfo(s), nil
}

func (l *svcSiteLookup) ListSiteInfoByTag(ctx context.Context, tenantID uuid.UUID, tag string) ([]update.SiteInfo, error) {
	sites, err := l.svc.List(ctx, site.ListInput{TenantID: tenantID, AnyTags: []string{tag}, Limit: 200})
	if err != nil {
		return nil, err
	}
	out := make([]update.SiteInfo, 0, len(sites))
	for _, s := range sites {
		if s.EnrolledAt == nil {
			continue
		}
		out = append(out, toUpdateSiteInfo(s))
	}
	return out, nil
}

func toUpdateSiteInfo(s site.Site) update.SiteInfo {
	plugins, themes := s.ParsedComponents()
	comps := make([]update.Component, 0, len(plugins)+len(themes))
	for _, p := range plugins {
		comps = append(comps, toUpdateComponentInfo(update.TargetPlugin, p))
	}
	for _, th := range themes {
		comps = append(comps, toUpdateComponentInfo(update.TargetTheme, th))
	}
	info := update.SiteInfo{ID: s.ID, URL: s.URL, Name: s.Name, Enrolled: s.EnrolledAt != nil, Components: comps}
	if core := s.ParsedCoreUpdate(); core != nil && core.NewVersion != "" {
		info.CoreUpdateAvailable = true
		info.CoreCurrentVersion = core.CurrentVersion
		info.CoreNewVersion = core.NewVersion
	}
	return info
}

func toUpdateComponentInfo(typ string, c site.Component) update.Component {
	out := update.Component{Type: typ, Slug: c.Slug, Version: c.Version}
	if c.AvailableUpdate != nil && c.AvailableUpdate.NewVersion != "" {
		out.UpdateAvailable = true
		out.NewVersion = c.AvailableUpdate.NewVersion
	}
	return out
}

// genEd25519PrivBase64 returns a fresh Ed25519 private key as base64-std (the
// WPMGR_AGENT_SIGNING_PRIVATE_KEY wire format).
func genEd25519PrivBase64(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(priv)
}

// updateTestHarness wires the update stack against a real DB and a fake agent.
type updateTestHarness struct {
	pool    *db.Pool
	repo    update.Repo
	hub     *update.Hub
	svc     *update.Service
	worker  *update.Worker
	client  *river.Client[pgx.Tx]
	siteSvc *site.Service
}

// seedPendingPlugin records a plugin inventory entry with a pending update
// advisory on the site, so update.Service.planTasks (#126: intersect a run's
// requested items against each site's OWN pending set) actually produces a
// task for it.
func seedPendingPlugin(t *testing.T, h *updateTestHarness, tenant uuid.UUID, s site.Site, slug, from, to string) {
	t.Helper()
	_, err := h.siteSvc.ApplyMetadata(context.Background(), tenant, s.ID, site.Metadata{
		Plugins: []site.Component{{
			Slug: slug, Version: from,
			AvailableUpdate: &site.AvailableUpdate{NewVersion: to},
		}},
	})
	if err != nil {
		t.Fatalf("seed pending plugin: %v", err)
	}
}

// seedPendingCore records a WordPress core update advisory on the site.
func seedPendingCore(t *testing.T, h *updateTestHarness, tenant uuid.UUID, s site.Site, from, to string) {
	t.Helper()
	_, err := h.siteSvc.ApplyMetadata(context.Background(), tenant, s.ID, site.Metadata{
		CoreUpdate: &site.CoreUpdate{CurrentVersion: from, NewVersion: to},
	})
	if err != nil {
		t.Fatalf("seed pending core: %v", err)
	}
}

// enrollFakeSite enrolls a site whose URL points at the fake agent.
func enrollFakeSite(t *testing.T, pool *db.Pool, tenant uuid.UUID, url string) site.Site {
	t.Helper()
	ctx := context.Background()
	svc := site.NewService(site.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})
	code, err := svc.CreatePairingCode(ctx, site.CreatePairingCodeInput{TenantID: tenant})
	if err != nil {
		t.Fatalf("pairing code: %v", err)
	}
	_, _, key := genKey(t)
	s, err := svc.Enroll(ctx, site.EnrollRequest{PairingCode: code.Plaintext, SiteURL: url, AgentPublicKey: key})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	return s
}

// startUpdateRiver migrates River, grants the app role, builds the worker with
// an SSRF client that may reach loopback (test-only), and starts River.
func startUpdateRiver(t *testing.T, pool *db.Pool, worker *update.Worker) *river.Client[pgx.Tx] {
	t.Helper()
	ctx := context.Background()
	admin := connectAdmin(t, pool)
	defer admin.Close()
	migrator, err := rivermigrate.New(riverpgxv5.New(admin.Pool), nil)
	if err != nil {
		t.Fatalf("river migrator: %v", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		t.Fatalf("river migrate: %v", err)
	}
	if _, err := admin.Exec(ctx, "GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO wpmgr_app"); err != nil {
		t.Fatalf("grant river tables: %v", err)
	}

	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	queues := map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 2}}
	for _, q := range update.QueueNames() {
		queues[q] = river.QueueConfig{MaxWorkers: 3}
	}
	client, err := river.NewClient[pgx.Tx](riverpgxv5.New(pool.Pool), &river.Config{Queues: queues, Workers: workers})
	if err != nil {
		t.Fatalf("river client: %v", err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatalf("river start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.Stop(stopCtx)
	})
	return client
}

// newTestCommander builds a real agentcmd.Client over a loopback-permitting
// SSRF client and an ephemeral signer.
func newTestCommander(t *testing.T) update.Commander {
	t.Helper()
	signer := newTestSigner(t)
	c := httpclient.New(httpclient.Config{AllowPrivateNetworks: true, Timeout: 5 * time.Second})
	return agentcmd.NewClient(c, signer)
}

func newTestProber(t *testing.T) update.HealthProber {
	t.Helper()
	c := httpclient.New(httpclient.Config{AllowPrivateNetworks: true, Timeout: 5 * time.Second})
	return agentcmd.NewProbe(c)
}

func newTestSigner(t *testing.T) *agentcmd.Signer {
	t.Helper()
	s, err := agentcmd.NewSigner(genEd25519PrivBase64(t))
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return s
}

// buildHarness assembles the update stack with the given commander/prober.
func buildHarness(t *testing.T, pool *db.Pool, commander update.Commander, prober update.HealthProber) *updateTestHarness {
	repo := update.NewRepo(pool)
	hub := update.NewHub()
	siteSvc := site.NewService(site.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})
	rec := audit.NewRecorder(pool, domain.SystemClock{})
	lookup := &svcSiteLookup{svc: siteSvc}
	worker := update.NewWorker(repo, lookup, commander, prober, hub, rec, nil, 5, 0)
	client := startUpdateRiver(t, pool, worker)
	svc := update.NewService(repo, lookup, update.NewRiverEnqueuer(client), domain.NewValidator(), domain.SystemClock{})
	return &updateTestHarness{pool: pool, repo: repo, hub: hub, svc: svc, worker: worker, client: client, siteSvc: siteSvc}
}

// waitRunCompleted polls until the run reaches completed or the deadline.
func waitRunCompleted(t *testing.T, h *updateTestHarness, tenant, runID uuid.UUID) (update.Run, []update.Task) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(25 * time.Second)
	for {
		run, tasks, err := h.svc.GetRun(ctx, tenant, runID)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run.Status == update.RunCompleted {
			return run, tasks
		}
		if time.Now().After(deadline) {
			t.Fatalf("run did not complete in time (status=%s tasks=%+v)", run.Status, tasks)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestUpdateRunHappyPath: create a run against a fake agent that succeeds and a
// healthy homepage → task succeeds, versions recorded, run completed.
func TestUpdateRunHappyPath(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "upd-happy")
	fa := newFakeAgent(t)
	fa.setHomepage(http.StatusOK, "<html>ok</html>")

	h := buildHarness(t, pool, newTestCommander(t), newTestProber(t))
	s := enrollFakeSite(t, pool, tenant, fa.url())
	fa.setExpectAud(s.ID.String())
	seedPendingPlugin(t, h, tenant, s, "akismet", "1.0.0", "1.1.0")

	run, tasks, err := h.svc.CreateRun(context.Background(), update.CreateRunInput{
		TenantID: tenant,
		SiteIDs:  []uuid.UUID{s.ID},
		Items:    []update.Item{{Type: "plugin", Slug: "akismet", Version: "latest"}},
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("want 1 task, got %d", len(tasks))
	}

	_, finalTasks := waitRunCompleted(t, h, tenant, run.ID)
	tk := finalTasks[0]
	if tk.Status != update.TaskSucceeded {
		t.Fatalf("task status = %s, want succeeded (detail=%s err=%s)", tk.Status, tk.Detail, tk.Error)
	}
	if tk.ToVersion != "1.1.0" {
		t.Fatalf("to_version = %q, want 1.1.0", tk.ToVersion)
	}
	if atomic.LoadInt32(&fa.updateCalls) != 1 {
		t.Fatalf("update called %d times, want 1", fa.updateCalls)
	}
	if atomic.LoadInt32(&fa.rollbackCalls) != 0 {
		t.Fatalf("rollback should not be called on happy path")
	}
	if !fa.snapshotSeen {
		t.Fatal("worker must request a pre-update snapshot on a real update")
	}
	// The worker must bind the command JWT to this site (aud) and command (cmd).
	fa.mu.Lock()
	gotAud, gotCmd := fa.updateAud, fa.updateCmd
	fa.mu.Unlock()
	if gotAud != s.ID.String() {
		t.Fatalf("update JWT aud = %q, want site id %q", gotAud, s.ID.String())
	}
	if gotCmd != "update" {
		t.Fatalf("update JWT cmd = %q, want update", gotCmd)
	}
}

// TestUpdateAutoRollback: agent reports a successful update, but the post-update
// homepage probe returns 5xx → the worker issues rollback and the task is
// rolled_back.
func TestUpdateAutoRollback(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "upd-rollback")
	fa := newFakeAgent(t)
	fa.setHomepage(http.StatusInternalServerError, "<html>fatal error</html>")

	h := buildHarness(t, pool, newTestCommander(t), newTestProber(t))
	s := enrollFakeSite(t, pool, tenant, fa.url())
	fa.setExpectAud(s.ID.String())
	seedPendingPlugin(t, h, tenant, s, "broken-plugin", "1.0.0", "1.1.0")

	run, _, err := h.svc.CreateRun(context.Background(), update.CreateRunInput{
		TenantID: tenant,
		SiteIDs:  []uuid.UUID{s.ID},
		Items:    []update.Item{{Type: "plugin", Slug: "broken-plugin", Version: "latest"}},
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	_, finalTasks := waitRunCompleted(t, h, tenant, run.ID)
	tk := finalTasks[0]
	if tk.Status != update.TaskRolledBack {
		t.Fatalf("task status = %s, want rolled_back (detail=%s)", tk.Status, tk.Detail)
	}
	if atomic.LoadInt32(&fa.rollbackCalls) != 1 {
		t.Fatalf("rollback called %d times, want 1", fa.rollbackCalls)
	}
	// The rollback command JWT must also be bound to this site and command.
	fa.mu.Lock()
	rbAud, rbCmd := fa.rbAud, fa.rbCmd
	fa.mu.Unlock()
	if rbAud != s.ID.String() {
		t.Fatalf("rollback JWT aud = %q, want site id %q", rbAud, s.ID.String())
	}
	if rbCmd != "rollback" {
		t.Fatalf("rollback JWT cmd = %q, want rollback", rbCmd)
	}
}

// TestUpdateAutoRollback_CachedHomepageMaskedFatal_StillRollsBack reproduces
// THE BUG this phase fixes end-to-end, over a real agentcmd.Client and a real
// River-driven worker: a page cache serves a stale pre-update 200 for the
// public homepage (what the old code alone would have read as "healthy"),
// while the SAME broken plugin update has made PHP fatal on EVERY request,
// including the agent's own signed ping route, which a cache can never serve
// stale because it is a fresh, uncacheable, signed round trip. The task must
// still roll back, and it must do so WITHOUT ever consulting the (misleading)
// public homepage probe at all.
//
// The fake agent's ping response here is persistently 500 (it never clears),
// so this also doubles as the end-to-end pin for fix 1's "persistent" half: a
// rollback is reached only after the agent-first check has been retried
// across its full window, not on the first observation. A tiny probe-retry
// schedule is installed on the worker so the test does not actually wait out
// the production ~21s window.
func TestUpdateAutoRollback_CachedHomepageMaskedFatal_StillRollsBack(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "upd-cache-mask")
	fa := newFakeAgent(t)
	// The public homepage: a page cache serving the stale pre-update 200.
	fa.setHomepage(http.StatusOK, "<html>looks fine (stale cache)</html>")
	// The signed ping route: PHP is actually fatal-ing on every request, and
	// stays that way for the whole test (a persistent, not transient, fatal).
	fa.setPing(http.StatusInternalServerError, "")

	h := buildHarness(t, pool, newTestCommander(t), newTestProber(t))
	// Fix 1 gives the agent-first check the same retry discipline as the
	// public probe, reusing probeRetryDelays. Override it here with a tiny
	// schedule so this test proves the retry-then-persist behavior without
	// spending the real ~21s window.
	h.worker.SetProbeRetryDelays([]time.Duration{10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond})
	s := enrollFakeSite(t, pool, tenant, fa.url())
	fa.setExpectAud(s.ID.String())
	seedPendingPlugin(t, h, tenant, s, "broken-plugin", "1.0.0", "1.1.0")

	run, _, err := h.svc.CreateRun(context.Background(), update.CreateRunInput{
		TenantID: tenant,
		SiteIDs:  []uuid.UUID{s.ID},
		Items:    []update.Item{{Type: "plugin", Slug: "broken-plugin", Version: "latest"}},
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	start := time.Now()
	_, finalTasks := waitRunCompleted(t, h, tenant, run.ID)
	elapsed := time.Since(start)

	tk := finalTasks[0]
	if tk.Status != update.TaskRolledBack {
		t.Fatalf("task status = %s, want rolled_back (detail=%s); a cached-200 homepage must not mask a fatal backend", tk.Status, tk.Detail)
	}
	if atomic.LoadInt32(&fa.rollbackCalls) != 1 {
		t.Fatalf("rollback called %d times, want 1", fa.rollbackCalls)
	}
	if atomic.LoadInt32(&fa.homepageCalls) != 0 {
		t.Fatalf("the public homepage probe was called %d time(s); a PERSISTENT agent-confirmed fatal is conclusive and should have skipped it entirely", fa.homepageCalls)
	}
	// With the tiny test schedule installed above, the whole retry-then-persist
	// sequence should finish in well under a second; a much larger bound is
	// used here only to absorb CI scheduling jitter, not to allow the real
	// production window to run.
	if elapsed > 5*time.Second {
		t.Fatalf("run took %s to complete using the tiny test retry schedule; the persistent-agent-fatal path should not take anywhere near that long", elapsed)
	}
}

// TestUpdateNoRollback_TransientAgentPingRecovers is the end-to-end pin for
// fix 1's "transient" half, over a real agentcmd.Client and a real
// River-driven worker: the signed ping route returns a server error for the
// FIRST couple of calls, then recovers, exactly like the synchronous
// DB-migration-on-activation window GH #127 documents. The task must
// succeed, not roll back, and the agent-first check must have been retried
// (not just observed once) before it did.
func TestUpdateNoRollback_TransientAgentPingRecovers(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "upd-agent-transient")
	fa := newFakeAgent(t)
	fa.setHomepage(http.StatusOK, "<html>ok</html>")
	// The signed ping route fails twice, then clears.
	fa.setPingTransientFailure(2, http.StatusInternalServerError, "")

	h := buildHarness(t, pool, newTestCommander(t), newTestProber(t))
	h.worker.SetProbeRetryDelays([]time.Duration{10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond})
	s := enrollFakeSite(t, pool, tenant, fa.url())
	fa.setExpectAud(s.ID.String())
	seedPendingPlugin(t, h, tenant, s, "migrating-plugin", "1.9.9", "2.0.0")

	run, _, err := h.svc.CreateRun(context.Background(), update.CreateRunInput{
		TenantID: tenant,
		SiteIDs:  []uuid.UUID{s.ID},
		Items:    []update.Item{{Type: "plugin", Slug: "migrating-plugin", Version: "latest"}},
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	_, finalTasks := waitRunCompleted(t, h, tenant, run.ID)
	tk := finalTasks[0]
	if tk.Status != update.TaskSucceeded {
		t.Fatalf("task status = %s, want succeeded (detail=%s); a transient agent-side failure that clears within the retry window must not roll back", tk.Status, tk.Detail)
	}
	if atomic.LoadInt32(&fa.rollbackCalls) != 0 {
		t.Fatalf("rollback called %d times, want 0: the agent-first check recovered before the retry window was exhausted", fa.rollbackCalls)
	}
	if atomic.LoadInt32(&fa.pingCalls) < 3 {
		t.Fatalf("expected at least 3 ping calls (2 transient failures + 1 recovery), got %d", fa.pingCalls)
	}
}

// TestUpdateDryRunDoesNotMutate: a dry-run must call the agent with dry_run=true
// and never the rollback command; the task succeeds with preview info.
func TestUpdateDryRunDoesNotMutate(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "upd-dry")
	fa := newFakeAgent(t)
	fa.mu.Lock()
	fa.updateResp = agentcmd.UpdateResponse{OK: true, Results: []agentcmd.ItemResult{{
		Status: agentcmd.ItemWouldUpdate, FromVersion: "1.0.0", ToVersion: "1.2.0",
	}}}
	fa.mu.Unlock()

	h := buildHarness(t, pool, newTestCommander(t), newTestProber(t))
	s := enrollFakeSite(t, pool, tenant, fa.url())
	fa.setExpectAud(s.ID.String())
	seedPendingCore(t, h, tenant, s, "6.4", "6.5")

	run, _, err := h.svc.CreateRun(context.Background(), update.CreateRunInput{
		TenantID: tenant,
		SiteIDs:  []uuid.UUID{s.ID},
		Items:    []update.Item{{Type: "core", Version: "latest"}},
		DryRun:   true,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	_, finalTasks := waitRunCompleted(t, h, tenant, run.ID)
	tk := finalTasks[0]
	if tk.Status != update.TaskSucceeded {
		t.Fatalf("dry-run task status = %s, want succeeded", tk.Status)
	}
	if !fa.dryRunSeen {
		t.Fatal("agent must have been called with dry_run=true")
	}
	if fa.snapshotSeen {
		t.Fatal("dry-run must not request a snapshot")
	}
	if atomic.LoadInt32(&fa.rollbackCalls) != 0 {
		t.Fatal("dry-run must never call rollback")
	}
	if tk.TargetSlug != "core" {
		t.Fatalf("core target slug = %q, want core", tk.TargetSlug)
	}
}
