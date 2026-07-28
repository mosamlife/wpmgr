package agentrelease

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// fakeStore is a minimal Store test double. get is called once per
// GetViaPresign invocation so tests can assert how many times the reader
// actually hit "storage".
type fakeStore struct {
	body  string
	err   error
	calls int
}

func (f *fakeStore) GetViaPresign(_ context.Context, key string) (io.ReadCloser, error) {
	f.calls++
	if key != ManifestKey {
		return nil, errors.New("unexpected key: " + key)
	}
	if f.err != nil {
		return nil, f.err
	}
	return io.NopCloser(strings.NewReader(f.body)), nil
}

func TestReader_LatestVersion_NilStoreDegradesToUnknown(t *testing.T) {
	r := NewReader(nil, 0)
	if got := r.LatestVersion(context.Background()); got != "" {
		t.Errorf("LatestVersion() with nil store = %q; want empty (unknown)", got)
	}
}

func TestReader_LatestVersion_StoreErrorDegradesToUnknown(t *testing.T) {
	store := &fakeStore{err: errors.New("simulated: object storage unreachable")}
	r := NewReader(store, time.Minute)
	if got := r.LatestVersion(context.Background()); got != "" {
		t.Errorf("LatestVersion() on store error = %q; want empty (unknown)", got)
	}
}

func TestReader_LatestVersion_MalformedJSONDegradesToUnknown(t *testing.T) {
	store := &fakeStore{body: "{not json"}
	r := NewReader(store, time.Minute)
	if got := r.LatestVersion(context.Background()); got != "" {
		t.Errorf("LatestVersion() on malformed manifest = %q; want empty (unknown)", got)
	}
}

func TestReader_LatestVersion_ReadsPublishedVersion(t *testing.T) {
	store := &fakeStore{body: `{"version":"0.61.95"}`}
	r := NewReader(store, time.Minute)
	if got := r.LatestVersion(context.Background()); got != "0.61.95" {
		t.Errorf("LatestVersion() = %q; want %q", got, "0.61.95")
	}
}

// TestReader_LatestVersion_CachesWithinTTL proves a second call inside the
// TTL window does not hit the store again.
func TestReader_LatestVersion_CachesWithinTTL(t *testing.T) {
	store := &fakeStore{body: `{"version":"0.61.95"}`}
	r := NewReader(store, time.Hour)
	ctx := context.Background()

	if got := r.LatestVersion(ctx); got != "0.61.95" {
		t.Fatalf("first LatestVersion() = %q; want %q", got, "0.61.95")
	}
	if got := r.LatestVersion(ctx); got != "0.61.95" {
		t.Fatalf("second LatestVersion() = %q; want %q", got, "0.61.95")
	}
	if store.calls != 1 {
		t.Errorf("store.calls = %d; want 1 (cached within TTL)", store.calls)
	}
}

// TestReader_LatestVersion_RefetchesAfterTTL proves an expired cache entry
// is re-fetched rather than served stale forever.
func TestReader_LatestVersion_RefetchesAfterTTL(t *testing.T) {
	store := &fakeStore{body: `{"version":"0.61.95"}`}
	r := NewReader(store, time.Millisecond)
	ctx := context.Background()

	if got := r.LatestVersion(ctx); got != "0.61.95" {
		t.Fatalf("first LatestVersion() = %q; want %q", got, "0.61.95")
	}
	time.Sleep(5 * time.Millisecond)
	if got := r.LatestVersion(ctx); got != "0.61.95" {
		t.Fatalf("second LatestVersion() = %q; want %q", got, "0.61.95")
	}
	if store.calls != 2 {
		t.Errorf("store.calls = %d; want 2 (re-fetched after TTL expiry)", store.calls)
	}
}

// scriptedStore serves a different outcome per call, so a test can stage a
// transient blip: fail on the listed 1-based call numbers, serve body on every
// other call.
// failFrom, when positive, additionally fails every call from that 1-based
// number onward: storage that breaks and stays broken, as opposed to a blip.
type scriptedStore struct {
	body      string
	failCalls map[int]bool
	failFrom  int
	calls     int
}

func (s *scriptedStore) GetViaPresign(_ context.Context, key string) (io.ReadCloser, error) {
	s.calls++
	if key != ManifestKey {
		return nil, errors.New("unexpected key: " + key)
	}
	if s.failCalls[s.calls] || (s.failFrom > 0 && s.calls >= s.failFrom) {
		return nil, errors.New("simulated: transient presign failure")
	}
	return io.NopCloser(strings.NewReader(s.body)), nil
}

// TestReader_LatestVersion_FailureServesLastKnownGood is the core of the
// regression: a blip after a good read must return the good read, not "".
// Serving "" hands the fleet rollup an unusable reference, which it then
// replaces with a fleet-derived one, turning a visible degradation into a
// plausible wrong answer.
func TestReader_LatestVersion_FailureServesLastKnownGood(t *testing.T) {
	store := &scriptedStore{body: `{"version":"0.62.0"}`, failCalls: map[int]bool{2: true, 3: true}}
	r := NewReaderWithNegativeTTL(store, 5*time.Millisecond, time.Millisecond)
	ctx := context.Background()

	if got := r.LatestVersion(ctx); got != "0.62.0" {
		t.Fatalf("first LatestVersion() = %q; want %q", got, "0.62.0")
	}
	time.Sleep(20 * time.Millisecond)
	if got := r.LatestVersion(ctx); got != "0.62.0" {
		t.Errorf("LatestVersion() during a blip = %q; want the last known good %q", got, "0.62.0")
	}
	time.Sleep(20 * time.Millisecond)
	if got := r.LatestVersion(ctx); got != "0.62.0" {
		t.Errorf("LatestVersion() during a second blip = %q; want the last known good %q", got, "0.62.0")
	}
	if store.calls < 3 {
		t.Errorf("store.calls = %d; want at least 3 (each expiry retried)", store.calls)
	}
}

// TestReader_LatestVersion_FailureUsesShortNegativeTTL proves a failed read is
// held for the negative TTL, not the full success TTL: with an hour-long
// success TTL, the very next call after the negative window must retry rather
// than serve the failure for the hour.
func TestReader_LatestVersion_FailureUsesShortNegativeTTL(t *testing.T) {
	store := &scriptedStore{body: `{"version":"0.62.0"}`, failCalls: map[int]bool{1: true}}
	r := NewReaderWithNegativeTTL(store, time.Hour, 5*time.Millisecond)
	ctx := context.Background()

	if got := r.LatestVersion(ctx); got != "" {
		t.Fatalf("first LatestVersion() = %q; want empty (nothing has ever been read)", got)
	}
	time.Sleep(20 * time.Millisecond)
	if got := r.LatestVersion(ctx); got != "0.62.0" {
		t.Errorf("LatestVersion() after the negative TTL = %q; want %q (a blip must not be cached for the success TTL)", got, "0.62.0")
	}
	if store.calls != 2 {
		t.Errorf("store.calls = %d; want 2 (retried once the negative TTL expired)", store.calls)
	}
}

// TestReader_NegativeTTLNeverExceedsTTL pins the invariant that a failure is
// never cached longer than a success, however the Reader is constructed.
func TestReader_NegativeTTLNeverExceedsTTL(t *testing.T) {
	if r := NewReader(nil, time.Second); r.negativeTTL > r.ttl {
		t.Errorf("NewReader negativeTTL = %v; must not exceed ttl = %v", r.negativeTTL, r.ttl)
	}
	if r := NewReaderWithNegativeTTL(nil, time.Second, time.Hour); r.negativeTTL != time.Second {
		t.Errorf("negativeTTL = %v; want it capped at ttl (1s)", r.negativeTTL)
	}
	if r := NewReader(nil, 0); r.negativeTTL != defaultNegativeTTL {
		t.Errorf("default negativeTTL = %v; want %v", r.negativeTTL, defaultNegativeTTL)
	}
}

// TestReader_TTLNeverExceedsMaxAge: a success TTL longer than the age bound
// would park the Reader on a value it is no longer willing to serve, returning
// unknown without re-reading until the TTL finally expired. Capping ttl keeps
// every expiry a retry.
func TestReader_TTLNeverExceedsMaxAge(t *testing.T) {
	if r := NewReaderWithLimits(nil, 2*time.Hour, time.Second, time.Hour); r.ttl != time.Hour {
		t.Errorf("ttl = %v; want it capped at maxLastKnownGood (1h)", r.ttl)
	}
	if r := NewReader(nil, 0); r.maxAge != maxLastKnownGoodAge {
		t.Errorf("default maxAge = %v; want %v", r.maxAge, maxLastKnownGoodAge)
	}
}

// TestReader_LastKnownGoodExpiresAtMaxAge is the bound on staleness. Storage
// serves the manifest once and is then broken permanently.
//
// Standing in for a live read is right while the good value is young: that is
// what stops one blip from handing the fleet rollup an unusable reference it
// would then replace with a fleet-derived one. It is wrong forever. Unbounded,
// this Reader reports 0.62.0 as the published version for the entire life of
// the process, so a release landing during the outage is never seen and the
// dashboard states a confident all-clear sourced from a channel it can no
// longer read. Past the bound the honest answer is unknown.
func TestReader_LastKnownGoodExpiresAtMaxAge(t *testing.T) {
	store := &scriptedStore{body: `{"version":"0.62.0"}`, failFrom: 2}
	r := NewReaderWithLimits(store, 5*time.Millisecond, time.Millisecond, 50*time.Millisecond)
	ctx := context.Background()

	if got := r.LatestVersion(ctx); got != "0.62.0" {
		t.Fatalf("first LatestVersion() = %q; want %q", got, "0.62.0")
	}
	time.Sleep(10 * time.Millisecond) // past the success TTL, well inside the bound
	if got := r.LatestVersion(ctx); got != "0.62.0" {
		t.Errorf("LatestVersion() inside the bound = %q; want the last known good %q", got, "0.62.0")
	}
	time.Sleep(60 * time.Millisecond) // past the bound
	if got := r.LatestVersion(ctx); got != "" {
		t.Errorf("LatestVersion() past the age bound = %q; want empty (a stale value must not stand in forever)", got)
	}
	// The channel demonstrably exists here, and that fact does not decay with
	// the value: it is what keeps the rollup on "none" rather than falling
	// through to a fleet-derived reference (see Service.referenceVersion).
	if !r.EverPublished() {
		t.Error("EverPublished() = false; a proven publish must stay proven")
	}
}

// TestReader_EverPublished distinguishes "this install has no release channel"
// (the self-hosted steady state) from "the channel this install does have was
// briefly unreadable". Only the first may unlock the fleet-derived fallback.
func TestReader_EverPublished(t *testing.T) {
	ctx := context.Background()

	never := NewReaderWithNegativeTTL(&scriptedStore{body: `{"version":"0.62.0"}`, failCalls: map[int]bool{1: true}}, time.Hour, time.Millisecond)
	if never.EverPublished() {
		t.Error("EverPublished() before any read = true; want false")
	}
	_ = never.LatestVersion(ctx)
	if never.EverPublished() {
		t.Error("EverPublished() after a failed read = true; want false")
	}

	garbage := NewReader(&fakeStore{body: `{"version":"not-a-version"}`}, time.Hour)
	_ = garbage.LatestVersion(ctx)
	if garbage.EverPublished() {
		t.Error("EverPublished() after reading a garbage version = true; want false (unusable is not published)")
	}

	published := NewReaderWithNegativeTTL(&scriptedStore{body: `{"version":"0.62.0"}`, failCalls: map[int]bool{2: true}}, 5*time.Millisecond, time.Millisecond)
	if got := published.LatestVersion(ctx); got != "0.62.0" {
		t.Fatalf("LatestVersion() = %q; want %q", got, "0.62.0")
	}
	if !published.EverPublished() {
		t.Fatal("EverPublished() after a successful read = false; want true")
	}
	time.Sleep(20 * time.Millisecond)
	_ = published.LatestVersion(ctx)
	if !published.EverPublished() {
		t.Error("EverPublished() after a later blip = false; want true (it stays true once proven)")
	}
}
