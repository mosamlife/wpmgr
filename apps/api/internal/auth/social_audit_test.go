package auth

// social_audit_test.go: what the audit log is told a social event WAS.
//
// The action key is the whole of it. It is what the log filters on and what the
// web renders a label from, so an event filed under the wrong action is an
// event nobody looking for it will find, however complete its metadata.

import (
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
)

// TestSocialAuditActionSeparatesCredentialChangesFromLogins pins that the four
// events recorded on the social path are not all recorded as a login.
//
// EVERY ONE OF THEM USED TO BE auth.oidc.login, which renders as "Signed in
// with SSO". A provider being bound to an existing account, an account being
// created out of a provider assertion, and a stored identity changing issuer
// are credential changes, and recordSocialAuditWith's own documentation says
// capturing them is why it exists. The only thing distinguishing them lived
// inside metadata.event, where no filter reaches and no reader looks first, so
// an owner scanning for "did somebody attach themselves to my account" saw a
// list of ordinary sign-ins. The sibling file got dedicated constants for
// exactly this reason; this path did not.
func TestSocialAuditActionSeparatesCredentialChangesFromLogins(t *testing.T) {
	cases := []struct {
		event string
		want  string
	}{
		{"link", audit.ActionSocialLinked},
		{"register", audit.ActionSocialRegistered},
		{"identity_issuer_migrated", audit.ActionIdentityIssuerMoved},
		{"legacy_identity_adopted", audit.ActionIdentityAdopted},
	}
	seen := make(map[string]string, len(cases))
	for _, tc := range cases {
		t.Run(tc.event, func(t *testing.T) {
			got := socialAuditAction(tc.event)
			if got != tc.want {
				t.Errorf("socialAuditAction(%q) = %q, want %q", tc.event, got, tc.want)
			}
			if got == audit.ActionOIDCLogin {
				t.Errorf("%q is recorded as a login; it is a credential change and the log will read as %q", tc.event, "Signed in with SSO")
			}
		})
		// Two events sharing an action would put the reader back where they
		// started, needing the metadata to tell them apart.
		if prev, dup := seen[tc.want]; dup {
			t.Errorf("%q and %q share the action %q", prev, tc.event, tc.want)
		}
		seen[tc.want] = tc.event
	}

	// An unrecognised event keeps the old action rather than inventing one.
	// This only ever runs on a genuine sign-in, so a login is the honest
	// fallback, and a new event type is expected to add its own case above.
	if got := socialAuditAction("something_new"); got != audit.ActionOIDCLogin {
		t.Errorf("socialAuditAction(unknown) = %q, want the login fallback", got)
	}
}
