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
