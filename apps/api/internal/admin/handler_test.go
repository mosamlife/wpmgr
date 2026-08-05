package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// The requireSuperadmin middleware rejects on the principal check BEFORE it ever
// reads its adminGateStore, so the unauthenticated + wrong-principal-type
// branches are testable hermetically with a nil store. The is_superadmin lookup
// branch is covered with a fake store in gate_test.go and against real rows in
// gate_integration_test.go.

func init() { gin.SetMode(gin.TestMode) }

// runGate drives a request with the given context principal through
// requireSuperadmin and reports whether the downstream handler was reached.
func runGate(t *testing.T, p *domain.Principal) (status int, reached bool) {
	t.Helper()
	r := gin.New()
	r.GET("/admin/x", requireSuperadmin(nil), func(c *gin.Context) {
		reached = true
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/admin/x", nil)
	if p != nil {
		req = req.WithContext(domain.WithPrincipal(req.Context(), *p))
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, reached
}

func TestRequireSuperadmin_NoPrincipal(t *testing.T) {
	status, reached := runGate(t, nil)
	if reached {
		t.Fatal("handler must not be reached without a principal")
	}
	if status != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", status)
	}
}

func TestRequireSuperadmin_APIKeyPrincipalRejected(t *testing.T) {
	p := domain.Principal{Type: domain.PrincipalAPIKey, APIKeyID: uuid.New()}
	status, reached := runGate(t, &p)
	if reached {
		t.Fatal("handler must not be reached for a non-user principal")
	}
	if status != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", status)
	}
}
