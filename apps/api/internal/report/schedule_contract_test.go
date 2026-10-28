package report

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// The schedule endpoints and packages/openapi/openapi.yaml must say the same
// thing. These tests pin the server's side of each point the spec states.

// scheduleCall runs the real GET or PUT handler over repo and returns the
// response's top-level JSON keys.
func scheduleCall(t *testing.T, repo *stubRepo, method, body string) map[string]json.RawMessage {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := NewHandler(NewService(repo, nil), nil)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), domain.Principal{
			Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: uuid.New(),
		}))
	})
	r.GET("/clients/:clientId/report-schedule", h.getSchedule)
	r.PUT("/clients/:clientId/report-schedule", h.putSchedule)
	req := httptest.NewRequest(method, "/clients/"+uuid.New().String()+"/report-schedule", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	out := map[string]json.RawMessage{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s status %d, body %q is not a JSON object: %v", method, rec.Code, rec.Body.String(), err)
	}
	if rec.Code != http.StatusOK {
		out["_status"] = json.RawMessage(`"` + http.StatusText(rec.Code) + `"`)
	}
	return out
}

// echoUpsert returns the schedule the PUT saved, the way the database row
// does: the run times the service computed come back as stored.
func echoUpsert(_ context.Context, in UpsertScheduleInput) (Schedule, error) {
	return Schedule{TenantID: in.TenantID, ClientID: in.ClientID, Enabled: in.Enabled, Cadence: in.Cadence,
		SendDay: in.SendDay, SendHour: in.SendHour, Sections: in.Sections, NextRunAt: in.NextRunAt}, nil
}

const putEnabled = `{"enabled":true,"cadence":"monthly","send_day":15,"send_hour":8,"recipients":[]}`
const putDisabled = `{"enabled":false,"cadence":"monthly","send_day":15,"send_hour":8,"recipients":[]}`

// next_run_at is "absent when disabled" and last_run_at "absent when never
// run": a missing key, not null.
func TestScheduleResponseOmitsRunTimesThatAreNotSet(t *testing.T) {
	later := time.Now().UTC().Add(48 * time.Hour)
	enabled := Schedule{Enabled: true, Cadence: "monthly", SendDay: 1, SendHour: 8, NextRunAt: &later}
	disabled := Schedule{Cadence: "monthly", SendDay: 1, SendHour: 8}
	cases := map[string]struct {
		got                      map[string]json.RawMessage
		wantNextRun, wantLastRun bool
	}{
		"GET disabled": {got: scheduleCall(t, &stubRepo{getSchedule: func(context.Context, uuid.UUID, uuid.UUID) (Schedule, bool, error) {
			return disabled, true, nil
		}}, http.MethodGet, ""), wantNextRun: false},
		"GET no row": {got: scheduleCall(t, &stubRepo{}, http.MethodGet, ""), wantNextRun: false},
		"GET enabled": {got: scheduleCall(t, &stubRepo{getSchedule: func(context.Context, uuid.UUID, uuid.UUID) (Schedule, bool, error) {
			return enabled, true, nil
		}}, http.MethodGet, ""), wantNextRun: true},
		"PUT disabled": {got: scheduleCall(t, &stubRepo{upsertSchedule: echoUpsert}, http.MethodPut, putDisabled), wantNextRun: false},
		"PUT enabled":  {got: scheduleCall(t, &stubRepo{upsertSchedule: echoUpsert}, http.MethodPut, putEnabled), wantNextRun: true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if st, bad := c.got["_status"]; bad {
				t.Fatalf("request failed: %s", st)
			}
			next, hasNext := c.got["next_run_at"]
			if hasNext != c.wantNextRun {
				t.Errorf("next_run_at present=%v (%s), want present=%v", hasNext, next, c.wantNextRun)
			}
			if last, hasLast := c.got["last_run_at"]; hasLast != c.wantLastRun {
				t.Errorf("last_run_at present=%v (%s), want present=%v", hasLast, last, c.wantLastRun)
			}
		})
	}
}

// The response carries the client's timezone, which the send hour is in.
func TestScheduleResponseCarriesTheClientsTimezone(t *testing.T) {
	tz := func(context.Context, uuid.UUID, uuid.UUID) (string, error) { return "Asia/Kolkata", nil }
	cases := map[string]map[string]json.RawMessage{
		"GET no row": scheduleCall(t, &stubRepo{getClientTimezone: tz}, http.MethodGet, ""),
		"GET stored": scheduleCall(t, &stubRepo{getClientTimezone: tz, getSchedule: func(context.Context, uuid.UUID, uuid.UUID) (Schedule, bool, error) {
			return Schedule{Cadence: "weekly"}, true, nil
		}}, http.MethodGet, ""),
		"PUT": scheduleCall(t, &stubRepo{getClientTimezone: tz, upsertSchedule: echoUpsert}, http.MethodPut, putEnabled),
	}
	for name, got := range cases {
		t.Run(name, func(t *testing.T) {
			if string(got["timezone"]) != `"Asia/Kolkata"` {
				t.Errorf("timezone = %s, want \"Asia/Kolkata\"", got["timezone"])
			}
		})
	}
}

// intro_text and closing_text are limited to 2000 characters, as the error
// message says and the spec's maxLength means, not 2000 bytes.
func TestScheduleTextLimitsCountCharactersNotBytes(t *testing.T) {
	valid := func(intro, closing string) error {
		_, err := validateScheduleInput(UpsertScheduleInput{Cadence: "weekly", SendDay: 1, SendHour: 8, IntroText: intro, ClosingText: closing})
		return err
	}
	euro := func(n int) string { return strings.Repeat("€", n) } // 3 bytes each
	emoji := func(n int) string { return strings.Repeat("\U0001F680", n) }
	ok := map[string][2]string{
		"2000 ascii":  {strings.Repeat("a", 2000), strings.Repeat("a", 2000)},
		"2000 euro":   {euro(2000), euro(2000)},
		"2000 emoji":  {emoji(2000), emoji(2000)},
		"empty":       {"", ""},
		"mixed 1999+": {euro(1000) + strings.Repeat("a", 1000), ""},
	}
	for name, c := range ok {
		if err := valid(c[0], c[1]); err != nil {
			t.Errorf("%s: rejected: %v", name, err)
		}
	}
	tooLong := map[string]struct {
		intro, closing, code string
	}{
		"2001 ascii intro":   {strings.Repeat("a", 2001), "", "intro_text_too_long"},
		"2001 euro intro":    {euro(2001), "", "intro_text_too_long"},
		"2001 emoji closing": {"", emoji(2001), "closing_text_too_long"},
		"2001 ascii closing": {"", strings.Repeat("a", 2001), "closing_text_too_long"},
	}
	for name, c := range tooLong {
		err := valid(c.intro, c.closing)
		de, isDomain := domain.AsDomain(err)
		if err == nil || !isDomain || de.Code != c.code {
			t.Errorf("%s: err = %v, want a validation error with code %s", name, err, c.code)
		}
	}
}

// ---------------------------------------------------------------------------
// The spec, read as data
// ---------------------------------------------------------------------------

func loadSpec(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../../../../packages/openapi/openapi.yaml")
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	return doc
}

// at walks nested maps by key and fails the test when a key is missing, so a
// renamed schema cannot make these checks pass by finding nothing.
func at(t *testing.T, node any, path ...string) any {
	t.Helper()
	for _, key := range path {
		m, ok := node.(map[string]any)
		if !ok {
			t.Fatalf("spec path %v: %q is not under an object", path, key)
		}
		next, found := m[key]
		if !found {
			t.Fatalf("spec path %v: no key %q", path, key)
		}
		node = next
	}
	return node
}

func TestSpecAgreesWithTheScheduleLimits(t *testing.T) {
	doc := loadSpec(t)
	update := at(t, doc, "components", "schemas", "ClientReportScheduleUpdate", "properties")
	for field, want := range map[string]int{
		"recipients":   maxRecipients,
		"intro_text":   maxIntroClosingLen,
		"closing_text": maxIntroClosingLen,
	} {
		key := "maxLength"
		if field == "recipients" {
			key = "maxItems"
		}
		if got := at(t, update, field, key); got != want {
			t.Errorf("spec %s.%s = %v, the server enforces %d", field, key, got, want)
		}
	}
}

func TestSpecListsTheStatusesTheSchedulePutAnswers(t *testing.T) {
	doc := loadSpec(t)
	responses := at(t, doc, "paths", "/api/v1/clients/{clientId}/report-schedule", "put", "responses")
	// PUT answers a failed check with 422 (domain.Validation).
	for _, code := range []string{"200", "401", "403", "404", "422"} {
		at(t, responses, code)
	}
}
