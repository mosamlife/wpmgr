package site

import "testing"

func TestCanTransition(t *testing.T) {
	legal := []struct{ from, to ConnectionState }{
		{StatePendingEnrollment, StateConnected},
		{StatePendingEnrollment, StateArchived},
		{StateConnected, StateDegraded},
		{StateConnected, StateDisconnected},
		{StateConnected, StateRevoked},
		{StateConnected, StateArchived},
		{StateDegraded, StateConnected},  // recovery
		{StateDegraded, StateDisconnected},
		{StateDisconnected, StateConnected},          // heartbeat returns
		{StateDisconnected, StatePendingEnrollment},  // begin re-enroll
		{StateDisconnected, StateRevoked},
		{StateRevoked, StatePendingEnrollment}, // re-enroll
		{StateRevoked, StateArchived},
		{StateArchived, StateDisconnected},      // restore
		{StateArchived, StatePendingEnrollment}, // restore → re-enroll
	}
	for _, c := range legal {
		if !CanTransition(c.from, c.to) {
			t.Errorf("expected %s→%s to be legal", c.from, c.to)
		}
	}

	illegal := []struct{ from, to ConnectionState }{
		{StatePendingEnrollment, StateDegraded},     // can't degrade before connecting
		{StatePendingEnrollment, StateDisconnected}, // ditto
		{StateArchived, StateConnected},             // must restore first
		{StateArchived, StateRevoked},
		{StateRevoked, StateConnected},  // revoked needs a re-enroll, not a direct connect
		{StateConnected, StatePendingEnrollment}, // can't un-enroll a live site directly
		{StateDegraded, StatePendingEnrollment},  // re-enroll only from disconnected/revoked/archived
	}
	for _, c := range illegal {
		if CanTransition(c.from, c.to) {
			t.Errorf("expected %s→%s to be ILLEGAL", c.from, c.to)
		}
	}

	// Idempotent self-transitions are always allowed (e.g. heartbeat while
	// already connected).
	for s := range legalTransitions {
		if !CanTransition(s, s) {
			t.Errorf("expected self-transition %s→%s to be legal", s, s)
		}
	}
}

func TestConnectionStateValid(t *testing.T) {
	for _, s := range []ConnectionState{
		StatePendingEnrollment, StateConnected, StateDegraded,
		StateDisconnected, StateRevoked, StateArchived,
	} {
		if !s.Valid() {
			t.Errorf("expected %s to be a valid state", s)
		}
	}
	if ConnectionState("bogus").Valid() {
		t.Error("expected bogus to be invalid")
	}
}
