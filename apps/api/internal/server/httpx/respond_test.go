package httpx

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

func answer(t *testing.T, err error) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/x", nil)
	Error(c, err)
	var body map[string]any
	if jerr := json.Unmarshal(rec.Body.Bytes(), &body); jerr != nil {
		t.Fatalf("body %q is not JSON: %v", rec.Body.String(), jerr)
	}
	return rec.Code, body
}

// TestErrorAnswersBadGatewayWithItsOwnCodeAndMessage: a 502 domain error
// reaches the dashboard as the status, code and fixed message it was built
// with, and nothing else.
func TestErrorAnswersBadGatewayWithItsOwnCodeAndMessage(t *testing.T) {
	status, body := answer(t, domain.BadGateway("site_refused", "Diagnostics check not started. The site refused the request."))
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", status)
	}
	if body["code"] != "site_refused" || body["message"] != "Diagnostics check not started. The site refused the request." {
		t.Fatalf("body = %v", body)
	}
	if _, has := body["details"]; has {
		t.Fatalf("body carries details: %v", body)
	}
}

// TestErrorKeepsHidingAnInternalFailure: the same envelope still hides what an
// ordinary error says.
func TestErrorKeepsHidingAnInternalFailure(t *testing.T) {
	status, body := answer(t, errors.New("connection to the database was lost"))
	if status != http.StatusInternalServerError || body["code"] != "internal_error" || body["message"] != "internal server error" {
		t.Fatalf("status %d body %v", status, body)
	}
}
