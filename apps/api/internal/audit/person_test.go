package audit

import (
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

// Loosening an AI control needs a signed-in person. Each case is a caller that
// holds the route's permission and is not a signed-in person; the handler must
// answer 403 session_required without reaching the recorder, whose nil pool
// would panic if it were reached.
func TestLooseningAnAIControlNeedsASignedInPerson(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tenant := uuid.New()
	cases := []domain.Principal{
		{Type: domain.PrincipalAPIKey, APIKeyID: uuid.New(), TenantID: tenant, Role: "owner", Scope: domain.ScopeOrg},
		{Type: domain.PrincipalAPIKey, APIKeyID: uuid.New(), UserID: uuid.New(), TenantID: tenant, Role: "owner", Scope: domain.ScopeOrg},
		{Type: domain.PrincipalUser, TenantID: tenant, Role: "owner", Scope: domain.ScopeOrg},
	}
	for i, p := range cases {
		t.Run(fmt.Sprintf("case_%d", i+1), func(t *testing.T) {
			r := gin.New()
			r.Use(func(c *gin.Context) {
				c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), p))
				c.Next()
			})
			NewHandler(NewRecorder(nil, domain.SystemClock{})).Register(r.Group("/api/v1"))

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/audit/integrity/rebaseline", strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			func() {
				defer func() {
					if v := recover(); v != nil {
						t.Fatalf("a credential that is not a person loosened a control: reached the recorder (%v)", v)
					}
				}()
				r.ServeHTTP(w, req)
			}()

			var env struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &env)
			if w.Code != http.StatusForbidden || env.Code != "session_required" {
				t.Fatalf("a credential that is not a person loosened a control: got %d %q", w.Code, env.Code)
			}
		})
	}
}
