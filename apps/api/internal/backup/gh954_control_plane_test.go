package backup

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

type gh954Repo struct {
	*watchdogFakeRepo
	settings       map[uuid.UUID]SiteBackupSettings
	settingsErr    error
	settingsWrites int
}

func (r *gh954Repo) GetBackupSettings(_ context.Context, tenantID, siteID uuid.UUID) (SiteBackupSettings, error) {
	if r.settingsErr != nil {
		return SiteBackupSettings{}, r.settingsErr
	}
	row, ok := r.settings[siteID]
	if !ok || row.TenantID != tenantID {
		return SiteBackupSettings{}, domain.NotFound("backup_settings_not_found", "no settings")
	}
	return row, nil
}

func (r *gh954Repo) UpsertBackupSettings(_ context.Context, tenantID uuid.UUID, in SiteBackupSettings) (SiteBackupSettings, error) {
	r.settingsWrites++
	in.TenantID = tenantID
	r.settings[in.SiteID] = in
	return in, nil
}

func (r *gh954Repo) CreateSnapshot(_ context.Context, in CreateSnapshotInput) (Snapshot, error) {
	snap := Snapshot{
		ID: uuid.New(), TenantID: in.TenantID, SiteID: in.SiteID,
		Kind: in.Kind, Status: StatusPending, AgeRecipient: in.AgeRecipient,
		IsIncremental: in.IsIncremental, Generation: in.Generation,
		ParentSnapshotID: in.ParentSnapshotID, BaseSnapshotID: in.BaseSnapshotID,
		ChainID: in.ChainID, DestinationID: in.DestinationID,
	}
	r.setSnapshot(snap)
	return snap, nil
}

type gh954Delivery struct {
	url           string
	authorization string
	body          []byte
}

type gh954Transport struct {
	deliveries []gh954Delivery
	err        error
	beforeSend func()
}

func (d *gh954Transport) Do(*http.Request) (*http.Response, error) {
	return nil, errors.New("signed commands must use DoOnce")
}

func (d *gh954Transport) DoOnce(req *http.Request) (*http.Response, error) {
	if d.beforeSend != nil {
		d.beforeSend()
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	d.deliveries = append(d.deliveries, gh954Delivery{
		url: req.URL.String(), authorization: req.Header.Get("Authorization"), body: body,
	})
	if d.err != nil {
		return nil, d.err
	}
	return &http.Response{
		StatusCode: http.StatusOK, Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(`{"ok":true}`)),
	}, nil
}

func gh954Fixture(t *testing.T) (*gh954Repo, *Service, *recordingEnqueuer, *gh954Transport, *BackupWorker) {
	t.Helper()
	repo := &gh954Repo{watchdogFakeRepo: newWatchdogFakeRepo(), settings: make(map[uuid.UUID]SiteBackupSettings)}
	enq := &recordingEnqueuer{}
	svc := NewService(repo, fakeWorkerSiteLookup{}, enq, nil, fakeClock{t: time.Now()}, Config{})
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	signer, err := agentcmd.NewSigner(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	transport := &gh954Transport{}
	worker := NewBackupWorker(svc, agentcmd.NewClient(transport, signer), nil, nil, "https://cp.example.com", 0)
	return repo, svc, enq, transport, worker
}

func gh954SaveContents(t *testing.T, svc *Service, tenantID, siteID uuid.UUID, paths, extensions []string) {
	t.Helper()
	_, err := svc.PutBackupContents(context.Background(), PutBackupContentsInput{
		TenantID: tenantID, SiteID: siteID, ExcludePaths: paths, ExcludeExtensions: extensions,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func gh954AssertExclusions(t *testing.T, delivery gh954Delivery, paths, extensions []string) {
	t.Helper()
	var body struct {
		ExcludePaths      []string `json:"exclude_paths"`
		ExcludeExtensions []string `json:"exclude_extensions"`
	}
	if err := json.Unmarshal(delivery.body, &body); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(body.ExcludePaths, paths) || !reflect.DeepEqual(body.ExcludeExtensions, extensions) {
		t.Fatalf("command exclusions = %v / %v; want %v / %v; body=%s", body.ExcludePaths, body.ExcludeExtensions, paths, extensions, delivery.body)
	}
	t.Logf("signed POST %s body=%s", delivery.url, delivery.body)
}

func TestSavedExclusionsReachManualAndScheduledCommands(t *testing.T) {
	for _, trigger := range []string{"manual", "scheduled"} {
		for _, kind := range []string{KindFull, KindFiles, KindDB} {
			t.Run(trigger+"/"+kind, func(t *testing.T) {
				repo, svc, enq, transport, worker := gh954Fixture(t)
				tenantID, siteID := uuid.New(), uuid.New()
				paths, extensions := []string{"2024", "2025", "2026"}, []string{"log"}
				gh954SaveContents(t, svc, tenantID, siteID, paths, extensions)
				stored, err := svc.GetBackupSettings(context.Background(), tenantID, siteID)
				if err != nil || !reflect.DeepEqual(stored.ExcludePaths, paths) || !reflect.DeepEqual(stored.ExcludeExtensions, extensions) {
					t.Fatalf("saved settings = %+v, error=%v", stored, err)
				}
				if trigger == "manual" {
					_, err = svc.CreateBackup(context.Background(), tenantID, siteID, uuid.New(), kind)
				} else {
					err = svc.EnqueueScheduledBackup(context.Background(), Schedule{
						ID: uuid.New(), TenantID: tenantID, SiteID: siteID, Kind: kind,
					})
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(enq.plainCalls) != 1 {
					t.Fatalf("queued snapshots = %v", enq.plainCalls)
				}
				snapshotID := enq.plainCalls[0]
				if repo.snapshots[snapshotID].SiteID != siteID {
					t.Fatal("wrong snapshot site")
				}
				if err := worker.Work(context.Background(), &river.Job[BackupArgs]{
					JobRow: newJobRow(1, 25), Args: BackupArgs{TenantID: tenantID, SnapshotID: snapshotID},
				}); err != nil {
					t.Fatal(err)
				}
				if len(transport.deliveries) != 1 {
					t.Fatalf("delivery count = %d", len(transport.deliveries))
				}
				gh954AssertExclusions(t, transport.deliveries[0], paths, extensions)
			})
		}
	}
}

func TestSettingsSavedAfterQueueReachDispatch(t *testing.T) {
	_, svc, _, transport, worker := gh954Fixture(t)
	tenantID, siteID := uuid.New(), uuid.New()
	snap, err := svc.CreateBackup(context.Background(), tenantID, siteID, uuid.New(), KindFull)
	if err != nil {
		t.Fatal(err)
	}
	gh954SaveContents(t, svc, tenantID, siteID, []string{"2024"}, []string{"log"})
	if err := worker.Work(context.Background(), &river.Job[BackupArgs]{
		JobRow: newJobRow(1, 25), Args: BackupArgs{TenantID: tenantID, SnapshotID: snap.ID},
	}); err != nil {
		t.Fatal(err)
	}
	if len(transport.deliveries) != 1 {
		t.Fatalf("delivery count = %d", len(transport.deliveries))
	}
	gh954AssertExclusions(t, transport.deliveries[0], []string{"2024"}, []string{"log"})
}

func TestIncrementalCommandCarriesSavedExclusions(t *testing.T) {
	repo, svc, _, transport, worker := gh954Fixture(t)
	tenantID, siteID := uuid.New(), uuid.New()
	gh954SaveContents(t, svc, tenantID, siteID, []string{"2024"}, []string{"log"})
	snap, err := repo.CreateSnapshot(context.Background(), CreateSnapshotInput{
		TenantID: tenantID, SiteID: siteID, Kind: KindFull, AgeRecipient: "age1test", IsIncremental: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Work(context.Background(), &river.Job[BackupArgs]{
		JobRow: newJobRow(1, 25), Args: BackupArgs{TenantID: tenantID, SnapshotID: snap.ID, IsIncremental: true},
	}); err != nil {
		t.Fatal(err)
	}
	if len(transport.deliveries) != 1 {
		t.Fatalf("delivery count = %d", len(transport.deliveries))
	}
	gh954AssertExclusions(t, transport.deliveries[0], []string{"2024"}, []string{"log"})
}

func TestDispatchSettingsReadErrorIsRetried(t *testing.T) {
	repo, svc, _, transport, worker := gh954Fixture(t)
	tenantID, siteID := uuid.New(), uuid.New()
	gh954SaveContents(t, svc, tenantID, siteID, []string{"2024"}, []string{"log"})
	snap, err := svc.CreateBackup(context.Background(), tenantID, siteID, uuid.New(), KindFull)
	if err != nil {
		t.Fatal(err)
	}
	repo.settingsErr = errors.New("settings database unavailable")
	readErr := repo.settingsErr
	err = worker.Work(context.Background(), &river.Job[BackupArgs]{
		JobRow: newJobRow(1, 25), Args: BackupArgs{TenantID: tenantID, SnapshotID: snap.ID},
	})
	if err == nil {
		t.Error("settings read failure must be retryable, got nil")
	} else if !errors.Is(err, readErr) {
		t.Errorf("dispatch error = %v, want original settings error %v", err, readErr)
	}
	if len(transport.deliveries) != 0 {
		t.Errorf("settings read failure dispatched an unfiltered backup: %s", transport.deliveries[0].body)
	}
	if got := repo.mustGet(t, snap.ID); got.Status != StatusRunning {
		t.Errorf("snapshot status = %q, want running for retry", got.Status)
	}
}

func TestMissingSettingsAllowsDefaultBackup(t *testing.T) {
	_, svc, _, transport, worker := gh954Fixture(t)
	tenantID, siteID := uuid.New(), uuid.New()
	snap, err := svc.CreateBackup(context.Background(), tenantID, siteID, uuid.New(), KindFull)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Work(context.Background(), &river.Job[BackupArgs]{
		JobRow: newJobRow(1, 25), Args: BackupArgs{TenantID: tenantID, SnapshotID: snap.ID},
	}); err != nil {
		t.Fatal(err)
	}
	if len(transport.deliveries) != 1 {
		t.Fatalf("delivery count = %d", len(transport.deliveries))
	}
	gh954AssertExclusions(t, transport.deliveries[0], nil, nil)
}

func TestNotificationUpdatePreservesContentsOnSuccessfulRead(t *testing.T) {
	repo, svc, _, _, _ := gh954Fixture(t)
	tenantID, siteID := uuid.New(), uuid.New()
	gh954SaveContents(t, svc, tenantID, siteID, []string{"2024"}, []string{"log"})
	_, err := svc.PutBackupNotifications(context.Background(), PutBackupNotificationsInput{
		TenantID: tenantID, SiteID: siteID, NotifyOnCompletion: "always",
	})
	if err != nil {
		t.Fatal(err)
	}
	stored := repo.settings[siteID]
	if !reflect.DeepEqual(stored.ExcludePaths, []string{"2024"}) || !reflect.DeepEqual(stored.ExcludeExtensions, []string{"log"}) {
		t.Fatalf("notification update replaced saved exclusions: paths=%v extensions=%v", stored.ExcludePaths, stored.ExcludeExtensions)
	}
}

func TestNotificationUpdateReadErrorPreservesContents(t *testing.T) {
	repo, svc, _, _, _ := gh954Fixture(t)
	tenantID, siteID := uuid.New(), uuid.New()
	gh954SaveContents(t, svc, tenantID, siteID, []string{"2024"}, []string{"log"})
	writesBefore := repo.settingsWrites
	repo.settingsErr = errors.New("settings database unavailable")
	_, err := svc.PutBackupNotifications(context.Background(), PutBackupNotificationsInput{
		TenantID: tenantID, SiteID: siteID, NotifyOnCompletion: "always",
	})
	if err == nil {
		t.Error("notification update must propagate the content read error")
	} else if !errors.Is(err, repo.settingsErr) {
		t.Errorf("notification error = %v, want original settings error %v", err, repo.settingsErr)
	}
	if repo.settingsWrites != writesBefore {
		t.Error("notification update wrote settings after the content read failed")
	}
	stored := repo.settings[siteID]
	if !reflect.DeepEqual(stored.ExcludePaths, []string{"2024"}) || !reflect.DeepEqual(stored.ExcludeExtensions, []string{"log"}) {
		t.Errorf("notification update replaced saved exclusions: paths=%v extensions=%v", stored.ExcludePaths, stored.ExcludeExtensions)
	}
}

func TestTransportRetryRedispatchesSameSnapshotWithFreshJWT(t *testing.T) {
	repo, svc, _, transport, worker := gh954Fixture(t)
	tenantID, siteID := uuid.New(), uuid.New()
	gh954SaveContents(t, svc, tenantID, siteID, []string{"2024"}, []string{"log"})
	snap, err := svc.CreateBackup(context.Background(), tenantID, siteID, uuid.New(), KindFull)
	if err != nil {
		t.Fatal(err)
	}
	transport.err = context.DeadlineExceeded
	args := BackupArgs{TenantID: tenantID, SnapshotID: snap.ID}
	if err := worker.Work(context.Background(), &river.Job[BackupArgs]{JobRow: newJobRow(1, 25), Args: args}); err == nil {
		t.Fatal("transport timeout must return an error for River retry")
	}
	if repo.mustGet(t, snap.ID).Status != StatusRunning {
		t.Fatal("transport timeout must leave snapshot running")
	}
	transport.err = nil
	if err := worker.Work(context.Background(), &river.Job[BackupArgs]{JobRow: newJobRow(2, 25), Args: args}); err != nil {
		t.Fatal(err)
	}
	if len(transport.deliveries) != 2 {
		t.Fatalf("delivery count = %d", len(transport.deliveries))
	}
	a, b := transport.deliveries[0], transport.deliveries[1]
	if string(a.body) != string(b.body) {
		t.Errorf("retry body differs: first=%s second=%s", a.body, b.body)
	}
	if a.authorization == b.authorization || !strings.HasPrefix(b.authorization, "Bearer ") {
		t.Error("retry must mint a fresh signed command token")
	}
	gh954AssertExclusions(t, b, []string{"2024"}, []string{"log"})
	t.Log("retry re-sent the same snapshot payload with a fresh JWT while status remained running")
}

func TestWatchdogMarksThenFailsWithoutRedispatch(t *testing.T) {
	repo, svc, _, transport, worker := gh954Fixture(t)
	tenantID, siteID := uuid.New(), uuid.New()
	snap, err := svc.CreateBackup(context.Background(), tenantID, siteID, uuid.New(), KindFull)
	if err != nil {
		t.Fatal(err)
	}
	args := BackupArgs{TenantID: tenantID, SnapshotID: snap.ID}
	if err := worker.Work(context.Background(), &river.Job[BackupArgs]{JobRow: newJobRow(1, 25), Args: args}); err != nil {
		t.Fatal(err)
	}
	watchdog := NewProgressWatchdogWorker(svc, 3*time.Minute, 90*time.Minute, nil)
	repo.stalledFeed = []StalledSnapshot{{ID: snap.ID, TenantID: tenantID, SiteID: siteID}}
	if err := watchdog.Work(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	soft := repo.mustGet(t, snap.ID)
	if soft.Status != StatusRunning || soft.StalledAt == nil {
		t.Fatalf("soft stall = %+v", soft)
	}
	repo.stalledFeed[0].Hard = true
	if err := watchdog.Work(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	hard := repo.mustGet(t, snap.ID)
	if hard.Status != StatusFailed || hard.Error != stallTimeoutMsg {
		t.Fatalf("hard stall = %+v", hard)
	}
	if err := worker.Work(context.Background(), &river.Job[BackupArgs]{JobRow: newJobRow(2, 25), Args: args}); err != nil {
		t.Fatal(err)
	}
	if len(transport.deliveries) != 1 {
		t.Fatalf("watchdog or terminal retry changed delivery count to %d", len(transport.deliveries))
	}
	t.Logf("soft status=%s; hard status=%s error=%q; no additional delivery", soft.Status, hard.Status, hard.Error)
}

func TestControlPlaneAnnouncesStartBeforeCommandDelivery(t *testing.T) {
	repo, svc, _, transport, worker := gh954Fixture(t)
	tenantID, siteID := uuid.New(), uuid.New()
	snap, err := svc.CreateBackup(context.Background(), tenantID, siteID, uuid.New(), KindFull)
	if err != nil {
		t.Fatal(err)
	}
	hub := NewHub()
	svc.SetHub(hub)
	events, unsubscribe := hub.Subscribe(snap.ID)
	defer unsubscribe()
	observed := false
	transport.beforeSend = func() {
		observed = true
		if repo.mustGet(t, snap.ID).Status != StatusRunning {
			t.Error("snapshot must be running before HTTP delivery")
		}
		select {
		case event := <-events:
			if event.Phase != "started" || event.Status != StatusRunning {
				t.Errorf("event before delivery = %+v", event)
			}
		default:
			t.Error("control-plane start must be announced before HTTP delivery")
		}
	}
	if err := worker.Work(context.Background(), &river.Job[BackupArgs]{
		JobRow: newJobRow(1, 25), Args: BackupArgs{TenantID: tenantID, SnapshotID: snap.ID},
	}); err != nil {
		t.Fatal(err)
	}
	if !observed {
		t.Fatal("command was not delivered")
	}
	t.Log("snapshot was running and started event was published before the outgoing HTTP request")
}

func TestAgentFailureRetainsProgressAndReason(t *testing.T) {
	repo, svc, _, _, _ := gh954Fixture(t)
	tenantID, siteID := uuid.New(), uuid.New()
	snap, err := repo.CreateSnapshot(context.Background(), CreateSnapshotInput{TenantID: tenantID, SiteID: siteID, Kind: KindFull})
	if err != nil {
		t.Fatal(err)
	}
	message := "FilesArchiver: zip close failed for uploads.g000.part018.zip"
	detail := map[string]any{"message": message, "current_artifact": "uploads.g000.part018.zip", "current_file": "uploads/2024/example.jpg"}
	if _, err := svc.RecordProgress(context.Background(), tenantID, snap.ID, "failed", detail); err != nil {
		t.Fatal(err)
	}
	got := repo.mustGet(t, snap.ID)
	if got.Status != StatusFailed || got.Error != message || got.TotalSize != 0 || got.ChunkCount != 0 {
		t.Fatalf("failed snapshot = %+v", got)
	}
	var progress struct {
		PhaseDetail map[string]any `json:"phase_detail"`
	}
	if err := json.Unmarshal(got.Progress, &progress); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(progress.PhaseDetail, detail) {
		t.Fatalf("stored failure detail = %v, want %v", progress.PhaseDetail, detail)
	}
	t.Logf("stored status=%s error=%q progress=%s total_size=%d chunk_count=%d", got.Status, got.Error, got.Progress, got.TotalSize, got.ChunkCount)
}

func TestSnapshotAPIExposesStoredProgress(t *testing.T) {
	for _, phase := range []string{"archiving_files", "failed"} {
		t.Run(phase, func(t *testing.T) {
			progress, err := json.Marshal(map[string]any{
				"phase": phase,
				"phase_detail": map[string]any{
					"message":          "FilesArchiver: zip close failed for uploads.g000.part018.zip",
					"current_artifact": "uploads.g000.part018.zip", "current_file": "uploads/2024/example.jpg",
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			status := StatusRunning
			if phase == "failed" {
				status = StatusFailed
			}
			api := toAPISnapshot(Snapshot{Kind: KindFull, Status: status, Progress: progress})
			encoded, err := json.Marshal(&api)
			if err != nil {
				t.Fatal(err)
			}
			var body struct {
				Progress json.RawMessage `json:"progress"`
			}
			if err := json.Unmarshal(encoded, &body); err != nil {
				t.Fatal(err)
			}
			if !json.Valid(body.Progress) {
				t.Fatalf("API omitted stored progress: stored=%s API=%s", progress, encoded)
			}
			var stored, returned any
			if err := json.Unmarshal(progress, &stored); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(body.Progress, &returned); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(stored, returned) {
				t.Fatalf("API progress differs from stored progress: stored=%s returned=%s", progress, body.Progress)
			}
		})
	}
}

func TestSnapshotAPIInvalidProgressIsLogged(t *testing.T) {
	for _, progress := range []string{`{"phase":"private-marker",`, `["private-marker"]`} {
		t.Run(progress, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			snapshotID := uuid.New()
			api := toAPISnapshot(Snapshot{ID: snapshotID, Kind: KindFull, Status: StatusRunning, Progress: []byte(progress)})
			if api.Progress.IsSet() {
				t.Error("invalid stored progress must be omitted")
			}
			if !strings.Contains(logs.String(), snapshotID.String()) || !strings.Contains(logs.String(), "error=") {
				t.Errorf("progress decode error must be logged with snapshot ID: %s", logs.String())
			}
			if strings.Contains(logs.String(), "private-marker") {
				t.Error("progress decode log must not contain the stored payload")
			}
		})
	}
}

func TestSnapshotAPIEmptyProgressIsOmittedWithoutLogging(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	for _, progress := range []string{"", "{}", "null"} {
		api := toAPISnapshot(Snapshot{Kind: KindFull, Status: StatusRunning, Progress: []byte(progress)})
		if api.Progress.IsSet() {
			t.Errorf("empty progress %q must be omitted", progress)
		}
	}
	if logs.Len() != 0 {
		t.Errorf("empty progress must not log an error: %s", logs.String())
	}
}

func TestFailureProgressReplacesPreviousFileContext(t *testing.T) {
	repo, svc, _, _, _ := gh954Fixture(t)
	tenantID, siteID := uuid.New(), uuid.New()
	snap, err := repo.CreateSnapshot(context.Background(), CreateSnapshotInput{TenantID: tenantID, SiteID: siteID, Kind: KindFull})
	if err != nil {
		t.Fatal(err)
	}
	svc.SetHub(NewHub())
	events, unsubscribe := svc.hub.Subscribe(snap.ID)
	defer unsubscribe()
	if _, err := svc.RecordProgress(context.Background(), tenantID, snap.ID, "archiving_files", map[string]any{
		"current_artifact": "uploads.g000.part018.zip", "current_file": "uploads/2024/example.jpg",
	}); err != nil {
		t.Fatal(err)
	}
	drainEvents(events)
	const message = "FilesArchiver: zip close failed for uploads.g000.part018.zip"
	if _, err := svc.RecordProgress(context.Background(), tenantID, snap.ID, "failed", map[string]any{"message": message}); err != nil {
		t.Fatal(err)
	}
	got := repo.mustGet(t, snap.ID)
	if strings.Contains(string(got.Progress), "current_file") || strings.Contains(string(got.Progress), "current_artifact") {
		t.Fatalf("progress must contain only the latest report: %s", got.Progress)
	}
	frames := drainEvents(events)
	if len(frames) != 1 || frames[0].Phase != "failed" || !reflect.DeepEqual(frames[0].PhaseDetail, map[string]any{"error": message}) {
		t.Fatalf("terminal frame = %+v", frames)
	}
	api := toAPISnapshot(got)
	encoded, err := json.Marshal(&api)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), message) {
		t.Fatalf("API lost failure reason: %s", encoded)
	}
	t.Logf("latest progress=%s terminal SSE detail=%v API=%s", got.Progress, frames[0].PhaseDetail, encoded)
}
