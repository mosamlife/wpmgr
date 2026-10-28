package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// POST /clients/{clientId}/reports and packages/openapi/openapi.yaml must say
// the same thing about how the request fails. The spec listed 400, which this
// endpoint never answers, and left out the 422 and the 503 it does.

// generateCall runs the real POST handler and returns the status it answers and
// the error code in the body ("" for a success).
func generateCall(t *testing.T, blob BlobStorer, body string) (status int, code string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc := NewService(&stubRepo{}, blob)
	svc.SetEnqueuer(&fakeEnqueuer{})
	h := NewHandler(svc, nil)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), domain.Principal{
			Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: uuid.New(),
		}))
	})
	r.POST("/clients/:clientId/reports", h.generateReport)
	req := httptest.NewRequest(http.MethodPost, "/clients/"+uuid.New().String()+"/reports", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var errBody struct {
		Code string `json:"code"`
	}
	if rec.Code >= 400 {
		if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
			t.Fatalf("status %d, body %q is not an error object: %v", rec.Code, rec.Body.String(), err)
		}
	}
	return rec.Code, errBody.Code
}

// generateFailures is every way the request is refused on its own merits, with
// the status and code the server answers.
var generateFailures = []struct {
	name       string
	blob       BlobStorer
	body       string
	wantStatus int
	wantCode   string
}{
	{"period ends before it starts", &noopBlobStorer{},
		`{"period_start":"2026-05-02T00:00:00Z","period_end":"2026-05-01T00:00:00Z"}`, http.StatusUnprocessableEntity, "invalid_period"},
	{"period is too long", &noopBlobStorer{},
		`{"period_start":"2025-01-01T00:00:00Z","period_end":"2025-12-01T00:00:00Z"}`, http.StatusUnprocessableEntity, "period_too_long"},
	{"body is not JSON", &noopBlobStorer{},
		`{not json`, http.StatusUnprocessableEntity, "invalid_body"},
	{"no object storage", nil,
		`{}`, http.StatusServiceUnavailable, "object_storage_required"},
}

func TestGenerateReportAnswersWhatTheSpecSays(t *testing.T) {
	status, code := generateCall(t, &noopBlobStorer{}, `{}`)
	if status != http.StatusAccepted || code != "" {
		t.Fatalf("a valid request answered %d %q, want 202", status, code)
	}

	doc := loadSpec(t)
	responses := at(t, doc, "paths", "/api/v1/clients/{clientId}/reports", "post", "responses")
	for _, c := range generateFailures {
		t.Run(c.name, func(t *testing.T) {
			status, code := generateCall(t, c.blob, c.body)
			if status != c.wantStatus || code != c.wantCode {
				t.Fatalf("answered %d %q, want %d %q", status, code, c.wantStatus, c.wantCode)
			}
			// The spec lists the status, and its text names the code.
			response := at(t, responses, fmt.Sprint(status))
			description, _ := at(t, response, "description").(string)
			if !strings.Contains(description, "`"+code+"`") {
				t.Errorf("the spec's %d response does not name the code `%s`:\n%s", status, code, description)
			}
		})
	}
}

// 400 is not a status this endpoint answers: a failed check is 422
// (domain.Validation), including a body that is not JSON.
func TestSpecDoesNotListA400ForGenerateReport(t *testing.T) {
	doc := loadSpec(t)
	responses, _ := at(t, doc, "paths", "/api/v1/clients/{clientId}/reports", "post", "responses").(map[string]any)
	if _, listed := responses["400"]; listed {
		t.Error(`the spec lists "400" for POST /clients/{clientId}/reports, which the server never answers`)
	}
	for _, c := range generateFailures {
		if status, _ := generateCall(t, c.blob, c.body); status == http.StatusBadRequest {
			t.Errorf("%s: the server now answers 400; list it in the spec", c.name)
		}
	}
}

// The spec states the longest period; it is the server's limit.
func TestSpecStatesTheLongestReportPeriod(t *testing.T) {
	doc := loadSpec(t)
	response := at(t, doc, "paths", "/api/v1/clients/{clientId}/reports", "post", "responses", "422")
	description, _ := at(t, response, "description").(string)
	if want := fmt.Sprintf("%d days", maxPeriodDays); !strings.Contains(description, want) {
		t.Errorf("the spec's 422 response does not state the %s limit:\n%s", want, description)
	}
}
