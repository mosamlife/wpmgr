package auth

import (
	"fmt"
	"testing"
	"time"
)

// TestStaleSettleLeavesARecreatedBucketAlone proves the rule settle documents:
// a charge settled against a bucket that has since left the map (evicted, or
// swept) must not touch whatever now sits under that key, even a bucket
// recreated for a later admission.
//
// Without the b.buckets[key] != bk check in keyedBudget.settle, a stale
// give-back returns a token the new bucket's admission took and, if that
// leaves it at pending == 0 and kept == 0, deletes the entry outright: the
// next admission's later failure is then never recorded, minting a token.
func TestStaleSettleLeavesARecreatedBucketAlone(t *testing.T) {
	const key = "victim"
	b := newKeyedBudget("test", 10)
	t0 := time.Unix(1_800_000_000, 0)

	// A failure makes key permanent, so it can be evicted later.
	keptCharge(t, b, key, t0)
	// Admission A charges key and is still in flight.
	bkA, ok := b.charge(key, t0)
	if !ok {
		t.Fatal("A could not charge")
	}
	// A window later key has refilled to its whole budget. Failed attempts on
	// other keys then fill the map past the cap, each leaving its entry a token
	// short: key is the fullest permanent entry and is evicted.
	t1 := t0.Add(loginWindow)
	for i := 0; i < loginBucketCap; i++ {
		keptCharge(t, b, fmt.Sprintf("filler-%d", i), t1)
	}
	b.mu.Lock()
	_, stillThere := b.buckets[key]
	b.mu.Unlock()
	if stillThere {
		t.Fatal("precondition: key was not evicted")
	}

	// Admission B charges key: a new, provisional bucket.
	t2 := t1.Add(time.Second)
	bkB, ok := b.charge(key, t2)
	if !ok || bkB == bkA {
		t.Fatal("precondition: B did not get a new bucket")
	}
	want := b.tokensAt(key, t2)

	// A's password verified: its give-back is against the evicted bucket.
	b.settle(key, bkA, t2, true)
	b.mu.Lock()
	cur, present := b.buckets[key]
	b.mu.Unlock()
	if !present || cur != bkB {
		t.Fatalf("A's stale give-back removed or replaced B's bucket (present=%v)", present)
	}
	if got := b.tokensAt(key, t2); got != want {
		t.Fatalf("A's stale give-back changed B's bucket from %.2f to %.2f tokens", want, got)
	}
	if bkB.pending != 1 || bkB.kept != 0 {
		t.Fatalf("B's bucket pending=%d kept=%d after A's stale settle, want 1/0", bkB.pending, bkB.kept)
	}

	// B fails: its charge must stay.
	b.settle(key, bkB, t2, false)
	if got := b.tokensAt(key, t2); got != want {
		t.Fatalf("B's failed charge was not kept: %.2f tokens, want %.2f", got, want)
	}
}
