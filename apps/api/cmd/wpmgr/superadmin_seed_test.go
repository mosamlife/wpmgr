package main

// superadmin_seed_test.go — regression lock on what WPMGR_SUPERADMIN_EMAILS is
// allowed to assert.
//
// The env var names people who may run the install. It used to also stamp
// users.email_verified_at, which is not a claim an operator can make on
// somebody's behalf: that column is this install's own record of having watched
// a human open a link it sent. It is also one half of the rule that lets a
// provider-verified identity attach itself to an existing account, so supplying
// it by environment variable handed that half over on the highest privilege
// account on the install.
//
// These assert the SQL the seeder actually executes, which is why both
// statements are constants rather than literals inline at their call sites.

import (
	"strings"
	"testing"
)

// seederStatements is every statement the WPMGR_SUPERADMIN_EMAILS path runs
// against users. Anything added here is subject to the same rule.
func seederStatements() map[string]string {
	return map[string]string{
		"grant (existing account)": superadminGrantSQL,
		"create (no account yet)":  superadminCreateSQL,
	}
}

// TestSuperadminSeedNeverStampsEmailVerification is the security lock. An
// operator env var must not be able to satisfy the local half of the social
// linking rule.
func TestSuperadminSeedNeverStampsEmailVerification(t *testing.T) {
	for name, stmt := range seederStatements() {
		if strings.Contains(stmt, "email_verified_at") {
			t.Errorf("the superadmin seeder's %s statement writes email_verified_at.\n"+
				"An environment variable is not evidence that anyone opened a link sent to that address, "+
				"and email_verified_at is half of what lets a provider-verified identity attach itself to "+
				"an existing account. The operator verifies their address like everyone else.\n%s", name, stmt)
		}
	}
}

// TestSuperadminSeedStillActivatesTheAccount guards the other direction. The
// point was to stop implying VERIFICATION, not to lock the operator out:
// password login gates on users.status, and an install whose mailbox domain
// does not accept mail has no other way in.
func TestSuperadminSeedStillActivatesTheAccount(t *testing.T) {
	for name, stmt := range seederStatements() {
		if !strings.Contains(stmt, "'active'") {
			t.Errorf("the superadmin seeder's %s statement no longer activates the account; "+
				"password login gates on users.status, so the operator would be locked out:\n%s", name, stmt)
		}
		if !strings.Contains(stmt, "is_superadmin") {
			t.Errorf("the superadmin seeder's %s statement no longer grants superadmin:\n%s", name, stmt)
		}
	}
}

// TestSuperadminGrantIsAdditive pins the two properties the seeder has always
// had and that the verification change must not quietly alter: it matches
// case-insensitively (emails are persisted lowercased) and it never demotes.
// Revocation is a separate, explicit env var.
func TestSuperadminGrantIsAdditive(t *testing.T) {
	if !strings.Contains(superadminGrantSQL, "lower(email) = $1") {
		t.Errorf("the grant must match on lower(email); emails are persisted lowercased:\n%s", superadminGrantSQL)
	}
	if strings.Contains(superadminGrantSQL, "is_superadmin = false") {
		t.Errorf("the grant seeder must never demote; that is WPMGR_SUPERADMIN_REVOKE_EMAILS:\n%s", superadminGrantSQL)
	}
}
