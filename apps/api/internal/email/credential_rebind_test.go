package email

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// THE TWO DOORS THIS FILE WATCHES (GH #380, sixth and seventh).
//
// Both are the same bug on two rows. A save that carries no secret preserves
// the ciphertext already stored, which is correct when the save only touches
// the from-name, and an escalation when the save moves the endpoint the
// credential authenticates to:
//
//	SIXTH   the site's own row. An actor entitled to edit it repoints host or
//	        username, sends no password, and the credential issued for the old
//	        account is offered to the new one.
//	SEVENTH the org row. PermEmailManage is RoleOperator, so an operator below
//	        the admin or owner who entered the credential can repoint the
//	        organisation's config and take the credential with it.
//
// The assertions below are all on what reaches the REPOSITORY, not on what a
// handler returns. Three review rounds have already added handler guards; the
// point of closing this at save time is that the credential must not survive
// the write, whatever route reached it.

// smtpAt builds an SMTP provider config map for one endpoint and identity.
func smtpAt(host, username string) map[string]any {
	return map[string]any{
		"host":       host,
		"port":       float64(587),
		"username":   username,
		"encryption": "tls",
		"auth":       true,
	}
}

// seedSiteWithSecret plants a site row that already holds a credential issued
// for host/username, the way an earlier legitimate save would have left it.
func seedSiteWithSecret(r *fakeRepo, tenantID, siteID uuid.UUID, host, username string) {
	r.site[siteKey(tenantID, siteID)] = Config{
		ID:        uuid.New(),
		TenantID:  tenantID,
		SiteID:    &siteID,
		Provider:  "smtp",
		Config:    smtpAt(host, username),
		SecretSet: true,
	}
}

// upsertInput is a minimally valid UpsertInput carrying no secret, which is
// the preserve path both doors ride.
func upsertInput(tenantID uuid.UUID, siteID *uuid.UUID, cfg map[string]any) UpsertInput {
	return UpsertInput{
		TenantID:      tenantID,
		SiteID:        siteID,
		Provider:      "smtp",
		Config:        cfg,
		RetentionDays: 14,
		SecretRaw:     nil, // the nil-sentinel: preserve whatever is stored
	}
}

// TestSiteOwnCredentialIsNotRebound is the SIXTH door: moving the site's own
// config to another mail server, with no new password, must not carry the
// stored credential across.
func TestSiteOwnCredentialIsNotRebound(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	seedSiteWithSecret(repo, tenantID, siteID, "smtp.legitimate.example", "postmaster")

	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	in := upsertInput(tenantID, &siteID, smtpAt("smtp.attacker.example", "postmaster"))
	if _, err := svc.UpsertSiteConfig(context.Background(), in); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if !repo.storedSetSecret {
		t.Fatal("the stored credential was PRESERVED onto a different mail server: " +
			"SetSecret must be true so the column is written")
	}
	if len(repo.storedCt) != 0 {
		t.Fatalf("the credential must be revoked (NULL ciphertext), got %d bytes", len(repo.storedCt))
	}
}

// TestSiteOwnCredentialSurvivesAnUnrelatedEdit is the other half of the sixth
// door, and the one that matters for not breaking a shipped feature: editing
// anything that does NOT move the endpoint must keep the credential, or every
// operator who changes a from-name is silently logged out of their mail server.
func TestSiteOwnCredentialSurvivesAnUnrelatedEdit(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	seedSiteWithSecret(repo, tenantID, siteID, "smtp.legitimate.example", "postmaster")

	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	// Same host, same username, same everything that decides where the
	// credential goes. Only the display name changes.
	in := upsertInput(tenantID, &siteID, smtpAt("smtp.legitimate.example", "postmaster"))
	in.FromName = "Support Desk"
	if _, err := svc.UpsertSiteConfig(context.Background(), in); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if repo.storedSetSecret {
		t.Fatal("an edit that did not move the endpoint revoked the credential; " +
			"the preserve path must still preserve")
	}
}

// TestSiteCredentialIsReboundOnUsernameChange pins that the identity, not just
// the destination, is part of the audience: the same server with a different
// account is a different account.
func TestSiteCredentialIsReboundOnUsernameChange(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	seedSiteWithSecret(repo, tenantID, siteID, "smtp.legitimate.example", "postmaster")

	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	in := upsertInput(tenantID, &siteID, smtpAt("smtp.legitimate.example", "someone-else"))
	if _, err := svc.UpsertSiteConfig(context.Background(), in); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !repo.storedSetSecret || len(repo.storedCt) != 0 {
		t.Fatal("changing the username kept the previous account's credential")
	}
}

// TestOrgCredentialIsNotRebound is the SEVENTH door. The org row is the one an
// operator can reach without being the admin or owner who entered the
// credential, and it is the credential every inheriting site in the fleet
// sends with.
func TestOrgCredentialIsNotRebound(t *testing.T) {
	tenantID := uuid.New()
	repo := newFakeRepo()
	repo.org[tenantID] = Config{
		ID:        uuid.New(),
		TenantID:  tenantID,
		Provider:  "smtp",
		Config:    smtpAt("smtp.org-relay.example", "fleet"),
		SecretSet: true,
	}

	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	in := upsertInput(tenantID, nil, smtpAt("smtp.attacker.example", "fleet"))
	if _, err := svc.UpsertOrgConfig(context.Background(), in); err != nil {
		t.Fatalf("upsert org: %v", err)
	}

	if !repo.storedSetSecret {
		t.Fatal("the ORGANISATION credential was preserved onto an operator-chosen " +
			"mail server: SetSecret must be true so the column is written")
	}
	if len(repo.storedCt) != 0 {
		t.Fatalf("the org credential must be revoked (NULL ciphertext), got %d bytes", len(repo.storedCt))
	}
}

// TestOrgCredentialSurvivesAnUnrelatedEdit is the seventh door's compatibility
// half.
func TestOrgCredentialSurvivesAnUnrelatedEdit(t *testing.T) {
	tenantID := uuid.New()
	repo := newFakeRepo()
	repo.org[tenantID] = Config{
		ID:        uuid.New(),
		TenantID:  tenantID,
		Provider:  "smtp",
		Config:    smtpAt("smtp.org-relay.example", "fleet"),
		SecretSet: true,
	}

	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	in := upsertInput(tenantID, nil, smtpAt("smtp.org-relay.example", "fleet"))
	in.RetentionDays = 30
	if _, err := svc.UpsertOrgConfig(context.Background(), in); err != nil {
		t.Fatalf("upsert org: %v", err)
	}
	if repo.storedSetSecret {
		t.Fatal("changing only the retention window revoked the org credential")
	}
}

// TestProviderChangeRebindsTheCredential covers the coarsest move: a credential
// issued for one provider is meaningless to another and must never travel.
func TestProviderChangeRebindsTheCredential(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	seedSiteWithSecret(repo, tenantID, siteID, "smtp.legitimate.example", "postmaster")

	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	in := upsertInput(tenantID, &siteID, map[string]any{"domain_name": "mg.example", "region": "eu"})
	in.Provider = "mailgun"
	if _, err := svc.UpsertSiteConfig(context.Background(), in); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !repo.storedSetSecret || len(repo.storedCt) != 0 {
		t.Fatal("switching provider carried the previous provider's credential over")
	}
}

// TestNewRowNeedsNoRebindCheck: a site with no row of its own has nothing
// stored, so the save must proceed and must NOT write a revoking NULL (which
// would be a pointless column write, and on the org-fallback path would look
// like an explicit clear).
func TestNewRowNeedsNoRebindCheck(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	repo := newFakeRepo()

	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	in := upsertInput(tenantID, &siteID, smtpAt("smtp.new.example", "postmaster"))
	if _, err := svc.UpsertSiteConfig(context.Background(), in); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if repo.storedSetSecret {
		t.Fatal("a brand new row must not write the secret column at all")
	}
}

// TestUnreadableStoredRowRefusesTheWrite pins the "never guess" rule shared
// with UpsertConnection: without the previous settings there is no way to tell
// a typo correction from a move to another account, so the save is refused and
// reported rather than resolved either way.
func TestUnreadableStoredRowRefusesTheWrite(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	repo.siteGetErr = errors.New("connection reset")

	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	in := upsertInput(tenantID, &siteID, smtpAt("smtp.somewhere.example", "postmaster"))
	_, err := svc.UpsertSiteConfig(context.Background(), in)
	if err == nil {
		t.Fatal("a stored row that will not load must refuse the write, not guess")
	}
	var de *domain.Error
	if !errors.As(err, &de) {
		t.Fatalf("want a domain error, got %T: %v", err, err)
	}
}
