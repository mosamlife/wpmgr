package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/ipprovider"
)

// fakeRepo records the writes the ingest path makes. Every other method is a
// no-op returning zero values.
type fakeRepo struct {
	upserts       []fakeUpsert
	timezones     int
	hostProviders int
}

type fakeUpsert struct {
	category    Category
	collectedAt time.Time
}

func (f *fakeRepo) UpsertDiagnostic(_ context.Context, tenantID, siteID uuid.UUID, category Category, payload json.RawMessage, collectedAt time.Time) (Diagnostic, error) {
	f.upserts = append(f.upserts, fakeUpsert{category: category, collectedAt: collectedAt})
	return Diagnostic{TenantID: tenantID, SiteID: siteID, Category: category, Payload: payload, CollectedAt: collectedAt}, nil
}

func (f *fakeRepo) ListDiagnosticsBySite(context.Context, uuid.UUID, uuid.UUID) ([]Diagnostic, error) {
	return nil, nil
}

func (f *fakeRepo) UpsertPHPError(context.Context, uuid.UUID, uuid.UUID, UpsertPHPErrorInput) error {
	return nil
}

func (f *fakeRepo) ListPHPErrorsBySite(context.Context, uuid.UUID, uuid.UUID, ListPHPErrorsFilter) ([]PHPError, string, error) {
	return nil, "", nil
}

func (f *fakeRepo) SetSilenced(context.Context, uuid.UUID, uuid.UUID, string, bool) error {
	return nil
}

func (f *fakeRepo) GetErrorConfig(context.Context, uuid.UUID, uuid.UUID) (ErrorConfig, bool, error) {
	return ErrorConfig{}, false, nil
}

func (f *fakeRepo) UpsertErrorConfig(_ context.Context, cfg ErrorConfig) (ErrorConfig, error) {
	return cfg, nil
}

func (f *fakeRepo) UpdateSiteTimezone(context.Context, uuid.UUID, uuid.UUID, string, float64) error {
	f.timezones++
	return nil
}

func (f *fakeRepo) SetSiteHostProvider(context.Context, uuid.UUID, uuid.UUID, string, string, string) error {
	f.hostProviders++
	return nil
}

// fakeDBSizeSink records every DB-size trend point the ingest path appends.
type fakeDBSizeSink struct{ points []time.Time }

func (f *fakeDBSizeSink) RecordDBSizeHistoryFromDiagnostics(_ context.Context, _, _ uuid.UUID, _ int64, scannedAt time.Time) error {
	f.points = append(f.points, scannedAt)
	return nil
}

// fakeURLSink counts reported-address hand-offs.
type fakeURLSink struct{ calls int }

func (f *fakeURLSink) EnqueueAdoptReportedURL(context.Context, uuid.UUID, uuid.UUID, string, string, string) error {
	f.calls++
	return nil
}

// fakeHostResolver counts host-provider lookups.
type fakeHostResolver struct{ calls int }

func (f *fakeHostResolver) Resolve(string) ipprovider.Result {
	f.calls++
	return ipprovider.Result{Provider: "test-host"}
}

// ingestHarness is a Service wired the way cmd/wpmgr wires it, with fakes in
// place of Postgres and the downstream sinks, and its log captured.
type ingestHarness struct {
	svc    *Service
	repo   *fakeRepo
	dbSize *fakeDBSizeSink
	urls   *fakeURLSink
	hosts  *fakeHostResolver
	logs   *bytes.Buffer
}

func newIngestHarness() *ingestHarness {
	h := &ingestHarness{
		repo:   &fakeRepo{},
		dbSize: &fakeDBSizeSink{},
		urls:   &fakeURLSink{},
		hosts:  &fakeHostResolver{},
		logs:   &bytes.Buffer{},
	}
	h.svc = NewService(h.repo)
	h.svc.SetDBSizeHistorySink(h.dbSize)
	h.svc.SetReportedURLSink(h.urls)
	h.svc.SetHostResolver(h.hosts)
	h.svc.logger = slog.New(slog.NewTextHandler(h.logs, nil))
	return h
}

func (h *ingestHarness) logLines() []string {
	out := strings.TrimSpace(h.logs.String())
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// pushBody builds a diagnostics body with four categories that each drive a
// side effect: identity (timezone), http (reported address), wp_native
// (DB-size trend point) and hosting (host inference). collected_at is set to
// the given value unless omit is true.
func pushBody(t *testing.T, collectedAt any, omit bool) []byte {
	t.Helper()
	m := map[string]any{
		"identity": map[string]any{"timezone": "Europe/London", "gmt_offset": 1},
		"http":     map[string]any{"home_url": "https://example.test"},
		"wp_native": map[string]any{"wp-paths-sizes": map[string]any{"fields": map[string]any{
			"database_size": map[string]any{"value": "1.2 GB", "debug": 1288490188},
		}}},
		"hosting": map[string]any{"public_ip": "8.8.8.8"},
	}
	if !omit {
		m["collected_at"] = collectedAt
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return b
}

// assertRefused checks that err is the 422 validation error with the given
// code, and that the push wrote nothing at all.
func assertRefused(t *testing.T, h *ingestHarness, count int, err error, wantCode string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want a %s error, got nil; the push was stored with collected_at %v", wantCode, h.repo.upserts)
	}
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("want a domain validation error, got %T: %v", err, err)
	}
	if de.Kind != domain.KindValidation || de.Code != wantCode {
		t.Fatalf("want KindValidation %q, got kind %d code %q", wantCode, de.Kind, de.Code)
	}
	if got := domain.HTTPStatus(err); got != http.StatusUnprocessableEntity {
		t.Fatalf("want HTTP 422 for the push, got %d", got)
	}
	if count != 0 {
		t.Fatalf("want 0 categories ingested, got %d", count)
	}
	if len(h.repo.upserts) != 0 {
		t.Fatalf("want 0 UpsertDiagnostic calls, got %d: %+v", len(h.repo.upserts), h.repo.upserts)
	}
	if len(h.dbSize.points) != 0 {
		t.Fatalf("want 0 DB-size trend points, got %d: %v", len(h.dbSize.points), h.dbSize.points)
	}
	if h.repo.timezones != 0 {
		t.Fatalf("want 0 timezone updates, got %d", h.repo.timezones)
	}
	if h.urls.calls != 0 {
		t.Fatalf("want 0 reported-address hand-offs, got %d", h.urls.calls)
	}
}

// TestIngestDiagnosticsRefusesMissingCollectedAt: a push without collected_at
// is refused whole. Before GH #618 every category was stored stamped with the
// time it arrived, so the operator view showed data of unknown age as fresh.
func TestIngestDiagnosticsRefusesMissingCollectedAt(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"key absent", pushBody(t, nil, true)},
		{"null", pushBody(t, nil, false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newIngestHarness()
			count, err := h.svc.IngestDiagnostics(context.Background(), uuid.New(), uuid.New(), tc.body)
			assertRefused(t, h, count, err, codeCollectedAtMissing)
		})
	}
}

// TestIngestDiagnosticsRefusesMalformedCollectedAt: a value that is not a
// positive integer is refused the same way, and logged exactly once with its
// length.
func TestIngestDiagnosticsRefusesMalformedCollectedAt(t *testing.T) {
	cases := []struct {
		name  string
		value any
	}{
		{"string", "abc"},
		{"negative", -5},
		{"zero", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newIngestHarness()
			count, err := h.svc.IngestDiagnostics(context.Background(), uuid.New(), uuid.New(), pushBody(t, tc.value, false))
			assertRefused(t, h, count, err, codeCollectedAtInvalid)

			raw, _ := json.Marshal(tc.value)
			lines := h.logLines()
			if len(lines) != 1 {
				t.Fatalf("want exactly 1 log line, got %d: %q", len(lines), lines)
			}
			wantLen := "collected_at_len=" + strconv.Itoa(len(raw))
			if !strings.Contains(lines[0], wantLen) {
				t.Fatalf("want the log line to carry %s, got %q", wantLen, lines[0])
			}
		})
	}
}

// TestIngestDiagnosticsLogsLengthNotValue: the refusal log names the value's
// length and never the value itself.
func TestIngestDiagnosticsLogsLengthNotValue(t *testing.T) {
	const marker = "corrupt-QZXJ-payload"
	h := newIngestHarness()
	_, err := h.svc.IngestDiagnostics(context.Background(), uuid.New(), uuid.New(), pushBody(t, marker, false))
	if err == nil {
		t.Fatal("want the push refused, got nil")
	}
	if strings.Contains(h.logs.String(), "QZXJ") {
		t.Fatalf("the log carries the rejected value: %q", h.logs.String())
	}
	if !strings.Contains(h.logs.String(), "collected_at_len="+strconv.Itoa(len(marker)+2)) {
		t.Fatalf("want the quoted value's length in the log, got %q", h.logs.String())
	}
}

// TestIngestDiagnosticsStampsAgentCollectedAt: a valid collected_at behaves
// as before. Every category is stored with the agent's time, the DB-size
// point uses it, and nothing is logged.
func TestIngestDiagnosticsStampsAgentCollectedAt(t *testing.T) {
	const ts = int64(1748505600)
	want := time.Unix(ts, 0).UTC()
	h := newIngestHarness()
	count, err := h.svc.IngestDiagnostics(context.Background(), uuid.New(), uuid.New(), pushBody(t, ts, false))
	if err != nil {
		t.Fatalf("IngestDiagnostics: %v", err)
	}
	if count != 4 || len(h.repo.upserts) != 4 {
		t.Fatalf("want 4 categories stored, got count %d, upserts %d", count, len(h.repo.upserts))
	}
	for _, u := range h.repo.upserts {
		if !u.collectedAt.Equal(want) {
			t.Fatalf("category %s stored with collected_at %v, want %v", u.category, u.collectedAt, want)
		}
	}
	if len(h.dbSize.points) != 1 || !h.dbSize.points[0].Equal(want) {
		t.Fatalf("want one DB-size point at %v, got %v", want, h.dbSize.points)
	}
	if h.repo.timezones != 1 || h.urls.calls != 1 {
		t.Fatalf("want 1 timezone update and 1 address hand-off, got %d and %d", h.repo.timezones, h.urls.calls)
	}
	if lines := h.logLines(); len(lines) != 0 {
		t.Fatalf("want no log output for a valid push, got %q", lines)
	}
}

// fakeDiagnosticsAgent answers the refresh's signed diagnostics call with a
// fixed body and counts the calls.
type fakeDiagnosticsAgent struct {
	body  []byte
	calls int
}

func (f *fakeDiagnosticsAgent) Diagnostics(context.Context, uuid.UUID, string, agentcmd.DiagnosticsRequest) ([]byte, error) {
	f.calls++
	return f.body, nil
}

type fakeSiteLookup struct{}

func (fakeSiteLookup) GetSiteURL(context.Context, uuid.UUID, uuid.UUID) (string, error) {
	return "https://example.test", nil
}

// TestRefreshStopsOnMissingCollectedAt: the operator refresh feeds the agent's
// reply into the real Service, as cmd/wpmgr wires it. A reply without
// collected_at ends the refresh with the 422 validation error after exactly
// one agent call: nothing stored, no host inference, no second attempt.
func TestRefreshStopsOnMissingCollectedAt(t *testing.T) {
	h := newIngestHarness()
	agent := &fakeDiagnosticsAgent{body: pushBody(t, nil, true)}
	enq := NewRefreshEnqueuer(agent, fakeSiteLookup{}, h.svc)
	h.svc.SetRefreshEnqueuer(enq)

	err := h.svc.RefreshAgent(context.Background(), uuid.New(), uuid.New())
	assertRefused(t, h, 0, err, codeCollectedAtMissing)
	if agent.calls != 1 {
		t.Fatalf("want exactly 1 agent call, got %d", agent.calls)
	}
	if h.hosts.calls != 0 || h.repo.hostProviders != 0 {
		t.Fatalf("want no host inference, got %d lookups and %d writes", h.hosts.calls, h.repo.hostProviders)
	}
	if errors.Is(err, errUnwired) {
		t.Fatal("the refusal must not read as the unwired sentinel")
	}
}
