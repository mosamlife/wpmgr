package report

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/report/render/html"
	"github.com/mosamlife/wpmgr/apps/api/internal/report/render/pdf"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// ---------------------------------------------------------------------------
// GH #971: an on-demand report uses the client's report settings.
// ---------------------------------------------------------------------------

// capturingBlob keeps every uploaded object by key.
type capturingBlob struct{ objects map[string][]byte }

func (b *capturingBlob) Put(_ context.Context, key string, body io.Reader, _ int64) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if b.objects == nil {
		b.objects = map[string][]byte{}
	}
	b.objects[key] = data
	return nil
}
func (b *capturingBlob) Delete(context.Context, string) error { return nil }
func (b *capturingBlob) PresignGet(context.Context, string, time.Duration) (string, error) {
	return "", nil
}

// snapshotRepo records the snapshot the worker completes the report with.
type snapshotRepo struct {
	*stubRepo
	snapshot []byte
	failed   string
}

func (r *snapshotRepo) CompleteReport(_ context.Context, _, _ uuid.UUID, _, _ string, snapshot []byte) (GeneratedReport, error) {
	r.snapshot = snapshot
	return GeneratedReport{}, nil
}

func (r *snapshotRepo) FailReport(_ context.Context, _, id uuid.UUID, msg string) (GeneratedReport, error) {
	r.failed = msg
	return GeneratedReport{ID: id, Status: StatusFailed}, nil
}

type onDemandResult struct {
	html     string
	snapshot ReportData
	err      error
	repo     *snapshotRepo
}

// runOnDemand runs the generate worker for an on-demand job (no ScheduleID),
// with getSchedule standing in for the client's stored report settings.
func runOnDemand(t *testing.T, getSchedule func(context.Context, uuid.UUID, uuid.UUID) (Schedule, bool, error), override *SectionFlagsPatch, attempt int) onDemandResult {
	t.Helper()
	repo := &snapshotRepo{stubRepo: &stubRepo{
		getSchedule:   getSchedule,
		getTenantName: func(context.Context, uuid.UUID) (string, error) { return "Agency", nil },
	}}
	blob := &capturingBlob{}
	svc := NewService(repo, blob)
	htmlR, err := html.NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	src := Sources{
		ListClientSites: func(context.Context, uuid.UUID, uuid.UUID) ([]site.Site, error) {
			return []site.Site{{ID: uuid.New(), Name: "site-a", URL: "https://a.example"}}, nil
		},
	}
	w := NewGenerateWorker(repo, svc, src, htmlR, pdf.NewFpdfRenderer(), nil, nil)
	args := GenerateArgs{
		TenantID:         uuid.New(),
		ClientID:         uuid.New(),
		ReportID:         uuid.New(),
		PeriodStart:      time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		PeriodEnd:        time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		SectionsOverride: override,
	}
	job := &river.Job[GenerateArgs]{JobRow: &rivertype.JobRow{Attempt: attempt, MaxAttempts: 3}, Args: args}
	res := onDemandResult{repo: repo}
	res.err = w.Work(context.Background(), job)
	if res.err != nil {
		return res
	}
	res.html = string(blob.objects[blobKey(args.TenantID, args.ClientID, args.ReportID, "html")])
	if err := json.Unmarshal(repo.snapshot, &res.snapshot); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return res
}

// storedSettings is a client whose agency switched off "Prepared with WPMgr"
// and turned off the uptime and email sections.
func storedSettings(context.Context, uuid.UUID, uuid.UUID) (Schedule, bool, error) {
	return Schedule{
		Enabled:          false,
		Sections:         SectionFlags{Overview: true, Uptime: false, Backups: true, Updates: true, Performance: true, Email: false},
		IntroText:        "Hello from your agency",
		PoweredByRemoved: true,
	}, true, nil
}

func TestOnDemandReportUsesClientWhiteLabelSettings(t *testing.T) {
	res := runOnDemand(t, storedSettings, nil, 1)
	if res.err != nil {
		t.Fatalf("Work: %v", res.err)
	}
	if strings.Contains(res.html, "Prepared with WPMgr") {
		t.Error(`on-demand report prints "Prepared with WPMgr" although the agency switched it off`)
	}
	if res.snapshot.ShowPoweredBy {
		t.Error("snapshot ShowPoweredBy is true although the agency switched it off")
	}
	want := SectionFlags{Overview: true, Uptime: false, Backups: true, Updates: true, Performance: true, Email: false}
	if res.snapshot.Sections != want {
		t.Errorf("on-demand sections %+v, want the client's stored %+v", res.snapshot.Sections, want)
	}
	if !strings.Contains(res.html, "Hello from your agency") {
		t.Error("on-demand report dropped the client's intro text")
	}
}

func TestOnDemandReportWithoutSettingsKeepsDefaults(t *testing.T) {
	res := runOnDemand(t, nil, nil, 1)
	if res.err != nil {
		t.Fatalf("Work: %v", res.err)
	}
	if !strings.Contains(res.html, "Prepared with WPMgr") {
		t.Error("a client with no saved settings should get the default footer")
	}
	if res.snapshot.Sections != DefaultSectionFlags() {
		t.Errorf("sections %+v, want defaults", res.snapshot.Sections)
	}
}

func TestOnDemandSectionsOverrideAppliesOverStoredSettings(t *testing.T) {
	on := true
	res := runOnDemand(t, storedSettings, &SectionFlagsPatch{Uptime: &on}, 1)
	if res.err != nil {
		t.Fatalf("Work: %v", res.err)
	}
	want := SectionFlags{Overview: true, Uptime: true, Backups: true, Updates: true, Performance: true, Email: false}
	if res.snapshot.Sections != want {
		t.Errorf("sections %+v, want stored with uptime overridden on %+v", res.snapshot.Sections, want)
	}
	if strings.Contains(res.html, "Prepared with WPMgr") {
		t.Error("a sections override must not bring back the removed footer")
	}
}

func TestOnDemandSettingsReadFailureDoesNotRenderDefaults(t *testing.T) {
	boom := func(context.Context, uuid.UUID, uuid.UUID) (Schedule, bool, error) {
		return Schedule{}, false, errors.New("db down")
	}
	res := runOnDemand(t, boom, nil, 3)
	if res.err == nil {
		t.Fatal("a failed settings read must fail the attempt, not render with the default footer")
	}
	if res.repo.snapshot != nil {
		t.Error("the report was completed although its settings could not be read")
	}
	if res.repo.failed == "" {
		t.Error("the final attempt must mark the report failed")
	}
}

func TestGenerateNowCarriesSectionsOverride(t *testing.T) {
	enq := &fakeEnqueuer{}
	svc := NewService(&stubRepo{}, &noopBlobStorer{})
	svc.SetEnqueuer(enq)
	if _, err := svc.GenerateNow(context.Background(), uuid.New(), uuid.New(), GenerateNowInput{}); err != nil {
		t.Fatalf("GenerateNow: %v", err)
	}
	off := false
	if _, err := svc.GenerateNow(context.Background(), uuid.New(), uuid.New(), GenerateNowInput{Sections: &SectionFlagsPatch{Email: &off}}); err != nil {
		t.Fatalf("GenerateNow: %v", err)
	}
	if len(enq.calls) != 2 {
		t.Fatalf("want 2 enqueued jobs, got %d", len(enq.calls))
	}
	if enq.calls[0].SectionsOverride != nil {
		t.Error("a request without sections must not override the client's stored sections")
	}
	if o := enq.calls[1].SectionsOverride; o == nil || o.Email == nil || *o.Email {
		t.Errorf("the request's sections override was dropped: %+v", o)
	}
}

// ---------------------------------------------------------------------------
// GH #972: saving report settings keeps a section the request left out.
// ---------------------------------------------------------------------------

func putSchedule(t *testing.T, stored *Schedule, body string) UpsertScheduleInput {
	t.Helper()
	gin.SetMode(gin.TestMode)
	var got UpsertScheduleInput
	repo := &stubRepo{
		getSchedule: func(context.Context, uuid.UUID, uuid.UUID) (Schedule, bool, error) {
			if stored == nil {
				return Schedule{}, false, nil
			}
			return *stored, true, nil
		},
		upsertSchedule: func(_ context.Context, in UpsertScheduleInput) (Schedule, error) {
			got = in
			return Schedule{TenantID: in.TenantID, ClientID: in.ClientID, Sections: in.Sections}, nil
		},
	}
	h := NewHandler(NewService(repo, nil), nil)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), domain.Principal{
			Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: uuid.New(),
		}))
	})
	r.PUT("/clients/:clientId/report-schedule", h.putSchedule)
	req := httptest.NewRequest(http.MethodPut, "/clients/"+uuid.New().String()+"/report-schedule", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status %d: %s", rec.Code, rec.Body.String())
	}
	return got
}

// The dashboard's form, which has no Overview control (#972).
const formBody = `{"enabled":true,"cadence":"monthly","send_day":1,"send_hour":8,"recipients":[],
	"sections":{"uptime":true,"backups":true,"updates":true,"performance":true,"email":false},
	"intro_text":"","closing_text":"","powered_by_removed":true}`

func TestPutScheduleWithoutOverviewKeepsStoredValue(t *testing.T) {
	stored := &Schedule{Sections: SectionFlags{Overview: true, Uptime: true, Backups: true, Updates: true, Performance: true, Email: true}}
	got := putSchedule(t, stored, formBody)
	if !got.Sections.Overview {
		t.Error("a PUT without overview turned the stored Overview section off")
	}
	if got.Sections.Email {
		t.Error("an explicit email:false was not applied")
	}
}

func TestPutScheduleWithoutOverviewOnNewRowUsesDefault(t *testing.T) {
	got := putSchedule(t, nil, formBody)
	if !got.Sections.Overview {
		t.Error("a first save without overview must take the default (on), not false")
	}
}

func TestPutScheduleExplicitOverviewFalseTurnsItOff(t *testing.T) {
	stored := &Schedule{Sections: DefaultSectionFlags()}
	body := strings.Replace(formBody, `"uptime":true`, `"overview":false,"uptime":true`, 1)
	got := putSchedule(t, stored, body)
	if got.Sections.Overview {
		t.Error("an explicit overview:false did not turn Overview off")
	}
}

func TestPutScheduleWithoutSectionsKeepsAllStored(t *testing.T) {
	stored := &Schedule{Sections: SectionFlags{Overview: false, Uptime: true, Email: true}}
	got := putSchedule(t, stored, `{"enabled":false,"cadence":"weekly","send_day":1,"send_hour":8}`)
	if got.Sections != stored.Sections {
		t.Errorf("a PUT with no sections object changed them: %+v, want %+v", got.Sections, stored.Sections)
	}
}
