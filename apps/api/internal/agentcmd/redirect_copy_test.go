package agentcmd

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/siteaddr"
)

// redirectFor builds the RedirectError newRedirectError returns for a
// command sent to saved+route and answered with status and loc ("" sends no
// Location).
func redirectFor(t *testing.T, command, saved, loc string, status int) *RedirectError {
	t.Helper()
	endpoint, err := joinCommandURL(saved, command)
	if err != nil {
		t.Fatalf("joinCommandURL(%q): %v", saved, err)
	}
	req, _ := http.NewRequest(http.MethodPost, endpoint, nil)
	h := http.Header{}
	if loc != "" {
		h.Set("Location", loc)
	}
	return newRedirectError(command, endpoint, &http.Response{StatusCode: status, Header: h, Request: req})
}

// TestRedirectExplanation_Cases pins the operator copy for each case, in the
// order the cases are checked, and that SuggestedSiteURL is exactly
// siteaddr.Plan's To for the target's site address.
func TestRedirectExplanation_Cases(t *testing.T) {
	const exempt = " Exempt /wp-json/wpmgr/ from the redirect on the site or its CDN."
	cases := []struct {
		name, saved, loc string
		status           int
		want             string
		wantSuggested    string
	}{
		{"A adopt: http apex to https www", "http://x.test", "https://www.x.test" + pingRoute, 301,
			"http://x.test redirects to https://www.x.test, so no command was sent. If WordPress on the site reports https://www.x.test as its address, the saved address updates to https://www.x.test automatically at a later check-in from the site. That update does not happen while another site in this workspace uses https://www.x.test: if one does, remove or change the duplicate site.",
			"https://www.x.test"},
		{"D self: the route redirects to itself with a slash", "https://x.test", "https://x.test" + pingRoute + "/", 308,
			"https://x.test redirects its command address back to itself (HTTP 308), so no command was sent." + exempt, ""},
		{"B2 downgrade", "https://x.test", "http://x.test" + pingRoute, 301,
			"https://x.test redirects to http://x.test" + pingRoute + ", which drops HTTPS, so no command was sent." + exempt, ""},
		{"B1 another subdomain", "https://x.test", "https://staging.x.test" + pingRoute, 302,
			"https://x.test redirects to https://staging.x.test" + pingRoute + ", so no command was sent." + exempt, ""},
		{"B1 a page that is not the route", "https://x.test", "https://www.x.test/login", 302,
			"https://x.test redirects to https://www.x.test/login, so no command was sent." + exempt, ""},
		{"C no Location", "https://x.test", "", 307,
			"https://x.test answered with a redirect (HTTP 307) that names no usable address, so no command was sent." + exempt, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			re := redirectFor(t, "ping", c.saved, c.loc, c.status)
			if got := re.Explanation(); got != c.want {
				t.Errorf("Explanation:\n got %q\nwant %q", got, c.want)
			}
			if re.SuggestedSiteURL != c.wantSuggested {
				t.Errorf("SuggestedSiteURL = %q, want %q", re.SuggestedSiteURL, c.wantSuggested)
			}
			// SuggestedSiteURL is PlanStrict's To whenever PlanStrict adopts,
			// and empty otherwise.
			target, ok := trimCommandSuffix(re.To, re.Command)
			var planTo string
			if ok && target != "" {
				if p := siteaddr.PlanStrict(re.SavedSiteURL(), target); p.Decision == siteaddr.Adopt {
					planTo = p.To
				}
			}
			if re.SuggestedSiteURL != planTo {
				t.Errorf("SuggestedSiteURL = %q, siteaddr.PlanStrict gives %q", re.SuggestedSiteURL, planTo)
			}
			if strings.ContainsAny(re.Explanation(), "–—") {
				t.Errorf("Explanation contains an en or em dash: %q", re.Explanation())
			}
		})
	}
}

// TestRedirect_IDNHostKeepsItsForm: an internationalised host is shown and
// suggested in the form an adopted address is stored in, never
// percent-encoded.
func TestRedirect_IDNHostKeepsItsForm(t *testing.T) {
	re := redirectFor(t, "ping", "https://bücher.de", "https://www.bücher.de"+pingRoute, 301)
	if want := "https://www.bücher.de" + pingRoute; re.To != want {
		t.Errorf("To = %q, want %q", re.To, want)
	}
	p := siteaddr.Plan("https://bücher.de", "https://www.bücher.de")
	if p.Decision != siteaddr.Adopt || re.SuggestedSiteURL != p.To || p.To != "https://www.bücher.de" {
		t.Errorf("SuggestedSiteURL = %q, Plan = %+v", re.SuggestedSiteURL, p)
	}
	if strings.Contains(re.To+re.SuggestedSiteURL+re.Explanation(), "%") {
		t.Errorf("percent-encoded host: To=%q Suggested=%q", re.To, re.SuggestedSiteURL)
	}
}

// TestRedirectError_ErrorTextIsPinned: the web client matches the tail of
// this text, so it must not move.
func TestRedirectError_ErrorTextIsPinned(t *testing.T) {
	re := &RedirectError{
		Command: "backup",
		Status:  301,
		From:    "https://example.com/wp-json/wpmgr/v1/command/backup",
		To:      "https://www.example.com/wp-json/wpmgr/v1/command/backup",
	}
	want := "backup command: https://example.com redirects to https://www.example.com/wp-json/wpmgr/v1/command/backup (HTTP 301); commands are sent only to the site's saved address, so the redirect was not followed"
	if got := re.Error(); got != want {
		t.Errorf("Error():\n got %q\nwant %q", got, want)
	}
	re.To = ""
	want = "backup command: https://example.com redirects to no usable target (HTTP 301); commands are sent only to the site's saved address, so the redirect was not followed"
	if got := re.Error(); got != want {
		t.Errorf("Error() with no target:\n got %q\nwant %q", got, want)
	}
}

// TestRedirect_HostileLocationIsNeverReadAsAStatus is the contract the text
// classifiers rely on: whatever path a site puts in its Location, the
// RedirectError text never matches the canonical agent-reject format, and
// VerifyReachableWithReason settles it as a redirect with one request and no
// metadata fallback.
func TestRedirect_HostileLocationIsNeverReadAsAStatus(t *testing.T) {
	statusRE := regexp.MustCompile(httpStatusPattern.String())
	for _, path := range []string{
		"/status 404" + pingRoute,
		"/rejected by agent: status 404" + pingRoute,
		"/status%20404" + pingRoute,
		"/status%20400/rejected%20by%20agent" + pingRoute,
	} {
		t.Run(path, func(t *testing.T) {
			origin := newRedirectingServer(t, false, http.StatusMovedPermanently, func() string { return "https://www.example.test" + path })
			re := redirectFor(t, "ping", origin.srv.URL, "https://www.example.test"+path, 301)
			msg := re.Error()
			if statusRE.MatchString(msg) || strings.Contains(msg, "rejected by agent") {
				t.Fatalf("RedirectError text %q reads as an agent status", msg)
			}

			alive, fallbackUsed, reason, err := realCommandClient(t).VerifyReachableWithReason(context.Background(), uuid.New(), origin.srv.URL)
			if err != nil {
				t.Fatalf("VerifyReachableWithReason: %v", err)
			}
			if alive || fallbackUsed || reason != ReasonRedirected {
				t.Errorf("alive=%v fallbackUsed=%v reason=%q, want false false %q", alive, fallbackUsed, reason, ReasonRedirected)
			}
			if n := origin.hits.Load(); n != 1 {
				t.Errorf("site received %d requests, want exactly 1 (no metadata fallback)", n)
			}
		})
	}
}
