package assistantrequest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
	"github.com/mosamlife/wpmgr/apps/api/internal/org"
	"github.com/mosamlife/wpmgr/apps/api/internal/perf"
)

func TestLifecycleLockKeyMatchesOrg(t *testing.T) {
	if lifecycleLockKey != org.LifecycleLockKey {
		t.Fatalf("lifecycleLockKey = %q, org.LifecycleLockKey = %q: the reservation's try-lock would not exclude an organisation delete",
			lifecycleLockKey, org.LifecycleLockKey)
	}
}

func TestWriteToolsFromEnv(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    bool
		wantErr bool
	}{
		{"", false, false}, {"off", false, false}, {"OFF", false, false}, {" on ", true, false},
		{"on", true, false}, {"true", false, true}, {"1", false, true}, {"yes", false, true},
	} {
		got, err := WriteToolsFromEnv(tc.in)
		if got != tc.want || (err != nil) != tc.wantErr {
			t.Errorf("WriteToolsFromEnv(%q) = %v, %v; want %v, err=%v", tc.in, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestRequireSession_RefusesEverythingButAPerson(t *testing.T) {
	tenant, user := uuid.New(), uuid.New()
	cases := map[string]domain.Principal{
		"api key":         {Type: domain.PrincipalAPIKey, APIKeyID: uuid.New(), TenantID: tenant, Role: "admin"},
		"no type":         {UserID: user, TenantID: tenant},
		"user, no id":     {Type: domain.PrincipalUser, TenantID: tenant},
		"user, no tenant": {Type: domain.PrincipalUser, UserID: user},
	}
	for name, p := range cases {
		err := requireSession(p)
		de, ok := domain.AsDomain(err)
		if !ok || domain.HTTPStatus(de) != 403 {
			t.Errorf("%s: requireSession = %v, want a 403", name, err)
		}
	}
	if err := requireSession(domain.Principal{Type: domain.PrincipalUser, UserID: user, TenantID: tenant}); err != nil {
		t.Fatalf("a signed-in person was refused: %v", err)
	}
}

func TestAgentMeetsFloor(t *testing.T) {
	floor := mcp.MinAgentVersionForOriginOnlyPurge
	for v, want := range map[string]bool{
		floor: true, "9.0.0": true, "0.61.152": false, "": false, "garbage": false,
		"0.61.153-beta": false, " " + floor + " ": true, "1.0": true,
	} {
		if got := agentMeetsFloor(v); got != want {
			t.Errorf("agentMeetsFloor(%q) = %v, want %v (floor %s)", v, got, want, floor)
		}
	}
}

// TestCloseWithoutSite_RefusesReasonOutsideItsSet proves a reason from the
// site transaction's set, or any other value, is refused before any database
// access: the service here has no pool, so reaching the database would panic.
func TestCloseWithoutSite_RefusesReasonOutsideItsSet(t *testing.T) {
	s := &Service{repo: &Repo{pool: nil}}
	for _, r := range []string{
		ReasonForbiddenByContext, ReasonAgentOutdated, ReasonDispatchDeadlinePassed,
		ReasonOrganisationDeleted, ReasonTransportPreSend, "", "purged",
	} {
		err := s.closeWithoutSite(context.Background(), uuid.New(), uuid.New(), r)
		if err == nil || !strings.Contains(err.Error(), "is not one it may record") {
			t.Errorf("closeWithoutSite(%q) = %v, want a refusal", r, err)
		}
	}
	if len(closeWithoutSiteReasons) != 4 {
		t.Fatalf("closeWithoutSite's reason set has %d entries, want the 4 closes decided before a site principal exists", len(closeWithoutSiteReasons))
	}
	for _, r := range []string{ReasonGrantInactive, ReasonAssistantPaused, ReasonCapabilityNotHeld, ReasonSiteAbsent} {
		if _, ok := closeWithoutSiteReasons[r]; !ok {
			t.Errorf("closeWithoutSite does not accept %q", r)
		}
	}
}

func boolp(b bool) *bool { return &b }

func TestClassify(t *testing.T) {
	notSent := fmt.Errorf("wrap: %w", agentcmd.ErrCommandNotSent)
	reported := fmt.Errorf("cache_purge rejected by agent: x: %w", agentcmd.ErrAgentReportedFailure)
	agentFailed := &agentcmd.CommandError{Command: "cache_purge", Status: 500, Code: "wpmgr_command_failed", DataStatus: 500}
	proxy500 := &agentcmd.CommandError{Command: "cache_purge", Status: 500}

	cases := []struct {
		name string
		res  perf.AssistantPurgeResult
		err  error
		want string
	}{
		{"not sent", perf.AssistantPurgeResult{}, notSent, OutcomeNotSent},
		{"no sender", perf.AssistantPurgeResult{}, perf.ErrAssistantPurgeNotWired, OutcomeNotSent},
		{"ok:false", perf.AssistantPurgeResult{Agent: agentcmd.CachePurgeResult{Detail: "failed at https://evil.example/x from 10.0.0.1 in /var/www/secret"}}, reported, OutcomeSiteReportedFailure},
		{"agent failed", perf.AssistantPurgeResult{}, fmt.Errorf("w: %w", agentFailed), OutcomeAgentFailed},
		{"proxy 500", perf.AssistantPurgeResult{}, proxy500, OutcomeUnknown},
		{"timeout", perf.AssistantPurgeResult{}, context.DeadlineExceeded, OutcomeUnknown},
		{"decode", perf.AssistantPurgeResult{}, errors.New("decode cache_purge response: bad"), OutcomeUnknown},
		{"purged, confirmed", perf.AssistantPurgeResult{Agent: agentcmd.CachePurgeResult{OK: true, OriginOnlyHonoured: boolp(true)}, WpmgrCDN: perf.AssistantCDNNotAttempted}, nil, OutcomePurged},
		{"purged, no echo", perf.AssistantPurgeResult{Agent: agentcmd.CachePurgeResult{OK: true}}, nil, OutcomePurged},
	}
	for _, tc := range cases {
		got := classify(tc.res, tc.err)
		if got.Outcome != tc.want {
			t.Errorf("%s: outcome %q, want %q", tc.name, got.Outcome, tc.want)
		}
		if (got.Outcome == OutcomeNotSent) != (got.NotSentReason != nil) {
			t.Errorf("%s: not_sent and its reason must go together: %+v", tc.name, got)
		}
		if got.NotSentReason != nil && *got.NotSentReason != ReasonTransportPreSend {
			t.Errorf("%s: reason %q", tc.name, *got.NotSentReason)
		}
	}

	// Site prose is redacted before it is stored.
	got := classify(cases[2].res, cases[2].err)
	if got.SiteReportedText == nil {
		t.Fatal("ok:false stored no site text")
	}
	for _, leak := range []string{"evil.example", "10.0.0.1", "/var/www/secret"} {
		if strings.Contains(*got.SiteReportedText, leak) {
			t.Errorf("site_reported_text kept %q: %q", leak, *got.SiteReportedText)
		}
	}

	// Without the echo the clear is recorded as not confirmed, with no report.
	noEcho := classify(cases[8].res, nil)
	if noEcho.OriginOnlyConfirmed == nil || *noEcho.OriginOnlyConfirmed || noEcho.HostingCleared != nil {
		t.Errorf("no echo: %+v", noEcho)
	}
}

func TestClassify_HostingReportIsFilteredToTheClosedSets(t *testing.T) {
	res := perf.AssistantPurgeResult{Agent: agentcmd.CachePurgeResult{
		OK: true, OriginOnlyHonoured: boolp(true),
		Integrations: []agentcmd.CachePurgeIntegration{
			{Slug: "kinsta", Action: "skipped_reach_unconfirmed"},
			{Slug: "wpcloud", Action: "purged_all"},
			{Slug: "<script>", Action: "purged_all"},
			{Slug: "varnish", Action: "rm -rf"},
			{Slug: "kinsta", Action: "purged_all"},
		},
	}}
	got := classify(res, nil)
	if strings.Join(got.HostingCleared, ",") != "wpcloud" || strings.Join(got.HostingSkipped, ",") != "kinsta" {
		t.Fatalf("cleared=%v skipped=%v", got.HostingCleared, got.HostingSkipped)
	}
}

func TestClassify_UnknownIntegrationsAreCountedNotEchoed(t *testing.T) {
	res := perf.AssistantPurgeResult{Agent: agentcmd.CachePurgeResult{
		OK: true, OriginOnlyHonoured: boolp(true),
		Integrations: []agentcmd.CachePurgeIntegration{
			{Slug: "wpcloud", Action: "purged_all"},
			{Slug: "<script>", Action: "purged_all"},
			{Slug: "varnish", Action: "rm -rf"},
			{Slug: "kinsta", Action: "purged_all"},
			{Slug: "kinsta", Action: "purged_all"},
		},
	}}
	got := classify(res, nil)
	// One unknown slug and one unknown action; a repeated known slug is not unknown.
	if got.UnknownIntegrations != 2 {
		t.Fatalf("UnknownIntegrations = %d, want 2", got.UnknownIntegrations)
	}
	ev := outcomeAuditEvent(DispatchArgs{}, sqlc.AssistantCachePurgeRequest{Scope: "all"}, got)
	if ev.Metadata["unknown_integrations"] != 2 {
		t.Fatalf("audit metadata unknown_integrations = %v", ev.Metadata["unknown_integrations"])
	}
}
