package update

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestHubPublishDeliversToSubscriber(t *testing.T) {
	h := NewHub()
	runID := uuid.New()
	ch, unsub := h.Subscribe(runID)
	defer unsub()

	if h.SubscriberCount(runID) != 1 {
		t.Fatalf("subscriber count = %d, want 1", h.SubscriberCount(runID))
	}

	want := Event{RunID: runID, Status: TaskRunning, RunStatus: RunRunning}
	h.Publish(want)

	select {
	case got := <-ch:
		if got.Status != TaskRunning {
			t.Fatalf("got status %s, want running", got.Status)
		}
	case <-time.After(time.Second):
		t.Fatal("event not delivered")
	}
}

func TestHubPublishIgnoresOtherRuns(t *testing.T) {
	h := NewHub()
	runA, runB := uuid.New(), uuid.New()
	chA, unsubA := h.Subscribe(runA)
	defer unsubA()

	h.Publish(Event{RunID: runB, Status: TaskSucceeded})

	select {
	case ev := <-chA:
		t.Fatalf("subscriber A received an event for run B: %+v", ev)
	case <-time.After(100 * time.Millisecond):
		// expected: nothing delivered
	}
}

func TestHubUnsubscribeClosesChannelAndDropsRun(t *testing.T) {
	h := NewHub()
	runID := uuid.New()
	ch, unsub := h.Subscribe(runID)
	unsub()

	if _, open := <-ch; open {
		t.Fatal("channel should be closed after unsubscribe")
	}
	if h.SubscriberCount(runID) != 0 {
		t.Fatalf("subscriber count = %d after unsubscribe, want 0", h.SubscriberCount(runID))
	}
}

func TestHubPublishNonBlockingOnFullBuffer(t *testing.T) {
	h := NewHub()
	runID := uuid.New()
	_, unsub := h.Subscribe(runID)
	defer unsub()

	// Publish far more than the buffer; must not block (slow consumer drops).
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			h.Publish(Event{RunID: runID, Status: TaskRunning})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a full subscriber buffer")
	}
}
