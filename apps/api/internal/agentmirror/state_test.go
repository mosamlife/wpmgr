package agentmirror

import (
	"testing"
	"time"
)

func ptrTime(t time.Time) *time.Time { return &t }

// TestStatus_Disabled: disabled always wins, regardless of any other field.
func TestStatus_Disabled(t *testing.T) {
	now := time.Now()
	s := State{
		LastAttemptAt:      ptrTime(now),
		LastAttemptOutcome: OutcomeMirrored,
		LastSuccessAt:      ptrTime(now),
	}
	if got := s.Status(false, now); got != StatusDisabled {
		t.Fatalf("Status = %q, want %q", got, StatusDisabled)
	}
}

// TestStatus_Pending: enabled, never attempted.
func TestStatus_Pending(t *testing.T) {
	now := time.Now()
	if got := (State{}).Status(true, now); got != StatusPending {
		t.Fatalf("Status = %q, want %q", got, StatusPending)
	}
}

// TestStatus_Misconfigured: never self-heals, so it must win even the FIRST
// time it is observed, and independent of staleness.
func TestStatus_Misconfigured(t *testing.T) {
	now := time.Now()
	s := State{
		LastAttemptAt:      ptrTime(now),
		LastAttemptOutcome: OutcomeNotConfigured,
	}
	if got := s.Status(true, now); got != StatusMisconfigured {
		t.Fatalf("Status = %q, want %q", got, StatusMisconfigured)
	}
}

// TestStatus_StandingDown: a foreign channel is correct, permanent behaviour,
// never a warning-shaped status.
func TestStatus_StandingDown(t *testing.T) {
	now := time.Now()
	s := State{
		LastAttemptAt:      ptrTime(now),
		LastAttemptOutcome: OutcomeForeignChannel,
	}
	if got := s.Status(true, now); got != StatusStandingDown {
		t.Fatalf("Status = %q, want %q", got, StatusStandingDown)
	}
}

// TestStatus_OKWithinThreshold: a fresh, ordinary confirmation is "ok", not
// "stale": the whole point of picking a threshold clear of the normal cycle.
func TestStatus_OKWithinThreshold(t *testing.T) {
	now := time.Now()
	s := State{
		LastAttemptAt:      ptrTime(now.Add(-10 * time.Minute)),
		LastAttemptOutcome: OutcomeUnchanged,
		LastSuccessAt:      ptrTime(now.Add(-6*time.Hour - 20*time.Minute)),
		LastSuccessOutcome: OutcomeUnchanged,
	}
	if got := s.Status(true, now); got != StatusOK {
		t.Fatalf("Status = %q, want %q (6h20m since success must not read as stale)", got, StatusOK)
	}
}

// TestStatus_OKAfterFailedAttemptWithFreshSuccess: C1/C5 in status form. A
// failed attempt just now, with a fresh success behind it, must still be
// "ok": the mirror's job is confirming the reference, and it has, recently.
func TestStatus_OKAfterFailedAttemptWithFreshSuccess(t *testing.T) {
	now := time.Now()
	s := State{
		LastAttemptAt:      ptrTime(now.Add(-1 * time.Minute)),
		LastAttemptOutcome: OutcomeUpstreamUnavailable,
		LastSuccessAt:      ptrTime(now.Add(-30 * time.Minute)),
		LastSuccessOutcome: OutcomeUnchanged,
	}
	if got := s.Status(true, now); got != StatusOK {
		t.Fatalf("Status = %q, want %q", got, StatusOK)
	}
}

// TestStatus_StaleAtThreshold proves the boundary is >= (a confirmation
// exactly StalenessThreshold old is already stale, not one tick shy of it).
func TestStatus_StaleAtThreshold(t *testing.T) {
	now := time.Now()
	s := State{
		LastAttemptAt:      ptrTime(now),
		LastAttemptOutcome: OutcomeUnchanged,
		LastSuccessAt:      ptrTime(now.Add(-StalenessThreshold)),
		LastSuccessOutcome: OutcomeUnchanged,
	}
	if got := s.Status(true, now); got != StatusStale {
		t.Fatalf("Status = %q, want %q at exactly the threshold", got, StatusStale)
	}
}

// TestStatus_StaleJustUnderThreshold: one second inside the threshold must
// still read "ok": proves the comparison isn't accidentally off by a wider
// margin than intended.
func TestStatus_StaleJustUnderThreshold(t *testing.T) {
	now := time.Now()
	s := State{
		LastAttemptAt:      ptrTime(now),
		LastAttemptOutcome: OutcomeUnchanged,
		LastSuccessAt:      ptrTime(now.Add(-StalenessThreshold + time.Second)),
		LastSuccessOutcome: OutcomeUnchanged,
	}
	if got := s.Status(true, now); got != StatusOK {
		t.Fatalf("Status = %q, want %q just under the threshold", got, StatusOK)
	}
}

// TestStatus_AttemptedButNeverSucceeded: the severe form of the reported bug,
// enabled, has tried, has never once confirmed anything. This must be
// "stale" immediately, not "pending" (pending is reserved for "never even
// tried yet") and not silently "ok".
func TestStatus_AttemptedButNeverSucceeded(t *testing.T) {
	now := time.Now()
	s := State{
		LastAttemptAt:      ptrTime(now),
		LastAttemptOutcome: OutcomeRateLimited,
	}
	if got := s.Status(true, now); got != StatusStale {
		t.Fatalf("Status = %q, want %q", got, StatusStale)
	}
}

// TestOutcome_IsSuccess pins the three-way split exactly: mirrored/current/
// unchanged confirm something; every other outcome does not.
func TestOutcome_IsSuccess(t *testing.T) {
	successes := map[Outcome]bool{
		OutcomeMirrored:            true,
		OutcomeCurrent:             true,
		OutcomeUnchanged:           true,
		OutcomeRateLimited:         false,
		OutcomeRefused:             false,
		OutcomeForeignChannel:      false,
		OutcomeUpstreamUnavailable: false,
		OutcomeStorageError:        false,
		OutcomeNotConfigured:       false,
	}
	for outcome, want := range successes {
		if got := outcome.IsSuccess(); got != want {
			t.Errorf("Outcome(%q).IsSuccess() = %v, want %v", outcome, got, want)
		}
	}
}
