package invitation

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// A session cookie is ambient authority: the browser attaches it to whatever it
// is told to send, including a request another site caused. Accepting an
// invitation grants a membership and moves the caller's active organisation, so
// letting a bare cookie authorise it would let a page the person merely visited
// do both on their behalf. There is no CSRF middleware in this tree, so the
// intent header is what supplies the missing proof, and these pin that the
// session counts for nothing without it.

func requestWithPrincipal(t *testing.T, userID uuid.UUID, header string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/invitations/accept", nil)
	if header != "" {
		req.Header.Set(acceptIntentHeader, header)
	}
	if userID != uuid.Nil {
		req = req.WithContext(domain.WithPrincipal(req.Context(), domain.Principal{
			Type: domain.PrincipalUser, UserID: userID,
		}))
	}
	c.Request = req
	return c
}

func TestSessionAuthorisesAcceptOnlyWithTheIntentHeader(t *testing.T) {
	user := uuid.New()

	got := authorizingSessionUser(requestWithPrincipal(t, user, "1"))
	if got != user {
		t.Fatalf("a signed-in caller who asked for this got %v, want %v", got, user)
	}
}

func TestASessionWithoutTheIntentHeaderIsTreatedAsAnonymous(t *testing.T) {
	user := uuid.New()

	if got := authorizingSessionUser(requestWithPrincipal(t, user, "")); got != uuid.Nil {
		t.Fatalf("a cookie alone authorised a membership grant (user %v); a page the person only visited could cause that", got)
	}
}

// The header is not a credential and must never behave like one: on its own it
// authorises nobody.
func TestTheIntentHeaderAloneAuthorisesNobody(t *testing.T) {
	if got := authorizingSessionUser(requestWithPrincipal(t, uuid.Nil, "1")); got != uuid.Nil {
		t.Fatalf("an unauthenticated request with the header resolved to user %v", got)
	}
}
