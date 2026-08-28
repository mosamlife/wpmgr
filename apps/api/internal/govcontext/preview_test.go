package govcontext

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// TestEffectiveContextPreview_IsByteIdenticalToDirectResolve is ADR-064
// property 3's proof, and the one Decision 8 calls a correctness
// requirement rather than a nicety: "the preview must call the same
// resolution function the model-facing assembly path calls, not a second
// implementation of the same idea." This test asserts it at the byte level —
// the HTTP response body from GET .../context/effective, and the JSON
// encoding of calling Resolver.Resolve directly and mapping it through
// toEffectiveContextDTO, must be byte-identical.
//
// To confirm this is a real assertion and not a tautology, it was run once
// with an artificial divergence planted in the handler (getEffectiveContext
// in handler.go temporarily setting `dto.TotalBytes++` before responding,
// simulating a second, drifted implementation) and went RED:
//
//	$ go test ./internal/govcontext/... -run TestEffectiveContextPreview_IsByteIdenticalToDirectResolve -v
//	--- FAIL: TestEffectiveContextPreview_IsByteIdenticalToDirectResolve (0.00s)
//	    preview_test.go:64: handler response body differs from direct Resolve+DTO output
//	        direct: ...,"total_bytes":19,...
//	        handler: ...,"total_bytes":20,...
//	FAIL
//
// Restored (the planted divergence removed), it is GREEN:
//
//	$ go test ./internal/govcontext/... -run TestEffectiveContextPreview_IsByteIdenticalToDirectResolve -v
//	--- PASS: TestEffectiveContextPreview_IsByteIdenticalToDirectResolve (0.00s)
//	PASS
func TestEffectiveContextPreview_IsByteIdenticalToDirectResolve(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	store := &fakeStore{
		orgOK: true,
		org: Snapshot{
			Restrictions: RestrictionSet{ForbiddenDomains: []string{"evil.example.com"}},
			Guidance:     GuidanceSet{BrandVoice: "org voice"},
		},
		siteOK: true,
		site: Snapshot{
			Restrictions: RestrictionSet{ForbiddenDomains: []string{"evil.example.com", "also.example.com"}},
			Guidance:     GuidanceSet{Audience: "small business owners"},
		},
	}
	resolver := &Resolver{Store: store}
	svc := NewService(nil, nil, resolver)
	h := NewHandler(svc)

	// 1. Direct call — the "ground truth" the preview must match exactly.
	directRC, err := resolver.Resolve(context.Background(), tenantID, siteID, nil)
	if err != nil {
		t.Fatalf("direct Resolve failed: %v", err)
	}
	directJSON, err := json.Marshal(toEffectiveContextDTO(directRC))
	if err != nil {
		t.Fatalf("marshal direct DTO: %v", err)
	}

	// 2. Through the handler, exactly as an HTTP client would receive it.
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest("GET", "/api/v1/sites/"+siteID.String()+"/context/effective", nil)
	p := domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenantID}
	c.Request = req.WithContext(domain.WithPrincipal(req.Context(), p))
	c.Params = gin.Params{{Key: "siteId", Value: siteID.String()}}

	h.getEffectiveContext(c)

	if rec.Code != 200 {
		t.Fatalf("handler returned status %d, body: %s", rec.Code, rec.Body.String())
	}
	handlerJSON := rec.Body.Bytes()

	if string(directJSON) != string(handlerJSON) {
		t.Errorf("handler response body differs from direct Resolve+DTO output\ndirect:  %s\nhandler: %s",
			directJSON, handlerJSON)
	}
}

// TestEffectiveContextPreview_NoSessionContent proves Decision 8's "the
// preview never carries live session content, because none exists at preview
// time" — the preview call must supply a nil session, so layer 6 is always
// empty in a preview response, regardless of what the (unused, at preview
// time) session field could otherwise hold.
func TestEffectiveContextPreview_NoSessionContent(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	resolver := &Resolver{Store: &fakeStore{}}
	svc := NewService(nil, nil, resolver)

	rc, err := svc.GetEffectiveContext(context.Background(), tenantID, siteID)
	if err != nil {
		t.Fatalf("GetEffectiveContext failed: %v", err)
	}
	for _, l := range rc.Layers {
		if l.Layer == 6 && l.Session != "" {
			t.Errorf("preview carried session content: %q", l.Session)
		}
	}
}
