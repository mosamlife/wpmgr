package tests

// GH #1004, part 1 (SQL). An enrollment the operator authorised clears the
// site's pinned backup recipient in the same UPDATE that rotates the agent
// key, and the read that precedes that UPDATE holds the row lock and returns
// the previous recipient, so the caller can record it after commit.
//
// Both attach paths run in InEnrollTx (app.enroll), against a database reached
// as wpmgr_app (NOSUPERUSER, NOBYPASSRLS), which gh755Setup checks rather than
// assumes:
//
//	legacy pairing code    Service.Enroll -> repo.Enroll:
//	                       GetSiteByURLForEnroll (FOR UPDATE), AttachAgentToSite
//	site-first, Reconnect  POST /enroll -> ConnectionService:
//	                       GetSiteForTransition (FOR UPDATE), AttachAgentAndConnect
//
// On sites the permissive sites_enroll policy is FOR ALL, so it admits both
// the row lock (which also checks the UPDATE policy) and the UPDATE, and under
// app.enroll every tenant's rows are visible. The tenant_id predicate in each
// query is what confines it to one tenant, so another tenant's site at the
// same address is checked to stay unlocked and unchanged.
//
// The agent path's first set of the recipient is the compare-and-set
// SetSiteAgeRecipientIfUnsetForAgentKey, run in InTenantTx as the metadata push
// runs it (repo.SetAgeRecipientIfUnset). There sites_tenant_isolation is the
// permissive policy that applies and sites_site_scope (RESTRICTIVE) passes
// because InTenantTx sets no app.site_scope.
//
// Selected by: go -C apps/api test -run '^TestDB1004_' ./tests/

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

const (
	db1004LegacyURL    = "https://db1004-legacy.example.test"
	db1004SiteFirstURL = "https://db1004-site-first.example.test"

	db1004LegacyOld    = "age1db1004legacyrecipientbeforereenrollment"
	db1004SiteFirstOld = "age1db1004sitefirstrecipientbeforereconnect"
	db1004OtherTenant  = "age1db1004othertenantrecipientmuststay"

	db1004FirstKeyRecipient   = "age1db1004recipientsetbythefirstagentkey"
	db1004ReplaceAttempt      = "age1db1004recipientthatmustneverreplaceaset"
	db1004OldKeyAttempt       = "age1db1004recipientpushedwiththerotatedkey"
	db1004CurrentKeyRecipient = "age1db1004recipientsetbythecurrentagentkey"
	db1004CrossTenantAttempt  = "age1db1004recipientfromanothertenantstx"
	db1004OtherOwnRecipient   = "age1db1004othertenantsetsitsownrecipient"
)

var errDB1004Rollback = errors.New("db1004: roll back the probe transaction")

// db1004LockedByOther reports whether another open transaction holds a row
// lock on the site. A FOR UPDATE NOWAIT from a second pooled connection fails
// with lock_not_available (55P03) exactly when one does. A row the probe
// cannot see fails the test instead of reading as "not locked".
func db1004LockedByOther(t *testing.T, pool *db.Pool, siteID uuid.UUID) bool {
	t.Helper()
	ctx := context.Background()
	err := pool.InEnrollTx(ctx, func(tx pgx.Tx) error {
		var one int
		return tx.QueryRow(ctx, `SELECT 1 FROM sites WHERE id = $1 FOR UPDATE NOWAIT`, siteID).Scan(&one)
	})
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
		return true
	}
	t.Fatalf("lock probe on site %s: %v", siteID, err)
	return false
}

// db1004LegacyEnroll mints a tenant-wide (legacy) pairing code and enrolls the
// address with it through the production Service.Enroll.
func db1004LegacyEnroll(t *testing.T, svc *site.Service, tenant uuid.UUID, url, agentKey string) site.Site {
	t.Helper()
	ctx := context.Background()
	code, err := svc.CreatePairingCode(ctx, site.CreatePairingCodeInput{TenantID: tenant, SiteName: "db1004"})
	if err != nil {
		t.Fatalf("create legacy pairing code: %v", err)
	}
	s, err := svc.Enroll(ctx, site.EnrollRequest{
		PairingCode:    code.Plaintext,
		SiteURL:        url,
		AgentPublicKey: agentKey,
		WPVersion:      "6.6",
		PHPVersion:     "8.3",
	})
	if err != nil {
		t.Fatalf("legacy enroll %s: %v", url, err)
	}
	return s
}

func db1004SetRecipient(t *testing.T, svc *site.Service, tenant, siteID uuid.UUID, recipient string) {
	t.Helper()
	got, err := svc.SetAgeRecipient(context.Background(), tenant, siteID, recipient)
	if err != nil {
		t.Fatalf("set recipient on %s: %v", siteID, err)
	}
	if got.AgeRecipient != recipient {
		t.Fatalf("seeded recipient = %q, want %q", got.AgeRecipient, recipient)
	}
}

func db1004AssertUnchanged(t *testing.T, before, after site.Site) {
	t.Helper()
	if after.AgeRecipient != before.AgeRecipient {
		t.Errorf("other tenant's site %s: age_recipient %q -> %q, want unchanged", before.ID, before.AgeRecipient, after.AgeRecipient)
	}
	if after.AgentPublicKey != before.AgentPublicKey {
		t.Errorf("other tenant's site %s: agent_public_key changed", before.ID)
	}
	if (before.EnrolledAt == nil) != (after.EnrolledAt == nil) ||
		(before.EnrolledAt != nil && !before.EnrolledAt.Equal(*after.EnrolledAt)) {
		t.Errorf("other tenant's site %s: enrolled_at %v -> %v, want unchanged", before.ID, before.EnrolledAt, after.EnrolledAt)
	}
}

func TestDB1004_ReEnrollmentClearsPinnedRecipient(t *testing.T) {
	env, tenant := gh755Setup(t, "db1004")
	ctx := context.Background()
	pool := env.pool
	other := seedTenant(t, pool, "db1004-other")

	// The other tenant holds a site at each of the two addresses, enrolled and
	// pinned. Under app.enroll its rows are visible to every query below.
	_, _, otherKey1 := genKey(t)
	otherLegacy := db1004LegacyEnroll(t, env.svc, other, db1004LegacyURL, otherKey1)
	db1004SetRecipient(t, env.svc, other, otherLegacy.ID, db1004OtherTenant)
	_, _, otherKey2 := genKey(t)
	otherSiteFirst := db1004LegacyEnroll(t, env.svc, other, db1004SiteFirstURL, otherKey2)
	db1004SetRecipient(t, env.svc, other, otherSiteFirst.ID, db1004OtherTenant)
	otherBefore := []site.Site{env.site(t, other, otherLegacy.ID), env.site(t, other, otherSiteFirst.ID)}

	t.Run("legacy pairing code: AttachAgentToSite", func(t *testing.T) {
		_, _, key1 := genKey(t)
		first := db1004LegacyEnroll(t, env.svc, tenant, db1004LegacyURL, key1)
		if first.TenantID != tenant || first.ID == otherLegacy.ID {
			t.Fatalf("first enrollment landed on %s in tenant %s", first.ID, first.TenantID)
		}
		db1004SetRecipient(t, env.svc, tenant, first.ID, db1004LegacyOld)

		// The read that precedes AttachAgentToSite returns the previous
		// recipient and holds the row, and only this tenant's row.
		err := pool.InEnrollTx(ctx, func(tx pgx.Tx) error {
			got, err := sqlc.New(tx).GetSiteByURLForEnroll(ctx, sqlc.GetSiteByURLForEnrollParams{TenantID: tenant, Url: db1004LegacyURL})
			if err != nil {
				return err
			}
			if got.ID != first.ID {
				t.Errorf("GetSiteByURLForEnroll returned site %s, want %s", got.ID, first.ID)
			}
			if got.AgeRecipient != db1004LegacyOld {
				t.Errorf("GetSiteByURLForEnroll age_recipient = %q, want the previous %q", got.AgeRecipient, db1004LegacyOld)
			}
			if !db1004LockedByOther(t, pool, first.ID) {
				t.Errorf("GetSiteByURLForEnroll returned without holding a row lock on site %s", first.ID)
			}
			if db1004LockedByOther(t, pool, otherLegacy.ID) {
				t.Errorf("GetSiteByURLForEnroll locked the other tenant's site %s at the same address", otherLegacy.ID)
			}
			return errDB1004Rollback
		})
		if !errors.Is(err, errDB1004Rollback) {
			t.Fatalf("locking read: %v", err)
		}

		_, _, key2 := genKey(t)
		returned := db1004LegacyEnroll(t, env.svc, tenant, db1004LegacyURL, key2)
		if returned.ID != first.ID {
			t.Fatalf("re-enrollment attached to %s, want the existing site %s", returned.ID, first.ID)
		}
		if returned.AgeRecipient != "" {
			t.Errorf("AttachAgentToSite returned age_recipient %q, want it cleared", returned.AgeRecipient)
		}
		after := env.site(t, tenant, first.ID)
		if after.AgeRecipient != "" {
			t.Errorf("after legacy re-enrollment age_recipient = %q, want it cleared", after.AgeRecipient)
		}
		if after.AgentPublicKey != key2 || after.AgentPublicKey == key1 {
			t.Errorf("after legacy re-enrollment the agent key did not rotate to the new one")
		}
	})

	t.Run("site-first Reconnect: AttachAgentAndConnect", func(t *testing.T) {
		siteID, code := env.addSite(t, db1004SiteFirstURL)
		env.mustEnroll(t, code, db1004SiteFirstURL, siteID)
		first := env.site(t, tenant, siteID)
		if first.AgentPublicKey == "" {
			t.Fatalf("first site-first enrollment stored no agent key")
		}
		db1004SetRecipient(t, env.svc, tenant, siteID, db1004SiteFirstOld)

		code2 := env.reconnect(t, siteID)

		// GetSiteForTransition is the read the consume path takes before
		// AttachAgentAndConnect; under app.enroll it returns the previous
		// recipient and holds the row, and only this tenant's row.
		err := pool.InEnrollTx(ctx, func(tx pgx.Tx) error {
			got, err := sqlc.New(tx).GetSiteForTransition(ctx, sqlc.GetSiteForTransitionParams{ID: siteID, TenantID: tenant})
			if err != nil {
				return err
			}
			if got.AgeRecipient != db1004SiteFirstOld {
				t.Errorf("GetSiteForTransition age_recipient = %q, want the previous %q", got.AgeRecipient, db1004SiteFirstOld)
			}
			if !db1004LockedByOther(t, pool, siteID) {
				t.Errorf("GetSiteForTransition returned without holding a row lock on site %s", siteID)
			}
			if db1004LockedByOther(t, pool, otherSiteFirst.ID) {
				t.Errorf("GetSiteForTransition locked the other tenant's site %s at the same address", otherSiteFirst.ID)
			}
			return errDB1004Rollback
		})
		if !errors.Is(err, errDB1004Rollback) {
			t.Fatalf("locking read: %v", err)
		}

		env.mustEnroll(t, code2, db1004SiteFirstURL, siteID)
		after := env.site(t, tenant, siteID)
		if after.AgeRecipient != "" {
			t.Errorf("after Reconnect age_recipient = %q, want it cleared", after.AgeRecipient)
		}
		if after.AgentPublicKey == "" || after.AgentPublicKey == first.AgentPublicKey {
			t.Errorf("after Reconnect the agent key did not rotate")
		}
	})

	// Neither re-enrollment touched the other tenant's sites at the same
	// addresses.
	db1004AssertUnchanged(t, otherBefore[0], env.site(t, other, otherLegacy.ID))
	db1004AssertUnchanged(t, otherBefore[1], env.site(t, other, otherSiteFirst.ID))
}

// db1004FirstSet runs the agent path's first-set query the way the metadata
// push does: in InTenantTx for txTenant, the tenant the pushing agent's key
// resolved to. It returns how many rows the query changed.
func db1004FirstSet(t *testing.T, pool *db.Pool, txTenant, siteTenant, siteID uuid.UUID, agentKey, recipient string) int64 {
	t.Helper()
	ctx := context.Background()
	var n int64
	err := pool.InTenantTx(ctx, txTenant, func(tx pgx.Tx) error {
		var err error
		n, err = sqlc.New(tx).SetSiteAgeRecipientIfUnsetForAgentKey(ctx, sqlc.SetSiteAgeRecipientIfUnsetForAgentKeyParams{
			ID:             siteID,
			TenantID:       siteTenant,
			AgeRecipient:   recipient,
			AgentPublicKey: agentKey,
		})
		return err
	})
	if err != nil {
		t.Fatalf("first-set query on site %s: %v", siteID, err)
	}
	return n
}

func db1004WantRecipient(t *testing.T, env *gh755Env, tenant, siteID uuid.UUID, want, when string) {
	t.Helper()
	if got := env.site(t, tenant, siteID).AgeRecipient; got != want {
		t.Errorf("%s: age_recipient = %q, want %q", when, got, want)
	}
}

func TestDB1004_FirstSetNeedsTheCurrentAgentKey(t *testing.T) {
	env, tenant := gh755Setup(t, "db1004-cas")
	pool := env.pool
	other := seedTenant(t, pool, "db1004-cas-other")

	// The other tenant's site sits at the same address with an empty
	// recipient, so nothing but tenant scoping keeps the writes below off it.
	_, _, otherKey := genKey(t)
	otherSite := db1004LegacyEnroll(t, env.svc, other, db1004LegacyURL, otherKey)
	if otherSite.AgeRecipient != "" {
		t.Fatalf("other tenant's site starts with recipient %q, want empty", otherSite.AgeRecipient)
	}

	_, _, key1 := genKey(t)
	s := db1004LegacyEnroll(t, env.svc, tenant, db1004LegacyURL, key1)

	// The key the site holds sets an empty recipient.
	if n := db1004FirstSet(t, pool, tenant, tenant, s.ID, key1, db1004FirstKeyRecipient); n != 1 {
		t.Errorf("current key on an empty recipient changed %d rows, want 1", n)
	}
	db1004WantRecipient(t, env, tenant, s.ID, db1004FirstKeyRecipient, "after the first set")

	// A recipient already set is never replaced, whatever the key.
	_, _, stranger := genKey(t)
	for _, k := range []struct{ name, key string }{{"the current key", key1}, {"a key no site holds", stranger}} {
		if n := db1004FirstSet(t, pool, tenant, tenant, s.ID, k.key, db1004ReplaceAttempt); n != 0 {
			t.Errorf("%s over a set recipient changed %d rows, want 0", k.name, n)
		}
	}
	db1004WantRecipient(t, env, tenant, s.ID, db1004FirstKeyRecipient, "after the replace attempts")

	// A re-enrollment rotates key1 to key2 and clears the recipient. A push
	// still signed with key1 then changes nothing; key2 sets it.
	_, _, key2 := genKey(t)
	db1004LegacyEnroll(t, env.svc, tenant, db1004LegacyURL, key2)
	db1004WantRecipient(t, env, tenant, s.ID, "", "after the re-enrollment")
	if n := db1004FirstSet(t, pool, tenant, tenant, s.ID, key1, db1004OldKeyAttempt); n != 0 {
		t.Errorf("rotated-away key on an empty recipient changed %d rows, want 0", n)
	}
	db1004WantRecipient(t, env, tenant, s.ID, "", "after the rotated-away key's push")
	if n := db1004FirstSet(t, pool, tenant, tenant, s.ID, key2, db1004CurrentKeyRecipient); n != 1 {
		t.Errorf("current key after the re-enrollment changed %d rows, want 1", n)
	}
	if n := db1004FirstSet(t, pool, tenant, tenant, s.ID, key1, db1004OldKeyAttempt); n != 0 {
		t.Errorf("rotated-away key over the new recipient changed %d rows, want 0", n)
	}
	db1004WantRecipient(t, env, tenant, s.ID, db1004CurrentKeyRecipient, "after the current key's first set")

	// Another tenant's row: from this tenant's transaction, naming the other
	// site under either tenant id, with the other site's own key, nothing
	// changes. Neither did any write above.
	if n := db1004FirstSet(t, pool, tenant, other, otherSite.ID, otherKey, db1004CrossTenantAttempt); n != 0 {
		t.Errorf("other tenant's site named with its own tenant id changed %d rows, want 0", n)
	}
	if n := db1004FirstSet(t, pool, tenant, tenant, otherSite.ID, otherKey, db1004CrossTenantAttempt); n != 0 {
		t.Errorf("other tenant's site named with this tenant id changed %d rows, want 0", n)
	}
	otherAfter := env.site(t, other, otherSite.ID)
	if otherAfter.AgeRecipient != "" || otherAfter.AgentPublicKey != otherKey {
		t.Errorf("other tenant's site changed: recipient %q, key kept %v", otherAfter.AgeRecipient, otherAfter.AgentPublicKey == otherKey)
	}

	// Positive control: the same call in the other tenant's own transaction
	// does set it, so the zeros above came from tenant scoping.
	if n := db1004FirstSet(t, pool, other, other, otherSite.ID, otherKey, db1004OtherOwnRecipient); n != 1 {
		t.Errorf("other tenant's own first set changed %d rows, want 1", n)
	}
	db1004WantRecipient(t, env, other, otherSite.ID, db1004OtherOwnRecipient, "other tenant's own first set")
}
