package events

import (
	"testing"
	"time"
)

// TestULIDMonotonicWithinMillisecond proves two ULIDs minted in the same
// millisecond still sort strictly increasing — the property the SSE replay
// cursor (?since / Last-Event-ID string compare) relies on.
func TestULIDMonotonicWithinMillisecond(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	prev := NewULID(now)
	for i := 0; i < 10_000; i++ {
		cur := NewULID(now)
		if len(cur) != 26 {
			t.Fatalf("ulid length = %d, want 26", len(cur))
		}
		if cur <= prev {
			t.Fatalf("ulid not monotonic at i=%d: %q <= %q", i, cur, prev)
		}
		prev = cur
	}
}

// TestULIDSortsByTime proves a later timestamp yields a lexicographically
// greater ULID (the time prefix dominates).
func TestULIDSortsByTime(t *testing.T) {
	a := NewULID(time.UnixMilli(1_700_000_000_000))
	b := NewULID(time.UnixMilli(1_700_000_001_000))
	if b <= a {
		t.Fatalf("later ulid should sort after earlier: %q <= %q", b, a)
	}
}

// TestParseNotifyPayload covers the '<tenant>:<event_id>' split used by the
// LISTEN listener.
func TestParseNotifyPayload(t *testing.T) {
	tid, eid, err := parseNotifyPayload("550e8400-e29b-41d4-a716-446655440000:01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tid.String() != "550e8400-e29b-41d4-a716-446655440000" {
		t.Fatalf("tenant = %s", tid)
	}
	if eid != "01ARZ3NDEKTSV4RRFFQ69G5FAV" {
		t.Fatalf("event_id = %s", eid)
	}
	if _, _, err := parseNotifyPayload("no-colon-here"); err == nil {
		t.Fatal("expected error for malformed payload")
	}
}
