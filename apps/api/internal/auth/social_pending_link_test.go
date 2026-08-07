package auth

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"
)

// The parked link is the mechanism that lets an approved identity link survive
// a two-factor round trip WITHOUT being written first. It is only safe if it
// cannot be applied to the wrong account, cannot be applied by a challenge the
// approving handshake did not produce, cannot be replayed later, and cannot
// outlive the handshake itself, so those are what is asserted.

func parkedLink(userID uuid.UUID) Identity {
	return Identity{
		UserID: userID, Provider: "google", Subject: "google-sub-1",
		Email: "sarah@acme.com", EmailVerified: true,
	}
}

func TestPendingSocialLinkRoundTripsForTheChallengeItNames(t *testing.T) {
	m := NewSessionManagerWithStore(scs.New(), false)
	ctx := loadCtx(t, m)

	user, challenge := uuid.New(), uuid.New()
	m.putPendingSocialLink(ctx, user, challenge, parkedLink(user))

	got, ok := m.takePendingSocialLink(ctx, user, challenge)
	if !ok {
		t.Fatal("a link parked for this user and challenge must come back to them")
	}
	if got.Provider != "google" || got.Subject != "google-sub-1" || got.UserID != user {
		t.Fatalf("round trip lost fields: %+v", got)
	}

	// Single use: the second factor completes once, and so does the link.
	if _, ok := m.takePendingSocialLink(ctx, user, challenge); ok {
		t.Fatal("the parked link must be consumed on read; a link that survives can be applied twice")
	}
}

// The binding that matters most. A link approved during one login must never be
// applied by whoever completes the next challenge on this browser.
func TestPendingSocialLinkIsRefusedForADifferentAccount(t *testing.T) {
	m := NewSessionManagerWithStore(scs.New(), false)
	ctx := loadCtx(t, m)

	victim, challenge := uuid.New(), uuid.New()
	m.putPendingSocialLink(ctx, victim, challenge, parkedLink(victim))

	if _, ok := m.takePendingSocialLink(ctx, uuid.New(), challenge); ok {
		t.Fatal("a link parked for one account was handed to another; that binds a provider to whoever finishes the next challenge")
	}
	// And it is gone, not left waiting for a luckier caller.
	if _, ok := m.takePendingSocialLink(ctx, victim, challenge); ok {
		t.Fatal("a refused link must be discarded, not left parked for a later attempt")
	}
}

// The same account can hold several live challenges at once: opening the
// sign-in page again issues another, and a password login and a provider
// callback each issue their own. Only the one the approving handshake produced
// may apply the link, otherwise abandoning the provider flow and finishing a
// password login instead would still bind the provider.
func TestPendingSocialLinkIsRefusedForADifferentChallengeOfTheSameUser(t *testing.T) {
	m := NewSessionManagerWithStore(scs.New(), false)
	ctx := loadCtx(t, m)

	user := uuid.New()
	fromProvider := uuid.New()
	m.putPendingSocialLink(ctx, user, fromProvider, parkedLink(user))

	otherChallenge := uuid.New()
	if _, ok := m.takePendingSocialLink(ctx, user, otherChallenge); ok {
		t.Fatal("a link approved by one handshake was applied by an unrelated challenge for the same account")
	}
	if _, ok := m.takePendingSocialLink(ctx, user, fromProvider); ok {
		t.Fatal("a refused link must be discarded, not left parked for the challenge that does match")
	}
}

func TestPendingSocialLinkExpires(t *testing.T) {
	m := NewSessionManagerWithStore(scs.New(), false)
	ctx := loadCtx(t, m)

	user, challenge := uuid.New(), uuid.New()
	// Park one that is already past its window. Written directly because the
	// TTL is what is under test.
	stale, err := json.Marshal(pendingSocialLinkEnvelope{
		UserID:      user.String(),
		ChallengeID: challenge.String(),
		Provider:    "google",
		Subject:     "google-sub-1",
		ExpiresAt:   time.Now().UTC().Add(-time.Second).Unix(),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	m.SCS().Put(ctx, sessKeyPendingSocialLink, string(stale))

	if _, ok := m.takePendingSocialLink(ctx, user, challenge); ok {
		t.Fatal("an expired link was applied; a handshake nobody finished authenticating for must not wait indefinitely for this account's next login")
	}
}

// Starting a new handshake supersedes an abandoned one, so a link the person
// walked away from mid challenge is not still sitting there.
func TestStartingANewHandshakeClearsAParkedLink(t *testing.T) {
	m := NewSessionManagerWithStore(scs.New(), false)
	ctx := loadCtx(t, m)

	user, challenge := uuid.New(), uuid.New()
	m.putPendingSocialLink(ctx, user, challenge, parkedLink(user))
	m.putSocial(ctx, "github", "state-2", "nonce-2", "verifier-2")

	if _, ok := m.takePendingSocialLink(ctx, user, challenge); ok {
		t.Fatal("a new handshake must discard the link approved by the abandoned one")
	}
}
