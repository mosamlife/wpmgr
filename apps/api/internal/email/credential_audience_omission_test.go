// credential_audience_omission_test.go: what an OMITTED provider-config field
// means to the credential audience check (GH #380).
//
// THE DECISION, STATED ONCE.
//
// An omitted field means DIFFERENT, not unchanged. Absent still compares equal
// to empty, because omitting a field and sending it blank are the same
// statement and the check must not depend on how a client serialises. But
// absent compares UNEQUAL to any value that is set.
//
// This is a real trade and it was made deliberately. The forgiving reading
// ("the client did not mention encryption, so leave it as it was") is the one
// that reopens the issue: credentialAudienceFields is exactly the set of fields
// deciding WHERE a credential is presented and WHO it authenticates as, so a
// client that drops "encryption" from the payload would pass the audience check
// on the strength of the fields it did send and be handed the organisation's
// password to present over an unencrypted connection. Dropping "host" would do
// the same for the destination.
//
// The cost is accepted and is the reason this file exists: a non-UI client
// doing a partial update loses the inherited credential and must re-enter it.
// These tests pin that outcome so it stays a decision rather than becoming a
// surprise, and they pin the log field that tells the operator which key caused
// it, because without that the incident is undiagnosable.
package email

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// TestOmittedAudienceFieldCountsAsDifferent is the decision itself. The org has
// encryption "tls"; the incoming config simply does not mention encryption.
func TestOmittedAudienceFieldCountsAsDifferent(t *testing.T) {
	org := Config{Provider: "smtp", Config: smtpAt("smtp.relay.example", "fleet")}

	partial := smtpAt("smtp.relay.example", "fleet")
	delete(partial, "encryption")
	site := Config{Provider: "smtp", Config: partial}

	field, diverged := credentialAudienceDivergence(site, org)
	if !diverged {
		t.Fatal("a config that omits \"encryption\" was treated as the same audience as one " +
			"that sets it to \"tls\". That config sends with no encryption, so honouring the " +
			"omission while lending the credential puts the password on the wire in clear")
	}
	if field != "encryption" {
		t.Fatalf("divergence reported on %q, want the omitted field \"encryption\"", field)
	}
}

// TestOmittedFieldMatchesAnOmittedField keeps the decision from becoming
// "anything partial is different". Two configs that both leave a field out
// agree about it, and must stay the same audience.
func TestOmittedFieldMatchesAnOmittedField(t *testing.T) {
	a := smtpAt("smtp.relay.example", "fleet")
	b := smtpAt("smtp.relay.example", "fleet")
	delete(a, "encryption")
	delete(b, "encryption")

	if _, diverged := credentialAudienceDivergence(
		Config{Provider: "smtp", Config: a},
		Config{Provider: "smtp", Config: b},
	); diverged {
		t.Fatal("two configs that both omit the same field are the same audience")
	}
}

// TestOmittedFieldMatchesAnEmptyOne is the other half of the rule: absent and
// empty are one statement, so a client that sends "encryption": "" must not get
// a different answer from one that leaves the key out.
func TestOmittedFieldMatchesAnEmptyOne(t *testing.T) {
	absent := smtpAt("smtp.relay.example", "fleet")
	delete(absent, "encryption")
	empty := smtpAt("smtp.relay.example", "fleet")
	empty["encryption"] = ""

	if _, diverged := credentialAudienceDivergence(
		Config{Provider: "smtp", Config: absent},
		Config{Provider: "smtp", Config: empty},
	); diverged {
		t.Fatal("an absent field and an empty one are the same statement and must compare equal; " +
			"otherwise the verdict depends on how the client serialised the request")
	}
}

// TestDivergenceNamesTheField is what makes the accepted cost survivable in
// production. When the credential is withheld the site stops sending mail, and
// the log line used to print only the two provider slugs, which for every case
// except an actual provider switch are identical. "smtp vs smtp" is not a
// diagnosis. Each audience field must be able to name itself.
func TestDivergenceNamesTheField(t *testing.T) {
	base := Config{Provider: "smtp", Config: smtpAt("smtp.relay.example", "fleet")}

	cases := map[string]map[string]any{
		"host":       smtpAt("smtp.elsewhere.example", "fleet"),
		"username":   smtpAt("smtp.relay.example", "someone-else"),
		"encryption": func() map[string]any { m := smtpAt("smtp.relay.example", "fleet"); m["encryption"] = "ssl"; return m }(),
		"port":       func() map[string]any { m := smtpAt("smtp.relay.example", "fleet"); m["port"] = float64(25); return m }(),
		"auth":       func() map[string]any { m := smtpAt("smtp.relay.example", "fleet"); m["auth"] = false; return m }(),
	}
	for want, cfg := range cases {
		got, diverged := credentialAudienceDivergence(Config{Provider: "smtp", Config: cfg}, base)
		if !diverged {
			t.Errorf("changing %q did not diverge", want)
			continue
		}
		if got != want {
			t.Errorf("changing %q was reported as a divergence on %q", want, got)
		}
	}

	// A provider switch names itself too, rather than falling through to some
	// arbitrary first field.
	if got, diverged := credentialAudienceDivergence(
		Config{Provider: "ses", Config: map[string]any{}}, base); !diverged || got != "provider" {
		t.Errorf("a provider switch reported (%q, %v), want (\"provider\", true)", got, diverged)
	}
}

// TestSameCredentialAudienceStillAgreesWithDivergence guards the refactor: the
// boolean helper is now a thin wrapper, and the two must not drift apart, since
// every security decision on this path still calls the boolean.
func TestSameCredentialAudienceStillAgreesWithDivergence(t *testing.T) {
	base := Config{Provider: "smtp", Config: smtpAt("smtp.relay.example", "fleet")}
	moved := Config{Provider: "smtp", Config: smtpAt("smtp.attacker.example", "fleet")}

	if !sameCredentialAudience(base, base) {
		t.Fatal("a config is not the same audience as itself")
	}
	if sameCredentialAudience(moved, base) {
		t.Fatal("a moved endpoint compared as the same audience")
	}
	if _, diverged := credentialAudienceDivergence(base, base); diverged {
		t.Fatal("divergence disagrees with sameCredentialAudience on identical configs")
	}
}

// TestPartialSaveRevokesRatherThanRebinds is the decision seen from the outside,
// on the path an operator actually walks: a save that omits an audience field
// must revoke the stored credential rather than carry it onto the endpoint the
// partial config now describes.
//
// This is the same shape as TestSiteCredentialIsReboundOnUsernameChange, with
// the difference being an ABSENT field instead of a changed one. That it lands
// in the same place is the decision.
func TestPartialSaveRevokesRatherThanRebinds(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	seedSiteWithSecret(repo, tenantID, siteID, "smtp.legitimate.example", "postmaster")

	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	partial := smtpAt("smtp.legitimate.example", "postmaster")
	delete(partial, "encryption")

	if _, err := svc.UpsertSiteConfig(context.Background(), upsertInput(tenantID, &siteID, partial)); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !repo.storedSetSecret || len(repo.storedCt) != 0 {
		t.Fatal("a save that dropped \"encryption\" kept the stored credential; it would then " +
			"be presented over an unencrypted connection the credential's owner never chose")
	}
}
