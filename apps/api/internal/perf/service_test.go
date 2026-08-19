package perf

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/dbclean"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

type fakeRepo struct {
	mu          sync.Mutex
	config      Config
	configFound bool
	ciphertext  []byte
	provider    string
	purges      []RecordPurgeInput
	upserts     []UpsertConfigInput
	markedKinds []string // kinds passed to MarkCachePurged
	// GH #174 — ack-based beacon-key reconcile stubs.
	beaconAckedCalls []bool // the `present` arg of each UpdateBeaconKeyAcked call
	beaconAckedErr   error
	// GH #197 — fleet DB health rows returned by GetFleetDbHealth.
	fleetDbHealthRows []FleetSiteDbSummary
}

func (r *fakeRepo) GetConfig(_ context.Context, tenantID, siteID uuid.UUID) (Config, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.configFound {
		return Config{}, ErrNotFound
	}
	c := r.config
	c.TenantID, c.SiteID = tenantID, siteID
	return c, nil
}

func (r *fakeRepo) UpsertConfig(_ context.Context, in UpsertConfigInput) (Config, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.upserts = append(r.upserts, in)
	cfg := in.Config
	// Mirror the real UpsertPerfConfig SQL: beacon_key_hash / beacon_key_acked_*
	// are agent-owned columns NOT included in the operator upsert's SET list —
	// they pass through unchanged from whatever was already stored (GH #174).
	cfg.BeaconKeySet = r.config.BeaconKeySet
	cfg.BeaconKeyAckedPresent = r.config.BeaconKeyAckedPresent
	cfg.BeaconKeyAckedAt = r.config.BeaconKeyAckedAt
	r.config = cfg
	r.configFound = true
	r.ciphertext = in.CDNCredentialsEncrypted
	return cfg, nil
}

func (r *fakeRepo) GetCDNCredentialsCiphertext(_ context.Context, _, _ uuid.UUID) ([]byte, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ciphertext, r.provider, nil
}

func (r *fakeRepo) UpdateInstallState(context.Context, uuid.UUID, string, bool, bool, bool) error {
	return nil
}

func (r *fakeRepo) GetCacheStats(_ context.Context, tenantID, siteID uuid.UUID) (CacheStats, error) {
	return CacheStats{}, ErrNotFound
}

func (r *fakeRepo) UpsertCacheStats(_ context.Context, s CacheStats) (CacheStats, error) {
	return s, nil
}

func (r *fakeRepo) RecordPurge(_ context.Context, in RecordPurgeInput) (PurgeAuditEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.purges = append(r.purges, in)
	return PurgeAuditEntry{ID: uuid.New(), TenantID: in.TenantID, SiteID: in.SiteID, Kind: string(in.Kind), TargetURLs: in.TargetURLs, URLsCount: len(in.TargetURLs)}, nil
}

func (r *fakeRepo) MarkCachePurged(_ context.Context, _, _ uuid.UUID, kind string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.markedKinds = append(r.markedKinds, kind)
	return nil
}

func (r *fakeRepo) ListPurgeAudit(context.Context, uuid.UUID, uuid.UUID, int32, int32) ([]PurgeAuditEntry, error) {
	return nil, nil
}

func (r *fakeRepo) GetDueDBCleanSites(_ context.Context, _ int) ([]DueDBCleanSite, error) {
	return nil, nil
}

func (r *fakeRepo) UpdateNextDBCleanAt(_ context.Context, _ uuid.UUID, _ time.Time) error {
	return nil
}

// GH #493 pause stubs. The default is "nothing is paused", so every existing
// test keeps its previous behaviour; monitoring_pause_test.go overrides them.
func (r *fakeRepo) PausedSiteIDs(_ context.Context, _ []uuid.UUID) (map[uuid.UUID]bool, error) {
	return map[uuid.UUID]bool{}, nil
}

func (r *fakeRepo) IsMonitoringPaused(_ context.Context, _ uuid.UUID) (bool, error) {
	return false, nil
}

// M39 watchdog + db_scan stubs — no-op for unit tests.
func (r *fakeRepo) SetActiveDBCleanJob(_ context.Context, _ uuid.UUID, _ string, _ time.Time) error {
	return nil
}
func (r *fakeRepo) ClearActiveDBCleanJob(_ context.Context, _ uuid.UUID) error { return nil }
func (r *fakeRepo) SetActiveDBScanJob(_ context.Context, _ uuid.UUID, _ string, _ time.Time) error {
	return nil
}
func (r *fakeRepo) ClearActiveDBScanJob(_ context.Context, _ uuid.UUID) error { return nil }
func (r *fakeRepo) GetActiveDBScanState(_ context.Context, _, _ uuid.UUID) (ActiveDBScanState, error) {
	return ActiveDBScanState{}, nil
}
func (r *fakeRepo) GetActiveDBCleanState(_ context.Context, _, _ uuid.UUID) (ActiveDBCleanState, error) {
	return ActiveDBCleanState{}, nil
}
func (r *fakeRepo) GetStalledDBCleanJobs(_ context.Context, _ time.Duration) ([]StalledDBCleanJob, error) {
	return nil, nil
}
func (r *fakeRepo) GetStalledDBScanJobs(_ context.Context, _ time.Duration) ([]StalledDBScanJob, error) {
	return nil, nil
}
func (r *fakeRepo) UpsertDBScanResult(_ context.Context, _ DBScanResultInput) error { return nil }
func (r *fakeRepo) GetDBScanResult(_ context.Context, _, _ uuid.UUID) (DBScanResult, error) {
	return DBScanResult{}, ErrNotFound
}
func (r *fakeRepo) UpsertDBCleanResult(_ context.Context, _ DBCleanResultInput) error { return nil }
func (r *fakeRepo) GetDBCleanResult(_ context.Context, _, _ uuid.UUID) (DBCleanResult, error) {
	return DBCleanResult{}, ErrNotFound
}

// M42 — DB-size history stubs.
func (r *fakeRepo) GetDBSizeHistory(_ context.Context, _, _ uuid.UUID, _ time.Time) ([]DbSizeTrendPoint, error) {
	return nil, nil
}
func (r *fakeRepo) PruneDBSizeHistory(_ context.Context, _ time.Duration) (int64, error) {
	return 0, nil
}

// M52 / #162 — cache hit-ratio history stubs.
func (r *fakeRepo) InsertCacheHitRatioHistoryTx(_ context.Context, _, _ uuid.UUID, _, _ int64, _ float64, _ time.Time) error {
	return nil
}
func (r *fakeRepo) GetCacheHitRatioHistory(_ context.Context, _, _ uuid.UUID, _ time.Time) ([]CacheHitRatioPoint, error) {
	return nil, nil
}
func (r *fakeRepo) PruneCacheHitRatioHistory(_ context.Context, _ time.Duration) (int64, error) {
	return 0, nil
}

// P3.7 — fleet DB health stub.
func (r *fakeRepo) GetFleetDbHealth(_ context.Context, _ uuid.UUID, _ time.Time) ([]FleetSiteDbSummary, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fleetDbHealthRows, nil
}

// P3.8 — orphan-delete watchdog stubs.
func (r *fakeRepo) SetActiveDBOrphanDeleteJob(_ context.Context, _ uuid.UUID, _ string, _ time.Time) error {
	return nil
}
func (r *fakeRepo) ClearActiveDBOrphanDeleteJob(_ context.Context, _ uuid.UUID) error { return nil }
func (r *fakeRepo) GetStalledDBOrphanDeleteJobs(_ context.Context, _ time.Duration) ([]StalledDBOrphanDeleteJob, error) {
	return nil, nil
}

// M53/M67 — agent-reported WooCommerce theme probe result (tri-state after M67).
func (r *fakeRepo) UpdateWooFragmentsSupported(_ context.Context, _ uuid.UUID, _ bool) (int64, error) {
	return 1, nil
}

// GH #174 — ack-based beacon-key presence signal. needsReconcile mirrors the
// real repo's semantics: rum_enabled && beacon_key_hash set && !present,
// evaluated against the CURRENT stored config (this call never mutates
// RumEnabled/BeaconKeySet — only the real acked_present/acked_at columns).
func (r *fakeRepo) UpdateBeaconKeyAcked(_ context.Context, _ uuid.UUID, present bool) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.beaconAckedCalls = append(r.beaconAckedCalls, present)
	if r.beaconAckedErr != nil {
		return false, r.beaconAckedErr
	}
	return r.config.RumEnabled && r.config.BeaconKeySet && !present, nil
}

type fakeEvents struct {
	mu     sync.Mutex
	events []site.ConnectionEvent
}

func (e *fakeEvents) Publish(_ context.Context, ev site.ConnectionEvent) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, ev)
	return nil
}

func (e *fakeEvents) types() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.events))
	for i, ev := range e.events {
		out[i] = ev.Type
	}
	return out
}

type fakeAgent struct {
	mu              sync.Mutex
	purgeCalls      []agentcmd.CachePurgeRequest
	purgeResult     agentcmd.CachePurgeResult
	purgeErr        error
	lastSnapshotReq agentcmd.DbSnapshotRequest
	snapshotErr     error
}

func (a *fakeAgent) SyncPerfConfig(context.Context, uuid.UUID, string, agentcmd.PerfConfigRequest) (agentcmd.PerfConfigResult, error) {
	return agentcmd.PerfConfigResult{OK: true}, nil
}
func (a *fakeAgent) CacheEnable(context.Context, uuid.UUID, string, agentcmd.CacheEnableRequest) (agentcmd.CacheEnableResult, error) {
	return agentcmd.CacheEnableResult{OK: true, Detail: "enabled"}, nil
}
func (a *fakeAgent) CacheDisable(context.Context, uuid.UUID, string, agentcmd.CacheDisableRequest) (agentcmd.CacheDisableResult, error) {
	return agentcmd.CacheDisableResult{OK: true, Detail: "disabled"}, nil
}
func (a *fakeAgent) CachePurge(_ context.Context, _ uuid.UUID, _ string, req agentcmd.CachePurgeRequest) (agentcmd.CachePurgeResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.purgeCalls = append(a.purgeCalls, req)
	return a.purgeResult, a.purgeErr
}
func (a *fakeAgent) CachePreload(context.Context, uuid.UUID, string, agentcmd.CachePreloadRequest) (agentcmd.CachePreloadResult, error) {
	return agentcmd.CachePreloadResult{OK: true, Detail: "preload"}, nil
}
func (a *fakeAgent) RucssCompute(context.Context, uuid.UUID, string, agentcmd.RucssComputeRequest) (agentcmd.RucssComputeResult, error) {
	return agentcmd.RucssComputeResult{OK: true, Detail: "rucss compute queued", Queued: 1}, nil
}
func (a *fakeAgent) DBClean(_ context.Context, _ uuid.UUID, _ string, req agentcmd.DBCleanRequest) (agentcmd.DBCleanResult, error) {
	return agentcmd.DBCleanResult{OK: true, JobID: req.JobID}, nil
}
func (a *fakeAgent) DBScan(_ context.Context, _ uuid.UUID, _ string, req agentcmd.DBScanRequest) (agentcmd.DBScanResult, error) {
	return agentcmd.DBScanResult{
		OK:    true,
		JobID: req.JobID,
		Categories: map[string]agentcmd.DBScanCategoryResult{
			"revisions": {Count: 10, Bytes: 0},
		},
		DBSizeBytes: 1024,
		TableCount:  5,
		ScannedAt:   1748994000,
	}, nil
}
func (a *fakeAgent) DBTableAction(_ context.Context, _ uuid.UUID, _ string, req agentcmd.DBTableActionRequest) (agentcmd.DBTableActionResult, error) {
	results := make([]agentcmd.DBTableActionTableResult, 0, len(req.Tables))
	for _, t := range req.Tables {
		results = append(results, agentcmd.DBTableActionTableResult{Table: t, Status: "done"})
	}
	return agentcmd.DBTableActionResult{OK: true, JobID: req.JobID, Action: req.Action, Results: results}, nil
}

func (a *fakeAgent) DBOrphanDelete(_ context.Context, _ uuid.UUID, _ string, req agentcmd.DBOrphanDeleteRequest) (agentcmd.DBOrphanDeleteResult, error) {
	return agentcmd.DBOrphanDeleteResult{OK: true, JobID: req.JobID}, nil
}
func (a *fakeAgent) SearchReplace(_ context.Context, _ uuid.UUID, _ string, req agentcmd.SearchReplaceRequest) (agentcmd.SearchReplaceResult, error) {
	matched := 5
	changed := matched
	if req.DryRun {
		changed = 0
	}
	return agentcmd.SearchReplaceResult{OK: true, JobID: req.JobID, TablesScanned: 10, RowsMatched: matched, RowsChanged: changed}, nil
}

func (a *fakeAgent) DbSnapshot(_ context.Context, _ uuid.UUID, _ string, req agentcmd.DbSnapshotRequest) (agentcmd.DbSnapshotResult, error) {
	a.mu.Lock()
	a.lastSnapshotReq = req
	snapshotErr := a.snapshotErr
	a.mu.Unlock()
	if snapshotErr != nil {
		return agentcmd.DbSnapshotResult{}, snapshotErr
	}
	switch req.Action {
	case "list":
		return agentcmd.DbSnapshotResult{OK: true, Snapshots: []agentcmd.DbSnapshotEntry{}}, nil
	case "create":
		return agentcmd.DbSnapshotResult{OK: true, Snapshot: &agentcmd.DbSnapshotEntry{ID: "snap_aabbccddeeff001122334455", Label: req.Label}}, nil
	case "revert":
		return agentcmd.DbSnapshotResult{OK: true, Detail: "reverted", SafetyID: "snap_112233445566778899aabbcc"}, nil
	case "delete":
		return agentcmd.DbSnapshotResult{OK: true, Detail: "deleted"}, nil
	default:
		return agentcmd.DbSnapshotResult{OK: false, Detail: "unknown action"}, nil
	}
}

func (a *fakeAgent) MediaClean(_ context.Context, _ uuid.UUID, _ string, req agentcmd.MediaCleanRequest) (agentcmd.MediaCleanResult, error) {
	switch req.Action {
	case "scan":
		return agentcmd.MediaCleanResult{
			OK:         true,
			Total:      0,
			Candidates: []agentcmd.MediaCleanCandidate{},
			HasMore:    false,
		}, nil
	case "list":
		return agentcmd.MediaCleanResult{
			OK:        true,
			Manifests: []agentcmd.MediaCleanManifest{},
		}, nil
	case "isolate":
		return agentcmd.MediaCleanResult{
			OK:         true,
			JobID:      req.JobID,
			Moved:      len(req.AttachmentIDs),
			ManifestID: "test-manifest-id",
		}, nil
	case "restore":
		return agentcmd.MediaCleanResult{
			OK:       true,
			JobID:    req.JobID,
			Restored: len(req.QuarantineIDs),
		}, nil
	case "delete":
		return agentcmd.MediaCleanResult{
			OK:      true,
			JobID:   req.JobID,
			Deleted: len(req.QuarantineIDs),
		}, nil
	default:
		return agentcmd.MediaCleanResult{OK: false, Detail: "unknown action"}, nil
	}
}

type fakeSites struct{ url string }

func (s *fakeSites) GetSiteURL(context.Context, uuid.UUID, uuid.UUID) (string, error) {
	return s.url, nil
}
func (s *fakeSites) ListSiteIDs(_ context.Context, _ uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}
func (s *fakeSites) ListSitesMeta(_ context.Context, _ uuid.UUID) ([]SiteMeta, error) {
	return nil, nil
}

// ---------------------------------------------------------------------------
// config validation
// ---------------------------------------------------------------------------

func TestValidateConfig(t *testing.T) {
	svc := NewService(&fakeRepo{}, nil, nil, nil)

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string // domain code, "" = ok
	}{
		{
			name:   "defaults applied for empty intervals/methods",
			mutate: func(c *Config) {},
		},
		{
			name:    "invalid cache refresh interval",
			mutate:  func(c *Config) { c.CacheRefreshInterval = "3seconds" },
			wantErr: "invalid_cache_refresh_interval",
		},
		{
			name:    "invalid js delay method",
			mutate:  func(c *Config) { c.JSDelayMethod = "lazy" },
			wantErr: "invalid_js_delay_method",
		},
		{
			name:    "invalid db clean interval",
			mutate:  func(c *Config) { c.DBAutoCleanInterval = "hourly" },
			wantErr: "invalid_db_clean_interval",
		},
		{
			name:    "invalid cdn file types",
			mutate:  func(c *Config) { c.CDNFileTypes = "fonts" },
			wantErr: "invalid_cdn_file_types",
		},
		{
			name:    "cdn enabled without url",
			mutate:  func(c *Config) { c.CDNEnabled = true; c.CDNURL = "" },
			wantErr: "invalid_cdn_url",
		},
		{
			name:    "cdn enabled with bad url scheme",
			mutate:  func(c *Config) { c.CDNEnabled = true; c.CDNURL = "ftp://x" },
			wantErr: "invalid_cdn_url",
		},
		{
			name:    "cdn enabled with valid url",
			mutate:  func(c *Config) { c.CDNEnabled = true; c.CDNURL = "https://cdn.example.com" },
			wantErr: "",
		},
		{
			name:    "invalid cdn provider",
			mutate:  func(c *Config) { c.CDNProvider = "fastly" },
			wantErr: "invalid_cdn_provider",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{}
			tc.mutate(&cfg)
			err := svc.validateConfig(&cfg)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected nil error, got %v", err)
				}
				// defaults applied
				if cfg.CacheRefreshInterval == "" || cfg.JSDelayMethod == "" || cfg.DBAutoCleanInterval == "" || cfg.CDNFileTypes == "" {
					t.Fatalf("expected defaults to be filled, got %+v", cfg)
				}
				return
			}
			de, ok := domain.AsDomain(err)
			if !ok {
				t.Fatalf("expected domain error, got %v", err)
			}
			if de.Code != tc.wantErr {
				t.Fatalf("expected code %q, got %q", tc.wantErr, de.Code)
			}
		})
	}
}

func TestValidateConfigNormalizesLists(t *testing.T) {
	svc := NewService(&fakeRepo{}, nil, nil, nil)
	cfg := Config{
		CacheBypassURLs:          []string{" /wp-admin ", "", "/cart"},
		CSSRucssIncludeSelectors: []string{"", "  .keep  "},
	}
	if err := svc.validateConfig(&cfg); err != nil {
		t.Fatalf("validateConfig: %v", err)
	}
	if len(cfg.CacheBypassURLs) != 2 || cfg.CacheBypassURLs[0] != "/wp-admin" {
		t.Fatalf("expected trimmed non-empty urls, got %#v", cfg.CacheBypassURLs)
	}
	if len(cfg.CSSRucssIncludeSelectors) != 1 || cfg.CSSRucssIncludeSelectors[0] != ".keep" {
		t.Fatalf("expected trimmed selectors, got %#v", cfg.CSSRucssIncludeSelectors)
	}
}

// ---------------------------------------------------------------------------
// purge records audit + emits SSE + calls agent
// ---------------------------------------------------------------------------

func TestPurgeRecordsAuditAndEmitsEvents(t *testing.T) {
	repo := &fakeRepo{}
	events := &fakeEvents{}
	ag := &fakeAgent{purgeResult: agentcmd.CachePurgeResult{OK: true, Detail: "purged", PurgedCount: 12}}
	svc := NewService(repo, nil, events, nil)
	svc.SetAgentClient(ag, &fakeSites{url: "https://site.example.com"})

	tenantID, siteID, userID := uuid.New(), uuid.New(), uuid.New()
	entry, detail, err := svc.Purge(context.Background(), tenantID, siteID, PurgeInput{
		Scope:       PurgeKindAll,
		InitiatorID: userID,
	})
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if detail != "purged" {
		t.Fatalf("expected agent detail 'purged', got %q", detail)
	}
	// audit recorded
	if len(repo.purges) != 1 {
		t.Fatalf("expected 1 purge audit record, got %d", len(repo.purges))
	}
	if repo.purges[0].Kind != PurgeKindAll || repo.purges[0].InitiatorUserID != userID {
		t.Fatalf("unexpected purge record: %+v", repo.purges[0])
	}
	if entry.SiteID != siteID {
		t.Fatalf("entry site mismatch")
	}
	// agent called with scope=all
	if len(ag.purgeCalls) != 1 || ag.purgeCalls[0].Scope != "all" {
		t.Fatalf("expected agent purge scope=all, got %+v", ag.purgeCalls)
	}
	// the "Last purge" gauge is stamped with the scope (kind=all)
	if len(repo.markedKinds) != 1 || repo.markedKinds[0] != string(PurgeKindAll) {
		t.Fatalf("expected last-purge gauge stamped kind=all, got %+v", repo.markedKinds)
	}
	// SSE: started + completed
	types := events.types()
	if !contains(types, site.EventCachePurgeStarted) || !contains(types, site.EventCachePurgeCompleted) {
		t.Fatalf("expected purge.started + purge.completed events, got %v", types)
	}
}

func TestPurgeURLScopeRequiresURLs(t *testing.T) {
	svc := NewService(&fakeRepo{}, nil, nil, nil)
	_, _, err := svc.Purge(context.Background(), uuid.New(), uuid.New(), PurgeInput{Scope: PurgeKindURL})
	de, ok := domain.AsDomain(err)
	if !ok || de.Code != "missing_urls" {
		t.Fatalf("expected missing_urls domain error, got %v", err)
	}
}

func TestPurgeInvalidScope(t *testing.T) {
	svc := NewService(&fakeRepo{}, nil, nil, nil)
	_, _, err := svc.Purge(context.Background(), uuid.New(), uuid.New(), PurgeInput{Scope: PurgeKind("nuke")})
	de, ok := domain.AsDomain(err)
	if !ok || de.Code != "invalid_scope" {
		t.Fatalf("expected invalid_scope domain error, got %v", err)
	}
}

// UpdateConfig should record an audit-able config update event and bump version.
func TestUpdateConfigBumpsVersionAndEmits(t *testing.T) {
	repo := &fakeRepo{config: Config{ConfigVersion: 3}, configFound: true}
	events := &fakeEvents{}
	svc := NewService(repo, nil, events, nil)
	saved, err := svc.UpdateConfig(context.Background(), uuid.New(), uuid.New(), UpdateConfigInput{
		Config: Config{CacheEnabled: true},
	})
	if err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if saved.ConfigVersion != 4 {
		t.Fatalf("expected version bump to 4, got %d", saved.ConfigVersion)
	}
	if !contains(events.types(), site.EventPerfConfigUpdated) {
		t.Fatalf("expected perf.config.updated event, got %v", events.types())
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// sanity: ensure ErrNotFound is the sentinel used by GetConfig's default path.
func TestGetConfigDefaultWhenAbsent(t *testing.T) {
	repo := &fakeRepo{configFound: false}
	svc := NewService(repo, nil, nil, nil)
	cfg, err := svc.GetConfig(context.Background(), uuid.New(), uuid.New())
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if cfg.CacheRefreshInterval != "2hours" || !cfg.LazyLoad {
		t.Fatalf("expected default config, got %+v", cfg)
	}
	if !errors.Is(ErrNotFound, ErrNotFound) {
		t.Fatal("sentinel")
	}
}

// ---------------------------------------------------------------------------
// DBClean — M38 contract tests
// ---------------------------------------------------------------------------

func TestDBCleanEmitsStartedAndReturnsJobID(t *testing.T) {
	repo := &fakeRepo{configFound: true, config: Config{
		DBPostRevisions: true,
		DBCommentsSpam:  true,
	}}
	events := &fakeEvents{}
	agent := &fakeAgent{}
	sites := &fakeSites{url: "https://example.com"}

	svc := NewService(repo, nil, events, nil)
	svc.SetAgentClient(agent, sites)

	jobID, err := svc.DBClean(context.Background(), uuid.New(), uuid.New(), "https://cp.example.com")
	if err != nil {
		t.Fatalf("DBClean: %v", err)
	}
	if jobID == "" {
		t.Fatal("expected non-empty job_id")
	}

	types := events.types()
	if !contains(types, "db.clean.started") {
		t.Fatalf("expected db.clean.started, got %v", types)
	}
}

func TestDBCleanStartedCarriesTasks(t *testing.T) {
	repo := &fakeRepo{configFound: true, config: Config{
		DBPostRevisions:     true,
		DBPostTrashed:       true,
		DBTransientsExpired: true,
	}}
	events := &fakeEvents{}
	agent := &fakeAgent{}
	sites := &fakeSites{url: "https://example.com"}

	svc := NewService(repo, nil, events, nil)
	svc.SetAgentClient(agent, sites)

	_, err := svc.DBClean(context.Background(), uuid.New(), uuid.New(), "")
	if err != nil {
		t.Fatalf("DBClean: %v", err)
	}

	ev := events.events[0]
	tasks, ok := ev.Data["tasks"].([]string)
	if !ok {
		t.Fatalf("tasks not []string: %T %v", ev.Data["tasks"], ev.Data["tasks"])
	}
	if !contains(tasks, "revisions") {
		t.Errorf("expected 'revisions' in tasks, got %v", tasks)
	}
	if !contains(tasks, "trashed_posts") {
		t.Errorf("expected 'trashed_posts' in tasks, got %v", tasks)
	}
	if !contains(tasks, "expired_transients") {
		t.Errorf("expected 'expired_transients' in tasks, got %v", tasks)
	}
	// Flags that were false must NOT appear.
	if contains(tasks, "spam_comments") {
		t.Errorf("unexpected 'spam_comments' in tasks (flag was false)")
	}
}

func TestDBCleanAgentRefusalEmitsFailedEvent(t *testing.T) {
	repo := &fakeRepo{configFound: true}
	events := &fakeEvents{}

	// An agent that returns ok=false.
	refusingAgent := &refuseDBCleanAgent{}
	sites := &fakeSites{url: "https://example.com"}

	svc := NewService(repo, nil, events, nil)
	svc.SetAgentClient(refusingAgent, sites)

	_, err := svc.DBClean(context.Background(), uuid.New(), uuid.New(), "")
	if err == nil {
		t.Fatal("expected error on agent refusal")
	}

	types := events.types()
	if !contains(types, "db.clean.failed") {
		t.Fatalf("expected db.clean.failed, got %v", types)
	}
}

func TestHandleDBCleanProgressProgress(t *testing.T) {
	repo := &fakeRepo{}
	events := &fakeEvents{}
	svc := NewService(repo, nil, events, nil)

	err := svc.HandleDBCleanProgress(context.Background(), DBCleanProgressInput{
		JobID:       "job-1",
		Category:    "revisions",
		RowsDeleted: 42,
		BytesFreed:  0,
		State:       "done",
		Done:        false,
		TenantID:    uuid.New(),
		SiteID:      uuid.New(),
	})
	if err != nil {
		t.Fatalf("HandleDBCleanProgress: %v", err)
	}

	types := events.types()
	if !contains(types, "db.clean.progress") {
		t.Fatalf("expected db.clean.progress, got %v", types)
	}
}

func TestHandleDBCleanProgressDoneEmitsCompleted(t *testing.T) {
	repo := &fakeRepo{}
	events := &fakeEvents{}
	svc := NewService(repo, nil, events, nil)

	err := svc.HandleDBCleanProgress(context.Background(), DBCleanProgressInput{
		JobID:       "job-2",
		Category:    "optimize_tables",
		RowsDeleted: 0,
		BytesFreed:  1024,
		State:       "done",
		Done:        true,
		TenantID:    uuid.New(),
		SiteID:      uuid.New(),
	})
	if err != nil {
		t.Fatalf("HandleDBCleanProgress: %v", err)
	}

	types := events.types()
	if !contains(types, "db.clean.completed") {
		t.Fatalf("expected db.clean.completed, got %v", types)
	}
}

func TestNextCleanTimeParsing(t *testing.T) {
	_ = time.Now() // ensure time import is used
	cases := []struct {
		interval string
		approx   time.Duration
	}{
		{"daily", 24 * time.Hour},
		{"weekly", 7 * 24 * time.Hour},
		{"monthly", 30 * 24 * time.Hour},
	}
	before := time.Now()
	for _, tc := range cases {
		got := nextCleanTime(tc.interval)
		if got.Before(before) {
			t.Errorf("nextCleanTime(%q) returned past time %v", tc.interval, got)
		}
	}
}

// refuseDBCleanAgent returns ok=false on DBClean.
type refuseDBCleanAgent struct{ fakeAgent }

func (a *refuseDBCleanAgent) DBClean(_ context.Context, _ uuid.UUID, _ string, req agentcmd.DBCleanRequest) (agentcmd.DBCleanResult, error) {
	return agentcmd.DBCleanResult{OK: false, Detail: "not supported"}, nil
}

// ---------------------------------------------------------------------------
// DBTableAction — Phase 2.3 empty action contract tests
// ---------------------------------------------------------------------------

// fakeBackupChecker is a test double for BackupChecker.
type fakeBackupChecker struct {
	hasRecent bool
	err       error
}

func (b *fakeBackupChecker) HasRecentBackup(_ context.Context, _, _ uuid.UUID, _ time.Duration) (bool, error) {
	return b.hasRecent, b.err
}

func newDBTableActionSvc() *Service {
	repo := &fakeRepo{configFound: true}
	events := &fakeEvents{}
	ag := &fakeAgent{}
	sites := &fakeSites{url: "https://example.com"}
	svc := NewService(repo, nil, events, nil)
	svc.SetAgentClient(ag, sites)
	return svc
}

// TestDBTableActionEmptyRequiresConfirm verifies that a single-table empty
// action is rejected when confirm does not equal the table name, and accepted
// when it does.
func TestDBTableActionEmptyRequiresConfirm(t *testing.T) {
	svc := newDBTableActionSvc()
	tenantID, siteID := uuid.New(), uuid.New()

	cases := []struct {
		name    string
		confirm string
		wantErr string // domain code; "" = success
	}{
		{
			name:    "empty confirm rejected",
			confirm: "",
			wantErr: "confirm_mismatch",
		},
		{
			name:    "wrong token rejected",
			confirm: "DROP 1 TABLES",
			wantErr: "confirm_mismatch",
		},
		{
			name:    "correct token accepted",
			confirm: "wp_actionscheduler_logs",
			wantErr: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.DBTableAction(context.Background(), tenantID, siteID, DBTableActionInput{
				Action:  "empty",
				Tables:  []string{"wp_actionscheduler_logs"},
				Confirm: tc.confirm,
			})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected success, got %v", err)
				}
				return
			}
			de, ok := domain.AsDomain(err)
			if !ok {
				t.Fatalf("expected domain error, got %v", err)
			}
			if de.Code != tc.wantErr {
				t.Fatalf("expected code %q, got %q", tc.wantErr, de.Code)
			}
		})
	}
}

// TestDBTableActionEmptyBulkConfirmToken verifies that bulk empty requires
// the exact "EMPTY N TABLES" token format.
func TestDBTableActionEmptyBulkConfirmToken(t *testing.T) {
	svc := newDBTableActionSvc()
	tenantID, siteID := uuid.New(), uuid.New()
	tables := []string{"wp_actionscheduler_logs", "wp_digits_failed_login_logs", "wp_wpmgr_activity_log"}

	cases := []struct {
		name    string
		confirm string
		wantErr string
	}{
		{
			name:    "drop token rejected for empty",
			confirm: "DROP 3 TABLES",
			wantErr: "confirm_mismatch",
		},
		{
			name:    "lowercase rejected",
			confirm: "empty 3 tables",
			wantErr: "confirm_mismatch",
		},
		{
			name:    "wrong count rejected",
			confirm: "EMPTY 2 TABLES",
			wantErr: "confirm_mismatch",
		},
		{
			name:    "correct EMPTY N TABLES token accepted",
			confirm: "EMPTY 3 TABLES",
			wantErr: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.DBTableAction(context.Background(), tenantID, siteID, DBTableActionInput{
				Action:  "empty",
				Tables:  tables,
				Confirm: tc.confirm,
			})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected success, got %v", err)
				}
				return
			}
			de, ok := domain.AsDomain(err)
			if !ok {
				t.Fatalf("expected domain error, got %v", err)
			}
			if de.Code != tc.wantErr {
				t.Fatalf("expected code %q, got %q", tc.wantErr, de.Code)
			}
		})
	}
}

// TestDBTableActionEmptyDispatchesToAgent verifies that a valid empty call
// is dispatched to the agent and returns the per-table results.
func TestDBTableActionEmptyDispatchesToAgent(t *testing.T) {
	svc := newDBTableActionSvc()
	tenantID, siteID := uuid.New(), uuid.New()
	table := "wp_wpmgr_login_events"

	out, err := svc.DBTableAction(context.Background(), tenantID, siteID, DBTableActionInput{
		Action:  "empty",
		Tables:  []string{table},
		Confirm: table, // single-table: confirm == table name
	})
	if err != nil {
		t.Fatalf("DBTableAction empty: %v", err)
	}
	if out.JobID == "" {
		t.Fatal("expected non-empty job_id")
	}
	if len(out.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(out.Results))
	}
	if out.Results[0].Table != table {
		t.Fatalf("expected result for table %q, got %q", table, out.Results[0].Table)
	}
	if out.Results[0].Status != "done" {
		t.Fatalf("expected status 'done', got %q", out.Results[0].Status)
	}
}

// TestDBTableActionEmptyBackupWarning verifies that the backup warning is set
// when no recent backup is found for a destructive (empty) action.
func TestDBTableActionEmptyBackupWarning(t *testing.T) {
	repo := &fakeRepo{configFound: true}
	events := &fakeEvents{}
	ag := &fakeAgent{}
	sites := &fakeSites{url: "https://example.com"}
	svc := NewService(repo, nil, events, nil)
	svc.SetAgentClient(ag, sites)
	svc.SetBackupChecker(&fakeBackupChecker{hasRecent: false})

	table := "wp_actionscheduler_logs"
	out, err := svc.DBTableAction(context.Background(), uuid.New(), uuid.New(), DBTableActionInput{
		Action:  "empty",
		Tables:  []string{table},
		Confirm: table,
	})
	if err != nil {
		t.Fatalf("DBTableAction empty: %v", err)
	}
	if out.BackupWarning == "" {
		t.Fatal("expected non-empty BackupWarning when no recent backup found")
	}
	// The warning must reference the generalised message (not drop-only copy).
	if !strings.Contains(out.BackupWarning, "destructive table actions") {
		t.Fatalf("BackupWarning should mention 'destructive table actions', got: %q", out.BackupWarning)
	}
}

// TestDBTableActionEmptyNoBackupWarningWhenRecentExists verifies that the
// backup warning is NOT set when a recent backup is found.
func TestDBTableActionEmptyNoBackupWarningWhenRecentExists(t *testing.T) {
	repo := &fakeRepo{configFound: true}
	events := &fakeEvents{}
	ag := &fakeAgent{}
	sites := &fakeSites{url: "https://example.com"}
	svc := NewService(repo, nil, events, nil)
	svc.SetAgentClient(ag, sites)
	svc.SetBackupChecker(&fakeBackupChecker{hasRecent: true})

	table := "wp_digits_failed_login_logs"
	out, err := svc.DBTableAction(context.Background(), uuid.New(), uuid.New(), DBTableActionInput{
		Action:  "empty",
		Tables:  []string{table},
		Confirm: table,
	})
	if err != nil {
		t.Fatalf("DBTableAction empty: %v", err)
	}
	if out.BackupWarning != "" {
		t.Fatalf("expected empty BackupWarning when recent backup exists, got: %q", out.BackupWarning)
	}
}

// TestDBTableActionDropAcceptsNonCoreOwnerTypes asserts that the CP accepts a
// drop request for plugin-owned and theme-owned tables (not just orphans).
// The orphan-only restriction lives exclusively on the agent side; the CP must
// pass plugin/theme tables through to the agent with a valid confirm token.
func TestDBTableActionDropAcceptsNonCoreOwnerTypes(t *testing.T) {
	svc := newDBTableActionSvc()
	tenantID, siteID := uuid.New(), uuid.New()

	cases := []struct {
		name  string
		table string
		note  string
	}{
		{
			name:  "plugin-owned log table",
			table: "wp_digits_failed_login_logs",
			note:  "active plugin table; plugin recreates schema on next run",
		},
		{
			name:  "theme-owned table",
			table: "wp_mytheme_cache",
			note:  "theme auxiliary table",
		},
		{
			name:  "orphan table",
			table: "wp_orphan_stale",
			note:  "orphan table (previous behaviour still works)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := svc.DBTableAction(context.Background(), tenantID, siteID, DBTableActionInput{
				Action:  "drop",
				Tables:  []string{tc.table},
				Confirm: tc.table, // single-table: confirm == table name
			})
			if err != nil {
				t.Fatalf("drop %s (%s): expected CP to accept and forward to agent, got error: %v", tc.table, tc.note, err)
			}
			if out.JobID == "" {
				t.Fatalf("drop %s: expected non-empty job_id", tc.table)
			}
			if len(out.Results) != 1 || out.Results[0].Table != tc.table {
				t.Fatalf("drop %s: expected one result for the table, got %+v", tc.table, out.Results)
			}
		})
	}
}

// TestDBTableActionDropBulkPluginTables verifies that a bulk drop of mixed
// plugin/orphan tables is accepted by the CP with the correct "DROP N TABLES"
// token; the agent-side non-core gate is not the CP's concern.
func TestDBTableActionDropBulkPluginTables(t *testing.T) {
	svc := newDBTableActionSvc()
	tenantID, siteID := uuid.New(), uuid.New()

	tables := []string{
		"wp_digits_failed_login_logs", // plugin
		"wp_orphan_old",               // orphan
		"wp_mytheme_data",             // theme
	}

	out, err := svc.DBTableAction(context.Background(), tenantID, siteID, DBTableActionInput{
		Action:  "drop",
		Tables:  tables,
		Confirm: "DROP 3 TABLES",
	})
	if err != nil {
		t.Fatalf("bulk drop plugin/orphan/theme tables: expected CP to accept, got: %v", err)
	}
	if out.JobID == "" {
		t.Fatal("expected non-empty job_id on bulk drop")
	}
	if len(out.Results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(out.Results))
	}
}

// ---------------------------------------------------------------------------
// DBTableAction — Phase 2.5 analyze / convert_innodb contract tests
// ---------------------------------------------------------------------------

// TestDBTableActionAnalyzeAcceptedWithNoConfirm verifies that analyze is a
// valid action, requires no type-to-confirm token, and is dispatched to the
// agent with only PermSiteCacheManage (the service layer does not enforce
// permissions — that is handler-side; the service must accept with no Confirm).
func TestDBTableActionAnalyzeAcceptedWithNoConfirm(t *testing.T) {
	svc := newDBTableActionSvc()
	tenantID, siteID := uuid.New(), uuid.New()

	cases := []struct {
		name   string
		tables []string
	}{
		{name: "single table", tables: []string{"wp_posts"}},
		{name: "core table allowed", tables: []string{"wp_options"}},
		{name: "plugin table", tables: []string{"wp_actionscheduler_logs"}},
		{name: "bulk tables", tables: []string{"wp_posts", "wp_users", "wp_terms"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := svc.DBTableAction(context.Background(), tenantID, siteID, DBTableActionInput{
				Action:  "analyze",
				Tables:  tc.tables,
				Confirm: "", // no confirm required for non-destructive action
			})
			if err != nil {
				t.Fatalf("analyze %v: expected success (no confirm required), got: %v", tc.tables, err)
			}
			if out.JobID == "" {
				t.Fatal("expected non-empty job_id")
			}
			if len(out.Results) != len(tc.tables) {
				t.Fatalf("expected %d results, got %d", len(tc.tables), len(out.Results))
			}
			for i, r := range out.Results {
				if r.Table != tc.tables[i] {
					t.Fatalf("result[%d].Table = %q, want %q", i, r.Table, tc.tables[i])
				}
				if r.Status != "done" {
					t.Fatalf("result[%d].Status = %q, want \"done\"", i, r.Status)
				}
			}
		})
	}
}

// TestDBTableActionAnalyzeNoBackupWarning verifies that the backup-warning
// advisory is NOT emitted for analyze (it is non-destructive).
func TestDBTableActionAnalyzeNoBackupWarning(t *testing.T) {
	repo := &fakeRepo{configFound: true}
	events := &fakeEvents{}
	ag := &fakeAgent{}
	sites := &fakeSites{url: "https://example.com"}
	svc := NewService(repo, nil, events, nil)
	svc.SetAgentClient(ag, sites)
	svc.SetBackupChecker(&fakeBackupChecker{hasRecent: false}) // no recent backup

	out, err := svc.DBTableAction(context.Background(), uuid.New(), uuid.New(), DBTableActionInput{
		Action: "analyze",
		Tables: []string{"wp_posts"},
	})
	if err != nil {
		t.Fatalf("analyze: expected success, got: %v", err)
	}
	if out.BackupWarning != "" {
		t.Fatalf("analyze must NOT emit a backup warning (non-destructive), got: %q", out.BackupWarning)
	}
}

// TestDBTableActionConvertInnodbAcceptedWithNoConfirm verifies that
// convert_innodb is a valid action, requires no type-to-confirm token, and is
// dispatched to the agent without any destructive gate.
func TestDBTableActionConvertInnodbAcceptedWithNoConfirm(t *testing.T) {
	svc := newDBTableActionSvc()
	tenantID, siteID := uuid.New(), uuid.New()

	cases := []struct {
		name   string
		tables []string
	}{
		{name: "single MyISAM table", tables: []string{"wp_myisam_table"}},
		{name: "core table allowed", tables: []string{"wp_options"}},
		{name: "plugin table", tables: []string{"wp_wc_orders"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := svc.DBTableAction(context.Background(), tenantID, siteID, DBTableActionInput{
				Action:  "convert_innodb",
				Tables:  tc.tables,
				Confirm: "", // no confirm required for non-destructive action
			})
			if err != nil {
				t.Fatalf("convert_innodb %v: expected success (no confirm required), got: %v", tc.tables, err)
			}
			if out.JobID == "" {
				t.Fatal("expected non-empty job_id")
			}
			if len(out.Results) != len(tc.tables) {
				t.Fatalf("expected %d results, got %d", len(tc.tables), len(out.Results))
			}
		})
	}
}

// TestDBTableActionConvertInnodbNoBackupWarning verifies that the backup-warning
// advisory is NOT emitted for convert_innodb (it is non-destructive).
func TestDBTableActionConvertInnodbNoBackupWarning(t *testing.T) {
	repo := &fakeRepo{configFound: true}
	events := &fakeEvents{}
	ag := &fakeAgent{}
	sites := &fakeSites{url: "https://example.com"}
	svc := NewService(repo, nil, events, nil)
	svc.SetAgentClient(ag, sites)
	svc.SetBackupChecker(&fakeBackupChecker{hasRecent: false}) // no recent backup

	out, err := svc.DBTableAction(context.Background(), uuid.New(), uuid.New(), DBTableActionInput{
		Action: "convert_innodb",
		Tables: []string{"wp_myisam_table"},
	})
	if err != nil {
		t.Fatalf("convert_innodb: expected success, got: %v", err)
	}
	if out.BackupWarning != "" {
		t.Fatalf("convert_innodb must NOT emit a backup warning (non-destructive), got: %q", out.BackupWarning)
	}
}

// TestDBTableActionInvalidActionListsAllSix asserts that the validation error
// message for an unrecognised action enumerates all six valid action strings,
// so the error message stays accurate after the Phase 2.5 additions.
func TestDBTableActionInvalidActionListsAllSix(t *testing.T) {
	svc := newDBTableActionSvc()
	tenantID, siteID := uuid.New(), uuid.New()

	_, err := svc.DBTableAction(context.Background(), tenantID, siteID, DBTableActionInput{
		Action: "truncate",
		Tables: []string{"wp_posts"},
	})
	if err == nil {
		t.Fatal("expected error for invalid action, got nil")
	}
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("expected domain error, got %T: %v", err, err)
	}
	if de.Code != "invalid_table_action" {
		t.Fatalf("expected code \"invalid_table_action\", got %q", de.Code)
	}
	// The message must list all six valid actions.
	for _, action := range []string{"optimize", "repair", "drop", "empty", "analyze", "convert_innodb"} {
		if !strings.Contains(de.Message, action) {
			t.Fatalf("validation error message does not mention action %q; full message: %q", action, de.Message)
		}
	}
}

// TestDBTableActionAnalyzeAndConvertNotInDestructiveSet asserts that analyze
// and convert_innodb are NOT treated as destructive (i.e. they do not require
// a confirm token even when the backup checker has no recent backup).
func TestDBTableActionAnalyzeAndConvertNotInDestructiveSet(t *testing.T) {
	svc := newDBTableActionSvc()
	tenantID, siteID := uuid.New(), uuid.New()

	for _, action := range []string{"analyze", "convert_innodb"} {
		t.Run(action, func(t *testing.T) {
			// No Confirm supplied — must succeed for non-destructive actions.
			out, err := svc.DBTableAction(context.Background(), tenantID, siteID, DBTableActionInput{
				Action:  action,
				Tables:  []string{"wp_some_table"},
				Confirm: "",
			})
			if err != nil {
				t.Fatalf("%s with no confirm: expected success, got: %v", action, err)
			}
			if out.JobID == "" {
				t.Fatalf("%s: expected non-empty job_id", action)
			}
		})
	}
}

// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// P3.8 DBOrphanDelete — service-layer safety gate tests
// ---------------------------------------------------------------------------

// orphanDeleteSvc builds a Service with a scan result whose options slice
// contains one exact-matched, uninstalled, single-candidate option (the item
// that must survive the re-classify gate) plus optionally an installed-plugin
// set. The fakeAgent returns ok=true for DBOrphanDelete.
func orphanDeleteSvc(optName, ownerSlug string, installedSlugs []string) (*Service, *fakeEvents, *fakeAgent) {
	optJSON := mustMarshal([]agentcmd.OrphanedOptionItem{{Name: optName, Autoload: true, SizeBytes: 64}})
	var plugJSON []byte
	if len(installedSlugs) == 0 {
		// Non-empty installed set (snapshot_available=true) that does NOT contain ownerSlug.
		plugJSON = mustMarshal([]agentcmd.InstalledPluginItem{{Slug: "some-other-plugin", Name: "Other", Active: true, Source: "plugin"}})
	} else {
		items := make([]agentcmd.InstalledPluginItem, len(installedSlugs))
		for i, s := range installedSlugs {
			items[i] = agentcmd.InstalledPluginItem{Slug: s, Name: s, Active: true, Source: "plugin"}
		}
		plugJSON = mustMarshal(items)
	}
	scanResult := DBScanResult{
		OrphanedOptionsJSON:  optJSON,
		OrphanedCronJSON:     []byte("[]"),
		TablesJSON:           []byte("[]"),
		InstalledPluginsJSON: plugJSON,
	}
	repo := &scanRepoWith{scanFound: true, result: scanResult}
	events := &fakeEvents{}
	ag := &fakeAgent{}
	svc := NewService(repo, nil, events, nil)
	svc.SetAgentClient(ag, &fakeSites{url: "https://example.com"})
	return svc, events, ag
}

// exactCorpus returns a fakeCorpus whose single signature matches optName with
// exact confidence under ownerSlug.
func exactCorpus(optName, ownerSlug string) *fakeCorpus {
	return &fakeCorpus{sigs: []dbclean.Signature{
		{Slug: ownerSlug, CorpusVersion: 1, OptionPatterns: []string{optName}},
	}}
}

// TestDBOrphanDeleteSurvivorIsDispatched verifies the happy path: a single
// eligible item passes re-classify, the correct confirm token is accepted, and
// the agent receives exactly that item with the report's owner_slug.
func TestDBOrphanDeleteSurvivorIsDispatched(t *testing.T) {
	optName := "my_plugin_option"
	ownerSlug := "my-plugin"
	svc, events, _ := orphanDeleteSvc(optName, ownerSlug, nil)
	corpus := exactCorpus(optName, ownerSlug)

	out, err := svc.DBOrphanDelete(context.Background(), corpus,
		uuid.New(), uuid.New(), "https://cp.example.com",
		OrphanDeleteInput{
			Items: []OrphanDeleteRequestItem{
				{Kind: "option", Name: optName, OwnerSlug: ownerSlug},
			},
			Confirm: optName, // single item: confirm == item name
		},
	)
	if err != nil {
		t.Fatalf("DBOrphanDelete: %v", err)
	}
	if out.JobID == "" {
		t.Fatal("expected non-empty job_id")
	}
	if out.AcceptedCount != 1 {
		t.Fatalf("expected accepted_count=1, got %d", out.AcceptedCount)
	}
	if out.DroppedCount != 0 {
		t.Fatalf("expected dropped_count=0, got %d", out.DroppedCount)
	}
	// The started SSE must have been emitted.
	if !contains(events.types(), "db.orphan.delete.started") {
		t.Fatalf("expected db.orphan.delete.started, got %v", events.types())
	}
}

// TestDBOrphanDeleteOwnerSlugSpoofRejected verifies that an item whose
// requested owner_slug does not match the report's owner_slug is dropped
// (re-classify override), and when no survivors remain the call is rejected.
func TestDBOrphanDeleteOwnerSlugSpoofRejected(t *testing.T) {
	optName := "my_plugin_option"
	realOwner := "my-plugin"
	svc, _, _ := orphanDeleteSvc(optName, realOwner, nil)
	corpus := exactCorpus(optName, realOwner)

	_, err := svc.DBOrphanDelete(context.Background(), corpus,
		uuid.New(), uuid.New(), "",
		OrphanDeleteInput{
			Items: []OrphanDeleteRequestItem{
				// Attacker supplies a different owner_slug.
				{Kind: "option", Name: optName, OwnerSlug: "attacker-plugin"},
			},
			Confirm: optName,
		},
	)
	if err == nil {
		t.Fatal("expected error when owner_slug is spoofed, got nil")
	}
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("expected domain error, got %T: %v", err, err)
	}
	if de.Code != "no_eligible_items" {
		t.Fatalf("expected code no_eligible_items, got %q", de.Code)
	}
}

// TestDBOrphanDeleteZeroSurvivorRejected verifies that when every requested
// item fails the re-classify gate (e.g. the item is now installed), the call
// returns a validation error before dispatching to the agent.
func TestDBOrphanDeleteZeroSurvivorRejected(t *testing.T) {
	optName := "my_plugin_option"
	ownerSlug := "my-plugin"
	// Build a service where the owner IS now in the installed set.
	svc, _, _ := orphanDeleteSvc(optName, ownerSlug, []string{ownerSlug})
	corpus := exactCorpus(optName, ownerSlug)

	_, err := svc.DBOrphanDelete(context.Background(), corpus,
		uuid.New(), uuid.New(), "",
		OrphanDeleteInput{
			Items: []OrphanDeleteRequestItem{
				{Kind: "option", Name: optName, OwnerSlug: ownerSlug},
			},
			Confirm: optName,
		},
	)
	if err == nil {
		t.Fatal("expected error when all items are filtered by re-classify, got nil")
	}
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("expected domain error, got %T: %v", err, err)
	}
	if de.Code != "no_eligible_items" {
		t.Fatalf("expected no_eligible_items, got %q", de.Code)
	}
}

// TestDBOrphanDeleteConfirmSingleItemGrammar verifies that a single-item
// request requires confirm == the item's Name, and rejects mismatches before
// re-classify.
func TestDBOrphanDeleteConfirmSingleItemGrammar(t *testing.T) {
	optName := "my_plugin_option"
	ownerSlug := "my-plugin"
	svc, _, _ := orphanDeleteSvc(optName, ownerSlug, nil)
	corpus := exactCorpus(optName, ownerSlug)

	cases := []struct {
		name    string
		confirm string
		wantErr string
	}{
		{
			name:    "empty confirm rejected",
			confirm: "",
			wantErr: "confirm_mismatch",
		},
		{
			name:    "wrong token rejected",
			confirm: "DELETE 1 ORPHANS",
			wantErr: "confirm_mismatch",
		},
		{
			name:    "correct item name accepted",
			confirm: optName,
			wantErr: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.DBOrphanDelete(context.Background(), corpus,
				uuid.New(), uuid.New(), "",
				OrphanDeleteInput{
					Items:   []OrphanDeleteRequestItem{{Kind: "option", Name: optName, OwnerSlug: ownerSlug}},
					Confirm: tc.confirm,
				},
			)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected success, got %v", err)
				}
				return
			}
			de, ok := domain.AsDomain(err)
			if !ok {
				t.Fatalf("expected domain error, got %T: %v", err, err)
			}
			if de.Code != tc.wantErr {
				t.Fatalf("expected code %q, got %q", tc.wantErr, de.Code)
			}
		})
	}
}

// TestDBOrphanDeleteConfirmMultiSameKindGrammar verifies that multiple items of
// the same kind require "DELETE N <KIND_LABEL>" (e.g. "DELETE 2 OPTIONS").
func TestDBOrphanDeleteConfirmMultiSameKindGrammar(t *testing.T) {
	optA := "plugin_a_settings"
	optB := "plugin_a_data"
	ownerSlug := "plugin-a"
	corpus := &fakeCorpus{sigs: []dbclean.Signature{
		{Slug: ownerSlug, CorpusVersion: 1, OptionPatterns: []string{optA, optB}},
	}}

	optJSON := mustMarshal([]agentcmd.OrphanedOptionItem{
		{Name: optA, Autoload: true, SizeBytes: 32},
		{Name: optB, Autoload: false, SizeBytes: 16},
	})
	plugJSON := mustMarshal([]agentcmd.InstalledPluginItem{{Slug: "other", Name: "Other", Active: true, Source: "plugin"}})
	repo := &scanRepoWith{scanFound: true, result: DBScanResult{
		OrphanedOptionsJSON:  optJSON,
		OrphanedCronJSON:     []byte("[]"),
		TablesJSON:           []byte("[]"),
		InstalledPluginsJSON: plugJSON,
	}}
	events := &fakeEvents{}
	svc := NewService(repo, nil, events, nil)
	svc.SetAgentClient(&fakeAgent{}, &fakeSites{url: "https://example.com"})

	items := []OrphanDeleteRequestItem{
		{Kind: "option", Name: optA, OwnerSlug: ownerSlug},
		{Kind: "option", Name: optB, OwnerSlug: ownerSlug},
	}

	cases := []struct {
		name    string
		confirm string
		wantErr string
	}{
		// "DELETE N ORPHANS" is the mixed-kind token, not the same-kind token.
		{name: "mixed-kind token rejected for single-kind request", confirm: "DELETE 2 ORPHANS", wantErr: "confirm_mismatch"},
		// Wrong count is always rejected.
		{name: "wrong count rejected", confirm: "DELETE 1 OPTIONS", wantErr: "confirm_mismatch"},
		// The check is case-insensitive, so lowercase is accepted.
		{name: "lowercase accepted (case-insensitive)", confirm: "delete 2 options", wantErr: ""},
		{name: "correct uppercased token accepted", confirm: "DELETE 2 OPTIONS", wantErr: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.DBOrphanDelete(context.Background(), corpus,
				uuid.New(), uuid.New(), "",
				OrphanDeleteInput{Items: items, Confirm: tc.confirm},
			)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected success, got %v", err)
				}
				return
			}
			de, ok := domain.AsDomain(err)
			if !ok {
				t.Fatalf("expected domain error, got %T: %v", err, err)
			}
			if de.Code != tc.wantErr {
				t.Fatalf("expected code %q, got %q", tc.wantErr, de.Code)
			}
		})
	}
}

// TestDBOrphanDeleteConfirmMixedKindGrammar verifies that items spanning
// multiple kinds require "DELETE N ORPHANS".
func TestDBOrphanDeleteConfirmMixedKindGrammar(t *testing.T) {
	optName := "the_plugin_option"
	cronHook := "the_plugin_cron"
	ownerSlug := "the-plugin"
	corpus := &fakeCorpus{sigs: []dbclean.Signature{
		{Slug: ownerSlug, CorpusVersion: 1,
			OptionPatterns:   []string{optName},
			CronHookPatterns: []string{cronHook}},
	}}

	optJSON := mustMarshal([]agentcmd.OrphanedOptionItem{{Name: optName, Autoload: true, SizeBytes: 32}})
	cronJSON := mustMarshal([]agentcmd.OrphanedCronItem{{Hook: cronHook, NextRunAt: 0, Recurrence: "daily"}})
	plugJSON := mustMarshal([]agentcmd.InstalledPluginItem{{Slug: "other", Name: "Other", Active: true, Source: "plugin"}})
	repo := &scanRepoWith{scanFound: true, result: DBScanResult{
		OrphanedOptionsJSON:  optJSON,
		OrphanedCronJSON:     cronJSON,
		TablesJSON:           []byte("[]"),
		InstalledPluginsJSON: plugJSON,
	}}
	events := &fakeEvents{}
	svc := NewService(repo, nil, events, nil)
	svc.SetAgentClient(&fakeAgent{}, &fakeSites{url: "https://example.com"})

	items := []OrphanDeleteRequestItem{
		{Kind: "option", Name: optName, OwnerSlug: ownerSlug},
		{Kind: "cron", Name: cronHook, OwnerSlug: ownerSlug},
	}

	// Mixed kinds must require "DELETE N ORPHANS".
	_, err := svc.DBOrphanDelete(context.Background(), corpus,
		uuid.New(), uuid.New(), "",
		OrphanDeleteInput{Items: items, Confirm: "DELETE 2 OPTIONS"},
	)
	if err == nil {
		t.Fatal("expected confirm_mismatch when using kind-specific token for mixed kinds")
	}
	de, ok := domain.AsDomain(err)
	if !ok || de.Code != "confirm_mismatch" {
		t.Fatalf("expected confirm_mismatch, got %v", err)
	}

	_, err = svc.DBOrphanDelete(context.Background(), corpus,
		uuid.New(), uuid.New(), "",
		OrphanDeleteInput{Items: items, Confirm: "DELETE 2 ORPHANS"},
	)
	if err != nil {
		t.Fatalf("expected success with DELETE 2 ORPHANS, got %v", err)
	}
}

// capturingOrphanAgent overrides DBOrphanDelete to capture the signed request
// for assertion. All other AgentPerfClient methods delegate to fakeAgent.
type capturingOrphanAgent struct {
	fakeAgent
	mu       sync.Mutex
	captured agentcmd.DBOrphanDeleteRequest
}

func (a *capturingOrphanAgent) DBOrphanDelete(_ context.Context, _ uuid.UUID, _ string, req agentcmd.DBOrphanDeleteRequest) (agentcmd.DBOrphanDeleteResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.captured = req
	return agentcmd.DBOrphanDeleteResult{OK: true, JobID: req.JobID}, nil
}

// watchdogTrackingRepo embeds scanRepoWith and counts calls to
// SetActiveDBOrphanDeleteJob so the watchdog-stamping test can assert it.
type watchdogTrackingRepo struct {
	scanRepoWith
	mu       sync.Mutex
	setCount int
}

func (r *watchdogTrackingRepo) SetActiveDBOrphanDeleteJob(_ context.Context, _ uuid.UUID, _ string, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setCount++
	return nil
}

// TestDBOrphanDeleteWatchdogStampedOnDispatch verifies that the watchdog columns
// are stamped (SetActiveDBOrphanDeleteJob called) when the agent accepts the job.
func TestDBOrphanDeleteWatchdogStampedOnDispatch(t *testing.T) {
	optName := "my_plugin_option"
	ownerSlug := "my-plugin"

	sr := &watchdogTrackingRepo{}
	sr.scanFound = true
	sr.result = DBScanResult{
		OrphanedOptionsJSON:  mustMarshal([]agentcmd.OrphanedOptionItem{{Name: optName, Autoload: true, SizeBytes: 64}}),
		OrphanedCronJSON:     []byte("[]"),
		TablesJSON:           []byte("[]"),
		InstalledPluginsJSON: mustMarshal([]agentcmd.InstalledPluginItem{{Slug: "other", Name: "Other", Active: true, Source: "plugin"}}),
	}

	events := &fakeEvents{}
	ag := &fakeAgent{}
	svc := NewService(sr, nil, events, nil)
	svc.SetAgentClient(ag, &fakeSites{url: "https://example.com"})

	corpus := exactCorpus(optName, ownerSlug)
	out, err := svc.DBOrphanDelete(context.Background(), corpus,
		uuid.New(), uuid.New(), "",
		OrphanDeleteInput{
			Items:   []OrphanDeleteRequestItem{{Kind: "option", Name: optName, OwnerSlug: ownerSlug}},
			Confirm: optName,
		},
	)
	if err != nil {
		t.Fatalf("DBOrphanDelete: %v", err)
	}
	if out.JobID == "" {
		t.Fatal("expected non-empty job_id")
	}
	// SetActiveDBOrphanDeleteJob must have been called exactly once.
	sr.mu.Lock()
	setCount := sr.setCount
	sr.mu.Unlock()
	if setCount != 1 {
		t.Fatalf("expected SetActiveDBOrphanDeleteJob called 1 time, got %d", setCount)
	}
	// The started event must carry the accepted_count so the UI knows the job is running.
	var startedEvent *site.ConnectionEvent
	for i, ev := range events.events {
		if ev.Type == "db.orphan.delete.started" {
			startedEvent = &events.events[i]
			break
		}
	}
	if startedEvent == nil {
		t.Fatal("expected db.orphan.delete.started event")
	}
	if ac, ok := startedEvent.Data["accepted_count"].(int); !ok || ac != 1 {
		t.Fatalf("expected accepted_count=1 in started event, got %v", startedEvent.Data["accepted_count"])
	}
}

// TestDBOrphanDeleteReportSlugUsedNotRequestSlug verifies that the item sent to
// the agent carries the report's owner_slug (from re-classify), not the raw
// request slug. This prevents the CP from forwarding an attacker-supplied slug
// to the agent even when the item itself is eligible.
func TestDBOrphanDeleteReportSlugUsedNotRequestSlug(t *testing.T) {
	optName := "my_plugin_option"
	ownerSlug := "my-plugin"

	optJSON := mustMarshal([]agentcmd.OrphanedOptionItem{{Name: optName, Autoload: true, SizeBytes: 64}})
	plugJSON := mustMarshal([]agentcmd.InstalledPluginItem{{Slug: "other", Name: "Other", Active: true, Source: "plugin"}})
	repo := &scanRepoWith{scanFound: true, result: DBScanResult{
		OrphanedOptionsJSON:  optJSON,
		OrphanedCronJSON:     []byte("[]"),
		TablesJSON:           []byte("[]"),
		InstalledPluginsJSON: plugJSON,
	}}
	events := &fakeEvents{}
	ca := &capturingOrphanAgent{}
	svc := NewService(repo, nil, events, nil)
	svc.SetAgentClient(ca, &fakeSites{url: "https://example.com"})

	corpus := exactCorpus(optName, ownerSlug)
	_, err := svc.DBOrphanDelete(context.Background(), corpus,
		uuid.New(), uuid.New(), "",
		OrphanDeleteInput{
			Items: []OrphanDeleteRequestItem{
				// Operator supplies the correct ownerSlug; we verify it
				// arrives at the agent from the report, not the raw request.
				{Kind: "option", Name: optName, OwnerSlug: ownerSlug},
			},
			Confirm: optName,
		},
	)
	if err != nil {
		t.Fatalf("DBOrphanDelete: %v", err)
	}
	ca.mu.Lock()
	req := ca.captured
	ca.mu.Unlock()
	if req.JobID == "" {
		t.Fatal("expected agent to have been called with a request")
	}
	if len(req.Items) != 1 {
		t.Fatalf("expected 1 item in agent request, got %d", len(req.Items))
	}
	if req.Items[0].OwnerSlug != ownerSlug {
		t.Fatalf("expected agent item owner_slug=%q (from report), got %q", ownerSlug, req.Items[0].OwnerSlug)
	}
}

// TestDBOrphanDeleteHeuristicItemDropped verifies that a heuristic-confidence
// item is silently filtered out (not signed) even if the operator includes it.
func TestDBOrphanDeleteHeuristicItemDropped(t *testing.T) {
	optName := "heuristic_option"
	ownerSlug := "contact-form-7"

	// Corpus: no exact/prefix patterns — classifier returns heuristic.
	corpus := &fakeCorpus{sigs: []dbclean.Signature{
		{Slug: ownerSlug, CorpusVersion: 1},
	}}

	optJSON := mustMarshal([]agentcmd.OrphanedOptionItem{{Name: optName, Autoload: false, SizeBytes: 8}})
	plugJSON := mustMarshal([]agentcmd.InstalledPluginItem{{Slug: "other", Name: "Other", Active: true, Source: "plugin"}})
	repo := &scanRepoWith{scanFound: true, result: DBScanResult{
		OrphanedOptionsJSON:  optJSON,
		OrphanedCronJSON:     []byte("[]"),
		TablesJSON:           []byte("[]"),
		InstalledPluginsJSON: plugJSON,
	}}
	events := &fakeEvents{}
	svc := NewService(repo, nil, events, nil)
	svc.SetAgentClient(&fakeAgent{}, &fakeSites{url: "https://example.com"})

	_, err := svc.DBOrphanDelete(context.Background(), corpus,
		uuid.New(), uuid.New(), "",
		OrphanDeleteInput{
			Items:   []OrphanDeleteRequestItem{{Kind: "option", Name: optName, OwnerSlug: ownerSlug}},
			Confirm: optName,
		},
	)
	if err == nil {
		t.Fatal("expected no_eligible_items error for heuristic-confidence item, got nil")
	}
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("expected domain error, got %T: %v", err, err)
	}
	if de.Code != "no_eligible_items" {
		t.Fatalf("expected no_eligible_items, got %q", de.Code)
	}
}

// TestDBOrphanDeleteNoItemsRejected verifies that an empty items slice returns
// a validation error.
func TestDBOrphanDeleteNoItemsRejected(t *testing.T) {
	svc, _, _ := orphanDeleteSvc("x", "y", nil)
	_, err := svc.DBOrphanDelete(context.Background(), &fakeCorpus{},
		uuid.New(), uuid.New(), "",
		OrphanDeleteInput{Items: nil, Confirm: ""},
	)
	de, ok := domain.AsDomain(err)
	if !ok || de.Code != "missing_items" {
		t.Fatalf("expected missing_items domain error, got %v", err)
	}
}

// TestDBOrphanDeleteNoSnapshotRejected verifies that when the scan result has
// no installed-plugins snapshot (empty InstalledPluginsJSON), the call is
// rejected with no_snapshot.
func TestDBOrphanDeleteNoSnapshotRejected(t *testing.T) {
	optName := "my_plugin_option"
	ownerSlug := "my-plugin"
	corpus := exactCorpus(optName, ownerSlug)

	// Empty installed plugins JSON — snapshot_available=false.
	optJSON := mustMarshal([]agentcmd.OrphanedOptionItem{{Name: optName, Autoload: true, SizeBytes: 64}})
	repo := &scanRepoWith{scanFound: true, result: DBScanResult{
		OrphanedOptionsJSON:  optJSON,
		OrphanedCronJSON:     []byte("[]"),
		TablesJSON:           []byte("[]"),
		InstalledPluginsJSON: []byte("[]"),
	}}
	events := &fakeEvents{}
	svc := NewService(repo, nil, events, nil)
	svc.SetAgentClient(&fakeAgent{}, &fakeSites{url: "https://example.com"})

	_, err := svc.DBOrphanDelete(context.Background(), corpus,
		uuid.New(), uuid.New(), "",
		OrphanDeleteInput{
			Items:   []OrphanDeleteRequestItem{{Kind: "option", Name: optName, OwnerSlug: ownerSlug}},
			Confirm: optName,
		},
	)
	if err == nil {
		t.Fatal("expected error when snapshot unavailable, got nil")
	}
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("expected domain error, got %T: %v", err, err)
	}
	if de.Code != "no_snapshot" {
		t.Fatalf("expected no_snapshot, got %q", de.Code)
	}
}

// TestDBOrphanDeleteBackupWarningWhenNoRecentBackup verifies the advisory
// backup nudge is surfaced when the backup checker reports no recent backup.
func TestDBOrphanDeleteBackupWarningWhenNoRecentBackup(t *testing.T) {
	optName := "my_plugin_option"
	ownerSlug := "my-plugin"
	svc, _, _ := orphanDeleteSvc(optName, ownerSlug, nil)
	svc.SetBackupChecker(&fakeBackupChecker{hasRecent: false})
	corpus := exactCorpus(optName, ownerSlug)

	out, err := svc.DBOrphanDelete(context.Background(), corpus,
		uuid.New(), uuid.New(), "",
		OrphanDeleteInput{
			Items:   []OrphanDeleteRequestItem{{Kind: "option", Name: optName, OwnerSlug: ownerSlug}},
			Confirm: optName,
		},
	)
	if err != nil {
		t.Fatalf("DBOrphanDelete: %v", err)
	}
	if out.BackupWarning == "" {
		t.Fatal("expected non-empty BackupWarning when no recent backup found")
	}
}

// TestDBOrphanDeleteNoBackupWarningWhenRecentExists verifies the advisory
// backup nudge is NOT set when a recent backup exists.
func TestDBOrphanDeleteNoBackupWarningWhenRecentExists(t *testing.T) {
	optName := "my_plugin_option"
	ownerSlug := "my-plugin"
	svc, _, _ := orphanDeleteSvc(optName, ownerSlug, nil)
	svc.SetBackupChecker(&fakeBackupChecker{hasRecent: true})
	corpus := exactCorpus(optName, ownerSlug)

	out, err := svc.DBOrphanDelete(context.Background(), corpus,
		uuid.New(), uuid.New(), "",
		OrphanDeleteInput{
			Items:   []OrphanDeleteRequestItem{{Kind: "option", Name: optName, OwnerSlug: ownerSlug}},
			Confirm: optName,
		},
	)
	if err != nil {
		t.Fatalf("DBOrphanDelete: %v", err)
	}
	if out.BackupWarning != "" {
		t.Fatalf("expected empty BackupWarning when recent backup exists, got %q", out.BackupWarning)
	}
}

// ---------------------------------------------------------------------------

// TestDBTableActionDropConfirmTokenUnchanged verifies that the drop confirm
// contract is not broken by the empty changes (single-table and bulk).
func TestDBTableActionDropConfirmTokenUnchanged(t *testing.T) {
	svc := newDBTableActionSvc()
	tenantID, siteID := uuid.New(), uuid.New()

	// Single-table drop: confirm must equal the table name.
	table := "wp_orphan_table"
	out, err := svc.DBTableAction(context.Background(), tenantID, siteID, DBTableActionInput{
		Action:  "drop",
		Tables:  []string{table},
		Confirm: table,
	})
	if err != nil {
		t.Fatalf("drop single-table: %v", err)
	}
	if out.JobID == "" {
		t.Fatal("expected non-empty job_id on drop")
	}

	// Bulk drop: confirm must equal "DROP N TABLES".
	tables := []string{"wp_orphan_a", "wp_orphan_b"}
	out, err = svc.DBTableAction(context.Background(), tenantID, siteID, DBTableActionInput{
		Action:  "drop",
		Tables:  tables,
		Confirm: "DROP 2 TABLES",
	})
	if err != nil {
		t.Fatalf("drop bulk: %v", err)
	}
	if out.JobID == "" {
		t.Fatal("expected non-empty job_id on bulk drop")
	}
}

// ---------------------------------------------------------------------------
// #188 — search-replace unit tests
// ---------------------------------------------------------------------------

// newSearchReplaceSvc builds a minimal *Service wired for SearchReplace tests.
// backupChecker may be nil.
func newSearchReplaceSvc(backupChecker BackupChecker) (*Service, *fakeEvents) {
	repo := &fakeRepo{configFound: true}
	events := &fakeEvents{}
	ag := &fakeAgent{}
	sites := &fakeSites{url: "https://example.com"}
	svc := NewService(repo, nil, events, nil)
	svc.SetAgentClient(ag, sites)
	if backupChecker != nil {
		svc.SetBackupChecker(backupChecker)
	}
	return svc, events
}

// TestSearchReplaceDryRunDoesNotWrite verifies that dry_run=true passes
// dry_run=true to the agent and returns rows_changed=0 regardless of
// rows_matched.
func TestSearchReplaceDryRunDoesNotWrite(t *testing.T) {
	svc, events := newSearchReplaceSvc(nil)
	tenantID, siteID := uuid.New(), uuid.New()

	out, err := svc.SearchReplace(context.Background(), tenantID, siteID, SearchReplaceInput{
		Search:  "https://old.example.com",
		Replace: "https://new.example.com",
		DryRun:  true,
	})
	if err != nil {
		t.Fatalf("SearchReplace dry_run=true: %v", err)
	}
	if out.RowsChanged != 0 {
		t.Fatalf("dry_run=true must return rows_changed=0, got %d", out.RowsChanged)
	}
	if out.RowsMatched == 0 {
		t.Fatal("expected rows_matched > 0 from fakeAgent for a dry run")
	}
	if out.JobID == "" {
		t.Fatal("expected non-empty job_id")
	}

	// SSE: completed event should be emitted.
	types := events.types()
	found := false
	for _, ty := range types {
		if ty == site.EventDbSearchReplaceCompleted {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected %s SSE event, got %v", site.EventDbSearchReplaceCompleted, types)
	}
}

// TestSearchReplaceLiveApplyWritesRows verifies that dry_run=false passes
// dry_run=false to the agent and returns rows_changed == rows_matched.
func TestSearchReplaceLiveApplyWritesRows(t *testing.T) {
	svc, events := newSearchReplaceSvc(&fakeBackupChecker{hasRecent: true})
	tenantID, siteID := uuid.New(), uuid.New()

	out, err := svc.SearchReplace(context.Background(), tenantID, siteID, SearchReplaceInput{
		Search:  "https://old.example.com",
		Replace: "https://new.example.com",
		DryRun:  false,
	})
	if err != nil {
		t.Fatalf("SearchReplace dry_run=false: %v", err)
	}
	if out.RowsChanged == 0 {
		t.Fatal("expected rows_changed > 0 for live apply")
	}
	if out.RowsChanged != out.RowsMatched {
		t.Fatalf("rows_changed=%d must equal rows_matched=%d for live apply (fakeAgent)", out.RowsChanged, out.RowsMatched)
	}

	// SSE: completed event should be emitted.
	types := events.types()
	found := false
	for _, ty := range types {
		if ty == site.EventDbSearchReplaceCompleted {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected %s SSE event, got %v", site.EventDbSearchReplaceCompleted, types)
	}
}

// TestSearchReplaceSearchTooShortReturnsDomainError verifies that a search
// string shorter than minSearchReplaceLength (3 bytes) is rejected with a
// domain validation error before the agent is called.
func TestSearchReplaceSearchTooShortReturnsDomainError(t *testing.T) {
	svc, _ := newSearchReplaceSvc(nil)
	tenantID, siteID := uuid.New(), uuid.New()

	cases := []struct {
		name   string
		search string
	}{
		{"empty string", ""},
		{"one byte", "x"},
		{"two bytes", "ab"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.SearchReplace(context.Background(), tenantID, siteID, SearchReplaceInput{
				Search:  tc.search,
				Replace: "y",
				DryRun:  true,
			})
			if err == nil {
				t.Fatal("expected error for short search, got nil")
			}
			de, ok := domain.AsDomain(err)
			if !ok {
				t.Fatalf("expected domain error, got %T: %v", err, err)
			}
			if de.Code != "search_too_short" {
				t.Fatalf("expected code search_too_short, got %q", de.Code)
			}
		})
	}
}

// TestSearchReplaceAgentOkFalseReturnsError verifies that when the agent
// returns ok=false the service surfaces it as a non-domain error (so the
// handler can map it to 200 ok=false, matching the dbTableAction pattern).
func TestSearchReplaceAgentOkFalseReturnsError(t *testing.T) {
	repo := &fakeRepo{configFound: true}
	events := &fakeEvents{}
	sites := &fakeSites{url: "https://example.com"}

	// Build a custom fakeAgent that always returns ok=false.
	type okFalseAgent struct{ fakeAgent }
	// We need a struct that satisfies AgentPerfClient but returns ok=false for
	// SearchReplace. We do this inline with an anonymous adapter.
	ag := &fakeAgent{} // base; we'll override via a wrapper Service.agent
	_ = ag             // keep linter happy

	// Use a service-level fake that wraps fakeAgent but overrides SearchReplace.
	svc := NewService(repo, nil, events, nil)

	// Wire a minimal inline client that returns ok=false.
	type agentStub struct {
		AgentPerfClient
	}

	// Build the service with the standard fakeAgent (which returns ok=true),
	// then verify the ok=false path via a dedicated sub-service with a
	// purpose-built fake.
	innerRepo := &fakeRepo{configFound: true}
	innerEvents := &fakeEvents{}
	innerSvc := NewService(innerRepo, nil, innerEvents, nil)
	innerSvc.agent = &rejectionAgent{}
	innerSvc.sites = sites

	tenantID, siteID := uuid.New(), uuid.New()
	_, err := innerSvc.SearchReplace(context.Background(), tenantID, siteID, SearchReplaceInput{
		Search:  "https://old.example.com",
		Replace: "https://new.example.com",
		DryRun:  true,
	})
	if err == nil {
		t.Fatal("expected error when agent returns ok=false, got nil")
	}
	// Must NOT be a domain error — it is surfaced by the handler as 200 ok=false.
	if _, isDomain := domain.AsDomain(err); isDomain {
		t.Fatalf("agent ok=false must produce a non-domain error, got domain error: %v", err)
	}

	// SSE: failed event should be emitted.
	types := innerEvents.types()
	found := false
	for _, ty := range types {
		if ty == site.EventDbSearchReplaceFailed {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected %s SSE event, got %v", site.EventDbSearchReplaceFailed, types)
	}

	// Prevent svc from being a "declared and not used" issue.
	_ = svc
}

// rejectionAgent is a fakeAgent override that returns ok=false for SearchReplace.
type rejectionAgent struct{ fakeAgent }

func (a *rejectionAgent) SearchReplace(_ context.Context, _ uuid.UUID, _ string, req agentcmd.SearchReplaceRequest) (agentcmd.SearchReplaceResult, error) {
	return agentcmd.SearchReplaceResult{OK: false, JobID: req.JobID, Detail: "search must be at least 3 bytes"}, nil
}

// TestSearchReplaceBackupWarningWhenNoRecentBackup verifies that the advisory
// X-Backup-Warning is surfaced in BackupWarning when dry_run=false and no
// recent backup is found. The call must NOT fail.
func TestSearchReplaceBackupWarningWhenNoRecentBackup(t *testing.T) {
	svc, _ := newSearchReplaceSvc(&fakeBackupChecker{hasRecent: false})
	tenantID, siteID := uuid.New(), uuid.New()

	out, err := svc.SearchReplace(context.Background(), tenantID, siteID, SearchReplaceInput{
		Search:  "https://old.example.com",
		Replace: "https://new.example.com",
		DryRun:  false,
	})
	if err != nil {
		t.Fatalf("SearchReplace with no recent backup: %v", err)
	}
	if out.BackupWarning == "" {
		t.Fatal("expected non-empty BackupWarning when no recent backup found")
	}
}

// TestSearchReplaceNoBackupWarningForDryRun verifies that dry_run=true never
// emits a backup advisory, even when no recent backup exists. Dry runs are
// read-only so no advisory is needed.
func TestSearchReplaceNoBackupWarningForDryRun(t *testing.T) {
	svc, _ := newSearchReplaceSvc(&fakeBackupChecker{hasRecent: false})
	tenantID, siteID := uuid.New(), uuid.New()

	out, err := svc.SearchReplace(context.Background(), tenantID, siteID, SearchReplaceInput{
		Search:  "https://old.example.com",
		Replace: "https://new.example.com",
		DryRun:  true,
	})
	if err != nil {
		t.Fatalf("SearchReplace dry_run=true with no backup: %v", err)
	}
	if out.BackupWarning != "" {
		t.Fatalf("dry_run=true must not emit backup advisory, got %q", out.BackupWarning)
	}
}

// TestSearchReplaceNoBackupWarningWhenRecentExists verifies that the advisory
// is NOT set when a recent backup exists.
func TestSearchReplaceNoBackupWarningWhenRecentExists(t *testing.T) {
	svc, _ := newSearchReplaceSvc(&fakeBackupChecker{hasRecent: true})
	tenantID, siteID := uuid.New(), uuid.New()

	out, err := svc.SearchReplace(context.Background(), tenantID, siteID, SearchReplaceInput{
		Search:  "https://old.example.com",
		Replace: "https://new.example.com",
		DryRun:  false,
	})
	if err != nil {
		t.Fatalf("SearchReplace with recent backup: %v", err)
	}
	if out.BackupWarning != "" {
		t.Fatalf("expected empty BackupWarning when recent backup exists, got %q", out.BackupWarning)
	}
}

// TestSearchReplaceSSEFailedOnTransportError verifies that a transport error
// from the agent (not ok=false, but an actual error) emits the failed SSE
// event and returns an error.
func TestSearchReplaceSSEFailedOnTransportError(t *testing.T) {
	repo := &fakeRepo{configFound: true}
	events := &fakeEvents{}
	sites := &fakeSites{url: "https://example.com"}

	svc := NewService(repo, nil, events, nil)
	svc.agent = &transportErrorAgent{}
	svc.sites = sites

	tenantID, siteID := uuid.New(), uuid.New()
	_, err := svc.SearchReplace(context.Background(), tenantID, siteID, SearchReplaceInput{
		Search:  "https://old.example.com",
		Replace: "https://new.example.com",
		DryRun:  true,
	})
	if err == nil {
		t.Fatal("expected error on transport failure, got nil")
	}

	types := events.types()
	found := false
	for _, ty := range types {
		if ty == site.EventDbSearchReplaceFailed {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected %s SSE event on transport error, got %v", site.EventDbSearchReplaceFailed, types)
	}
}

// transportErrorAgent is a fakeAgent override that returns a transport error
// for SearchReplace.
type transportErrorAgent struct{ fakeAgent }

func (a *transportErrorAgent) SearchReplace(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.SearchReplaceRequest) (agentcmd.SearchReplaceResult, error) {
	return agentcmd.SearchReplaceResult{}, errors.New("connection refused")
}

// ---------------------------------------------------------------------------
// #189 — Database Snapshots service tests
// ---------------------------------------------------------------------------

// newDbSnapshotSvc builds a minimal *Service wired for DbSnapshot tests.
func newDbSnapshotSvc() (*Service, *fakeAgent) {
	repo := &fakeRepo{configFound: true}
	events := &fakeEvents{}
	ag := &fakeAgent{}
	sites := &fakeSites{url: "https://example.com"}
	svc := NewService(repo, nil, events, nil)
	svc.SetAgentClient(ag, sites)
	return svc, ag
}

// TestDbSnapshotListReturnsNonNilSlice verifies that action=list always
// returns a non-nil Snapshots slice (never nil — JSON marshals as [] not null).
func TestDbSnapshotListReturnsNonNilSlice(t *testing.T) {
	svc, _ := newDbSnapshotSvc()
	out, err := svc.DbSnapshot(context.Background(), uuid.New(), uuid.New(), DbSnapshotInput{
		Action: "list",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.OK {
		t.Fatalf("expected ok=true, got ok=false (detail=%q)", out.Detail)
	}
	if out.Snapshots == nil {
		t.Fatal("Snapshots must never be nil (would JSON-marshal as null)")
	}
}

// TestDbSnapshotCreatePropagatesLabelAndRetention verifies that the label and
// retention fields are forwarded to the agent verbatim.
func TestDbSnapshotCreatePropagatesLabelAndRetention(t *testing.T) {
	svc, ag := newDbSnapshotSvc()
	tenantID, siteID := uuid.New(), uuid.New()

	out, err := svc.DbSnapshot(context.Background(), tenantID, siteID, DbSnapshotInput{
		Action:    "create",
		Label:     "Before plugin upgrade",
		Retention: 7,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.OK {
		t.Fatalf("expected ok=true, got ok=false")
	}
	ag.mu.Lock()
	req := ag.lastSnapshotReq
	ag.mu.Unlock()

	if req.Label != "Before plugin upgrade" {
		t.Errorf("label: got %q, want %q", req.Label, "Before plugin upgrade")
	}
	if req.Retention != 7 {
		t.Errorf("retention: got %d, want 7", req.Retention)
	}
	if out.Snapshot == nil {
		t.Fatal("expected Snapshot entry on create, got nil")
	}
	if out.SnapshotID == "" {
		t.Fatal("expected non-empty SnapshotID convenience accessor on create")
	}
}

// TestDbSnapshotCreateDefaultRetention verifies that retention=0 (unset) is
// normalised to 5 before forwarding to the agent.
func TestDbSnapshotCreateDefaultRetention(t *testing.T) {
	svc, ag := newDbSnapshotSvc()
	_, err := svc.DbSnapshot(context.Background(), uuid.New(), uuid.New(), DbSnapshotInput{
		Action: "create",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ag.mu.Lock()
	req := ag.lastSnapshotReq
	ag.mu.Unlock()
	if req.Retention != 5 {
		t.Errorf("default retention: got %d, want 5", req.Retention)
	}
}

// TestDbSnapshotRevertPassesConfirmAndSnapshotID verifies that the confirm
// token and snapshotID are forwarded verbatim to the agent (the agent enforces
// hash_equals independently; CP must not strip or normalise the token).
func TestDbSnapshotRevertPassesConfirmAndSnapshotID(t *testing.T) {
	svc, ag := newDbSnapshotSvc()
	const snapID = "snap_aabbccddeeff001122334455"

	out, err := svc.DbSnapshot(context.Background(), uuid.New(), uuid.New(), DbSnapshotInput{
		Action:     "revert",
		SnapshotID: snapID,
		Confirm:    "REVERT",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.OK {
		t.Fatalf("expected ok=true, got ok=false (detail=%q)", out.Detail)
	}
	if out.SafetyID == "" {
		t.Fatal("expected non-empty SafetyID on revert, got empty string")
	}
	ag.mu.Lock()
	req := ag.lastSnapshotReq
	ag.mu.Unlock()
	if req.SnapshotID != snapID {
		t.Errorf("snapshot_id: got %q, want %q", req.SnapshotID, snapID)
	}
	if req.Confirm != "REVERT" {
		t.Errorf("confirm: got %q, want %q", req.Confirm, "REVERT")
	}
}

// TestDbSnapshotDeletePassesSnapshotID verifies that the snapshotID is
// forwarded correctly for the delete action.
func TestDbSnapshotDeletePassesSnapshotID(t *testing.T) {
	svc, ag := newDbSnapshotSvc()
	const snapID = "snap_112233445566778899aabbcc"

	out, err := svc.DbSnapshot(context.Background(), uuid.New(), uuid.New(), DbSnapshotInput{
		Action:     "delete",
		SnapshotID: snapID,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.OK {
		t.Fatalf("expected ok=true, got ok=false (detail=%q)", out.Detail)
	}
	ag.mu.Lock()
	req := ag.lastSnapshotReq
	ag.mu.Unlock()
	if req.SnapshotID != snapID {
		t.Errorf("snapshot_id: got %q, want %q", req.SnapshotID, snapID)
	}
}

// TestDbSnapshotTransportErrorSurfacesAsErr verifies that a transport-level
// error (network failure, non-2xx) is returned as err, NOT wrapped as ok=false.
// This follows the same contract as DBScan and DBTableAction.
func TestDbSnapshotTransportErrorSurfacesAsErr(t *testing.T) {
	svc, ag := newDbSnapshotSvc()
	ag.mu.Lock()
	ag.snapshotErr = fmt.Errorf("connection refused")
	ag.mu.Unlock()

	_, err := svc.DbSnapshot(context.Background(), uuid.New(), uuid.New(), DbSnapshotInput{
		Action: "list",
	})
	if err == nil {
		t.Fatal("expected transport error to surface as err, got nil")
	}
}

// TestSearchReplaceMinLengthBoundary verifies that a 3-byte search string is
// accepted (the minimum), and a 2-byte string is rejected.
func TestSearchReplaceMinLengthBoundary(t *testing.T) {
	svc, _ := newSearchReplaceSvc(nil)
	tenantID, siteID := uuid.New(), uuid.New()

	// 2 bytes: rejected.
	_, err := svc.SearchReplace(context.Background(), tenantID, siteID, SearchReplaceInput{
		Search: "ab", Replace: "", DryRun: true,
	})
	if err == nil {
		t.Fatal("expected error for 2-byte search, got nil")
	}

	// 3 bytes: accepted.
	_, err = svc.SearchReplace(context.Background(), tenantID, siteID, SearchReplaceInput{
		Search: "abc", Replace: "", DryRun: true,
	})
	if err != nil {
		t.Fatalf("3-byte search must be accepted, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// sanitizeEditURL
// ---------------------------------------------------------------------------

func ptr(s string) *string { return &s }

func TestSanitizeEditURL(t *testing.T) {
	tests := []struct {
		name  string
		input *string
		want  *string // nil means expect nil output
	}{
		{name: "nil input", input: nil, want: nil},
		{name: "https allowed", input: ptr("https://example.com/wp-admin/post.php?post=1&action=edit"), want: ptr("https://example.com/wp-admin/post.php?post=1&action=edit")},
		{name: "http allowed", input: ptr("http://example.com/wp-admin/post.php?post=2&action=edit"), want: ptr("http://example.com/wp-admin/post.php?post=2&action=edit")},
		{name: "javascript scheme blocked", input: ptr("javascript:alert(1)"), want: nil},
		{name: "data scheme blocked", input: ptr("data:text/html,<h1>xss</h1>"), want: nil},
		{name: "vbscript scheme blocked", input: ptr("vbscript:MsgBox(1)"), want: nil},
		{name: "ftp scheme blocked", input: ptr("ftp://example.com/file"), want: nil},
		{name: "relative path blocked", input: ptr("/wp-admin/post.php"), want: nil},
		{name: "empty string blocked", input: ptr(""), want: nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeEditURL(tc.input)
			if tc.want == nil {
				if got != nil {
					t.Errorf("expected nil, got %q", *got)
				}
				return
			}
			if got == nil {
				t.Errorf("expected %q, got nil", *tc.want)
				return
			}
			if *got != *tc.want {
				t.Errorf("expected %q, got %q", *tc.want, *got)
			}
		})
	}
}

// TestMediaCleanScanSanitizesEditURL verifies that MediaCleanScan strips
// non-http(s) edit_url values from Referenced entries before returning.
func TestMediaCleanScanSanitizesEditURL(t *testing.T) {
	jsURL := "javascript:alert(1)"
	goodURL := "https://example.com/wp-admin/post.php?post=7&action=edit"

	svc := NewService(&fakeRepo{}, nil, &fakeEvents{}, nil)
	agentSpy := &mediaCleanSanitizeAgent{
		jsURL:   &jsURL,
		goodURL: &goodURL,
	}
	svc.SetAgentClient(agentSpy, &fakeSites{url: "https://example.com"})

	out, err := svc.MediaCleanScan(context.Background(), uuid.New(), uuid.New(), MediaCleanScanInput{
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(out.Referenced) != 1 {
		t.Fatalf("expected 1 referenced entry, got %d", len(out.Referenced))
	}
	usages := out.Referenced[0].Usages
	if len(usages) != 2 {
		t.Fatalf("expected 2 usages, got %d", len(usages))
	}

	// First usage had javascript: — must be nil after sanitization.
	if usages[0].EditURL != nil {
		t.Errorf("javascript: URL should have been sanitized to nil, got %q", *usages[0].EditURL)
	}
	// Second usage had a valid https URL — must be preserved.
	if usages[1].EditURL == nil {
		t.Error("https URL should have been preserved, got nil")
	} else if *usages[1].EditURL != goodURL {
		t.Errorf("https URL: got %q, want %q", *usages[1].EditURL, goodURL)
	}
}

// mediaCleanSanitizeAgent is a minimal AgentPerfClient fake for the edit-URL
// sanitization test. All methods except MediaClean are delegated to fakeAgent.
type mediaCleanSanitizeAgent struct {
	fakeAgent
	jsURL   *string
	goodURL *string
}

func (a *mediaCleanSanitizeAgent) MediaClean(_ context.Context, _ uuid.UUID, _ string, req agentcmd.MediaCleanRequest) (agentcmd.MediaCleanResult, error) {
	if req.Action != "scan" {
		return agentcmd.MediaCleanResult{OK: false, Detail: "unexpected action"}, nil
	}
	return agentcmd.MediaCleanResult{
		OK:    true,
		Total: 0,
		Referenced: []agentcmd.MediaCleanReferenced{
			{
				ID:    42,
				Title: "test-image.jpg",
				URL:   "https://example.com/wp-content/uploads/test-image.jpg",
				Usages: []agentcmd.MediaCleanUsage{
					{Surface: "post_content", EditURL: a.jsURL},
					{Surface: "thumbnail", EditURL: a.goodURL},
				},
			},
		},
	}, nil
}

// TestGetFleetDbHealthSitesNeedingReviewNotCapped is GH #197: a site flagged
// for review (orphan candidates present) must appear in SitesNeedingReview
// even when it's far outside the top-10-by-DB-size TopSites list.
func TestGetFleetDbHealthSitesNeedingReviewNotCapped(t *testing.T) {
	const totalSites = 20
	const flaggedSites = 14

	rows := make([]FleetSiteDbSummary, 0, totalSites)
	for i := 0; i < totalSites; i++ {
		row := FleetSiteDbSummary{
			SiteID: uuid.New(),
			// Name encodes rank so we can identify entries later. Sorted
			// descending by DBSizeBytes below, mirroring the SQL's ORDER BY.
			SiteName:    fmt.Sprintf("site-%02d", i),
			DBSizeBytes: int64(totalSites-i) * 1024 * 1024, // largest first
			TableCount:  50,
		}
		// Flag the first 14 sites by rank (i=0..13) for review. Since rows
		// are constructed largest-first (i=0 is the biggest DB), TopSites
		// (capped at fleetTopN=10) only covers i=0..9 — flagged sites
		// i=10..13 fall OUTSIDE TopSites, reproducing GH #197.
		if i < flaggedSites {
			row.OrphanedOptionsCount = i + 1
			row.OrphanedCronCount = 1
		}
		rows = append(rows, row)
	}

	repo := &fakeRepo{fleetDbHealthRows: rows}
	svc := NewService(repo, nil, nil, nil)

	out, err := svc.GetFleetDbHealth(context.Background(), uuid.New(), 90)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(out.TopSites) != fleetTopN {
		t.Fatalf("TopSites: got %d entries, want %d (capped)", len(out.TopSites), fleetTopN)
	}

	if out.SitesWithOrphans != flaggedSites {
		t.Fatalf("SitesWithOrphans: got %d, want %d", out.SitesWithOrphans, flaggedSites)
	}
	if len(out.SitesNeedingReview) != flaggedSites {
		t.Fatalf("SitesNeedingReview: got %d entries, want %d (uncapped)", len(out.SitesNeedingReview), flaggedSites)
	}

	// Confirm at least one flagged site OUTSIDE the top-10-by-size window is
	// present in SitesNeedingReview but absent from TopSites — this is the
	// exact bug being fixed.
	inTopSites := make(map[uuid.UUID]bool, len(out.TopSites))
	for _, s := range out.TopSites {
		inTopSites[s.SiteID] = true
	}
	foundOutsideTop := false
	for _, s := range out.SitesNeedingReview {
		if !inTopSites[s.SiteID] {
			foundOutsideTop = true
			break
		}
	}
	if !foundOutsideTop {
		t.Fatal("expected at least one flagged site outside TopSites to be present in SitesNeedingReview")
	}

	// Sort order: descending total orphan count (options+cron), tie-break by
	// site name ascending.
	for i := 1; i < len(out.SitesNeedingReview); i++ {
		prev, cur := out.SitesNeedingReview[i-1], out.SitesNeedingReview[i]
		prevTotal := prev.OrphanedOptionsCount + prev.OrphanedCronCount
		curTotal := cur.OrphanedOptionsCount + cur.OrphanedCronCount
		if prevTotal < curTotal {
			t.Fatalf("SitesNeedingReview not sorted by total orphan count desc at index %d: %d < %d", i, prevTotal, curTotal)
		}
		if prevTotal == curTotal && prev.SiteName > cur.SiteName {
			t.Fatalf("SitesNeedingReview tie-break not sorted by name asc at index %d: %q > %q", i, prev.SiteName, cur.SiteName)
		}
	}

	// Every review entry must carry the slim 4-field shape correctly mapped
	// from the source row (spot-check by id).
	bySiteID := make(map[uuid.UUID]FleetSiteDbSummary, len(rows))
	for _, r := range rows {
		bySiteID[r.SiteID] = r
	}
	for _, review := range out.SitesNeedingReview {
		src, ok := bySiteID[review.SiteID]
		if !ok {
			t.Fatalf("SitesNeedingReview entry %s not found in source rows", review.SiteID)
		}
		if review.SiteName != src.SiteName || review.OrphanedOptionsCount != src.OrphanedOptionsCount || review.OrphanedCronCount != src.OrphanedCronCount {
			t.Fatalf("SitesNeedingReview entry mismatch for %s: got %+v, want fields from %+v", review.SiteID, review, src)
		}
	}
}
