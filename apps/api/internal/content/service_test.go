package content

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/httpclient"
)

// fakeRepo records what the service stores.
type fakeRepo struct {
	mu           sync.Mutex
	target       SiteTarget
	integrations []Integration
	replaced     [][]Row
	truncated    []bool
	deleteStale  []bool
	stored       []IntegrationRecord
	run          *Run
	checkedAt    []time.Time
	sweep        []SweepSite
}

func (f *fakeRepo) GetSiteTarget(context.Context, uuid.UUID, uuid.UUID) (SiteTarget, error) {
	return f.target, nil
}
func (f *fakeRepo) ListEnabledIntegrations(context.Context, uuid.UUID) ([]Integration, error) {
	return f.integrations, nil
}
func (f *fakeRepo) ReplaceInventory(_ context.Context, _, _ uuid.UUID, at time.Time, rows []Row, truncated bool, deleteStale bool) error {
	f.truncated = append(f.truncated, truncated)
	f.deleteStale = append(f.deleteStale, deleteStale)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replaced = append(f.replaced, rows)
	f.checkedAt = append(f.checkedAt, at)
	return nil
}
func (f *fakeRepo) ListInventory(context.Context, domain.Principal, uuid.UUID, int64, *string, int32) ([]InventoryRow, error) {
	return nil, nil
}
func (f *fakeRepo) FleetReport(context.Context, uuid.UUID) ([]FleetVerdictShare, []FleetBuilderShare, error) {
	return nil, nil, nil
}
func (f *fakeRepo) GetRun(context.Context, domain.Principal, uuid.UUID) (*Run, error) {
	return f.run, nil
}
func (f *fakeRepo) ListSweepSites(context.Context) ([]SweepSite, error) { return f.sweep, nil }
func (f *fakeRepo) AdminUpsertIntegration(_ context.Context, in AdminUpsertInput) (IntegrationRecord, error) {
	rec := IntegrationRecord{IntegrationID: in.IntegrationID, DisplayName: in.DisplayName, Enabled: in.Enabled, Status: in.Status,
		Descriptor: in.Descriptor, Abilities: in.Abilities, MinVersion: in.MinVersion, ThemeSlug: in.ThemeSlug}
	f.stored = []IntegrationRecord{rec}
	return rec, nil
}
func (f *fakeRepo) ListIntegrations(context.Context, uuid.UUID) ([]IntegrationRecord, error) {
	return f.stored, nil
}

// fakeAgent is an httptest server speaking the content_probe wire.
type fakeAgent struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []agentcmd.ContentProbeRequest
	paths  []string
	reply  func(req agentcmd.ContentProbeRequest) (int, any)
}

func newFakeAgent(t *testing.T, reply func(agentcmd.ContentProbeRequest) (int, any)) *fakeAgent {
	t.Helper()
	fa := &fakeAgent{reply: reply}
	fa.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req agentcmd.ContentProbeRequest
		_ = json.Unmarshal(raw, &req)
		fa.mu.Lock()
		fa.bodies = append(fa.bodies, req)
		fa.paths = append(fa.paths, r.URL.Path)
		fa.mu.Unlock()
		code, body := fa.reply(req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(fa.srv.Close)
	return fa
}

func (f *fakeAgent) calls() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.bodies) }

func realClient(t *testing.T) *agentcmd.Client {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := agentcmd.NewSigner(base64.StdEncoding.EncodeToString(priv))
	if err != nil {
		t.Fatal(err)
	}
	return agentcmd.NewClient(httpclient.New(httpclient.Config{AllowPrivateNetworks: true}), signer)
}

func rowsJSON(from, n int) []map[string]any {
	out := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, map[string]any{
			"post": from + i, "type": "page", "status": "publish", "verdict": "classic",
			"route": map[string]any{"number": 1, "reason": "content_column"}, "owner": nil, "title": fmt.Sprintf("Page %d", from+i),
		})
	}
	return out
}

func listReply(rows []map[string]any, next any) map[string]any {
	return map[string]any{"ok": true, "probe_version": 2, "mode": "list", "rows": rows, "next_offset": next, "wp_version": "6.6", "php_version": "8.2"}
}

func newSvc(t *testing.T, repo *fakeRepo, fa *fakeAgent) *Service {
	t.Helper()
	repo.target.URL = fa.srv.URL
	svc := NewService(repo, realClient(t), nil)
	svc.SetClock(func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) })
	return svc
}

func connected() SiteTarget {
	return SiteTarget{AgentVersion: "0.61.154", ConnectionState: "connected", Enrolled: true, Components: []byte(`{"plugins":[{"slug":"elementor/elementor.php","active":true}]}`)}
}

func TestRefresh_PagesThroughTheSiteAndReplacesInOneWrite(t *testing.T) {
	fa := newFakeAgent(t, func(req agentcmd.ContentProbeRequest) (int, any) {
		if req.List.Offset == 0 {
			return 200, listReply(rowsJSON(1, 200), 200)
		}
		if req.List.Offset != 200 {
			return 200, map[string]any{"ok": false, "code": "wrong_offset", "retryable": false}
		}
		return 200, listReply(rowsJSON(201, 3), nil)
	})
	repo := &fakeRepo{target: connected(), integrations: []Integration{{ID: "elementor", DisplayName: "Elementor", Descriptor: []byte(`{"plugin_dir":"elementor"}`)}}}
	svc := newSvc(t, repo, fa)

	res, err := svc.Refresh(context.Background(), uuid.New(), uuid.New(), false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stored != 203 || res.Truncated {
		t.Fatalf("result %+v", res)
	}
	if len(repo.replaced) != 1 || len(repo.replaced[0]) != 203 {
		t.Fatalf("replace calls = %d, want exactly one write of every row", len(repo.replaced))
	}
	if fa.calls() != 2 {
		t.Errorf("agent calls = %d, want 2", fa.calls())
	}
	first := fa.bodies[0]
	if fa.paths[0] != "/wp-json/wpmgr/v1/command/content_probe" {
		t.Errorf("path = %s", fa.paths[0])
	}
	if first.List.Limit != 200 || first.List.Status[0] != "publish" || len(first.List.Types) != 2 {
		t.Errorf("list request = %+v", first.List)
	}
	if len(first.Descriptors) != 0 {
		t.Errorf("a row with no detection data was sent as a descriptor: %d", len(first.Descriptors))
	}
	found := false
	for _, s := range first.Indicators.PluginSlugs {
		found = found || s == "elementor"
	}
	if !found {
		t.Errorf("indicators = %+v", first.Indicators)
	}
	if !repo.checkedAt[0].Equal(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("checked_at = %v", repo.checkedAt[0])
	}
}

func TestRefresh_BelowTheFloorIsAnOutcomeAndSendsNothing(t *testing.T) {
	fa := newFakeAgent(t, func(agentcmd.ContentProbeRequest) (int, any) { return 200, listReply(nil, nil) })
	repo := &fakeRepo{target: connected()}
	repo.target.AgentVersion = "0.61.153"
	svc := newSvc(t, repo, fa)

	_, err := svc.Refresh(context.Background(), uuid.New(), uuid.New(), false)
	if !errors.Is(err, ErrNotSendable) {
		t.Fatalf("err = %v, want ErrNotSendable", err)
	}
	if fa.calls() != 0 || len(repo.replaced) != 0 {
		t.Errorf("calls=%d replaced=%d; nothing may be sent or stored", fa.calls(), len(repo.replaced))
	}
}

func TestRefresh_ScheduledSkipsAPausedSiteButAnOperatorDoesNot(t *testing.T) {
	fa := newFakeAgent(t, func(agentcmd.ContentProbeRequest) (int, any) { return 200, listReply(rowsJSON(1, 1), nil) })
	repo := &fakeRepo{target: connected()}
	repo.target.Paused = true
	svc := newSvc(t, repo, fa)

	if _, err := svc.Refresh(context.Background(), uuid.New(), uuid.New(), true); !errors.Is(err, ErrNotSendable) {
		t.Fatalf("scheduled: err = %v", err)
	}
	if _, err := svc.Refresh(context.Background(), uuid.New(), uuid.New(), false); err != nil {
		t.Fatalf("operator refresh of a paused site: %v", err)
	}
}

func TestRefresh_BrokenReplyStoresNothing(t *testing.T) {
	fa := newFakeAgent(t, func(agentcmd.ContentProbeRequest) (int, any) {
		rows := rowsJSON(1, 2)
		rows[1]["type"] = "Not A Type"
		return 200, listReply(rows, nil)
	})
	repo := &fakeRepo{target: connected()}
	svc := newSvc(t, repo, fa)

	_, err := svc.Refresh(context.Background(), uuid.New(), uuid.New(), false)
	if !errors.Is(err, ErrInvalidProbeResponse) {
		t.Fatalf("err = %v", err)
	}
	if len(repo.replaced) != 0 {
		t.Error("a reply that failed validation replaced the stored inventory")
	}
}

func TestRefresh_AgentRefusalIsTypedAndStoresNothing(t *testing.T) {
	fa := newFakeAgent(t, func(agentcmd.ContentProbeRequest) (int, any) {
		return 200, map[string]any{"ok": false, "outcome": "failed", "code": "invalid_descriptor", "detail": "descriptor[0]: bad‮", "retryable": false}
	})
	repo := &fakeRepo{target: connected()}
	svc := newSvc(t, repo, fa)

	_, err := svc.Refresh(context.Background(), uuid.New(), uuid.New(), false)
	var ref *agentcmd.ContentProbeRefusal
	if !errors.As(err, &ref) || ref.Code != "invalid_descriptor" || ref.Retryable {
		t.Fatalf("err = %v", err)
	}
	if len(repo.replaced) != 0 {
		t.Error("a refusal replaced the stored inventory")
	}
}

func TestWorker_DecidesRetryFromTheOutcome(t *testing.T) {
	cancelled := func(err error) bool {
		var jc *rivertype.JobCancelError
		return errors.As(err, &jc)
	}
	run := func(reply func(agentcmd.ContentProbeRequest) (int, any), mutate func(*fakeRepo)) error {
		fa := newFakeAgent(t, reply)
		repo := &fakeRepo{target: connected()}
		if mutate != nil {
			mutate(repo)
		}
		svc := newSvc(t, repo, fa)
		w := NewRefreshWorker(svc, nil)
		return w.Work(context.Background(), &river.Job[RefreshArgs]{Args: RefreshArgs{TenantID: uuid.New(), SiteID: uuid.New()}})
	}

	if err := run(func(agentcmd.ContentProbeRequest) (int, any) { return 200, listReply(rowsJSON(1, 1), nil) }, nil); err != nil {
		t.Errorf("success: %v", err)
	}
	if err := run(func(agentcmd.ContentProbeRequest) (int, any) { return 200, listReply(nil, nil) }, func(r *fakeRepo) { r.target.AgentVersion = "0.61.153" }); err != nil {
		t.Errorf("below the floor must complete quietly, got %v", err)
	}
	if err := run(func(agentcmd.ContentProbeRequest) (int, any) {
		return 200, map[string]any{"ok": false, "code": "invalid_params", "retryable": false}
	}, nil); !cancelled(err) {
		t.Errorf("a non-retryable refusal must cancel the job, got %v", err)
	}
	if err := run(func(agentcmd.ContentProbeRequest) (int, any) {
		return 200, map[string]any{"ok": false, "code": "internal", "retryable": true}
	}, nil); err == nil || cancelled(err) {
		t.Errorf("a retryable refusal must be retried, got %v", err)
	}
	if err := run(func(agentcmd.ContentProbeRequest) (int, any) { return 500, map[string]any{"code": "boom"} }, nil); err == nil || cancelled(err) {
		t.Errorf("a transport-level failure must be retried, got %v", err)
	}
	if err := run(func(agentcmd.ContentProbeRequest) (int, any) {
		rows := rowsJSON(1, 1)
		rows[0]["post"] = 0
		return 200, listReply(rows, nil)
	}, nil); !cancelled(err) {
		t.Errorf("a broken reply must cancel the job, got %v", err)
	}
}

func TestRefreshArgs_BoundedAndDeduplicated(t *testing.T) {
	o := RefreshArgs{}.InsertOpts()
	if o.MaxAttempts != 3 {
		t.Errorf("max attempts = %d", o.MaxAttempts)
	}
	if !o.UniqueOpts.ByArgs || o.UniqueOpts.ByPeriod != RefreshRateWindow {
		t.Errorf("unique opts = %+v", o.UniqueOpts)
	}
	if RefreshTimeout > 15*time.Minute {
		t.Errorf("job timeout %v is not bounded", RefreshTimeout)
	}
}

type fakeEnq struct {
	mu      sync.Mutex
	args    []RefreshArgs
	at      []time.Time
	dupOf   map[uuid.UUID]bool
	failFor uuid.UUID
}

func (f *fakeEnq) EnqueueRefresh(_ context.Context, a RefreshArgs, at time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a.SiteID == f.failFor {
		return false, errors.New("insert failed")
	}
	f.args = append(f.args, a)
	f.at = append(f.at, at)
	return !f.dupOf[a.SiteID], nil
}

func TestSweep_EnqueuesEveryConnectedSiteJitteredAndSurvivesOneFailure(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	repo := &fakeRepo{sweep: []SweepSite{{TenantID: uuid.New(), SiteID: a}, {TenantID: uuid.New(), SiteID: b}, {TenantID: uuid.New(), SiteID: c}}}
	enq := &fakeEnq{failFor: b}
	w := NewSweepWorker(NewService(repo, nil, nil), nil)
	w.SetEnqueuer(enq)

	before := time.Now()
	if err := w.Work(context.Background(), &river.Job[SweepArgs]{}); err != nil {
		t.Fatal(err)
	}
	if len(enq.args) != 2 {
		t.Fatalf("enqueued %d, want 2 (one failed, the rest continue)", len(enq.args))
	}
	for i, at := range enq.at {
		if !enq.args[i].Scheduled {
			t.Error("a sweep job must be marked scheduled")
		}
		if at.Before(before) || at.After(before.Add(sweepSpread+time.Minute)) {
			t.Errorf("schedule time %v outside the spread window", at)
		}
	}
}

func TestSweep_WithoutAnEnqueuerDoesNothing(t *testing.T) {
	w := NewSweepWorker(NewService(&fakeRepo{sweep: []SweepSite{{}}}, nil, nil), nil)
	if err := w.Work(context.Background(), &river.Job[SweepArgs]{}); err != nil {
		t.Fatal(err)
	}
}

func TestRequestRefresh_RateLimitsAndNeverQueuesWithoutAnEnqueuer(t *testing.T) {
	svc := NewService(&fakeRepo{}, realClient(t), nil)
	site := uuid.New()
	enq := &fakeEnq{dupOf: map[uuid.UUID]bool{site: true}}

	err := svc.RequestRefresh(context.Background(), enq, uuid.New(), site)
	if de, ok := domain.AsDomain(err); !ok || de.Kind != domain.KindRateLimited {
		t.Fatalf("a duplicate inside the window must be rate limited, got %v", err)
	}
	if err := svc.RequestRefresh(context.Background(), enq, uuid.New(), uuid.New()); err != nil {
		t.Fatalf("first request: %v", err)
	}
	if err := svc.RequestRefresh(context.Background(), nil, uuid.New(), uuid.New()); err == nil {
		t.Fatal("no enqueuer must be an error, not a silent success")
	}
}

func TestEntrySHA256_IsCanonicalAndSensitive(t *testing.T) {
	base := AdminUpsertInput{
		IntegrationID: "elementor", DisplayName: "Elementor", Enabled: true, Status: "detect_only",
		Descriptor: []byte(`{"b": 1, "a": {"y": 2, "x": [1, 2]}}`),
	}
	reordered := base
	reordered.Descriptor = []byte(`{"a":{"x":[1,2],"y":2},"b":1}`)
	h1, err := EntrySHA256(base)
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := EntrySHA256(reordered)
	if h1 != h2 || len(h1) != 64 {
		t.Errorf("whitespace and key order changed the hash: %s vs %s", h1, h2)
	}
	changed := base
	changed.Enabled = false
	if h3, _ := EntrySHA256(changed); h3 == h1 {
		t.Error("a changed field did not change the hash")
	}
	other := base
	other.Descriptor = []byte(`{"b": 2, "a": {"y": 2, "x": [1, 2]}}`)
	if h4, _ := EntrySHA256(other); h4 == h1 {
		t.Error("a changed descriptor did not change the hash")
	}
}

func TestUpsertIntegration_Validation(t *testing.T) {
	svc := NewService(&fakeRepo{}, nil, nil)
	ok := AdminUpsertInput{ActorUserID: uuid.New(), IntegrationID: "bricks", DisplayName: "Bricks", Status: "detect_only", Descriptor: []byte(`{}`)}
	if _, err := svc.UpsertIntegration(context.Background(), ok); err != nil {
		t.Fatalf("good input: %v", err)
	}
	bad := map[string]func(*AdminUpsertInput){
		"id shape":       func(i *AdminUpsertInput) { i.IntegrationID = "Bad_ID" },
		"blank name":     func(i *AdminUpsertInput) { i.DisplayName = "  ‮ " },
		"admitted":       func(i *AdminUpsertInput) { i.Status = "admitted" },
		"descriptor arr": func(i *AdminUpsertInput) { i.Descriptor = []byte(`[]`) },
		"unknown key":    func(i *AdminUpsertInput) { i.Descriptor = []byte(`{"callback":"evil"}`) },
	}
	for name, mutate := range bad {
		in := ok
		mutate(&in)
		if _, err := svc.UpsertIntegration(context.Background(), in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRefresh_DuplicateBoundaryRowIsStoredOnce(t *testing.T) {
	fa := newFakeAgent(t, func(req agentcmd.ContentProbeRequest) (int, any) {
		if req.List.Offset == 0 {
			return 200, listReply(rowsJSON(1, 200), 200)
		}
		return 200, listReply(rowsJSON(200, 3), nil) // post 200 repeats
	})
	repo := &fakeRepo{target: connected()}
	svc := newSvc(t, repo, fa)
	res, err := svc.Refresh(context.Background(), uuid.New(), uuid.New(), false)
	if err != nil || res.Stored != 202 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	seen := map[int64]bool{}
	for _, r := range repo.replaced[0] {
		if seen[r.PostID] {
			t.Fatalf("post %d is in the upsert twice", r.PostID)
		}
		seen[r.PostID] = true
	}
}

func TestRefresh_PageCapMarksTruncated(t *testing.T) {
	fa := newFakeAgent(t, func(req agentcmd.ContentProbeRequest) (int, any) {
		return 200, listReply(rowsJSON(req.List.Offset+1, 200), req.List.Offset+200)
	})
	repo := &fakeRepo{target: connected()}
	svc := newSvc(t, repo, fa)
	site := uuid.New()
	res, err := svc.Refresh(context.Background(), uuid.New(), site, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || res.Stored != maxPages*200 || fa.calls() != maxPages {
		t.Fatalf("res=%+v calls=%d", res, fa.calls())
	}
	_ = site
	if len(repo.truncated) != 1 || !repo.truncated[0] {
		t.Errorf("truncated was not persisted with the run: %v", repo.truncated)
	}
}

func TestRefreshTimeout_CoversEveryPageOfASlowSite(t *testing.T) {
	if RefreshTimeout < maxPages*callTimeout {
		t.Fatalf("job timeout %v cannot cover %d calls of %v", RefreshTimeout, maxPages, callTimeout)
	}
}

func TestRefresh_UntruncatedRunIsPersistedAsSuch(t *testing.T) {
	fa := newFakeAgent(t, func(agentcmd.ContentProbeRequest) (int, any) { return 200, listReply(rowsJSON(1, 3), nil) })
	repo := &fakeRepo{target: connected()}
	svc := newSvc(t, repo, fa)
	if _, err := svc.Refresh(context.Background(), uuid.New(), uuid.New(), false); err != nil {
		t.Fatal(err)
	}
	if len(repo.truncated) != 1 || repo.truncated[0] {
		t.Errorf("truncated = %v", repo.truncated)
	}
}

func TestRefresh_SendsDescriptorsWithIdentityFromTheColumns(t *testing.T) {
	fa := newFakeAgent(t, func(agentcmd.ContentProbeRequest) (int, any) { return 200, listReply(nil, nil) })
	repo := &fakeRepo{target: connected(), integrations: []Integration{{
		ID: "elementor", Status: "detect_only", Enabled: true,
		Descriptor: []byte(`{"mode_flag":{"meta_key":"_e","on_values":["1"]},"payload_keys":["_d"]}`),
	}}}
	svc := newSvc(t, repo, fa)
	if _, err := svc.Refresh(context.Background(), uuid.New(), uuid.New(), false); err != nil {
		t.Fatal(err)
	}
	if len(fa.bodies[0].Descriptors) != 1 {
		t.Fatalf("descriptors = %d", len(fa.bodies[0].Descriptors))
	}
	var d map[string]any
	if err := json.Unmarshal(fa.bodies[0].Descriptors[0], &d); err != nil {
		t.Fatal(err)
	}
	if d["integration_id"] != "elementor" || d["status"] != "detect_only" || d["enabled"] != true {
		t.Errorf("descriptor = %v", d)
	}
}

func TestRequestRefresh_NoProbeClientIsUnavailableNotQueued(t *testing.T) {
	svc := NewService(&fakeRepo{}, nil, nil)
	enq := &fakeEnq{}
	err := svc.RequestRefresh(context.Background(), enq, uuid.New(), uuid.New())
	if de, ok := domain.AsDomain(err); !ok || de.Kind != domain.KindUnavailable && de.Kind != domain.KindServiceUnavailable {
		t.Fatalf("err = %v, want unavailable", err)
	}
	if len(enq.args) != 0 {
		t.Error("a job was queued with no probe client")
	}
}

func TestRefreshArgs_UniqueKeyIsTheSiteOnly(t *testing.T) {
	f, ok := reflect.TypeOf(RefreshArgs{}).FieldByName("Scheduled")
	if !ok || f.Tag.Get("river") == "unique" {
		t.Fatal("Scheduled must not be part of the unique key")
	}
	s, _ := reflect.TypeOf(RefreshArgs{}).FieldByName("SiteID")
	if s.Tag.Get("river") != "unique" {
		t.Fatal("SiteID must be the unique key")
	}
	if _, has := reflect.TypeOf(RefreshArgs{}).FieldByName("TenantID"); !has || reflect.TypeOf(RefreshArgs{}).Field(0).Tag.Get("river") == "unique" {
		t.Fatal("TenantID must not be in the unique key")
	}
}

func TestRefresh_UnknownRowKeepsEarlierRowsAndMarksRunIncomplete(t *testing.T) {
	fa := newFakeAgent(t, func(agentcmd.ContentProbeRequest) (int, any) {
		rows := rowsJSON(1, 3)
		rows[1]["verdict"] = "a_future_verdict"
		return 200, listReply(rows, nil)
	})
	repo := &fakeRepo{target: connected()}
	svc := newSvc(t, repo, fa)
	if _, err := svc.Refresh(context.Background(), uuid.New(), uuid.New(), false); err != nil {
		t.Fatal(err)
	}
	if len(repo.deleteStale) != 1 || repo.deleteStale[0] {
		t.Errorf("stale rows were deleted despite an unknown row: %v", repo.deleteStale)
	}
	if !repo.truncated[0] {
		t.Error("the run was not marked incomplete")
	}
	if len(repo.replaced[0]) != 2 {
		t.Errorf("stored %d rows, want the 2 known", len(repo.replaced[0]))
	}
}

func TestRefresh_CleanRunDeletesStale(t *testing.T) {
	fa := newFakeAgent(t, func(agentcmd.ContentProbeRequest) (int, any) { return 200, listReply(rowsJSON(1, 2), nil) })
	repo := &fakeRepo{target: connected()}
	svc := newSvc(t, repo, fa)
	if _, err := svc.Refresh(context.Background(), uuid.New(), uuid.New(), false); err != nil {
		t.Fatal(err)
	}
	if !repo.deleteStale[0] || repo.truncated[0] {
		t.Errorf("deleteStale=%v truncated=%v", repo.deleteStale, repo.truncated)
	}
}

func TestRefresh_NonAdvancingNextOffsetFailsAndStoresNothing(t *testing.T) {
	fa := newFakeAgent(t, func(req agentcmd.ContentProbeRequest) (int, any) {
		if req.List.Offset == 0 {
			return 200, listReply(rowsJSON(1, 200), 200)
		}
		return 200, listReply(rowsJSON(201, 200), 200) // does not advance
	})
	repo := &fakeRepo{target: connected()}
	svc := newSvc(t, repo, fa)
	_, err := svc.Refresh(context.Background(), uuid.New(), uuid.New(), false)
	if !errors.Is(err, ErrInvalidProbeResponse) {
		t.Fatalf("err = %v", err)
	}
	if len(repo.replaced) != 0 {
		t.Error("inventory replaced after a failed probe")
	}
}

func TestUpsertIntegration_OmittedFieldsKeepStoredValues(t *testing.T) {
	repo := &fakeRepo{}
	svc := NewService(repo, nil, nil)
	ver := "1.2.3"
	theme := "bricks"
	full := AdminUpsertInput{
		ActorUserID: uuid.New(), IntegrationID: "bricks", DisplayName: "Bricks", Enabled: true, Status: "detect_only",
		Descriptor: []byte(`{"plugin_dir":"bricks-plugin"}`), Abilities: []byte(`{"a":1}`), MinVersion: &ver, ThemeSlug: &theme,
	}
	if _, err := svc.UpsertIntegration(context.Background(), full); err != nil {
		t.Fatal(err)
	}
	// Kill switch: only display_name, enabled and status are sent.
	off := AdminUpsertInput{
		ActorUserID: full.ActorUserID, IntegrationID: "bricks", DisplayName: "Bricks", Enabled: false, Status: "detect_only",
		Present: map[string]bool{},
	}
	rec, err := svc.UpsertIntegration(context.Background(), off)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Enabled || string(rec.Descriptor) != `{"plugin_dir":"bricks-plugin"}` || string(rec.Abilities) != `{"a":1}` ||
		rec.MinVersion == nil || *rec.MinVersion != "1.2.3" || rec.ThemeSlug == nil || *rec.ThemeSlug != "bricks" {
		t.Fatalf("disable erased data: %+v", rec)
	}
	on := off
	on.Enabled = true
	rec, err = svc.UpsertIntegration(context.Background(), on)
	if err != nil || !rec.Enabled || string(rec.Descriptor) != `{"plugin_dir":"bricks-plugin"}` {
		t.Fatalf("re-enable: %+v %v", rec, err)
	}
	// An explicit null clears abilities.
	clr := on
	clr.Present = map[string]bool{"abilities": true}
	clr.Abilities = []byte(`null`)
	rec, _ = svc.UpsertIntegration(context.Background(), clr)
	if len(rec.Abilities) != 0 {
		t.Errorf("explicit null did not clear abilities: %s", rec.Abilities)
	}
}
