package tests

// GH #1004, part 2 (Go). A re-enrollment the operator authorised re-pins the
// site's backup recipient: the attach clears it, the clear is audited after
// the enroll transaction commits, and the agent's next metadata push, signed
// with the new key, records the site's current recipient as a first set. A
// push on its own never replaces a recipient, and a push attributed to a key
// the re-enrollment rotated away cannot take the first set.
//
// Everything runs on one control plane mounted as server.go mounts it
// (gh755PushEnv): public /enroll, the authed /api/v1 site routes, and the real
// Ed25519 agent Authenticator in front of /agent/v1/metadata, against a
// database reached as wpmgr_app (NOSUPERUSER, NOBYPASSRLS; gh755PushSetup
// checks it). The audit rows are read back through the recorder's own
// tenant-scoped List.
//
// Selected by: go -C apps/api test -run '^TestC1004_' ./tests/

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	agentpkg "github.com/mosamlife/wpmgr/apps/api/internal/agent"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

const (
	c1004LegacyURL    = "https://c1004-legacy.example.test"
	c1004SiteFirstURL = "https://c1004-site-first.example.test"
	c1004NoReenrolURL = "https://c1004-no-reenrollment.example.test"
	c1004OldKeyURL    = "https://c1004-old-key.example.test"

	c1004RecipientBefore = "age1c1004recipientthesiteheldbeforereenrollment"
	c1004RecipientAfter  = "age1c1004recipientthesiteholdsafterreenrollment"
	c1004RecipientOther  = "age1c1004recipientpushedwithoutanyreenrollment"
	c1004RecipientStale  = "age1c1004recipientattributedtotherotatedkey"
)

// c1004PushRecipient is the agent's metadata push carrying rec, signed with
// the given key and sent through the real Authenticator.
func c1004PushRecipient(t *testing.T, env *gh755PushEnv, priv ed25519.PrivateKey, pub, rec string) int {
	t.Helper()
	body, err := json.Marshal(map[string]any{"wp_version": "6.6", "age_recipient": rec})
	if err != nil {
		t.Fatalf("marshal metadata body: %v", err)
	}
	status, _ := env.signedPush(t, priv, pub, "/agent/v1/metadata", body)
	return status
}

// c1004LegacyEnroll mints a tenant-wide (legacy) pairing code and enrolls url
// with a fresh agent key through POST /enroll, returning the site and the key.
func c1004LegacyEnroll(t *testing.T, env *gh755PushEnv, tenant uuid.UUID, url string) (uuid.UUID, ed25519.PrivateKey, string) {
	t.Helper()
	code, err := env.svc.CreatePairingCode(context.Background(), site.CreatePairingCodeInput{TenantID: tenant, SiteName: "c1004"})
	if err != nil {
		t.Fatalf("create legacy pairing code: %v", err)
	}
	_, priv, pub := genKey(t)
	status, body := env.do(t, http.MethodPost, "/enroll", map[string]any{
		"pairing_code":     code.Plaintext,
		"site_url":         url,
		"agent_public_key": pub,
		"wp_version":       "6.6",
		"php_version":      "8.3",
	})
	if status != http.StatusOK {
		t.Fatalf("legacy POST /enroll %s answered %d, want 200: %v", url, status, body)
	}
	id, err := uuid.Parse(body["site_id"].(string))
	if err != nil {
		t.Fatalf("site_id: %v", err)
	}
	return id, priv, pub
}

// c1004Reconnect is the dashboard's Disconnect then Reconnect, returning the
// fresh enrollment code.
func c1004Reconnect(t *testing.T, env *gh755PushEnv, siteID uuid.UUID) string {
	t.Helper()
	status, body := env.do(t, http.MethodPost, "/api/v1/sites/"+siteID.String()+"/revoke", map[string]any{"reason": "c1004"})
	if status != http.StatusOK {
		t.Fatalf("disconnect answered %d, want 200: %v", status, body)
	}
	status, body = env.do(t, http.MethodPost, "/api/v1/sites/"+siteID.String()+"/enrollment-codes", nil)
	if status != http.StatusCreated {
		t.Fatalf("reconnect answered %d, want 201: %v", status, body)
	}
	return body["enrollment_code"].(string)
}

// c1004RecipientAudits returns the site's site.backup.recipient.* rows in
// hash-chain order (oldest first), so the test asserts the order the events
// happened in rather than an order that ties on created_at could scramble.
func c1004RecipientAudits(t *testing.T, env *gh755PushEnv, tenant, siteID uuid.UUID) []audit.Entry {
	t.Helper()
	entries, err := env.rec.List(context.Background(), tenant, 1000, 0)
	if err != nil {
		t.Fatalf("audit list: %v", err)
	}
	byPrev := make(map[string]audit.Entry, len(entries))
	hashes := make(map[string]bool, len(entries))
	for _, e := range entries {
		byPrev[e.PrevHash] = e
		hashes[e.Hash] = true
	}
	var start audit.Entry
	found := false
	for _, e := range entries {
		if !hashes[e.PrevHash] {
			start, found = e, true
			break
		}
	}
	if !found && len(entries) > 0 {
		t.Fatalf("audit chain for tenant %s has no first entry", tenant)
	}
	var out []audit.Entry
	for e, ok := start, found; ok; e, ok = byPrev[e.Hash] {
		if e.TargetType != "site" || e.TargetID != siteID.String() {
			continue
		}
		switch e.Action {
		case audit.ActionSiteBackupRecipientSet, audit.ActionSiteBackupRecipientChangeRejected, audit.ActionSiteBackupRecipientCleared:
			out = append(out, e)
		}
	}
	return out
}

type c1004Want struct {
	action string
	meta   map[string]string
}

func c1004AssertAudits(t *testing.T, got []audit.Entry, want []c1004Want) {
	t.Helper()
	if len(got) != len(want) {
		var actions []string
		for _, e := range got {
			actions = append(actions, e.Action)
		}
		t.Fatalf("recipient audit rows = %v, want %d rows", actions, len(want))
	}
	for i, w := range want {
		if got[i].Action != w.action {
			t.Errorf("recipient audit row %d action = %s, want %s", i, got[i].Action, w.action)
			continue
		}
		if got[i].ActorType != audit.ActorSystem {
			t.Errorf("recipient audit row %d actor = %s, want %s", i, got[i].ActorType, audit.ActorSystem)
		}
		for k, v := range w.meta {
			if got[i].Metadata[k] != v {
				t.Errorf("recipient audit row %d (%s) metadata %s = %v, want %q", i, w.action, k, got[i].Metadata[k], v)
			}
		}
	}
}

func c1004WantRecipient(t *testing.T, env *gh755PushEnv, tenant, siteID uuid.UUID, want, when string) {
	t.Helper()
	if got := env.site(t, tenant, siteID).AgeRecipient; got != want {
		t.Fatalf("%s: age_recipient = %q, want %q", when, got, want)
	}
}

// (a) A legacy re-enrollment (tenant-wide pairing code, same address) clears
// the recipient and audits it; the next push, signed with the new key, sets
// the site's current one.
func TestC1004_LegacyReEnrollmentRepinsRecipient(t *testing.T) {
	env, tenant := gh755PushSetup(t, "c1004-legacy")

	siteID, priv1, pub1 := c1004LegacyEnroll(t, env, tenant, c1004LegacyURL)
	if status := c1004PushRecipient(t, env, priv1, pub1, c1004RecipientBefore); status != http.StatusOK {
		t.Fatalf("first push answered %d, want 200", status)
	}
	c1004WantRecipient(t, env, tenant, siteID, c1004RecipientBefore, "after the first push")

	again, priv2, pub2 := c1004LegacyEnroll(t, env, tenant, c1004LegacyURL)
	if again != siteID {
		t.Fatalf("legacy re-enrollment attached to %s, want the existing site %s", again, siteID)
	}
	c1004WantRecipient(t, env, tenant, siteID, "", "after the legacy re-enrollment")

	if status := c1004PushRecipient(t, env, priv2, pub2, c1004RecipientAfter); status != http.StatusOK {
		t.Fatalf("push with the new key answered %d, want 200", status)
	}
	c1004WantRecipient(t, env, tenant, siteID, c1004RecipientAfter, "after the push signed with the new key")

	c1004AssertAudits(t, c1004RecipientAudits(t, env, tenant, siteID), []c1004Want{
		{audit.ActionSiteBackupRecipientSet, map[string]string{"recipient": c1004RecipientBefore}},
		{audit.ActionSiteBackupRecipientCleared, map[string]string{"previous_recipient": c1004RecipientBefore, "reason": "re_enrollment"}},
		{audit.ActionSiteBackupRecipientSet, map[string]string{"recipient": c1004RecipientAfter}},
	})
}

// (b) The site-first Reconnect (Disconnect, Reconnect, enroll with the new
// code) clears the recipient and audits it; the next push, signed with the
// new key, sets the site's current one.
func TestC1004_SiteFirstReconnectRepinsRecipient(t *testing.T) {
	env, tenant := gh755PushSetup(t, "c1004-site-first")

	siteID, priv1, pub1 := env.connectSite(t, c1004SiteFirstURL)
	if status := c1004PushRecipient(t, env, priv1, pub1, c1004RecipientBefore); status != http.StatusOK {
		t.Fatalf("first push answered %d, want 200", status)
	}
	c1004WantRecipient(t, env, tenant, siteID, c1004RecipientBefore, "after the first push")

	code := c1004Reconnect(t, env, siteID)
	priv2, pub2 := env.enroll(t, code, c1004SiteFirstURL, siteID)
	c1004WantRecipient(t, env, tenant, siteID, "", "after the reconnect")

	if status := c1004PushRecipient(t, env, priv2, pub2, c1004RecipientAfter); status != http.StatusOK {
		t.Fatalf("push with the new key answered %d, want 200", status)
	}
	c1004WantRecipient(t, env, tenant, siteID, c1004RecipientAfter, "after the push signed with the new key")

	c1004AssertAudits(t, c1004RecipientAudits(t, env, tenant, siteID), []c1004Want{
		{audit.ActionSiteBackupRecipientSet, map[string]string{"recipient": c1004RecipientBefore}},
		{audit.ActionSiteBackupRecipientCleared, map[string]string{"previous_recipient": c1004RecipientBefore, "reason": "re_enrollment"}},
		{audit.ActionSiteBackupRecipientSet, map[string]string{"recipient": c1004RecipientAfter}},
	})
}

// (c) Negative control: with no re-enrollment, a push carrying a different
// recipient is still refused (#731) and audited as change_rejected.
func TestC1004_PushWithoutReEnrollmentIsStillRefused(t *testing.T) {
	env, tenant := gh755PushSetup(t, "c1004-no-reenrol")

	siteID, priv, pub := env.connectSite(t, c1004NoReenrolURL)
	if status := c1004PushRecipient(t, env, priv, pub, c1004RecipientBefore); status != http.StatusOK {
		t.Fatalf("first push answered %d, want 200", status)
	}
	if status := c1004PushRecipient(t, env, priv, pub, c1004RecipientOther); status != http.StatusOK {
		t.Fatalf("refused push answered %d, want 200 (a refused recipient never fails the push)", status)
	}
	c1004WantRecipient(t, env, tenant, siteID, c1004RecipientBefore, "after a push carrying another recipient")

	c1004AssertAudits(t, c1004RecipientAudits(t, env, tenant, siteID), []c1004Want{
		{audit.ActionSiteBackupRecipientSet, map[string]string{"recipient": c1004RecipientBefore}},
		{audit.ActionSiteBackupRecipientChangeRejected, map[string]string{
			"current_recipient": c1004RecipientBefore, "proposed_recipient": c1004RecipientOther, "reason": "already_set",
		}},
	})
}

// (d) After the attach, the rotated-away key cannot set the recipient: a push
// signed with it is refused at authentication, and metadata attributed to it
// (a request that authenticated just before the attach committed and reached
// the write after it) changes nothing and is audited as a refusal. The new key
// then sets the recipient as usual.
func TestC1004_OldKeyCannotSetRecipientAfterAttach(t *testing.T) {
	env, tenant := gh755PushSetup(t, "c1004-old-key")
	ctx := context.Background()

	siteID, priv1, pub1 := env.connectSite(t, c1004OldKeyURL)
	if status := c1004PushRecipient(t, env, priv1, pub1, c1004RecipientBefore); status != http.StatusOK {
		t.Fatalf("first push answered %d, want 200", status)
	}

	code := c1004Reconnect(t, env, siteID)
	priv2, pub2 := env.enroll(t, code, c1004OldKeyURL, siteID)
	c1004WantRecipient(t, env, tenant, siteID, "", "after the reconnect")

	if status := c1004PushRecipient(t, env, priv1, pub1, c1004RecipientStale); status != http.StatusUnauthorized {
		t.Fatalf("push signed with the rotated-away key answered %d, want 401", status)
	}
	c1004WantRecipient(t, env, tenant, siteID, "", "after a push signed with the rotated-away key")

	// The race: the old key resolved the site before the attach committed,
	// so the write is attributed to it. The production service path decides.
	if _, err := env.svc.ApplyAgentMetadata(ctx, tenant, siteID, pub1, agentpkg.Metadata{
		WPVersion:    "6.6",
		AgeRecipient: c1004RecipientStale,
	}); err != nil {
		t.Fatalf("ApplyAgentMetadata attributed to the rotated-away key: %v (a refused first set is not an error)", err)
	}
	c1004WantRecipient(t, env, tenant, siteID, "", "after metadata attributed to the rotated-away key")

	if status := c1004PushRecipient(t, env, priv2, pub2, c1004RecipientAfter); status != http.StatusOK {
		t.Fatalf("push with the new key answered %d, want 200", status)
	}
	c1004WantRecipient(t, env, tenant, siteID, c1004RecipientAfter, "after the push signed with the new key")

	c1004AssertAudits(t, c1004RecipientAudits(t, env, tenant, siteID), []c1004Want{
		{audit.ActionSiteBackupRecipientSet, map[string]string{"recipient": c1004RecipientBefore}},
		{audit.ActionSiteBackupRecipientCleared, map[string]string{"previous_recipient": c1004RecipientBefore, "reason": "re_enrollment"}},
		{audit.ActionSiteBackupRecipientChangeRejected, map[string]string{
			"current_recipient": "", "proposed_recipient": c1004RecipientStale, "reason": "agent_key_not_current",
		}},
		{audit.ActionSiteBackupRecipientSet, map[string]string{"recipient": c1004RecipientAfter}},
	})
}

const (
	c1004RecheckURL = "https://c1004-recheck.example.test"

	c1004RecipientFromRecheck = "age1c1004recipientinanunsignedrecheckanswer"
)

// c1004Rechecker answers the dashboard's Re-check pull with a fixed body, the
// way whatever answers at the site's address would. Nothing signs that body.
type c1004Rechecker struct {
	mu   sync.Mutex
	body []byte
}

func (r *c1004Rechecker) MetadataRaw(context.Context, uuid.UUID, string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body, nil
}

func (r *c1004Rechecker) answer(t *testing.T, rec string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"wp_version": "6.6", "age_recipient": rec})
	if err != nil {
		t.Fatalf("marshal recheck answer: %v", err)
	}
	r.mu.Lock()
	r.body = body
	r.mu.Unlock()
}

// c1004RecheckSetup is gh755PushSetup's control plane with the Re-check
// connection route wired to rc: the same site service, repo, recorder and
// agent Authenticator, as wpmgr_app.
func c1004RecheckSetup(t *testing.T, slug string, rc site.AgentRechecker) (*gh755PushEnv, uuid.UUID) {
	t.Helper()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, slug)
	gh755AssertAppRole(t, pool, tenant)

	gin.SetMode(gin.TestMode)
	repo := site.NewRepo(pool)
	rec := audit.NewRecorder(pool, domain.SystemClock{})
	conn := site.NewConnectionService(repo, domain.NewValidator(), rec, nil, domain.SystemClock{}, nil)
	svc := site.NewService(repo, domain.NewValidator(), domain.SystemClock{})
	svc.SetConnectionService(conn)
	svc.SetAuditRecorder(rec)
	h := site.NewHandler(svc, rec, "")
	h.SetConnectionService(conn)
	h.SetRechecker(rc)
	agentH := agentpkg.NewHandler(svc)
	authn := agentpkg.NewAuthenticator(svc, domain.SystemClock{}, 5*time.Minute)

	env := &gh755PushEnv{pool: pool, svc: svc, rec: rec, adoptQ: &gh755AdoptQueue{}}
	eng := gin.New()
	h.RegisterPublic(eng)
	v1 := eng.Group("/api/v1")
	v1.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), env.as))
		c.Next()
	})
	h.Register(v1)
	ag := eng.Group("/agent/v1")
	ag.Use(authn.Authenticate())
	agentH.Register(ag)
	env.eng = eng
	env.as = gh755Owner(tenant, gh755SeedUser(t, pool, slug+"@example.test"))
	return env, tenant
}

func c1004Recheck(t *testing.T, env *gh755PushEnv, siteID uuid.UUID) {
	t.Helper()
	status, body := env.do(t, http.MethodPost, "/api/v1/sites/"+siteID.String()+"/recheck", nil)
	if status != http.StatusOK {
		t.Fatalf("POST /sites/%s/recheck answered %d, want 200: %v", siteID, status, body)
	}
}

// The first backup key a site gets is recorded only from the agent's own
// signed metadata push carrying the site's current agent key. After a
// Reconnect cleared the recorded key, a Re-check answer (which nothing signs)
// leaves it empty and is recorded as refused; the agent's next signed push
// then records its key. A Re-check over an established key still leaves it
// alone and is refused as before.
func TestC1004_OnlyASignedAgentReportRecordsTheFirstBackupKey(t *testing.T) {
	rc := &c1004Rechecker{}
	env, tenant := c1004RecheckSetup(t, "c1004-recheck", rc)

	siteID, priv1, pub1 := env.connectSite(t, c1004RecheckURL)
	if status := c1004PushRecipient(t, env, priv1, pub1, c1004RecipientBefore); status != http.StatusOK {
		t.Fatalf("first push answered %d, want 200", status)
	}
	code := c1004Reconnect(t, env, siteID)
	priv2, pub2 := env.enroll(t, code, c1004RecheckURL, siteID)
	c1004WantRecipient(t, env, tenant, siteID, "", "after the reconnect")

	// A Re-check answer carrying a key does not record it.
	rc.answer(t, c1004RecipientFromRecheck)
	c1004Recheck(t, env, siteID)
	c1004WantRecipient(t, env, tenant, siteID, "", "after a Re-check answer carrying a backup key")

	// The agent's next signed push records its key.
	if status := c1004PushRecipient(t, env, priv2, pub2, c1004RecipientAfter); status != http.StatusOK {
		t.Fatalf("push with the new key answered %d, want 200", status)
	}
	c1004WantRecipient(t, env, tenant, siteID, c1004RecipientAfter, "after the agent's signed push")

	// Over an established key, a Re-check answer carrying another one is
	// refused exactly as a push would be.
	c1004Recheck(t, env, siteID)
	c1004WantRecipient(t, env, tenant, siteID, c1004RecipientAfter, "after a Re-check over the established key")

	c1004AssertAudits(t, c1004RecipientAudits(t, env, tenant, siteID), []c1004Want{
		{audit.ActionSiteBackupRecipientSet, map[string]string{"recipient": c1004RecipientBefore}},
		{audit.ActionSiteBackupRecipientCleared, map[string]string{"previous_recipient": c1004RecipientBefore, "reason": "re_enrollment"}},
		{audit.ActionSiteBackupRecipientChangeRejected, map[string]string{
			"current_recipient": "", "proposed_recipient": c1004RecipientFromRecheck, "reason": "unsigned_report",
		}},
		{audit.ActionSiteBackupRecipientSet, map[string]string{"recipient": c1004RecipientAfter}},
		{audit.ActionSiteBackupRecipientChangeRejected, map[string]string{
			"current_recipient": c1004RecipientAfter, "proposed_recipient": c1004RecipientFromRecheck, "reason": "already_set",
		}},
	})
}
