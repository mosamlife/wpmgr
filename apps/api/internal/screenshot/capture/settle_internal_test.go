package capture

// Internal (white-box) tests for the GH #229 ready-wait helpers. These pin the
// bounded-settle / budget-clamp logic that guarantees a slow or broken page can
// never make waitUntilReady hang the worker — verified here without launching a
// real Chromium browser.

import (
	"testing"
	"time"
)

func TestSettleClamp(t *testing.T) {
	tests := []struct {
		name      string
		remaining time.Duration
		want      time.Duration
	}{
		{"no budget left is zero", 0, 0},
		{"negative budget is zero", -5 * time.Second, 0},
		{"ample budget yields full settle", 5 * time.Second, renderSettleWait},
		{"exactly the settle yields full settle", renderSettleWait, renderSettleWait},
		{"tight budget is shortened to what remains", 400 * time.Millisecond, 400 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := settleClamp(tt.remaining)
			if got != tt.want {
				t.Errorf("settleClamp(%v) = %v, want %v", tt.remaining, got, tt.want)
			}
			// Invariant: the settle can never exceed the fixed cap and is never negative.
			if got > renderSettleWait {
				t.Errorf("settleClamp(%v) = %v exceeds renderSettleWait %v", tt.remaining, got, renderSettleWait)
			}
			if got < 0 {
				t.Errorf("settleClamp(%v) = %v is negative", tt.remaining, got)
			}
		})
	}
}

func TestReadyWaitBudget(t *testing.T) {
	t.Run("default when unset", func(t *testing.T) {
		t.Setenv("WPMGR_SCREENSHOT_READY_WAIT", "")
		if got := readyWaitBudget(); got != defaultReadyWaitBudget {
			t.Errorf("readyWaitBudget() = %v, want default %v", got, defaultReadyWaitBudget)
		}
	})

	t.Run("valid override honored", func(t *testing.T) {
		t.Setenv("WPMGR_SCREENSHOT_READY_WAIT", "5")
		if got := readyWaitBudget(); got != 5*time.Second {
			t.Errorf("readyWaitBudget() = %v, want 5s", got)
		}
	})

	t.Run("override clamped to captureTimeout", func(t *testing.T) {
		t.Setenv("WPMGR_SCREENSHOT_READY_WAIT", "999")
		if got := readyWaitBudget(); got != captureTimeout {
			t.Errorf("readyWaitBudget() = %v, want clamp to captureTimeout %v", got, captureTimeout)
		}
	})

	// Any non-positive / non-numeric override falls back to the default so a
	// misconfiguration can never zero out or invert the wait.
	for _, bad := range []string{"0", "-3", "abc", "1.5"} {
		t.Run("falls back for "+bad, func(t *testing.T) {
			t.Setenv("WPMGR_SCREENSHOT_READY_WAIT", bad)
			if got := readyWaitBudget(); got != defaultReadyWaitBudget {
				t.Errorf("readyWaitBudget() with %q = %v, want default %v", bad, got, defaultReadyWaitBudget)
			}
		})
	}

	// The budget must always stay within the hard capture deadline so the wait
	// can never outlast the job.
	t.Run("budget never exceeds captureTimeout", func(t *testing.T) {
		for _, v := range []string{"", "1", "8", "15", "60"} {
			t.Setenv("WPMGR_SCREENSHOT_READY_WAIT", v)
			if got := readyWaitBudget(); got > captureTimeout {
				t.Errorf("readyWaitBudget() with %q = %v exceeds captureTimeout %v", v, got, captureTimeout)
			}
		}
	})
}
