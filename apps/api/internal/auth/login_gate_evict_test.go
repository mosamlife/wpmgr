package auth

import (
	"fmt"
	"math"
	"net/http"
	"net/netip"
	"testing"
	"time"
)

// Proofs for the eviction choice at loginBucketCap: the permanent entry with
// the most tokens goes, refill included, least recently seen among equals,
// never the entry just made permanent and never a provisional one.

// drainedKept makes key permanent with a failed attempt's charge and keeps
// failed charges on it until it has no token left at now.
func drainedKept(t *testing.T, b *keyedBudget, key string, now time.Time) {
	t.Helper()
	keptCharge(t, b, key, now)
	for b.tokensAt(key, now) >= 1 {
		keptCharge(t, b, key, now)
	}
}

// keptCharges keeps n failed charges on key at now.
func keptCharges(t *testing.T, b *keyedBudget, key string, now time.Time, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		keptCharge(t, b, key, now)
	}
}

// bucketOf returns key's bucket, or nil if the map holds none.
func bucketOf(b *keyedBudget, key string) *loginBucket {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buckets[key]
}

// permanentCount counts the entries carrying a kept charge.
func permanentCount(b *keyedBudget) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, bk := range b.buckets {
		if bk.kept > 0 {
			n++
		}
	}
	return n
}

// fillerAddr is the i-th of a run of distinct IPv4 sources, one per filler.
func fillerAddr(i int) string {
	return netip.AddrFrom4([4]byte{198, 18 + byte(i>>16), byte(i >> 8), byte(i)}).String()
}

// TestCapEvictionCannotResetADrainedPairOrSource: failed attempts from many
// distinct sources, enough to push every refusing map past its cap, must not
// hand a drained pair or a drained source back its budget while the map holds
// entries with more tokens than they have. Proven over HTTP in enforce mode:
// the victim's next attempt is still refused.
func TestCapEvictionCannotResetADrainedPairOrSource(t *testing.T) {
	const (
		victim       = "victim[at]example.test"
		victimSource = "198.51.100.7"
	)
	g, _ := newEnforceGate(t)
	clock := gateEpoch
	g.now = func() time.Time { return clock }
	e := loginHandlerForTest(t, g, 2)

	// The victim pair is drained first, and then the rest of the victim
	// source's budget, each at its own instant: the pair is strictly the least
	// recently seen entry in the pair map and the source strictly the least
	// recently seen in the source map.
	for i := 0; i < loginPairBudget; i++ {
		assertAdmitted(t, postLogin(e, victimSource, victim), "draining the pair")
	}
	clock = gateEpoch.Add(500 * time.Millisecond)
	for i := loginPairBudget; i < loginSrcBudget; i++ {
		assertAdmitted(t, postLogin(e, victimSource, fmt.Sprintf("drain%d[at]example.test", i)), "draining the source")
	}
	pairKey := srcKeyFor(netip.MustParseAddr(victimSource)) + "|" + g.AccountDigest(victim)
	srcKey := srcKeyFor(netip.MustParseAddr(victimSource))
	pairBk, srcBk := bucketOf(g.pair, pairKey), bucketOf(g.src, srcKey)
	if pairBk == nil || srcBk == nil {
		t.Fatal("precondition: the victim pair and source have no entries")
	}

	// One failed attempt from each of twice the cap's worth of new sources,
	// each against its own account. Each leaves its pair a token short and its
	// source a token short: every one holds more tokens than the victims.
	clock = gateEpoch.Add(time.Second)
	fillers := loginBucketCap * 2
	for i := 0; i < fillers; i++ {
		assertAdmitted(t, postLogin(e, fillerAddr(i), fmt.Sprintf("filler%d[at]example.test", i)), "a filler")
	}
	at := clock

	// Positive control: the maps really were pushed past the cap, so evictions
	// ran and this is not a test of a map that never filled.
	for _, b := range []*keyedBudget{g.pair, g.src} {
		if got := permanentCount(b); got != loginBucketCap {
			t.Fatalf("%s holds %d permanent entries after %d fillers, want exactly the cap %d", b.scope, got, fillers, loginBucketCap)
		}
	}

	for _, v := range []struct {
		b       *keyedBudget
		key     string
		bk      *loginBucket
		drained time.Time
		limit   int
		spent   int
	}{
		{g.pair, pairKey, pairBk, gateEpoch, loginPairBudget, loginPairBudget},
		{g.src, srcKey, srcBk, gateEpoch.Add(500 * time.Millisecond), loginSrcBudget, loginSrcBudget},
	} {
		if got := bucketOf(v.b, v.key); got != v.bk {
			t.Errorf("%s: the drained victim entry was evicted (present=%v): it would come back with its whole budget", v.b.scope, got != nil)
			continue
		}
		// Refill alone, from the instant it was drained.
		want := at.Sub(v.drained).Seconds() * float64(v.limit) / loginWindow.Seconds()
		if v.b == g.src {
			// The source's first ten tokens went at gateEpoch, the rest half
			// a second later with that half second's refill still in it.
			want = float64(v.limit-v.spent) + 0.5*float64(v.limit)/loginWindow.Seconds() + at.Sub(v.drained).Seconds()*float64(v.limit)/loginWindow.Seconds()
		}
		if got := v.b.tokensAt(v.key, at); math.Abs(got-want) > 1e-9 || got >= 1 {
			t.Errorf("%s: victim holds %.4f tokens, want %.4f from refill alone", v.b.scope, got, want)
		}
	}

	// The pair waits longer than the source (a budget a sixth the size,
	// drained earlier), so a refusal on "source" means the pair came back.
	w := postLogin(e, victimSource, victim)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("the victim pair was admitted (%d) after failed attempts from %d sources pushed the map past its cap", w.Code, fillers)
	}
	if r := decodeRefusal(t, w); r.Details.Scope != "pair" {
		t.Fatalf("victim refused on %q, want pair: the drained pair was evicted and came back full", r.Details.Scope)
	}
	w = postLogin(e, victimSource, "someone-new[at]example.test")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("the drained source was admitted (%d) after failed attempts from %d sources pushed the map past its cap", w.Code, fillers)
	}
	if r := decodeRefusal(t, w); r.Details.Scope != "source" {
		t.Errorf("drained source refused on %q, want source", r.Details.Scope)
	}
}

// TestCapEvictsTheFullestEntryCountingRefill: the tokens compared are the
// tokens at the moment of eviction, refill included. An entry drained long ago
// has refilled past a recently drained entry and past entries holding two
// tokens, so it is the one evicted, even though its stored count is the lowest
// in the map.
func TestCapEvictsTheFullestEntryCountingRefill(t *testing.T) {
	const limit = 10
	b := newKeyedBudget("test", limit)
	t0 := time.Unix(1_800_000_000, 0)

	drainedKept(t, b, "old", t0) // 0 stored, as of t0
	now := t0.Add(5 * time.Minute)
	drainedKept(t, b, "recent", now) // 0 stored, as of now
	// Every other entry keeps two tokens, stored at now.
	for i := 0; i < loginBucketCap-2; i++ {
		keptCharges(t, b, fmt.Sprintf("filler-%d", i), now, limit-2)
	}
	if got := permanentCount(b); got != loginBucketCap {
		t.Fatalf("precondition: %d permanent entries, want the cap %d", got, loginBucketCap)
	}
	oldAt := b.tokensAt("old", now)
	if oldAt <= 2 || oldAt >= limit {
		t.Fatalf("precondition: old holds %.3f tokens at now, want between 2 and %d", oldAt, limit)
	}

	keptCharge(t, b, "trigger", now)

	if bucketOf(b, "old") != nil {
		t.Errorf("old holds %.3f tokens and was kept; an entry holding fewer was evicted in its place", oldAt)
	}
	if bucketOf(b, "recent") == nil {
		t.Error("recent, drained, was evicted while old held more tokens")
	}
	if got := permanentCount(b); got != loginBucketCap {
		t.Errorf("%d permanent entries after the eviction, want %d", got, loginBucketCap)
	}
}

// TestCapEvictionTieBreaksOnLeastRecentlySeen: among entries holding equally
// many tokens, the one seen least recently goes first. Entries refilled to
// their whole budget are the exact tie.
func TestCapEvictionTieBreaksOnLeastRecentlySeen(t *testing.T) {
	b := newKeyedBudget("test", 10)
	t0 := time.Unix(1_800_000_000, 0)

	keptCharge(t, b, "earlier", t0)
	keptCharge(t, b, "later", t0.Add(time.Minute))
	// Both have refilled to the whole budget by now, and neither is idle.
	now := t0.Add(loginWindow + 5*time.Minute)
	for i := 0; i < loginBucketCap-2; i++ {
		drainedKept(t, b, fmt.Sprintf("filler-%d", i), now)
	}
	if got := permanentCount(b); got != loginBucketCap {
		t.Fatalf("precondition: %d permanent entries, want the cap %d", got, loginBucketCap)
	}
	if e, l := b.tokensAt("earlier", now), b.tokensAt("later", now); e != 10 || l != 10 {
		t.Fatalf("precondition: earlier %.3f and later %.3f tokens, want both 10", e, l)
	}

	keptCharge(t, b, "trigger-1", now)
	if bucketOf(b, "earlier") != nil || bucketOf(b, "later") == nil {
		t.Fatalf("first eviction: earlier present=%v, later present=%v; want earlier evicted, later kept",
			bucketOf(b, "earlier") != nil, bucketOf(b, "later") != nil)
	}
	keptCharge(t, b, "trigger-2", now)
	if bucketOf(b, "later") != nil {
		t.Error("second eviction kept later, the fullest entry left")
	}
}

// TestCapNeverEvictsTheNewEntryOrAProvisionalOne: the entry whose charge is
// being made permanent is never the one evicted, even when it is the fullest,
// and neither is an entry with only pending charges.
func TestCapNeverEvictsTheNewEntryOrAProvisionalOne(t *testing.T) {
	b := newKeyedBudget("test", 10)
	now := time.Unix(1_800_000_000, 0)

	for i := 0; i < loginBucketCap; i++ {
		drainedKept(t, b, fmt.Sprintf("filler-%d", i), now)
	}
	// Provisional entries, each holding 9 tokens: fuller than every
	// permanent entry.
	var provisional []*loginBucket
	for i := 0; i < 8; i++ {
		bk, ok := b.charge(fmt.Sprintf("in-flight-%d", i), now)
		if !ok {
			t.Fatal("could not charge a provisional entry")
		}
		provisional = append(provisional, bk)
	}

	// The new entry holds 9 tokens as well, fuller than every other
	// permanent entry.
	keptCharge(t, b, "new", now)

	if bucketOf(b, "new") == nil {
		t.Error("the entry just made permanent was evicted")
	}
	for i, bk := range provisional {
		if got := bucketOf(b, fmt.Sprintf("in-flight-%d", i)); got != bk {
			t.Errorf("provisional entry in-flight-%d was evicted", i)
		}
	}
	if got := permanentCount(b); got != loginBucketCap {
		t.Errorf("%d permanent entries, want %d: a drained filler should have gone", got, loginBucketCap)
	}
	if got := b.size(); got != loginBucketCap+len(provisional) {
		t.Errorf("map holds %d entries, want %d permanent plus %d provisional", got, loginBucketCap, len(provisional))
	}
}
