package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/mosamlife/wpmgr/apps/api/internal/config"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// A GitHub failure must say WHICH failure it is
// ---------------------------------------------------------------------------

// Every non-200 from GitHub used to become fmt.Errorf("status %d"), so a spent
// rate budget, a token the user revoked, an OAuth app an organisation has not
// approved, and GitHub being down were one indistinguishable error. They need
// four different responses from an operator, and the Retry-After that says how
// long to wait was thrown away entirely.
func TestGitHubGetClassifiesFailuresAndKeepsRetryAfter(t *testing.T) {
	cases := []struct {
		name           string
		status         int
		headers        map[string]string
		body           string
		wantFailure    githubFailure
		wantMessage    string
		wantRetryAfter time.Duration
	}{
		{
			// A secondary rate limit: GitHub answers 403 and names the wait.
			name:           "secondary rate limit with Retry-After",
			status:         http.StatusForbidden,
			headers:        map[string]string{"Retry-After": "47"},
			body:           `{"message":"You have exceeded a secondary rate limit"}`,
			wantFailure:    githubFailureRateLimited,
			wantMessage:    "You have exceeded a secondary rate limit",
			wantRetryAfter: 47 * time.Second,
		},
		{
			// A 403 that is NOT a rate limit at all. Waiting will never fix it:
			// the operator has to approve the app or fix the scopes. Reaching
			// the same conclusion as the case above, which is what the old code
			// forced, is exactly the wrong move.
			name:        "forbidden is not a rate limit",
			status:      http.StatusForbidden,
			body:        `{"message":"Resource protected by organization SAML enforcement"}`,
			wantFailure: githubFailureForbidden,
			wantMessage: "Resource protected by organization SAML enforcement",
		},
		{
			name:        "revoked or invalid token",
			status:      http.StatusUnauthorized,
			body:        `{"message":"Bad credentials"}`,
			wantFailure: githubFailureTokenInvalid,
			wantMessage: "Bad credentials",
		},
		{
			// GitHub answers 404 rather than 403 for things a token may not
			// see, so this is frequently a missing scope wearing a disguise.
			name:        "not found",
			status:      http.StatusNotFound,
			body:        `{"message":"Not Found"}`,
			wantFailure: githubFailureNotFound,
			wantMessage: "Not Found",
		},
		{
			name:        "provider outage",
			status:      http.StatusBadGateway,
			wantFailure: githubFailureUnavailable,
		},
		{
			name:           "explicit 429",
			status:         http.StatusTooManyRequests,
			headers:        map[string]string{"Retry-After": "60"},
			wantFailure:    githubFailureRateLimited,
			wantRetryAfter: 60 * time.Second,
		},
	}

	seen := map[githubFailure]bool{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			err := githubGet(context.Background(), srv.Client(), srv.URL+"/user", &struct{}{})
			if err == nil {
				t.Fatal("a non-200 must be an error")
			}
			var ge *githubAPIError
			if !errors.As(err, &ge) {
				t.Fatalf("failure must be typed so a caller can tell them apart, got %T: %v", err, err)
			}
			if ge.Failure != tc.wantFailure {
				t.Errorf("Failure = %q, want %q", ge.Failure, tc.wantFailure)
			}
			if ge.Status != tc.status {
				t.Errorf("Status = %d, want %d", ge.Status, tc.status)
			}
			if ge.Message != tc.wantMessage {
				t.Errorf("Message = %q, want %q (GitHub's own message names the cause)", ge.Message, tc.wantMessage)
			}
			if ge.RetryAfter != tc.wantRetryAfter {
				t.Errorf("RetryAfter = %v, want %v", ge.RetryAfter, tc.wantRetryAfter)
			}
			seen[ge.Failure] = true
		})
	}

	// The whole point: these are not the same failure.
	if len(seen) != 5 {
		t.Errorf("the cases above must produce 5 distinct classifications, got %d: %v", len(seen), seen)
	}
}

// A primary rate limit carries no Retry-After. It carries the epoch second the
// budget refills, which is the only way to learn that the wait is an hour
// rather than a moment.
func TestGitHubRetryAfterFallsBackToRateLimitReset(t *testing.T) {
	reset := time.Now().Add(50 * time.Minute)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	}))
	defer srv.Close()

	err := githubGet(context.Background(), srv.Client(), srv.URL+"/user/emails", &struct{}{})
	var ge *githubAPIError
	if !errors.As(err, &ge) {
		t.Fatalf("want a typed error, got %v", err)
	}
	if ge.Failure != githubFailureRateLimited {
		t.Fatalf("a 403 with an exhausted budget is a rate limit, got %q", ge.Failure)
	}
	if ge.RetryAfter < 45*time.Minute || ge.RetryAfter > 50*time.Minute {
		t.Fatalf("RetryAfter = %v, want roughly 50m read from the reset epoch", ge.RetryAfter)
	}
}

// A connection that never completes is not a status, and must not be dressed
// up as one: an operator reading "status 0" would go looking for a response
// GitHub never sent.
func TestGitHubGetTransportFailureIsNotAnAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL + "/user"
	srv.Close()

	err := githubGet(context.Background(), srv.Client(), url, &struct{}{})
	if err == nil {
		t.Fatal("an unreachable endpoint must be an error")
	}
	var ge *githubAPIError
	if errors.As(err, &ge) {
		t.Fatalf("a transport failure must not be reported as an API status: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The code exchange must be bounded
// ---------------------------------------------------------------------------

// The exchange ran on http.DefaultClient, which has no timeout, under whatever
// context the inbound request carried. A token endpoint that accepts the
// connection and then goes quiet held the handler open for as long as it liked.
func TestGitHubCodeExchangeIsBounded(t *testing.T) {
	// The handler answers nothing until the test releases it. stop is what
	// releases it: httptest's Close waits for outstanding handlers, and a
	// handler waiting only on the request context can outlive the client that
	// gave up on it.
	stop := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-stop:
		case <-r.Context().Done():
		}
	}))
	defer func() {
		close(stop)
		hang.Close()
	}()

	g := newGitHubAdapter(config.GitHubConfig{ClientID: "id", ClientSecret: "secret"})
	g.endpoint = oauth2.Endpoint{AuthURL: hang.URL + "/authorize", TokenURL: hang.URL + "/token"}
	g.httpTimeout = 200 * time.Millisecond

	done := make(chan error, 1)
	go func() {
		// context.Background() has no deadline of its own on purpose: the bound
		// has to come from the adapter, not from a caller who might not set one.
		_, err := g.Exchange(context.Background(), "https://app.test/cb", "code", "verifier", "")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a token endpoint that never answers must not produce a successful exchange")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the code exchange never gave up: it is running unbounded")
	}
}

// ---------------------------------------------------------------------------
// GitHub's privacy address
// ---------------------------------------------------------------------------

func githubStub(t *testing.T, user githubUser, emails []githubEmail) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(user)
	})
	mux.HandleFunc("/user/emails", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(emails)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func githubIdentityFrom(t *testing.T, emails []githubEmail) SocialIdentity {
	t.Helper()
	srv := githubStub(t, githubUser{ID: 4242, Login: "sarah", Name: "Sarah"}, emails)
	g := newGitHubAdapter(config.GitHubConfig{ClientID: "id", ClientSecret: "secret"})
	g.apiBase = srv.URL
	id, err := g.identity(context.Background(), srv.Client())
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	return id
}

// "Keep my email addresses private" makes an outbound-only noreply address the
// account's primary, and GitHub reports it as primary AND verified. Taken at
// face value it is a perfectly good address, so the old code minted an account
// on it: an account that can never be sent a verification link, a password
// reset or an alert, with that GitHub id pinned to it by (provider, subject)
// for the rest of its life.
func TestGitHubPrivateEmailIsMarkedUnreachable(t *testing.T) {
	id := githubIdentityFrom(t, []githubEmail{
		{Email: "4242+sarah@users.noreply.github.com", Primary: true, Verified: true},
	})

	if !id.EmailUnreachable {
		t.Fatal("a users.noreply.github.com primary must be marked unreachable")
	}
	if id.usableEmail() {
		t.Fatal("an address GitHub discards mail for must not count as a usable address")
	}
	// It IS verified. Saying otherwise would be a lie that produces the wrong
	// advice further down.
	if !id.EmailVerified {
		t.Error("the address is genuinely verified by GitHub; only its reachability is in question")
	}
}

func TestGitHubOrdinaryEmailIsReachable(t *testing.T) {
	id := githubIdentityFrom(t, []githubEmail{
		{Email: "sarah@acme.com", Primary: true, Verified: true},
	})
	if id.EmailUnreachable {
		t.Fatal("an ordinary verified primary must not be marked unreachable")
	}
	if !id.usableEmail() {
		t.Fatal("an ordinary verified primary is exactly the case that must work")
	}
}

func TestIsGitHubNoreply(t *testing.T) {
	unreachable := []string{
		"4242+sarah@users.noreply.github.com",
		"sarah@users.noreply.github.com",
		"SARAH@Users.NoReply.GitHub.com",
		"  sarah@noreply.github.com  ", // the old form, before the users. prefix
	}
	for _, e := range unreachable {
		if !isGitHubNoreply(e) {
			t.Errorf("%q is a GitHub privacy address", e)
		}
	}
	reachable := []string{
		"sarah@acme.com",
		"sarah@github.com",
		"noreply.github.com@acme.com", // the domain appears, as the local part
		"",
	}
	for _, e := range reachable {
		if isGitHubNoreply(e) {
			t.Errorf("%q is a deliverable address", e)
		}
	}
}

// The refusal has to be the ACCURATE one. "Verify your email with GitHub"
// sends this person looking for a problem they do not have: their address is
// verified. What resolves it is one setting.
func TestDecideSocial_RefusesUnreachableProviderEmail(t *testing.T) {
	in := SocialIdentity{
		Provider: "github", Subject: "4242",
		Email: "4242+sarah@users.noreply.github.com", EmailVerified: true, EmailUnreachable: true,
	}

	_, err := decideSocial(in, nil, nil)
	if got := codeOf(t, err); got != "social_email_unreachable" {
		t.Fatalf("code = %q, want social_email_unreachable", got)
	}
	de, _ := domain.AsDomain(err)
	for _, want := range []string{"private", "GitHub"} {
		if !contains(de.Message, want) {
			t.Errorf("refusal must name the setting to change, got %q", de.Message)
		}
	}
}

// An unreachable address must not reach an existing account either. It cannot
// have been verified here (nothing sent to it arrives), so linking on it would
// be linking on an address nobody has ever proven they hold.
func TestDecideSocial_UnreachableEmailNeverLinks(t *testing.T) {
	in := SocialIdentity{
		Provider: "github", Subject: "4242",
		Email: "sarah@acme.com", EmailVerified: true, EmailUnreachable: true,
	}
	if _, err := decideSocial(in, nil, verifiedUser()); codeOf(t, err) != "social_email_unreachable" {
		t.Fatalf("an unreachable address must not link onto an existing account: %v", err)
	}
}

// AND THE REGRESSION THAT MATTERS MORE. Someone who signed in before turning
// privacy on already has an account here. Their identity is known, so the
// address is not consulted at all and they keep signing in. Refusing at the
// adapter, which was the obvious place to put this, would have locked them out.
func TestDecideSocial_UnreachableEmailDoesNotLockOutAKnownIdentity(t *testing.T) {
	in := SocialIdentity{
		Provider: "github", Subject: "4242",
		Email: "4242+sarah@users.noreply.github.com", EmailVerified: true, EmailUnreachable: true,
	}
	got, err := decideSocial(in, verifiedUser(), nil)
	if err != nil {
		t.Fatalf("a known identity must still sign in: %v", err)
	}
	if got != socialSignIn {
		t.Fatalf("got %v, want socialSignIn", got)
	}
}
