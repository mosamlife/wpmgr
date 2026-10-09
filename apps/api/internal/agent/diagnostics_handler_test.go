package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/diagnostics"
)

// postDiagnostics drives POST /agent/v1/diagnostics through a real gin engine
// and the real diagnostics service, with the verified identity injected the
// way the agent auth middleware attaches it. The service has no repository:
// every body below carries no category, so a correct handler never reaches
// one.
func postDiagnostics(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := NewDiagnosticsHandler(diagnostics.NewService(nil))

	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/agent/v1")
	g.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(WithIdentity(c.Request.Context(), Identity{SiteID: uuid.New(), TenantID: uuid.New()}))
		c.Next()
	})
	h.Register(g)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/agent/v1/diagnostics", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

// TestDiagnosticsPushWithoutValidCollectedAtIs422 (GH #618): the agent push
// answers 422 with a stable code when collected_at is absent or unreadable,
// instead of a 200 that stored the report as collected now.
func TestDiagnosticsPushWithoutValidCollectedAtIs422(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantCode string
	}{
		{"absent", `{}`, "collected_at_missing"},
		{"null", `{"collected_at":null}`, "collected_at_missing"},
		{"string", `{"collected_at":"abc"}`, "collected_at_invalid"},
		{"negative", `{"collected_at":-5}`, "collected_at_invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := postDiagnostics(t, tc.body)
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("want 422, got %d: %s", w.Code, w.Body.String())
			}
			var env struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode error envelope: %v (%s)", err, w.Body.String())
			}
			if env.Code != tc.wantCode {
				t.Fatalf("want code %q, got %q", tc.wantCode, env.Code)
			}
		})
	}
}

// TestDiagnosticsPushWithCollectedAtIs200: a valid collected_at still answers
// 200, so the refusal above does not over-fire.
func TestDiagnosticsPushWithCollectedAtIs200(t *testing.T) {
	w := postDiagnostics(t, `{"collected_at":1748505600}`)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := strings.TrimSpace(w.Body.String()); got != `{"categories_ingested":0}` {
		t.Fatalf("want categories_ingested 0, got %s", got)
	}
}
