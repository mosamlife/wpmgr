package tests

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	dryRunSeen    bool
	snapshotSeen  bool

	// homepageStatus is returned for GET / (the health probe). 0 ⇒ 200.
	homepageStatus int
	homepageBody   string
	// updateResp/rollbackResp override the command responses.
	updateResp   agentcmd.UpdateResponse
	rollbackResp agentcmd.RollbackResponse
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
		fa.mu.Lock()
		fa.dryRunSeen = req.DryRun
		fa.snapshotSeen = req.Snapshot
		resp := fa.updateResp
		fa.mu.Unlock()
		// Require a Bearer token (the signed JWT) — the real agent rejects without it.
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		writeJSON(w, resp)
	})
	mux.HandleFunc("/wp-json/wpmgr/v1/command/rollback", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fa.rollbackCalls, 1)
		fa.mu.Lock()
		resp := fa.rollbackResp
		fa.mu.Unlock()
		writeJSON(w, resp)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
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
	sites, err := l.svc.List(ctx, site.ListInput{TenantID: tenantID, Tag: tag, Limit: 200})
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
		comps = append(comps, update.Component{Type: update.TargetPlugin, Slug: p.Slug, Version: p.Version})
	}
	for _, th := range themes {
		comps = append(comps, update.Component{Type: update.TargetTheme, Slug: th.Slug, Version: th.Version})
	}
	return update.SiteInfo{ID: s.ID, URL: s.URL, Name: s.Name, Enrolled: s.EnrolledAt != nil, Components: comps}
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
	worker := update.NewWorker(repo, lookup, commander, prober, hub, rec, nil, 5)
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
