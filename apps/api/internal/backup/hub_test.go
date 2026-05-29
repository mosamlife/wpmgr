package backup

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestHubPublishDeliversToSubscriber(t *testing.T) {
	h := NewHub()
	snapshotID := uuid.New()
	ch, unsub := h.Subscribe(snapshotID)
	defer unsub()

	if h.SubscriberCount(snapshotID) != 1 {
		t.Fatalf("subscriber count = %d, want 1", h.SubscriberCount(snapshotID))
	}

	want := BackupEvent{SnapshotID: snapshotID, Phase: "encrypting_uploading", Status: StatusRunning}
	h.Publish(want)

	select {
	case got := <-ch:
		if got.Phase != "encrypting_uploading" {
			t.Fatalf("got phase %s, want encrypting_uploading", got.Phase)
		}
	case <-time.After(time.Second):
		t.Fatal("event not delivered")
	}
}

func TestHubPublishIgnoresOtherSnapshots(t *testing.T) {
	h := NewHub()
	snapA, snapB := uuid.New(), uuid.New()
	chA, unsubA := h.Subscribe(snapA)
	defer unsubA()

	h.Publish(BackupEvent{SnapshotID: snapB, Phase: "completed", Status: StatusCompleted})

	select {
	case ev := <-chA:
		t.Fatalf("subscriber A received an event for snapshot B: %+v", ev)
	case <-time.After(100 * time.Millisecond):
		// expected: nothing delivered
	}
}

func TestHubUnsubscribeClosesChannelAndDropsSnapshot(t *testing.T) {
	h := NewHub()
	snapshotID := uuid.New()
	ch, unsub := h.Subscribe(snapshotID)
	unsub()

	if _, open := <-ch; open {
		t.Fatal("channel should be closed after unsubscribe")
	}
	if h.SubscriberCount(snapshotID) != 0 {
		t.Fatalf("subscriber count = %d after unsubscribe, want 0", h.SubscriberCount(snapshotID))
	}
}

func TestHubPublishNonBlockingOnFullBuffer(t *testing.T) {
	h := NewHub()
	snapshotID := uuid.New()
	_, unsub := h.Subscribe(snapshotID)
	defer unsub()

	// Publish far more than the buffer; must not block (slow consumer drops).
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			h.Publish(BackupEvent{SnapshotID: snapshotID, Phase: "encrypting_uploading", Status: StatusRunning})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a full subscriber buffer")
	}
}
