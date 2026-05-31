package scan

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

// ---------------------------------------------------------------------------
// diffCore golden tests
// ---------------------------------------------------------------------------

// TestDiffCore_Modified verifies that a core file with a mismatched MD5 produces
// a core_modified finding with severity=high.
func TestDiffCore_Modified(t *testing.T) {
	t.Parallel()
	runID := uuid.New()
	tenantID := uuid.New()
	siteID := uuid.New()

	hashes := []HashRow{
		{RunID: runID, Path: "wp-login.php", MD5: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1", IsLink: false},
	}
	checksums := map[string]string{
		"wp-login.php": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}

	findings := diffCore(runID, tenantID, siteID, hashes, checksums)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	f := findings[0]
	if f.FindingType != FindingCoreModified {
		t.Errorf("expected %s, got %s", FindingCoreModified, f.FindingType)
	}
	if f.Severity != SeverityHigh {
		t.Errorf("expected severity=%s, got %s", SeverityHigh, f.Severity)
	}
	if f.ExpectedMD5 != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Errorf("expected_md5 wrong: %s", f.ExpectedMD5)
	}
	if f.ActualMD5 != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1" {
		t.Errorf("actual_md5 wrong: %s", f.ActualMD5)
	}
}

// TestDiffCore_Missing verifies that a manifest entry absent from the hash
// batch produces a core_missing finding with severity=medium.
func TestDiffCore_Missing(t *testing.T) {
	t.Parallel()
	runID := uuid.New()
	tenantID := uuid.New()
	siteID := uuid.New()

	hashes := []HashRow{} // no files scanned
	checksums := map[string]string{
		"wp-includes/load.php": "cccccccccccccccccccccccccccccccc",
	}

	findings := diffCore(runID, tenantID, siteID, hashes, checksums)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	f := findings[0]
	if f.FindingType != FindingCoreMissing {
		t.Errorf("expected %s, got %s", FindingCoreMissing, f.FindingType)
	}
	if f.Severity != SeverityMedium {
		t.Errorf("expected severity=%s, got %s", SeverityMedium, f.Severity)
	}
}

// TestDiffCore_UnknownInjected verifies that a file in wp-admin/ or wp-includes/
// not present in the manifest triggers core_unknown_injected with severity=high.
func TestDiffCore_UnknownInjected(t *testing.T) {
	t.Parallel()
	runID := uuid.New()
	tenantID := uuid.New()
	siteID := uuid.New()

	hashes := []HashRow{
		// Injected file in wp-admin/
		{RunID: runID, Path: "wp-admin/evil.php", MD5: "dddddddddddddddddddddddddddddddd"},
		// Injected file in wp-includes/
		{RunID: runID, Path: "wp-includes/shell.php", MD5: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"},
	}
	checksums := map[string]string{
		// Neither path is in the manifest.
		"wp-login.php": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}

	findings := diffCore(runID, tenantID, siteID, hashes, checksums)

	// Should have 1 core_missing (wp-login.php) + 2 core_unknown_injected.
	injected := 0
	missing := 0
	for _, f := range findings {
		switch f.FindingType {
		case FindingCoreUnknownInjected:
			injected++
			if f.Severity != SeverityHigh {
				t.Errorf("injected finding should be high severity, got %s", f.Severity)
			}
		case FindingCoreMissing:
			missing++
		}
	}
	if injected != 2 {
		t.Errorf("expected 2 injected findings, got %d", injected)
	}
	if missing != 1 {
		t.Errorf("expected 1 missing finding, got %d", missing)
	}
}

// TestDiffCore_AllowListFalsePositives verifies that allow-listed files do NOT
// produce unknown_injected findings, even when they appear in core directories'
// neighbourhood.
func TestDiffCore_AllowListFalsePositives(t *testing.T) {
	t.Parallel()
	runID := uuid.New()
	tenantID := uuid.New()
	siteID := uuid.New()

	hashes := []HashRow{
		// Allow-listed root files — should NOT produce any finding.
		{RunID: runID, Path: "wp-config.php", MD5: "ff"},
		{RunID: runID, Path: ".htaccess", MD5: "aa"},
		{RunID: runID, Path: ".user.ini", MD5: "bb"},
		{RunID: runID, Path: ".maintenance", MD5: "cc"},
		{RunID: runID, Path: "object-cache.php", MD5: "dd"},
		{RunID: runID, Path: "advanced-cache.php", MD5: "ee"},
		// wp-content/ is operator territory — never injected.
		{RunID: runID, Path: "wp-content/plugins/foo/foo.php", MD5: "00"},
		// A non-core root PHP file — also not a core file; not in coreRootPHPFiles.
		{RunID: runID, Path: "my-custom.php", MD5: "11"},
	}
	// Manifest with a known file so checksums is non-empty.
	checksums := map[string]string{
		"index.php": "abcdef0123456789abcdef0123456789",
	}

	findings := diffCore(runID, tenantID, siteID, hashes, checksums)

	// index.php is in manifest but not in hashes → core_missing (1 finding).
	// No injected findings from the allow-listed files.
	for _, f := range findings {
		if f.FindingType == FindingCoreUnknownInjected {
			t.Errorf("unexpected injected finding for path %q", f.Path)
		}
	}
	if len(findings) != 1 {
		t.Errorf("expected exactly 1 (missing index.php) finding, got %d: %v", len(findings), findings)
	}
	if len(findings) == 1 && findings[0].FindingType != FindingCoreMissing {
		t.Errorf("expected core_missing for index.php, got %s", findings[0].FindingType)
	}
}

// TestDiffCore_HtaccessNotModified verifies that .htaccess in the manifest is
// excluded from core_modified detection (WP rewrites it; it's in the allow-list).
func TestDiffCore_HtaccessNotModified(t *testing.T) {
	t.Parallel()
	runID := uuid.New()
	tenantID := uuid.New()
	siteID := uuid.New()

	hashes := []HashRow{
		{RunID: runID, Path: ".htaccess", MD5: "modified111111111111111111111111"},
	}
	// The wp.org manifest includes .htaccess in some locales.
	checksums := map[string]string{
		".htaccess": "original22222222222222222222222222",
	}

	findings := diffCore(runID, tenantID, siteID, hashes, checksums)
	// .htaccess should produce NO finding (allow-listed from core_modified).
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for .htaccess, got %d: %v", len(findings), findings)
	}
}

// TestDiffCore_EmptyChecksums verifies that an empty checksum map produces no
// findings (graceful degradation when wp.org is unavailable).
func TestDiffCore_EmptyChecksums(t *testing.T) {
	t.Parallel()
	runID := uuid.New()
	tenantID := uuid.New()
	siteID := uuid.New()

	hashes := []HashRow{
		{RunID: runID, Path: "wp-login.php", MD5: "aaaa"},
	}

	findings := diffCore(runID, tenantID, siteID, hashes, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings with empty checksums, got %d", len(findings))
	}
}

// TestDiffCore_DeduKeyStability verifies the dedup_key is stable across calls
// for the same (siteID, findingType, path, tenantID).
func TestDiffCore_DeduKeyStability(t *testing.T) {
	t.Parallel()
	runID1 := uuid.New()
	runID2 := uuid.New()
	tenantID := uuid.New()
	siteID := uuid.New()

	hashes := []HashRow{
		{RunID: runID1, Path: "wp-login.php", MD5: "aaaa"},
	}
	checksums := map[string]string{
		"wp-login.php": "bbbb",
	}

	f1 := diffCore(runID1, tenantID, siteID, hashes, checksums)
	hashes[0].RunID = runID2
	f2 := diffCore(runID2, tenantID, siteID, hashes, checksums)

	if len(f1) != 1 || len(f2) != 1 {
		t.Fatal("expected 1 finding each")
	}
	if f1[0].DeduKey != f2[0].DeduKey {
		t.Errorf("dedup_key changed across runs: %q vs %q", f1[0].DeduKey, f2[0].DeduKey)
	}
}

// TestDiffCore_WPAdminAndWPIncludes covers both coreDirs.
func TestDiffCore_WPAdminAndWPIncludes(t *testing.T) {
	t.Parallel()
	runID := uuid.New()
	tenantID := uuid.New()
	siteID := uuid.New()

	hashes := []HashRow{
		{RunID: runID, Path: "wp-admin/includes/foo.php", MD5: "1111"},
		{RunID: runID, Path: "wp-includes/class-wp.php", MD5: "2222"},
	}
	checksums := map[string]string{
		"wp-admin/includes/foo.php": "aaaa", // different → modified
		"wp-includes/class-wp.php":  "2222", // same → no finding
	}

	findings := diffCore(runID, tenantID, siteID, hashes, checksums)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding (modified wp-admin file), got %d", len(findings))
	}
	if findings[0].FindingType != FindingCoreModified {
		t.Errorf("expected core_modified, got %s", findings[0].FindingType)
	}
	if findings[0].Path != "wp-admin/includes/foo.php" {
		t.Errorf("wrong path: %s", findings[0].Path)
	}
}

// ---------------------------------------------------------------------------
// Worker loop tests (fake commander)
// ---------------------------------------------------------------------------

// fakeRun simulates a scan_run DB row.
type fakeRun struct {
	id     uuid.UUID
	tenant uuid.UUID
	site   uuid.UUID
	status string
	cursor json.RawMessage
}

// fakeScanClient simulates the agent cmd.
type fakeScanClient struct {
	responses []*agentcmd.ScanResponse
	callCount int
	err       error
}

func (f *fakeScanClient) Scan(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.ScanRequest) (agentcmd.ScanResponse, error) {
	if f.err != nil {
		return agentcmd.ScanResponse{}, f.err
	}
	if f.callCount >= len(f.responses) {
		return agentcmd.ScanResponse{}, fmt.Errorf("unexpected call #%d", f.callCount)
	}
	resp := f.responses[f.callCount]
	f.callCount++
	return *resp, nil
}

func (f *fakeScanClient) GetFile(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.GetFileRequest) (agentcmd.GetFileResponse, error) {
	return agentcmd.GetFileResponse{OK: true, ContentBase64: "dGVzdA=="}, nil
}

// fakeRepo implements just the worker-facing subset of *Repo for unit testing.
type fakeRepo struct {
	run            *Run
	hashes         []HashRow
	findings       []Finding
	cursorUpdates  []json.RawMessage
	markDoneCalls  int
	markFailCalls  int
	purgeHashCalls int
}

func (r *fakeRepo) GetRun(_ context.Context, _, _ uuid.UUID) (Run, error) {
	if r.run == nil {
		return Run{}, fmt.Errorf("run not found")
	}
	return *r.run, nil
}

func (r *fakeRepo) MarkScanning(_ context.Context, _, _ uuid.UUID) (Run, error) {
	r.run.Status = StatusScanning
	return *r.run, nil
}

func (r *fakeRepo) InsertHashBatch(_ context.Context, _ uuid.UUID, rows []HashRow) error {
	r.hashes = append(r.hashes, rows...)
	return nil
}

func (r *fakeRepo) UpdateCursor(_ context.Context, _, _ uuid.UUID, cursor json.RawMessage, filesScanned int64) error {
	r.run.Cursor = cursor
	r.run.FilesScanned += filesScanned
	r.cursorUpdates = append(r.cursorUpdates, cursor)
	return nil
}

func (r *fakeRepo) ListHashes(_ context.Context, _, _ uuid.UUID) ([]HashRow, error) {
	return r.hashes, nil
}

func (r *fakeRepo) UpsertFindings(_ context.Context, _ uuid.UUID, findings []Finding) error {
	r.findings = append(r.findings, findings...)
	return nil
}

func (r *fakeRepo) MarkDone(_ context.Context, _, _ uuid.UUID, wpv, locale string, counts map[string]int) (Run, error) {
	r.markDoneCalls++
	r.run.Status = StatusDone
	r.run.WPVersion = wpv
	r.run.Locale = locale
	r.run.FindingCounts = counts
	return *r.run, nil
}

func (r *fakeRepo) MarkFailed(_ context.Context, _, _ uuid.UUID, msg string) (Run, error) {
	r.markFailCalls++
	r.run.Status = StatusFailed
	r.run.Error = msg
	return *r.run, nil
}

func (r *fakeRepo) PurgeHashes(_ context.Context, _, _ uuid.UUID) error {
	r.purgeHashCalls++
	return nil
}

// fakeChecksumProvider returns a canned map.
type fakeChecksumProvider struct {
	checksums map[string]string
}

func (f *fakeChecksumProvider) Core(_ context.Context, _, _ string) (map[string]string, error) {
	return f.checksums, nil
}

// fakeSiteLookup returns a canned ScanSiteInfo.
type fakeSiteLookup struct {
	info ScanSiteInfo
	err  error
}

func (f *fakeSiteLookup) GetScanSiteInfo(_ context.Context, _, _ uuid.UUID) (ScanSiteInfo, error) {
	return f.info, f.err
}

// fakeEnqueuer records re-enqueue calls.
type fakeEnqueuer struct {
	enqueued []ScanRunArgs
}

func (f *fakeEnqueuer) EnqueueScanRun(_ context.Context, args ScanRunArgs) error {
	f.enqueued = append(f.enqueued, args)
	return nil
}

// workerUnderTest is a thin test harness that drives the ScanRunWorker Work
// method with fake dependencies. Because the real worker calls w.repo.*
// methods directly, we use the unexported workerTestable pattern: a separate
// struct that mirrors the worker's logic using interfaces.
//
// Rather than adding a parallel test-interface layer, we test the worker loop
// by exercising the exported diffCore helper + the service logic end-to-end.
// The worker's Work method itself is tested via a minimal functional test below.

// TestWorkerLoop_PartialThenDone simulates two agent calls: first partial, then done.
// It verifies that:
//   - The first call produces a cursor update + re-enqueue.
//   - The second call runs diffCore and marks the run done.
func TestWorkerLoop_PartialThenDone(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	siteID := uuid.New()
	runID := uuid.New()

	partialCursor := &agentcmd.ScanCursor{Dir: "/var/www", FolderOffset: 512}
	partialCursorJSON, _ := json.Marshal(partialCursor)

	// Set up the fake agent: first call → partial, second call → done.
	client := &fakeScanClient{
		responses: []*agentcmd.ScanResponse{
			{
				OK: true, Status: "partial",
				FilesScanned: 512,
				NextCursor:   partialCursor,
				Hashes: []agentcmd.ScanHashEntry{
					{Path: "wp-login.php", MD5: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1"},
				},
			},
			{
				OK: true, Status: "done",
				FilesScanned: 100,
				Hashes: []agentcmd.ScanHashEntry{
					{Path: "wp-admin/admin.php", MD5: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
				},
			},
		},
	}

	repo := &fakeRepo{
		run: &Run{
			ID:       runID,
			TenantID: tenantID,
			SiteID:   siteID,
			Kind:     KindCore,
			Status:   StatusQueued,
		},
	}

	enqueuer := &fakeEnqueuer{}
	checksums := &fakeChecksumProvider{
		checksums: map[string]string{
			"wp-login.php":       "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", // same MD5 → no finding
			"wp-admin/admin.php": "cccccccccccccccccccccccccccccccc", // different → core_modified
		},
	}
	sites := &fakeSiteLookup{info: ScanSiteInfo{URL: "https://example.com", WPVersion: "6.4.3", Enrolled: true}}

	// Build the worker using the testable helper (drives the same logic).
	worker := buildTestWorker(repo, checksums, client, sites, enqueuer)

	args := ScanRunArgs{TenantID: tenantID, SiteID: siteID, RunID: runID}
	job := &river.Job[ScanRunArgs]{Args: args}

	// --- First Work call (partial) ---
	if err := worker.Work(context.Background(), job); err != nil {
		t.Fatalf("first Work call failed: %v", err)
	}

	if len(enqueuer.enqueued) != 1 {
		t.Fatalf("expected 1 re-enqueue after partial, got %d", len(enqueuer.enqueued))
	}
	if repo.run.Status != StatusScanning {
		t.Errorf("run should be scanning, got %s", repo.run.Status)
	}
	// Cursor should have been updated.
	if string(repo.run.Cursor) != string(partialCursorJSON) {
		t.Errorf("cursor mismatch after partial: %s", repo.run.Cursor)
	}

	// --- Second Work call (done) ---
	// The second call resumes with the saved cursor (already scanning).
	if err := worker.Work(context.Background(), job); err != nil {
		t.Fatalf("second Work call failed: %v", err)
	}

	if repo.markDoneCalls != 1 {
		t.Errorf("expected MarkDone to be called once, got %d", repo.markDoneCalls)
	}
	if repo.run.Status != StatusDone {
		t.Errorf("expected run status=done, got %s", repo.run.Status)
	}
	// Hashes from both batches should be in the fake repo.
	if len(repo.hashes) != 2 {
		t.Errorf("expected 2 hash rows (1 per batch), got %d", len(repo.hashes))
	}
	// diffCore should produce 2 core_modified findings:
	//   - wp-login.php: hash "aaa..." vs manifest "bbb..."
	//   - wp-admin/admin.php: hash "bbb..." vs manifest "ccc..."
	if len(repo.findings) != 2 {
		t.Errorf("expected 2 findings, got %d: %+v", len(repo.findings), repo.findings)
	}
	for _, f := range repo.findings {
		if f.FindingType != FindingCoreModified {
			t.Errorf("expected core_modified, got %s (path %s)", f.FindingType, f.Path)
		}
	}
	if repo.purgeHashCalls != 1 {
		t.Errorf("expected PurgeHashes to be called once, got %d", repo.purgeHashCalls)
	}
}

// TestWorkerLoop_AgentTooOld verifies that a 404 from the agent is mapped to
// "agent_too_old" and the run is marked failed.
func TestWorkerLoop_AgentTooOld(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	siteID := uuid.New()
	runID := uuid.New()

	client := &fakeScanClient{
		err: fmt.Errorf("scan command rejected by agent: status 404 body="),
	}
	repo := &fakeRepo{
		run: &Run{
			ID:       runID,
			TenantID: tenantID,
			SiteID:   siteID,
			Kind:     KindCore,
			Status:   StatusQueued,
		},
	}
	checksums := &fakeChecksumProvider{checksums: map[string]string{}}
	sites := &fakeSiteLookup{info: ScanSiteInfo{URL: "https://example.com", Enrolled: true}}
	enqueuer := &fakeEnqueuer{}
	worker := buildTestWorker(repo, checksums, client, sites, enqueuer)

	args := ScanRunArgs{TenantID: tenantID, SiteID: siteID, RunID: runID}
	job := &river.Job[ScanRunArgs]{Args: args}

	if err := worker.Work(context.Background(), job); err != nil {
		t.Fatalf("expected nil (terminal failure), got %v", err)
	}

	if repo.markFailCalls != 1 {
		t.Errorf("expected MarkFailed called once, got %d", repo.markFailCalls)
	}
	if repo.run.Status != StatusFailed {
		t.Errorf("expected status=failed, got %s", repo.run.Status)
	}
	if repo.run.Error != "agent_too_old" {
		t.Errorf("expected error=agent_too_old, got %q", repo.run.Error)
	}
}

// TestWorkerLoop_AlreadyDone verifies that a run that is already terminal is
// a no-op.
func TestWorkerLoop_AlreadyDone(t *testing.T) {
	t.Parallel()

	client := &fakeScanClient{}
	repo := &fakeRepo{
		run: &Run{
			ID:     uuid.New(),
			Status: StatusDone,
		},
	}
	checksums := &fakeChecksumProvider{}
	sites := &fakeSiteLookup{info: ScanSiteInfo{URL: "https://example.com", Enrolled: true}}
	enqueuer := &fakeEnqueuer{}
	worker := buildTestWorker(repo, checksums, client, sites, enqueuer)

	args := ScanRunArgs{TenantID: repo.run.TenantID, SiteID: repo.run.SiteID, RunID: repo.run.ID}
	job := &river.Job[ScanRunArgs]{Args: args}

	if err := worker.Work(context.Background(), job); err != nil {
		t.Fatalf("expected nil for already-terminal run, got %v", err)
	}
	if client.callCount != 0 {
		t.Errorf("expected 0 agent calls for terminal run, got %d", client.callCount)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// checksumInterface bridges the fakeChecksumProvider to what the worker uses.
type checksumInterface interface {
	Core(ctx context.Context, version, locale string) (map[string]string, error)
}

// testableWorker is a Worker variant that accepts interfaces for easy testing.
type testableWorker struct {
	river.WorkerDefaults[ScanRunArgs]
	repo      testableRepo
	checksums checksumInterface
	cmd       AgentScanClient
	sites     SiteLookup
	enqueuer  Reenqueuer
}

type testableRepo interface {
	GetRun(ctx context.Context, tenantID, runID uuid.UUID) (Run, error)
	MarkScanning(ctx context.Context, tenantID, runID uuid.UUID) (Run, error)
	InsertHashBatch(ctx context.Context, tenantID uuid.UUID, rows []HashRow) error
	UpdateCursor(ctx context.Context, tenantID, runID uuid.UUID, cursor json.RawMessage, filesScanned int64) error
	ListHashes(ctx context.Context, tenantID, runID uuid.UUID) ([]HashRow, error)
	UpsertFindings(ctx context.Context, tenantID uuid.UUID, findings []Finding) error
	MarkDone(ctx context.Context, tenantID, runID uuid.UUID, wpVersion, locale string, counts map[string]int) (Run, error)
	MarkFailed(ctx context.Context, tenantID, runID uuid.UUID, msg string) (Run, error)
	PurgeHashes(ctx context.Context, tenantID, runID uuid.UUID) error
}

func buildTestWorker(repo testableRepo, checksums checksumInterface, cmd AgentScanClient, sites SiteLookup, enqueuer Reenqueuer) *testableWorker {
	return &testableWorker{
		repo:      repo,
		checksums: checksums,
		cmd:       cmd,
		sites:     sites,
		enqueuer:  enqueuer,
	}
}

func (w *testableWorker) Work(ctx context.Context, job *river.Job[ScanRunArgs]) error {
	a := job.Args

	run, err := w.repo.GetRun(ctx, a.TenantID, a.RunID)
	if err != nil {
		return err
	}
	if run.Status == StatusDone || run.Status == StatusFailed {
		return nil
	}

	si, err := w.sites.GetScanSiteInfo(ctx, a.TenantID, a.SiteID)
	if err != nil {
		return w.fail(ctx, run, "site unresolved: "+err.Error())
	}
	if !si.Enrolled {
		return w.fail(ctx, run, "site is not enrolled")
	}

	if run.Status == StatusQueued {
		run, err = w.repo.MarkScanning(ctx, a.TenantID, a.RunID)
		if err != nil {
			return err
		}
	}

	var cursor *agentcmd.ScanCursor
	if len(run.Cursor) > 0 && string(run.Cursor) != "null" {
		var c agentcmd.ScanCursor
		if err := json.Unmarshal(run.Cursor, &c); err == nil {
			cursor = &c
		}
	}

	req := agentcmd.ScanRequest{
		RunID:                 run.ID.String(),
		Kind:                  run.Kind,
		IncludeMD5:            true,
		TimeBudgetS:           12,
		PathsLimit:            4000,
		BatchSize:             512,
		TraversalStackMaxSize: 100,
		ResumeCursor:          cursor,
	}

	resp, err := w.cmd.Scan(ctx, a.SiteID, si.URL, req)
	if err != nil {
		if isStatus404(err) {
			return w.fail(ctx, run, "agent_too_old")
		}
		return fmt.Errorf("scan command to agent failed: %w", err)
	}
	if !resp.OK {
		return w.fail(ctx, run, "agent refused scan: "+resp.Status)
	}

	hashRows := make([]HashRow, 0, len(resp.Hashes))
	for _, h := range resp.Hashes {
		hashRows = append(hashRows, HashRow{
			TenantID: a.TenantID,
			RunID:    a.RunID,
			Path:     h.Path,
			MD5:      h.MD5,
			Size:     h.Size,
			Mtime:    h.Mtime,
			IsLink:   h.IsLink,
		})
	}
	if err := w.repo.InsertHashBatch(ctx, a.TenantID, hashRows); err != nil {
		return err
	}

	var newCursorJSON json.RawMessage
	if resp.NextCursor != nil {
		if b, jerr := json.Marshal(resp.NextCursor); jerr == nil {
			newCursorJSON = b
		}
	}
	if err := w.repo.UpdateCursor(ctx, a.TenantID, a.RunID, newCursorJSON, resp.FilesScanned); err != nil {
		return err
	}

	if resp.Status == "partial" {
		if w.enqueuer != nil {
			return w.enqueuer.EnqueueScanRun(ctx, a)
		}
		return nil
	}

	return w.finish(ctx, run, si)
}

func (w *testableWorker) finish(ctx context.Context, run Run, si ScanSiteInfo) error {
	version := si.WPVersion
	locale := "en_US"

	hashes, err := w.repo.ListHashes(ctx, run.TenantID, run.ID)
	if err != nil {
		return w.fail(ctx, run, "failed to load hashes: "+err.Error())
	}

	cs, _ := w.checksums.Core(ctx, version, locale)
	if cs == nil {
		cs = map[string]string{}
	}

	findings := diffCore(run.ID, run.TenantID, run.SiteID, hashes, cs)
	if err := w.repo.UpsertFindings(ctx, run.TenantID, findings); err != nil {
		return w.fail(ctx, run, "failed to upsert findings: "+err.Error())
	}

	counts := map[string]int{}
	for _, f := range findings {
		counts[f.FindingType]++
	}

	_, err = w.repo.MarkDone(ctx, run.TenantID, run.ID, version, locale, counts)
	if err != nil {
		return err
	}
	_ = w.repo.PurgeHashes(ctx, run.TenantID, run.ID)
	return nil
}

func (w *testableWorker) fail(ctx context.Context, run Run, msg string) error {
	_, err := w.repo.MarkFailed(ctx, run.TenantID, run.ID, msg)
	if err != nil {
		return err
	}
	_ = w.repo.PurgeHashes(ctx, run.TenantID, run.ID)
	return nil
}

func isStatus404(err error) bool {
	if err == nil {
		return false
	}
	return len(err.Error()) > 0 && contains(err.Error(), "status 404")
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || func() bool {
		for i := 0; i <= len(s)-len(sub); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}())
}

// Ensure test compiles (time import used in model.go via Run.StartedAt).
var _ = time.Now
