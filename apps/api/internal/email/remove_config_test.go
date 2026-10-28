// remove_config_test.go: GH #968, removing a site's own email provider.
//
// The order is the contract. The agent is told to clear its email config
// first, and the site's row is deleted only once it confirms. The opposite
// order fails open: a deleted row with an agent that never heard about it
// leaves the site sending through the removed provider while the dashboard
// says it does not. So the fakes below write every agent call and every delete
// into one shared log, and the tests read the order from it.
package email

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// removeLog records, in order, what the agent and the repository were asked
// to do.
type removeLog struct{ events []string }

func (l *removeLog) add(e string) { l.events = append(l.events, e) }

// orderedAgent is the agent half of the order proofs. A sync is logged with
// the from address it carried, so a test can tell which config was pushed.
type orderedAgent struct {
	log      *removeLog
	clearErr error
	syncErr  error
	syncs    []agentcmd.EmailConfigRequest
}

func (a *orderedAgent) SyncEmailConfig(_ context.Context, _ uuid.UUID, _ string, req agentcmd.EmailConfigRequest) (agentcmd.EmailConfigResult, error) {
	a.log.add("agent.sync:" + req.FromAddress)
	a.syncs = append(a.syncs, req)
	if a.syncErr != nil {
		return agentcmd.EmailConfigResult{}, a.syncErr
	}
	return agentcmd.EmailConfigResult{OK: true}, nil
}

func (a *orderedAgent) SendTestEmail(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.SendTestEmailRequest) (agentcmd.SendTestEmailResult, error) {
	a.log.add("agent.send_test")
	return agentcmd.SendTestEmailResult{}, errors.New("send_test_email is not on the removal path")
}

func (a *orderedAgent) ResendEmail(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.ResendEmailRequest) (agentcmd.ResendEmailResult, error) {
	a.log.add("agent.resend")
	return agentcmd.ResendEmailResult{}, errors.New("resend_email is not on the removal path")
}

func (a *orderedAgent) ClearEmailConfig(_ context.Context, _ uuid.UUID, _ string) (agentcmd.ClearEmailConfigResult, error) {
	a.log.add("agent.clear")
	if a.clearErr != nil {
		return agentcmd.ClearEmailConfigResult{}, a.clearErr
	}
	return agentcmd.ClearEmailConfigResult{OK: true, Cleared: true, Detail: "email config cleared"}, nil
}

// orderedRepo wraps fakeRepo and logs the delete. beforeDelete runs at the
// moment of the delete, which is after the agent was cleared: it is where a
// test lands an edit, or a concurrent removal, between the read and the delete.
type orderedRepo struct {
	*fakeRepo
	log          *removeLog
	beforeDelete func()
	deleteErr    error
	// refuse makes the delete report zero rows and leave the row in place,
	// which is how the m112 site-scope policy reports a refusal.
	refuse bool
}

func (r *orderedRepo) DeleteSiteConfig(ctx context.Context, tenantID, siteID uuid.UUID, expected time.Time) (int64, error) {
	if r.beforeDelete != nil {
		r.beforeDelete()
	}
	r.log.add("repo.delete")
	if r.deleteErr != nil {
		return 0, r.deleteErr
	}
	if r.refuse {
		return 0, nil
	}
	return r.fakeRepo.DeleteSiteConfig(ctx, tenantID, siteID, expected)
}

type removeFixture struct {
	svc      *Service
	repo     *orderedRepo
	agent    *orderedAgent
	log      *removeLog
	tenantID uuid.UUID
	siteID   uuid.UUID
}

// readAt is the updated_at the site's row carries when the service reads it.
// Microseconds on purpose: that is the precision the column stores.
var readAt = time.Date(2026, 10, 28, 12, 0, 0, 123456000, time.UTC)

// newRemoveFixture seeds one site with its own smtp config and no
// organisation default.
func newRemoveFixture(t *testing.T) *removeFixture {
	t.Helper()
	log := &removeLog{}
	base := newFakeRepo()
	f := &removeFixture{
		repo:     &orderedRepo{fakeRepo: base, log: log},
		agent:    &orderedAgent{log: log},
		log:      log,
		tenantID: uuid.New(),
		siteID:   uuid.New(),
	}
	sid := f.siteID
	base.site[siteKey(f.tenantID, f.siteID)] = Config{
		ID:          uuid.New(),
		TenantID:    f.tenantID,
		SiteID:      &sid,
		Provider:    "smtp",
		FromAddress: "site@example.com",
		Config:      map[string]any{"host": "smtp.site.example", "port": 587, "username": "site"},
		SecretSet:   true,
		UpdatedAt:   readAt,
	}
	f.svc = NewService(&Repo{}, &fakeEncryptor{}, nil)
	f.svc.repo = f.repo
	f.svc.SetAgentClient(f.agent, &fakeSiteLookup{})
	return f
}

// withOrgDefault gives the organisation a default config with a credential.
func (f *removeFixture) withOrgDefault() {
	f.repo.org[f.tenantID] = Config{
		ID:          uuid.New(),
		TenantID:    f.tenantID,
		Provider:    "smtp",
		FromAddress: "org@example.com",
		Config:      map[string]any{"host": "smtp.org.example", "port": 587, "username": "org"},
		SecretSet:   true,
		UpdatedAt:   readAt,
	}
}

func (f *removeFixture) siteRow() (Config, bool) {
	cfg, ok := f.repo.site[siteKey(f.tenantID, f.siteID)]
	return cfg, ok
}

func (f *removeFixture) remove() (RemoveSiteConfigResult, error) {
	return f.svc.RemoveSiteConfig(context.Background(), f.tenantID, f.siteID)
}

func wantDomainError(t *testing.T, err error, kind domain.Kind, code string) *domain.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("want a %s error, got success", code)
	}
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("want a domain error %s, got %T: %v", code, err, err)
	}
	if de.Kind != kind || de.Code != code {
		t.Fatalf("got %v/%s (%q), want %v/%s", de.Kind, de.Code, de.Message, kind, code)
	}
	return de
}

func wantEvents(t *testing.T, got []string, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	if got == nil {
		got = []string{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("calls, in order:\n got  %q\n want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// The order, and the happy path
// ---------------------------------------------------------------------------

// TestRemoveSiteConfig_TellsTheAgentBeforeDeletingTheRow is the order proof.
// It also pins that the removed config is never pushed back afterwards: with
// no organisation default there is nothing to push at all.
func TestRemoveSiteConfig_TellsTheAgentBeforeDeletingTheRow(t *testing.T) {
	f := newRemoveFixture(t)

	res, err := f.remove()
	if err != nil {
		t.Fatalf("RemoveSiteConfig: %v", err)
	}
	wantEvents(t, f.log.events, "agent.clear", "repo.delete")
	if _, ok := f.siteRow(); ok {
		t.Fatal("the site's own row is still there after a confirmed removal")
	}
	if res.Fallback != FallbackWordPress || res.OrgDefaultSynced {
		t.Fatalf("result = %+v, want fallback %q and nothing synced", res, FallbackWordPress)
	}
	if res.RemovedProvider != "smtp" {
		t.Fatalf("RemovedProvider = %q, want smtp", res.RemovedProvider)
	}
}

// TestRemoveSiteConfig_FallsBackToTheOrgDefault: once the row is gone, a site
// in an organisation with a default gets that default pushed, carrying the
// organisation's credential, and never the config that was just removed.
func TestRemoveSiteConfig_FallsBackToTheOrgDefault(t *testing.T) {
	f := newRemoveFixture(t)
	f.withOrgDefault()

	res, err := f.remove()
	if err != nil {
		t.Fatalf("RemoveSiteConfig: %v", err)
	}
	wantEvents(t, f.log.events, "agent.clear", "repo.delete", "agent.sync:org@example.com")
	if res.Fallback != FallbackOrgDefault || !res.OrgDefaultSynced {
		t.Fatalf("result = %+v, want the org default, synced", res)
	}
	push := f.agent.syncs[0]
	if push.Secret == nil || *push.Secret != f.repo.orgPlain {
		t.Fatalf("the org default was pushed with secret %v, want the org credential", push.Secret)
	}
	if push.Config["host"] != "smtp.org.example" {
		t.Fatalf("pushed host %v, want the org default's", push.Config["host"])
	}
	if _, ok := f.repo.org[f.tenantID]; !ok {
		t.Fatal("removing a site's provider deleted the organisation default")
	}
}

// TestRemoveSiteConfig_OrgDefaultPushFailureIsReported: the removal stands
// (the row is gone and the agent is clear), and the result says the default
// has not reached the site instead of claiming it has.
func TestRemoveSiteConfig_OrgDefaultPushFailureIsReported(t *testing.T) {
	f := newRemoveFixture(t)
	f.withOrgDefault()
	f.agent.syncErr = errors.New("sync_email_config command transport: connection refused")

	res, err := f.remove()
	if err != nil {
		t.Fatalf("RemoveSiteConfig: %v", err)
	}
	if res.Fallback != FallbackOrgDefault || res.OrgDefaultSynced {
		t.Fatalf("result = %+v, want the org default, NOT synced", res)
	}
	if !strings.Contains(res.Detail, "Sync to site") {
		t.Fatalf("detail %q does not tell the operator how to finish", res.Detail)
	}
}

// ---------------------------------------------------------------------------
// The agent does not confirm: nothing is deleted
// ---------------------------------------------------------------------------

func TestRemoveSiteConfig_AgentFailureDeletesNothing(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantText string
	}{
		{
			name:     "site unreachable",
			err:      errors.New("clear_email_config command transport: dial tcp: connection refused"),
			wantText: "Could not connect to the site.",
		},
		{
			name: "agent refused the token",
			err: &agentcmd.CommandError{Command: agentcmd.ClearEmailCommand, Status: http.StatusForbidden,
				Code: "wpmgr_sig_failed"},
			wantText: "wpmgr_sig_failed",
		},
		{
			name: "agent answered cleared=false",
			err: fmt.Errorf("%w: %s", agentcmd.ErrEmailConfigNotCleared,
				"email settings removed; the stored email secret could not be removed"),
			wantText: "the stored email secret could not be removed",
		},
		{
			// A 404 that is not wpmgr_unknown_command is not an old agent:
			// rest_no_route means the plugin is not active at all.
			name:     "plugin inactive (rest_no_route)",
			err:      &agentcmd.CommandError{Command: agentcmd.ClearEmailCommand, Status: http.StatusNotFound, Code: "rest_no_route"},
			wantText: "HTTP 404",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRemoveFixture(t)
			f.withOrgDefault()
			f.agent.clearErr = tc.err

			_, err := f.remove()
			de := wantDomainError(t, err, domain.KindServiceUnavailable, "email_agent_clear_failed")
			if !strings.Contains(de.Message, tc.wantText) {
				t.Fatalf("message %q does not say why (want it to contain %q)", de.Message, tc.wantText)
			}
			if !strings.Contains(de.Message, "nothing was removed") {
				t.Fatalf("message %q does not say nothing was removed", de.Message)
			}
			wantEvents(t, f.log.events, "agent.clear")
			if _, ok := f.siteRow(); !ok {
				t.Fatal("the site's row was deleted although the agent did not confirm")
			}
		})
	}
}

// TestRemoveSiteConfig_OldAgentIsAskedToUpdate: an agent that predates the
// command answers 404 wpmgr_unknown_command, which gets its own answer.
func TestRemoveSiteConfig_OldAgentIsAskedToUpdate(t *testing.T) {
	f := newRemoveFixture(t)
	f.agent.clearErr = fmt.Errorf("wrapped: %w", &agentcmd.CommandError{
		Command: agentcmd.ClearEmailCommand, Status: http.StatusNotFound, Code: "wpmgr_unknown_command",
	})

	_, err := f.remove()
	de := wantDomainError(t, err, domain.KindConflict, "email_agent_outdated")
	if de.Message != "Update the WPMgr agent on this site to remove its email provider." {
		t.Fatalf("message = %q", de.Message)
	}
	wantEvents(t, f.log.events, "agent.clear")
	if _, ok := f.siteRow(); !ok {
		t.Fatal("the site's row was deleted for an agent that could not clear it")
	}
}

// ---------------------------------------------------------------------------
// The delete matches nothing after the agent confirmed
// ---------------------------------------------------------------------------

// TestRemoveSiteConfig_EditedSinceReadIsAConflictAndRepushesTheEdit: an edit
// saved while the agent was clearing stands. The agent was already cleared,
// so the CURRENT row goes back to it, and the caller is told nothing was
// removed.
func TestRemoveSiteConfig_EditedSinceReadIsAConflictAndRepushesTheEdit(t *testing.T) {
	f := newRemoveFixture(t)
	key := siteKey(f.tenantID, f.siteID)
	f.repo.beforeDelete = func() {
		edited := f.repo.site[key]
		edited.FromAddress = "edited@example.com"
		edited.UpdatedAt = readAt.Add(time.Microsecond)
		f.repo.site[key] = edited
	}

	_, err := f.remove()
	de := wantDomainError(t, err, domain.KindConflict, "email_config_changed")
	if de.Message != "The email settings changed while removing them; nothing was removed." {
		t.Fatalf("message = %q", de.Message)
	}
	wantEvents(t, f.log.events, "agent.clear", "repo.delete", "agent.sync:edited@example.com")
	row, ok := f.siteRow()
	if !ok || row.FromAddress != "edited@example.com" {
		t.Fatalf("the edited row did not survive: %+v (present %v)", row, ok)
	}
}

// TestRemoveSiteConfig_ConflictRepushFailureIsSaid: when the edit cannot be
// sent back, the conflict says the site is on WordPress's mailer meanwhile.
func TestRemoveSiteConfig_ConflictRepushFailureIsSaid(t *testing.T) {
	f := newRemoveFixture(t)
	key := siteKey(f.tenantID, f.siteID)
	f.repo.beforeDelete = func() {
		edited := f.repo.site[key]
		edited.UpdatedAt = readAt.Add(time.Second)
		f.repo.site[key] = edited
	}
	f.agent.syncErr = errors.New("sync_email_config command transport: timeout")

	_, err := f.remove()
	de := wantDomainError(t, err, domain.KindConflict, "email_config_changed")
	if !strings.HasPrefix(de.Message, removeChangedMessage) || !strings.Contains(de.Message, "Sync to site") {
		t.Fatalf("message = %q", de.Message)
	}
}

// TestRemoveSiteConfig_ConcurrentRemovalStillFallsBack: another request
// removed the row between this one's read and its delete. That is the end
// state asked for, and the organisation default still has to reach the site,
// because this request's clear may have landed after the other's push.
func TestRemoveSiteConfig_ConcurrentRemovalStillFallsBack(t *testing.T) {
	f := newRemoveFixture(t)
	f.withOrgDefault()
	f.repo.beforeDelete = func() { delete(f.repo.site, siteKey(f.tenantID, f.siteID)) }

	res, err := f.remove()
	if err != nil {
		t.Fatalf("RemoveSiteConfig: %v", err)
	}
	if res.Fallback != FallbackOrgDefault || !res.OrgDefaultSynced {
		t.Fatalf("result = %+v, want the org default, synced", res)
	}
	wantEvents(t, f.log.events, "agent.clear", "repo.delete", "agent.sync:org@example.com")
}

// TestRemoveSiteConfig_RefusedDeletePushesNothing: zero rows with the row
// unchanged is the site-scope policy refusing. The removed config is not
// pushed back to the agent that just confirmed it stopped using it.
func TestRemoveSiteConfig_RefusedDeletePushesNothing(t *testing.T) {
	f := newRemoveFixture(t)
	f.repo.refuse = true

	_, err := f.remove()
	wantDomainError(t, err, domain.KindForbidden, "email_site_config_delete_forbidden")
	wantEvents(t, f.log.events, "agent.clear", "repo.delete")
}

// TestRemoveSiteConfig_DeleteErrorPushesNothing: the database failed after the
// agent confirmed. The answer says so (a retry finishes it), and the removed
// config is not pushed back.
func TestRemoveSiteConfig_DeleteErrorPushesNothing(t *testing.T) {
	f := newRemoveFixture(t)
	f.withOrgDefault()
	f.repo.deleteErr = errors.New("connection reset by peer")

	_, err := f.remove()
	de := wantDomainError(t, err, domain.KindServiceUnavailable, "email_delete_site_config_failed")
	if !strings.Contains(de.Message, "Remove the email provider again") {
		t.Fatalf("message = %q", de.Message)
	}
	wantEvents(t, f.log.events, "agent.clear", "repo.delete")
}

// ---------------------------------------------------------------------------
// Nothing of its own to remove
// ---------------------------------------------------------------------------

// TestRemoveSiteConfig_NoOwnRowIs404: a site that inherits the organisation
// default has nothing to remove, and the agent is not touched.
func TestRemoveSiteConfig_NoOwnRowIs404(t *testing.T) {
	f := newRemoveFixture(t)
	f.withOrgDefault()
	delete(f.repo.site, siteKey(f.tenantID, f.siteID))

	_, err := f.remove()
	wantDomainError(t, err, domain.KindNotFound, "email_site_config_not_found")
	wantEvents(t, f.log.events)
	if _, ok := f.repo.org[f.tenantID]; !ok {
		t.Fatal("the organisation default was touched")
	}
}

// ---------------------------------------------------------------------------
// The handler: status, body, and the audit row
// ---------------------------------------------------------------------------

type capturingAudit struct{ events []audit.Event }

func (a *capturingAudit) Record(_ context.Context, e audit.Event) (audit.Entry, error) {
	a.events = append(a.events, e)
	return audit.Entry{}, nil
}

func deleteConfigRequest(t *testing.T, f *removeFixture) (*httptest.ResponseRecorder, *capturingAudit) {
	t.Helper()
	rec := &capturingAudit{}
	h := NewHandler(f.svc, nil)
	h.audit = rec
	engine := newTestOperatorEngine(h, f.tenantID)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/sites/"+f.siteID.String()+"/email/config", nil)
	engine.ServeHTTP(w, req)
	return w, rec
}

func TestDeleteConfigHandler_RemovesAndRecordsTheReset(t *testing.T) {
	f := newRemoveFixture(t)
	f.withOrgDefault()

	w, rec := deleteConfigRequest(t, f)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var body struct {
		RemovedProvider  string `json:"removed_provider"`
		Fallback         string `json:"fallback"`
		OrgDefaultSynced bool   `json:"org_default_synced"`
		Detail           string `json:"detail"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.RemovedProvider != "smtp" || body.Fallback != "org_default" || !body.OrgDefaultSynced || body.Detail == "" {
		t.Fatalf("body = %+v", body)
	}

	if len(rec.events) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(rec.events))
	}
	ev := rec.events[0]
	// The literal pins the wire name the audit log and its filters key on.
	if ev.Action != "site.email.config.reset" {
		t.Fatalf("audit action = %q, want site.email.config.reset", ev.Action)
	}
	if audit.ActionEmailConfigReset != ev.Action {
		t.Fatalf("audit.ActionEmailConfigReset = %q", audit.ActionEmailConfigReset)
	}
	if ev.TargetType != "site" || ev.TargetID != f.siteID.String() || ev.TenantID != f.tenantID {
		t.Fatalf("audit target = %s/%s in %s", ev.TargetType, ev.TargetID, ev.TenantID)
	}
	if ev.Metadata["provider"] != "smtp" || ev.Metadata["fallback"] != "org_default" || ev.Metadata["org_default_synced"] != true {
		t.Fatalf("audit metadata = %v", ev.Metadata)
	}
}

func TestDeleteConfigHandler_FailureStatusesWriteNoAudit(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(f *removeFixture)
		status int
		code   string
	}{
		{"no own config", func(f *removeFixture) { delete(f.repo.site, siteKey(f.tenantID, f.siteID)) },
			http.StatusNotFound, "email_site_config_not_found"},
		{"old agent", func(f *removeFixture) {
			f.agent.clearErr = &agentcmd.CommandError{Command: agentcmd.ClearEmailCommand, Status: 404, Code: "wpmgr_unknown_command"}
		}, http.StatusConflict, "email_agent_outdated"},
		{"edited since read", func(f *removeFixture) {
			key := siteKey(f.tenantID, f.siteID)
			f.repo.beforeDelete = func() {
				row := f.repo.site[key]
				row.UpdatedAt = readAt.Add(time.Minute)
				f.repo.site[key] = row
			}
		}, http.StatusConflict, "email_config_changed"},
		{"agent unreachable", func(f *removeFixture) { f.agent.clearErr = errors.New("transport: connection refused") },
			http.StatusServiceUnavailable, "email_agent_clear_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRemoveFixture(t)
			tc.setup(f)
			w, rec := deleteConfigRequest(t, f)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.code) {
				t.Fatalf("body %s does not carry code %s", w.Body.String(), tc.code)
			}
			if len(rec.events) != 0 {
				t.Fatalf("a failed removal wrote %d audit row(s): %+v", len(rec.events), rec.events)
			}
		})
	}
}
