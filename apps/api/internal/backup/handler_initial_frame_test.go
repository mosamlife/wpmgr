package backup

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// Bug 3 fix coverage (M5.6 / ADR-034 restore-UI fix). The initial SSE frame
// MUST be suppressed when the snapshot's persisted progress is a stale
// terminal echo — otherwise the browser's TanStack Query cache is overwritten
// with the OLD failed/completed phase moments after the operator clicks
// Restore, and the new restore lifecycle's first phase event is the only
// signal that ever clears it. We assert the suppression boundary explicitly
// because the bug is invisible from the wire-format SSE test (which only
// covers post-subscribe live events).
func TestInitialFrameToSend(t *testing.T) {
	snapID := uuid.New()
	fresh := time.Now().UTC().Add(-5 * time.Second)
	stale := time.Now().UTC().Add(-5 * time.Minute)

	cases := []struct {
		name      string
		snap      Snapshot
		wantSend  bool
		wantPhase string
	}{
		{
			name: "running phase always sent",
			snap: Snapshot{
				ID:                snapID,
				Status:            StatusRunning,
				Progress:          []byte(`{"phase":"encrypting_uploading","phase_detail":{}}`),
				ProgressUpdatedAt: &stale, // even stale running is sent — only terminals are suppressed
				UpdatedAt:         stale,
			},
			wantSend:  true,
			wantPhase: "encrypting_uploading",
		},
		{
			name: "fresh completed sent (legitimate current state)",
			snap: Snapshot{
				ID:                snapID,
				Status:            StatusCompleted,
				Progress:          []byte(`{"phase":"completed","phase_detail":{}}`),
				ProgressUpdatedAt: &fresh,
				UpdatedAt:         fresh,
			},
			wantSend:  true,
			wantPhase: "completed",
		},
		{
			name: "stale failed suppressed (would poison new restore cache)",
			snap: Snapshot{
				ID:                snapID,
				Status:            StatusFailed,
				Progress:          []byte(`{"phase":"failed","phase_detail":{"stage":"encrypting_uploading"}}`),
				ProgressUpdatedAt: &stale,
				UpdatedAt:         stale,
			},
			wantSend: false,
		},
		{
			name: "stale completed suppressed",
			snap: Snapshot{
				ID:                snapID,
				Status:            StatusCompleted,
				Progress:          []byte(`{"phase":"completed","phase_detail":{}}`),
				ProgressUpdatedAt: &stale,
				UpdatedAt:         stale,
			},
			wantSend: false,
		},
		{
			name: "empty progress with terminal status falls through to status (sent if fresh)",
			snap: Snapshot{
				ID:        snapID,
				Status:    StatusCompleted,
				Progress:  nil,
				UpdatedAt: fresh,
			},
			wantSend:  true,
			wantPhase: StatusCompleted,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, ok := initialFrameToSend(tc.snap)
			if ok != tc.wantSend {
				t.Fatalf("send=%v, want %v (ev=%+v)", ok, tc.wantSend, ev)
			}
			if ok && tc.wantPhase != "" && ev.Phase != tc.wantPhase {
				t.Fatalf("phase=%q, want %q", ev.Phase, tc.wantPhase)
			}
		})
	}
}
