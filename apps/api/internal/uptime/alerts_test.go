package uptime

import (
	"testing"
	"time"
)

// TestEvaluateTransitionDedupe exercises the alert state machine: N consecutive
// downs fire exactly ONE down alert (on the threshold crossing), subsequent
// downs are de-duped, and the first up after an incident fires exactly ONE
// recovery.
func TestEvaluateTransitionDedupe(t *testing.T) {
	const threshold = 2
	now := time.Now()
	st := AlertState{LastStatus: StatusUnknown}

	// Probe 1: down. consecutive=1 < threshold ⇒ no alert.
	tr := Evaluate(st, false, threshold, now)
	if tr.FireDown || tr.FireRecovery {
		t.Fatalf("probe 1: expected no alert, got %+v", tr)
	}
	if tr.NewState.ConsecutiveDown != 1 || tr.NewState.InIncident {
		t.Fatalf("probe 1: unexpected state %+v", tr.NewState)
	}
	st = tr.NewState

	// Probe 2: down. consecutive=2 >= threshold ⇒ fire ONE down alert.
	tr = Evaluate(st, false, threshold, now)
	if !tr.FireDown || tr.FireRecovery {
		t.Fatalf("probe 2: expected one down alert, got %+v", tr)
	}
	if !tr.NewState.InIncident {
		t.Fatalf("probe 2: expected in_incident, got %+v", tr.NewState)
	}
	st = tr.NewState

	// Probe 3: down again. Already in incident ⇒ de-duped (no alert).
	tr = Evaluate(st, false, threshold, now)
	if tr.FireDown || tr.FireRecovery {
		t.Fatalf("probe 3: expected de-dupe (no alert), got %+v", tr)
	}
	if tr.NewState.ConsecutiveDown != 3 {
		t.Fatalf("probe 3: expected consecutive=3, got %d", tr.NewState.ConsecutiveDown)
	}
	st = tr.NewState

	// Probe 4: up. In incident ⇒ fire ONE recovery, clear incident.
	tr = Evaluate(st, true, threshold, now)
	if !tr.FireRecovery || tr.FireDown {
		t.Fatalf("probe 4: expected one recovery alert, got %+v", tr)
	}
	if tr.NewState.InIncident || tr.NewState.ConsecutiveDown != 0 || tr.NewState.LastStatus != StatusUp {
		t.Fatalf("probe 4: unexpected recovered state %+v", tr.NewState)
	}
	st = tr.NewState

	// Probe 5: up again. Not in incident ⇒ no alert (no recovery spam).
	tr = Evaluate(st, true, threshold, now)
	if tr.FireDown || tr.FireRecovery {
		t.Fatalf("probe 5: expected no alert, got %+v", tr)
	}
}

// TestEvaluateThresholdOne fires immediately on the first down when threshold=1.
func TestEvaluateThresholdOne(t *testing.T) {
	tr := Evaluate(AlertState{LastStatus: StatusUnknown}, false, 1, time.Now())
	if !tr.FireDown {
		t.Fatalf("threshold=1: expected immediate down alert, got %+v", tr)
	}
}
