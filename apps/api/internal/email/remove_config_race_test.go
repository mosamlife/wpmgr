// remove_config_race_test.go: GH #968, a push of a site's email config that is
// already in flight when the site's provider is removed.
//
// A push of the row as it already stood does not move updated_at, so the
// removal's compare-and-delete cannot see it. Only excluding the two from each
// other keeps the site and the control plane in agreement: either the push
// finishes first and the removal then clears what it pushed, or the removal
// finishes first and the push finds nothing to send.
package email

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

// heldSite models what the site's agent holds after each command lands.
type heldSite struct {
	mu       sync.Mutex
	provider string // the from address of the config it holds; "" once cleared
	secret   string // the credential it holds; "" once cleared
}

func (h *heldSite) get() (string, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.provider, h.secret
}

// holdingAgent holds its first sync on the wire: the push has left the control
// plane, and lands on the site only once the test releases it.
type holdingAgent struct {
	orderedAgent
	site       *heldSite
	mu         sync.Mutex
	holdNext   bool
	dispatched chan struct{}
	release    chan struct{}
}

func (a *holdingAgent) SyncEmailConfig(ctx context.Context, siteID uuid.UUID, url string, req agentcmd.EmailConfigRequest) (agentcmd.EmailConfigResult, error) {
	a.mu.Lock()
	hold := a.holdNext
	a.holdNext = false
	a.mu.Unlock()
	if hold {
		close(a.dispatched)
		<-a.release
	}
	a.site.mu.Lock()
	a.site.provider = req.FromAddress
	if req.Secret != nil {
		a.site.secret = *req.Secret
	}
	a.site.mu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.orderedAgent.SyncEmailConfig(ctx, siteID, url, req)
}

func (a *holdingAgent) ClearEmailConfig(ctx context.Context, siteID uuid.UUID, url string) (agentcmd.ClearEmailConfigResult, error) {
	a.site.mu.Lock()
	a.site.provider, a.site.secret = "", ""
	a.site.mu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.orderedAgent.ClearEmailConfig(ctx, siteID, url)
}

func newHoldingFixture(t *testing.T) (*removeFixture, *holdingAgent) {
	t.Helper()
	f := newRemoveFixture(t)
	a := &holdingAgent{
		orderedAgent: orderedAgent{log: f.log},
		site:         &heldSite{provider: "site@example.com", secret: "stored_secret"},
		holdNext:     true,
		dispatched:   make(chan struct{}),
		release:      make(chan struct{}),
	}
	f.svc.SetAgentClient(a, &fakeSiteLookup{})
	return f, a
}

// raceRemoval starts the removal while the held push is on the wire, gives it
// time to run as far as it can, then lets the push land, and waits for both.
func raceRemoval(t *testing.T, f *removeFixture, a *holdingAgent, push func()) (RemoveSiteConfigResult, error) {
	t.Helper()
	pushed := make(chan struct{})
	go func() {
		defer close(pushed)
		push()
	}()
	select {
	case <-a.dispatched:
	case <-time.After(5 * time.Second):
		t.Fatal("the push never reached the agent")
	}

	type outcome struct {
		res RemoveSiteConfigResult
		err error
	}
	removed := make(chan outcome, 1)
	go func() {
		res, err := f.remove()
		removed <- outcome{res, err}
	}()
	// Long enough for a removal that nothing holds back to finish.
	time.Sleep(150 * time.Millisecond)
	close(a.release)

	var out outcome
	select {
	case out = <-removed:
	case <-time.After(15 * time.Second):
		t.Fatal("the removal never returned")
	}
	select {
	case <-pushed:
	case <-time.After(5 * time.Second):
		t.Fatal("the push never returned")
	}
	return out.res, out.err
}

// assertSiteAgrees fails when the control plane and the site disagree about
// whether the site still has a provider of its own.
func assertSiteAgrees(t *testing.T, f *removeFixture, a *holdingAgent) {
	t.Helper()
	_, rowLeft := f.siteRow()
	provider, secret := a.site.get()
	t.Logf("events=%q; row present=%v; the site holds provider=%q secret=%q", f.log.events, rowLeft, provider, secret)
	if rowLeft {
		if provider != "site@example.com" {
			t.Fatalf("the row stands but the site holds provider=%q", provider)
		}
		return
	}
	if provider != "" || secret != "" {
		t.Fatalf("the removal reported done and the row is gone, but the site still holds provider=%q secret=%q", provider, secret)
	}
}

func TestRemoveSiteConfigWaitsForASyncInFlight(t *testing.T) {
	f, a := newHoldingFixture(t)
	res, err := raceRemoval(t, f, a, func() {
		_, _ = f.svc.SyncConfigToAgent(context.Background(), f.tenantID, f.siteID)
	})
	if err != nil {
		t.Fatalf("removal: %v", err)
	}
	if res.Fallback != FallbackWordPress {
		t.Fatalf("fallback = %q, want %q", res.Fallback, FallbackWordPress)
	}
	assertSiteAgrees(t, f, a)
}

func TestRemoveSiteConfigWaitsForASendTestPreSyncInFlight(t *testing.T) {
	f, a := newHoldingFixture(t)
	res, err := raceRemoval(t, f, a, func() {
		_, _ = f.svc.SendTest(context.Background(), f.tenantID, f.siteID, TestSendInput{To: "ops@example.com"})
	})
	if err != nil {
		t.Fatalf("removal: %v", err)
	}
	if res.Fallback != FallbackWordPress {
		t.Fatalf("fallback = %q, want %q", res.Fallback, FallbackWordPress)
	}
	assertSiteAgrees(t, f, a)
}

func TestRemoveSiteConfigWaitsForASaveInFlight(t *testing.T) {
	f, a := newHoldingFixture(t)
	sid := f.siteID
	secret := "stored_secret"
	res, err := raceRemoval(t, f, a, func() {
		_, _ = f.svc.UpsertSiteConfig(context.Background(), UpsertInput{
			TenantID: f.tenantID, SiteID: &sid, Provider: "smtp",
			FromAddress: "site@example.com",
			Config:      map[string]any{"host": "smtp.site.example", "port": 587, "username": "site"},
			SecretRaw:   &secret, RetentionDays: 30,
		})
	})
	if err != nil {
		t.Fatalf("removal: %v", err)
	}
	if res.Fallback != FallbackWordPress {
		t.Fatalf("fallback = %q, want %q", res.Fallback, FallbackWordPress)
	}
	assertSiteAgrees(t, f, a)
}
