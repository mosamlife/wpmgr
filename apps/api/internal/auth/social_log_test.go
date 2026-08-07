package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// emptyDB is a database with nobody in it. Enough to drive SignInWithSocial to
// the policy, which is where the refusal being tested comes from, without a
// live Postgres in a unit test.
type emptyDB struct{}

func (emptyDB) Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("emptyDB: no writes in this test")
}

// An empty result set, NOT pgx.ErrNoRows. Every caller of Query is a sqlc
// :many, and pgx only reports ErrNoRows from QueryRow().Scan(); a list query
// against an empty table returns zero rows and a nil error. Returning the error
// here made "nobody in the database" indistinguishable from "the database is
// broken", so the identity lookups added for issuer continuity aborted the
// sign-in before the policy ran and this file's refusal tests stopped seeing
// the refusal they assert on.
func (emptyDB) Query(context.Context, string, ...interface{}) (pgx.Rows, error) {
	return emptyRows{}, nil
}

func (emptyDB) QueryRow(context.Context, string, ...interface{}) pgx.Row { return noRow{} }

type noRow struct{}

func (noRow) Scan(...any) error { return pgx.ErrNoRows }

type emptyRows struct{}

func (emptyRows) Close()                                       {}
func (emptyRows) Err() error                                   { return nil }
func (emptyRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (emptyRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (emptyRows) Next() bool                                   { return false }
func (emptyRows) Scan(...any) error                            { return pgx.ErrNoRows }
func (emptyRows) Values() ([]any, error)                       { return nil, nil }
func (emptyRows) RawValues() [][]byte                          { return nil }
func (emptyRows) Conn() *pgx.Conn                              { return nil }

// stubAdapter stands in for a provider that fails in a specific way.
type stubAdapter struct {
	key string
	err error
	id  SocialIdentity
}

func (s stubAdapter) Key() string { return s.key }

func (s stubAdapter) AuthCodeURL(context.Context, string) (string, string, string, string, error) {
	return "https://provider.test/authorize", "st", "", "vf", s.err
}

func (s stubAdapter) Exchange(context.Context, string, string, string, string) (SocialIdentity, error) {
	if s.err != nil {
		return SocialIdentity{}, s.err
	}
	return s.id, nil
}

// socialCallbackHarness drives one callback request through the real handler
// and returns whatever it logged plus the redirect the browser is sent to.
func socialCallbackHarness(t *testing.T, adapter SocialProviderAdapter, query string) (logged string, redirect string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	sessions := NewSessionManagerWithStore(scs.New(), false)
	sctx := loadCtx(t, sessions)
	sessions.putSocial(sctx, adapter.Key(), "st", "", "vf", "")

	var buf bytes.Buffer
	h := &Handler{
		svc:      &Service{baseURL: "https://app.test", repo: &Repo{q: sqlc.New(emptyDB{})}},
		sessions: sessions,
		social:   &SocialProviders{byKey: map[string]SocialProviderAdapter{adapter.Key(): adapter}},
		logger:   slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/auth/social/"+adapter.Key()+"/callback?"+query, nil).
		WithContext(sctx)
	c.Params = gin.Params{{Key: "provider", Value: adapter.Key()}}

	h.socialCallback(c)
	return buf.String(), w.Header().Get("Location")
}

// THE FINDING THIS TEST EXISTS FOR: internal/auth logged nothing at all. A
// social sign-in that failed answered the browser with a deliberately coarse
// ?social_error= code and left no trace anywhere else, so an operator holding
// "the GitHub button does nothing" had no evidence to work from. The detail
// GitHub sent has to land somewhere, and this is where.
func TestSocialCallbackLogsProviderFailureDetail(t *testing.T) {
	logged, redirect := socialCallbackHarness(t, stubAdapter{
		key: "github",
		err: &githubAPIError{
			URL:        "https://api.github.com/user/emails",
			Status:     http.StatusForbidden,
			Failure:    githubFailureRateLimited,
			Message:    "API rate limit exceeded",
			RetryAfter: 47 * time.Second,
		},
	}, "state=st&code=abc")

	if logged == "" {
		t.Fatal("a failed social sign-in must log something; this path used to log nothing at all")
	}

	var line map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(logged)), &line); err != nil {
		t.Fatalf("expected one structured line, got %q: %v", logged, err)
	}
	for k, want := range map[string]any{
		"component":        "auth.social",
		"provider":         "github",
		"provider_failure": "rate_limited",
		"provider_status":  float64(http.StatusForbidden),
		"provider_message": "API rate limit exceeded",
		"retry_after":      float64(47 * time.Second), // slog encodes a Duration as nanoseconds
	} {
		if line[k] != want {
			t.Errorf("log field %q = %v, want %v", k, line[k], want)
		}
	}

	// The redirect stays coarse. Naming the failed step in a query parameter
	// tells an attacker which step to work on, and it travels in browser
	// history and proxy logs.
	if !strings.Contains(redirect, "social_error=social_exchange_failed") {
		t.Errorf("redirect = %q, want the generic exchange failure code", redirect)
	}
	if strings.Contains(redirect, "rate_limit") || strings.Contains(redirect, "403") {
		t.Errorf("provider detail must not travel in the redirect URL: %q", redirect)
	}
}

// A refusal is the policy working, but "it will not let me in" is a support
// ticket, and answering it needs the rule that said no and the identity it
// said no to.
func TestSocialCallbackLogsPolicyRefusal(t *testing.T) {
	logged, redirect := socialCallbackHarness(t, stubAdapter{
		key: "github",
		id: SocialIdentity{
			Provider: "github", Subject: "4242",
			Email: "4242+sarah@users.noreply.github.com", EmailVerified: true, EmailUnreachable: true,
		},
	}, "state=st&code=abc")

	if !strings.Contains(logged, `"code":"social_email_unreachable"`) {
		t.Fatalf("the refusal code must be logged, got %q", logged)
	}
	if !strings.Contains(logged, `"subject":"4242"`) {
		t.Errorf("the refused identity must be identifiable in the log, got %q", logged)
	}
	if !strings.Contains(logged, `"provider_email_unreachable":true`) {
		t.Errorf("the fact that decided the refusal must be in the line, got %q", logged)
	}
	if !strings.Contains(redirect, "social_error=social_email_unreachable") {
		t.Errorf("an actionable refusal must reach the sign-in page: %q", redirect)
	}
}

// A stale tab or a replayed callback. Neither is loud, and both are worth
// being able to count.
func TestSocialCallbackLogsStateMismatch(t *testing.T) {
	logged, redirect := socialCallbackHarness(t, stubAdapter{key: "github"}, "state=wrong&code=abc")
	if !strings.Contains(logged, "state mismatch") {
		t.Fatalf("a state mismatch must be logged, got %q", logged)
	}
	if !strings.Contains(redirect, "social_error=social_state_mismatch") {
		t.Errorf("redirect = %q", redirect)
	}
}

// socialErrorAttrs is what carries the detail out of an error and into a log
// line, so it has to hold on to every kind of detail an error here can have.
func TestSocialErrorAttrsUnpacksBothErrorShapes(t *testing.T) {
	if got := socialErrorAttrs(nil); got != nil {
		t.Fatalf("no error means no attributes, got %v", got)
	}

	find := func(attrs []any, key string) (any, bool) {
		for _, a := range attrs {
			if at, ok := a.(slog.Attr); ok && at.Key == key {
				return at.Value.Any(), true
			}
		}
		return nil, false
	}

	// A domain refusal carries a code.
	attrs := socialErrorAttrs(domain.Forbidden("social_email_unreachable", "no"))
	if got, ok := find(attrs, "code"); !ok || got != "social_email_unreachable" {
		t.Errorf("code = %v (present %v), want the refusal code", got, ok)
	}

	// A provider failure carries the status, the classification and the wait,
	// through a wrapping fmt.Errorf, which is how it actually arrives.
	wrapped := domain.Internal("x", "fetch emails").WithCause(&githubAPIError{
		URL: "https://api.github.com/user", Status: 401, Failure: githubFailureTokenInvalid,
	})
	attrs = socialErrorAttrs(wrapped)
	if got, ok := find(attrs, "provider_failure"); !ok || got != "token_invalid" {
		t.Errorf("provider_failure = %v (present %v), want token_invalid through the wrapper", got, ok)
	}
	if got, ok := find(attrs, "provider_status"); !ok || got != int64(401) {
		t.Errorf("provider_status = %v (present %v), want 401", got, ok)
	}
	if _, ok := find(attrs, "retry_after"); ok {
		t.Error("a failure with no wait attached must not invent one")
	}
}
