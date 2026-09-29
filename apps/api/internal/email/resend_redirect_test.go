package email

// resend_redirect_test.go — GH #755 F4: a site that redirects its command
// address answers ResendEmail with OK:false and a Detail naming the redirect
// target (see service.go's ResendEmail, the `agentcmd.AsRedirect(err)` check
// checked before the generic resendFailureMessage text match). Before this
// file, deleting that check left the email package green: the redirect error
// fell through to resendFailureMessage instead, which produces a DIFFERENT
// Detail (and can misread the redirect's target path as a stale-plugin 404),
// so this test catches the regression on the exact wording, not just on
// "ok=false".

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

// TestResendEmail_Redirect_NotSentToWrongAddress: a site that redirects its
// command address fails the resend with OK:false, a Detail starting "Resend
// not started." and naming the redirect target, and never spends the
// resent_count increment or the email.resent audit row a real send would.
func TestResendEmail_Redirect_NotSentToWrongAddress(t *testing.T) {
	tenantID, siteID, logID := uuid.New(), uuid.New(), uuid.New()
	repo := newFakeResendRepo(logID, 42)
	agent := &fakeResendAgent{resulErr: &agentcmd.RedirectError{
		Command:          "resend_email",
		Status:           301,
		From:             "https://example.com/wp-json/wpmgr/v1/command/resend_email",
		To:               "https://www.example.com/wp-json/wpmgr/v1/command/resend_email",
		SuggestedSiteURL: "https://www.example.com",
	}}
	svc := newResendSvc(repo, agent)

	res, err := svc.ResendEmail(context.Background(), tenantID, siteID, logID)
	if err != nil {
		t.Fatalf("ResendEmail: unexpected error: %v", err)
	}
	if res.OK {
		t.Fatal("expected ok=false when the site redirects its command address")
	}
	if !strings.HasPrefix(res.Detail, "Resend not started.") {
		t.Errorf("Detail = %q, want prefix %q", res.Detail, "Resend not started.")
	}
	if !strings.Contains(res.Detail, "https://www.example.com") {
		t.Errorf("Detail %q does not name the redirect target", res.Detail)
	}
	if strings.Contains(res.Detail, "too old") {
		t.Errorf("a redirect must not be misread as a stale-plugin 404, got %q", res.Detail)
	}
	if agent.calls != 1 {
		t.Errorf("resend_email command sent %d times, want 1", agent.calls)
	}
	if repo.incrCalls != 0 {
		t.Errorf("resent_count incremented %d time(s) on a redirected resend, want 0", repo.incrCalls)
	}
	if _, ok := resendAuditMeta(logID, res); ok {
		t.Error("a redirected resend must not be audited as email.resent")
	}
}
