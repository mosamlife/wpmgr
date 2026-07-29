// bulk_log_ids_test.go: GH #307.
//
// The bulk log-delete and bulk log-resend handlers bound `json:"ids"` while
// their OpenAPI schemas (BulkDeleteLogsRequest, BulkResendRequest) declare the
// field as `log_ids`, which is what the generated client and the web app have
// always sent. The field never bound, the id list was always empty, and both
// endpoints returned 200 with a zero count: "0 log entries deleted", nothing
// deleted, and an audit row recording the no-op as a success.
//
// Every test below sends a SPEC-SHAPED body ({"log_ids": [...]}) and asserts
// the ids actually reached the repository, which is the part that was broken.
package email

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
)

// recordingRepo wraps the shared fakeRepo and captures the ids the service
// passes down, so a test can prove the handler observed the request body
// rather than silently substituting an empty list.
type recordingRepo struct {
	*fakeRepo
	deletedIDs  []uuid.UUID
	deleteCalls int
}

func (r *recordingRepo) DeleteEmailLogsBulk(_ context.Context, _, _ uuid.UUID, ids []uuid.UUID) (int64, error) {
	r.deleteCalls++
	r.deletedIDs = append(r.deletedIDs, ids...)
	return int64(len(ids)), nil
}

func newBulkTestHandler() (*Handler, *recordingRepo) {
	repo := &recordingRepo{fakeRepo: newFakeRepo()}
	svc := NewService(&Repo{}, nil, nil)
	svc.repo = repo
	return NewHandlerWithPublisher(svc, (*audit.Recorder)(nil), nil), repo
}

func bulkLogPath(siteID uuid.UUID, suffix string) string {
	return "/api/v1/sites/" + siteID.String() + "/email/log" + suffix
}

func idListJSON(field string, ids []uuid.UUID) []byte {
	quoted := make([]string, 0, len(ids))
	for _, id := range ids {
		quoted = append(quoted, `"`+id.String()+`"`)
	}
	return []byte(fmt.Sprintf(`{%q:[%s]}`, field, strings.Join(quoted, ",")))
}

// ---------------------------------------------------------------------------
// The regression: the spec field must actually bind.
// ---------------------------------------------------------------------------

// TestBulkDeleteLog_BindsSpecLogIDsField is the GH #307 regression test. It
// fails against the pre-fix handler, which bound "ids": log_ids never decoded,
// the service short-circuited on an empty list, DeleteEmailLogsBulk was never
// called, and the response was {"deleted":0} with HTTP 200.
func TestBulkDeleteLog_BindsSpecLogIDsField(t *testing.T) {
	h, repo := newBulkTestHandler()
	tenantID, siteID := uuid.New(), uuid.New()
	engine := newTestOperatorEngine(h, tenantID)

	ids := []uuid.UUID{uuid.New(), uuid.New()}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, bulkLogPath(siteID, ""), bytes.NewReader(idListJSON("log_ids", ids)))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Deleted int64 `json:"deleted"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Deleted != int64(len(ids)) {
		t.Errorf("expected deleted=%d, got %d: the log_ids field did not bind", len(ids), resp.Deleted)
	}
	if repo.deleteCalls != 1 {
		t.Fatalf("expected exactly 1 repo delete call, got %d", repo.deleteCalls)
	}
	if len(repo.deletedIDs) != len(ids) {
		t.Fatalf("repo received %d ids, want %d", len(repo.deletedIDs), len(ids))
	}
	for i, want := range ids {
		if repo.deletedIDs[i] != want {
			t.Errorf("repo id[%d] = %s, want %s", i, repo.deletedIDs[i], want)
		}
	}
}

// TestBulkResendLog_BindsSpecLogIDsField is the sibling regression: the resend
// button on the same screen was broken the same way and returned
// {"results":[]}, which the web read as "0 emails queued for resend" on the
// SUCCESS path (failed === 0). One result per requested id proves the ids bound;
// each is a failure here only because the fake repo stores no message body.
func TestBulkResendLog_BindsSpecLogIDsField(t *testing.T) {
	h, _ := newBulkTestHandler()
	tenantID, siteID := uuid.New(), uuid.New()
	engine := newTestOperatorEngine(h, tenantID)

	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, bulkLogPath(siteID, "/resend"), bytes.NewReader(idListJSON("log_ids", ids)))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Results []struct {
			LogID string `json:"log_id"`
		} `json:"results"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Results) != len(ids) {
		t.Fatalf("expected %d results, got %d: the log_ids field did not bind", len(ids), len(resp.Results))
	}
	for i, want := range ids {
		if resp.Results[i].LogID != want.String() {
			t.Errorf("result[%d].log_id = %s, want %s", i, resp.Results[i].LogID, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Back-compatibility: the old handler-side name keeps working.
// ---------------------------------------------------------------------------

// TestBulkDeleteLog_LegacyIDsFieldStillAccepted pins the deliberate alias. A
// caller written against the handler source rather than the spec has been
// sending "ids" and getting real deletions, so that name must not break.
func TestBulkDeleteLog_LegacyIDsFieldStillAccepted(t *testing.T) {
	h, repo := newBulkTestHandler()
	tenantID, siteID := uuid.New(), uuid.New()
	engine := newTestOperatorEngine(h, tenantID)

	ids := []uuid.UUID{uuid.New()}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, bulkLogPath(siteID, ""), bytes.NewReader(idListJSON("ids", ids)))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(repo.deletedIDs) != 1 || repo.deletedIDs[0] != ids[0] {
		t.Errorf("legacy \"ids\" field no longer reaches the repo: got %v, want %v", repo.deletedIDs, ids)
	}
}

// TestBulkDeleteLog_LogIDsWinsOverLegacyIDs pins the precedence rule: when a
// body carries both names, the spec field is authoritative.
func TestBulkDeleteLog_LogIDsWinsOverLegacyIDs(t *testing.T) {
	h, repo := newBulkTestHandler()
	tenantID, siteID := uuid.New(), uuid.New()
	engine := newTestOperatorEngine(h, tenantID)

	specID, legacyID := uuid.New(), uuid.New()
	body := fmt.Sprintf(`{"log_ids":[%q],"ids":[%q]}`, specID, legacyID)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, bulkLogPath(siteID, ""), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(repo.deletedIDs) != 1 || repo.deletedIDs[0] != specID {
		t.Errorf("expected the spec field to win with %s, repo got %v", specID, repo.deletedIDs)
	}
}

// ---------------------------------------------------------------------------
// The documented maxItems ceilings are enforced before the list is parsed.
// ---------------------------------------------------------------------------

// TestBulkDeleteLog_RejectsOversizedList checks the BulkDeleteLogsRequest
// maxItems: 500 ceiling is enforced server-side, and that rejection happens
// before the handler parses (and allocates) the whole id list.
func TestBulkDeleteLog_RejectsOversizedList(t *testing.T) {
	h, repo := newBulkTestHandler()
	tenantID, siteID := uuid.New(), uuid.New()
	engine := newTestOperatorEngine(h, tenantID)

	ids := make([]uuid.UUID, MaxBulkDelete+1)
	for i := range ids {
		ids[i] = uuid.New()
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, bulkLogPath(siteID, ""), bytes.NewReader(idListJSON("log_ids", ids)))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for %d ids, got %d: %s", len(ids), w.Code, w.Body.String())
	}
	if repo.deleteCalls != 0 {
		t.Errorf("an oversized request reached the repo (%d calls)", repo.deleteCalls)
	}
}

// TestBulkResendLog_RejectsOversizedList is the same ceiling for
// BulkResendRequest (maxItems: 100). Without it, an unbounded list would fan
// out into one agent command per id.
func TestBulkResendLog_RejectsOversizedList(t *testing.T) {
	h, _ := newBulkTestHandler()
	tenantID, siteID := uuid.New(), uuid.New()
	engine := newTestOperatorEngine(h, tenantID)

	ids := make([]uuid.UUID, MaxBulkResend+1)
	for i := range ids {
		ids[i] = uuid.New()
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, bulkLogPath(siteID, "/resend"), bytes.NewReader(idListJSON("log_ids", ids)))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for %d ids, got %d: %s", len(ids), w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// An empty or absent list is a caller error, not a successful no-op.
// ---------------------------------------------------------------------------

// emptyBodyCases are the shapes that carry no ids at all. `{}` is the
// spec-required field simply missing; the two empty arrays are the field
// present but asking for nothing. All three must be rejected identically.
var emptyBodyCases = []struct {
	name string
	body string
}{
	{"absent log_ids", `{}`},
	{"empty log_ids", `{"log_ids":[]}`},
	{"empty legacy ids", `{"ids":[]}`},
}

// TestBulkDeleteLog_RejectsEmptyList is the GH #307 follow-up: log_ids is
// `required` in BulkDeleteLogsRequest, but presence was never enforced, so a
// non-TypeScript caller sending `{}` got 200 {"deleted": 0} plus an audit row
// recording a successful deletion of nothing. That is the original symptom,
// still reachable outside the web app.
//
// repo.deleteCalls == 0 also proves no audit row was written: the handler
// records the audit event only after the repository call returns, so a request
// that never reaches the repo cannot have been audited.
func TestBulkDeleteLog_RejectsEmptyList(t *testing.T) {
	for _, tc := range emptyBodyCases {
		t.Run(tc.name, func(t *testing.T) {
			h, repo := newBulkTestHandler()
			tenantID, siteID := uuid.New(), uuid.New()
			engine := newTestOperatorEngine(h, tenantID)

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodDelete, bulkLogPath(siteID, ""), strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			engine.ServeHTTP(w, req)

			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("expected 422 for body %s, got %d: %s", tc.body, w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "log_ids") {
				t.Errorf("validation message should name the log_ids field, got: %s", w.Body.String())
			}
			if strings.Contains(w.Body.String(), `"deleted"`) {
				t.Errorf("a rejected request must not report a deletion count, got: %s", w.Body.String())
			}
			if repo.deleteCalls != 0 {
				t.Errorf("an empty request reached the repo (%d calls), so it was also audited", repo.deleteCalls)
			}
		})
	}
}

// TestBulkResendLog_RejectsEmptyList is the same guard on the sibling endpoint,
// which returned 200 {"results": []} and the web app's "0 emails queued for
// resend" success toast for a body that asked for nothing.
func TestBulkResendLog_RejectsEmptyList(t *testing.T) {
	for _, tc := range emptyBodyCases {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newBulkTestHandler()
			tenantID, siteID := uuid.New(), uuid.New()
			engine := newTestOperatorEngine(h, tenantID)

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, bulkLogPath(siteID, "/resend"), strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			engine.ServeHTTP(w, req)

			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("expected 422 for body %s, got %d: %s", tc.body, w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "log_ids") {
				t.Errorf("validation message should name the log_ids field, got: %s", w.Body.String())
			}
			if strings.Contains(w.Body.String(), `"results"`) {
				t.Errorf("a rejected request must not report a results list, got: %s", w.Body.String())
			}
		})
	}
}

// TestBulkDeleteLog_RejectsInvalidUUID pins the per-id validation error, which
// now names the spec field rather than the old one.
func TestBulkDeleteLog_RejectsInvalidUUID(t *testing.T) {
	h, repo := newBulkTestHandler()
	tenantID, siteID := uuid.New(), uuid.New()
	engine := newTestOperatorEngine(h, tenantID)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, bulkLogPath(siteID, ""), strings.NewReader(`{"log_ids":["not-a-uuid"]}`))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "log_ids") {
		t.Errorf("validation message should name the log_ids field, got: %s", w.Body.String())
	}
	if repo.deleteCalls != 0 {
		t.Errorf("a malformed request reached the repo (%d calls)", repo.deleteCalls)
	}
}
