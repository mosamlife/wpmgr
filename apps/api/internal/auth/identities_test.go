package auth

// identities_test.go -- the account-must-never-reach-zero-sign-in-methods
// invariant, exercised exhaustively over every combination of the three facts
// that decide it.
//
// decideUnlink is the whole rule, so this file is the whole test: everything
// around it (the row lock, the transaction, the handler) exists to feed this
// function correct inputs and to carry out its answer, and those are checked
// by the build and by the routes. What could not be recovered from is a wrong
// answer HERE, so this is what gets the table.

import (
	"strings"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

func TestDecideUnlink(t *testing.T) {
	cases := []struct {
		name          string
		linked        bool
		identityCount int
		hasPassword   bool
		wantKind      domain.Kind // zero value means "allowed"
		wantCode      string
	}{
		// THE ONE THAT MATTERS. Sole identity, no password: removing it would
		// leave no way to sign in and no way to recover, because password reset
		// refuses to mint a set-password link for a passwordless account.
		{
			name:   "refuses the last identity when no password is set",
			linked: true, identityCount: 1, hasPassword: false,
			wantKind: domain.KindConflict, wantCode: "last_sign_in_method",
		},
		// The same account after adding a password: the refusal must lift, or
		// the message telling people to set a password would be a lie.
		{
			name:   "allows the last identity once a password exists",
			linked: true, identityCount: 1, hasPassword: true,
		},
		// Two identities and no password: one can go, the other still works.
		{
			name:   "allows one of two identities with no password",
			linked: true, identityCount: 2, hasPassword: false,
		},
		{
			name:   "allows one of two identities with a password",
			linked: true, identityCount: 2, hasPassword: true,
		},
		{
			name:   "allows one of many identities",
			linked: true, identityCount: 5, hasPassword: false,
		},
		// Not linked is answered as not linked, whatever the rest of the state.
		// Reporting "last sign-in method" for a provider the account never had
		// would be both wrong and confusing.
		{
			name:   "reports an unlinked provider as not found, not as the last method",
			linked: false, identityCount: 1, hasPassword: false,
			wantKind: domain.KindNotFound, wantCode: "identity_not_linked",
		},
		{
			name:   "reports an unlinked provider as not found when a password exists",
			linked: false, identityCount: 0, hasPassword: true,
			wantKind: domain.KindNotFound, wantCode: "identity_not_linked",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := decideUnlink(tc.linked, tc.identityCount, tc.hasPassword, "google")

			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("expected the unlink to be allowed, got %v", err)
				}
				return
			}

			de, ok := domain.AsDomain(err)
			if !ok {
				t.Fatalf("expected a domain error, got %v", err)
			}
			if de.Kind != tc.wantKind {
				t.Errorf("kind = %v, want %v", de.Kind, tc.wantKind)
			}
			if de.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", de.Code, tc.wantCode)
			}
		})
	}
}

// The refusal is only useful if it tells the person what to do instead. Without
// this, "you cannot remove this" reads as an arbitrary block on a button the
// page just offered them, and the way out (set a password first, from the same
// card) is discoverable only by guessing.
func TestDecideUnlink_RefusalSaysWhatToDoInstead(t *testing.T) {
	err := decideUnlink(true, 1, false, "github")
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("expected a domain error, got %v", err)
	}
	msg := strings.ToLower(de.Message)
	if !strings.Contains(msg, "set a password") {
		t.Errorf("refusal does not name the next step; message = %q", de.Message)
	}
	// Naming the provider matters when more than one is connected: "one of your
	// sign-in methods" leaves the reader checking which.
	if !strings.Contains(de.Message, "GitHub") {
		t.Errorf("refusal does not name the provider; message = %q", de.Message)
	}
}

// CanUnlink is what the settings page renders its Disconnect button from, so it
// has to agree with decideUnlink in every state. Two implementations of one
// rule is how a UI ends up offering an action the server always refuses (or,
// worse, hiding one it would allow).
func TestCanUnlinkAgreesWithDecideUnlink(t *testing.T) {
	for _, hasPassword := range []bool{false, true} {
		for count := 1; count <= 3; count++ {
			methods := SignInMethods{
				HasPassword: hasPassword,
				Identities:  make([]Identity, count),
			}
			serverAllows := decideUnlink(true, count, hasPassword, "google") == nil
			if methods.CanUnlink() != serverAllows {
				t.Errorf("hasPassword=%v count=%d: CanUnlink()=%v but the server would %s",
					hasPassword, count, methods.CanUnlink(),
					map[bool]string{true: "allow", false: "refuse"}[serverAllows])
			}
		}
	}
}
