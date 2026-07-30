package blobstore

// blobstore_test.go — ADR-036 P1 (GH #146 security review) unit tests for
// Store.PathPrefix: an s3_compat destination's configured key prefix must be
// applied CONSISTENTLY to both the backup PUT and the restore GET presign, so
// the two sides always agree on the same effective object key. Presigning is
// computed entirely offline (SigV4 needs no network round trip), so these
// tests need no real S3 endpoint.

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStore_PresignPutAndGet_ApplyPathPrefixConsistently(t *testing.T) {
	store, err := New(Config{
		Bucket:         "test-bucket",
		Endpoint:       "http://127.0.0.1:9000",
		ForcePathStyle: true,
		AccessKey:      "test-access-key",
		SecretKey:      "test-secret-key",
		Region:         "us-east-1",
		PathPrefix:     "/clientA/backups/",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if store.PathPrefix() != "clientA/backups" {
		t.Fatalf("PathPrefix() = %q, want %q (leading/trailing slashes normalised)", store.PathPrefix(), "clientA/backups")
	}

	putURL, err := store.PresignPut(context.Background(), "chunks/tenant-x/hash123", 5*time.Minute)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	getURL, err := store.PresignGet(context.Background(), "chunks/tenant-x/hash123", 5*time.Minute)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}

	const wantSegment = "/test-bucket/clientA/backups/chunks/tenant-x/hash123"
	if !strings.Contains(putURL, wantSegment) {
		t.Errorf("PresignPut URL = %s, want to contain %q", putURL, wantSegment)
	}
	if !strings.Contains(getURL, wantSegment) {
		t.Errorf("PresignGet URL = %s, want to contain %q", getURL, wantSegment)
	}

	// Backup PUT and restore GET must agree on the SAME effective key (the
	// path component; the query string legitimately differs — different verb,
	// different signature/expiry).
	putPath := strings.SplitN(putURL, "?", 2)[0]
	getPath := strings.SplitN(getURL, "?", 2)[0]
	if putPath != getPath {
		t.Errorf("PUT and GET presign paths differ: put=%q get=%q", putPath, getPath)
	}
}

func TestStore_PresignPut_NoPathPrefix_BucketRootUnchanged(t *testing.T) {
	store, err := New(Config{
		Bucket:         "test-bucket",
		Endpoint:       "http://127.0.0.1:9000",
		ForcePathStyle: true,
		AccessKey:      "test-access-key",
		SecretKey:      "test-secret-key",
		Region:         "us-east-1",
		// PathPrefix intentionally empty — the CP-global default Store's
		// pre-existing, unchanged behaviour.
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if store.PathPrefix() != "" {
		t.Fatalf("PathPrefix() = %q, want empty (managed/legacy default)", store.PathPrefix())
	}

	url, err := store.PresignPut(context.Background(), "chunks/tenant-x/hash123", 5*time.Minute)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	const wantSegment = "/test-bucket/chunks/tenant-x/hash123"
	if !strings.Contains(url, wantSegment) {
		t.Errorf("PresignPut URL = %s, want to contain %q (bucket-root, no prefix)", url, wantSegment)
	}
	// The prefixed segment (with any client-side prefix) must NOT appear —
	// belt-and-suspenders against an accidental leading-slash double segment.
	if strings.Contains(url, "//chunks") {
		t.Errorf("PresignPut URL has a spurious double slash: %s", url)
	}
}

// ---------------------------------------------------------------------------
// Large-object transfer posture (H2 / H1b).
//
// These run against a real HTTP server and a real response body, because the
// defect they pin lives entirely in the HTTP client configuration: any in-memory
// double reports success no matter which client the store uses.
// ---------------------------------------------------------------------------

// newTestStore points a Store at a local test server. Presigning is offline, so
// the server only ever has to answer the request the store then makes.
func newTestStore(t *testing.T, endpoint string) *Store {
	t.Helper()
	store, err := New(Config{
		Bucket:         "test-bucket",
		Endpoint:       endpoint,
		Region:         "us-east-1",
		AccessKey:      "test-access-key",
		SecretKey:      "test-secret-key",
		ForcePathStyle: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return store
}

// TestClientTimeoutPosture pins the three clients' whole-request bounds, which
// is the whole point of the split. The small-object client keeps its short,
// decisive cap; the streaming client must have NO whole-request cap, because
// http.Client.Timeout covers reading the response body and would therefore let a
// slow consumer kill the storage read mid-object.
func TestClientTimeoutPosture(t *testing.T) {
	if presignFetchClient.Timeout != 15*time.Second {
		t.Errorf("presignFetchClient.Timeout = %s, want 15s (callers depend on a short bound)", presignFetchClient.Timeout)
	}
	if presignStreamClient.Timeout != 0 {
		t.Errorf("presignStreamClient.Timeout = %s, want 0: a whole-request cap truncates a large body", presignStreamClient.Timeout)
	}
	if streamTransport.ResponseHeaderTimeout <= 0 {
		t.Error("streamTransport needs a ResponseHeaderTimeout: no whole-request cap must not mean no bound at all")
	}
	if streamTransport.TLSHandshakeTimeout <= 0 {
		t.Error("streamTransport needs a TLSHandshakeTimeout")
	}
}

// TestGetStreamViaPresign_SurvivesASlowConsumer is the H2 reproduction. The same
// object, the same server, the same slow consumer: the small-object read dies
// part-way through the body with a deadline error, and the streaming read
// delivers every byte.
//
// The small-object client's real cap is 15s, which at a few MiB works out to a
// floor of roughly 200 KB/s from the consumer. This test shrinks the cap so the
// same arithmetic plays out in a fraction of a second instead of forcing a
// 15-second test; the failure mode reproduced is identical.
func TestGetStreamViaPresign_SurvivesASlowConsumer(t *testing.T) {
	const objectSize = 512 << 10 // 512 KiB
	object := make([]byte, objectSize)
	for i := range object {
		object[i] = byte(i % 251)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "524288")
		const chunk = 16 << 10
		for off := 0; off < len(object); off += chunk {
			end := off + chunk
			if end > len(object) {
				end = len(object)
			}
			if _, err := w.Write(object[off:end]); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer server.Close()

	store := newTestStore(t, server.URL)

	// A consumer that drains at roughly 512 KiB/s: 16 reads of 32 KiB, 30ms
	// apart, so the transfer needs about half a second of wall clock.
	drainSlowly := func(rc io.ReadCloser) ([]byte, error) {
		defer func() { _ = rc.Close() }()
		var got []byte
		buf := make([]byte, 32<<10)
		for {
			n, err := rc.Read(buf)
			got = append(got, buf[:n]...)
			if err == io.EOF {
				return got, nil
			}
			if err != nil {
				return got, err
			}
			time.Sleep(30 * time.Millisecond)
		}
	}

	// THE DEFECT. Same object, same consumer, on the whole-request-capped client.
	restore := presignFetchClient.Timeout
	presignFetchClient.Timeout = 200 * time.Millisecond
	rc, err := store.GetViaPresign(context.Background(), "agent-releases/1.0.0/wpmgr-agent.zip")
	if err != nil {
		t.Fatalf("GetViaPresign returned headers fine, it is the body that fails: %v", err)
	}
	short, rerr := drainSlowly(rc)
	presignFetchClient.Timeout = restore
	if rerr == nil && len(short) == objectSize {
		t.Fatal("the capped client delivered the whole body; this test no longer reproduces H2")
	}
	if len(short) >= objectSize {
		t.Fatalf("capped read returned %d bytes, expected a truncated body", len(short))
	}
	t.Logf("capped read stopped at %d of %d bytes: %v", len(short), objectSize, rerr)

	// THE FIX. No whole-request cap, so the consumer's pace decides nothing but
	// how long it takes.
	src, err := store.GetStreamViaPresign(context.Background(), "agent-releases/1.0.0/wpmgr-agent.zip")
	if err != nil {
		t.Fatalf("GetStreamViaPresign: %v", err)
	}
	got, err := drainSlowly(src)
	if err != nil {
		t.Fatalf("streaming read failed at %d of %d bytes: %v", len(got), objectSize, err)
	}
	if !bytes.Equal(got, object) {
		t.Fatalf("streaming read returned %d bytes, want the whole %d-byte object", len(got), objectSize)
	}
}

// TestGetStreamViaPresign_ContextIsTheBound: dropping the whole-request timeout
// must not mean the transfer is unbounded. The caller's context still tears it
// down, which on the download route is the site's own connection going away.
func TestGetStreamViaPresign_ContextIsTheBound(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1048576")
		_, _ = w.Write(make([]byte, 16<<10))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release // stall mid-body, exactly like a wedged backend
	}))
	defer func() {
		close(release)
		server.Close()
	}()

	store := newTestStore(t, server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	rc, err := store.GetStreamViaPresign(ctx, "agent-releases/1.0.0/wpmgr-agent.zip")
	if err != nil {
		t.Fatalf("GetStreamViaPresign: %v", err)
	}
	defer func() { _ = rc.Close() }()
	if _, err := io.ReadAll(rc); err == nil {
		t.Fatal("a stalled body outlived its context; the context must bound the transfer")
	}
}

func TestGetStreamViaPresign_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	store := newTestStore(t, server.URL)
	if _, err := store.GetStreamViaPresign(context.Background(), "agent-releases/nope.zip"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// TestPutViaPresign_SlowUploadIsNotCutOff is the H1b regression test. The upload
// client used to cap the WHOLE request, body included, at 60s, so an object near
// the size cap needed a sustained upload rate the control plane cannot promise
// itself, and missing it aborted a write that was making perfectly good progress.
// The bound is now the caller's context (proved below), not a fixed wall clock.
func TestPutViaPresign_SlowUploadIsNotCutOff(t *testing.T) {
	const objectSize = 256 << 10

	// Atomic: the counter is written on the server's goroutine and read on the
	// test's, with only a TCP round trip in between.
	var received atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read the body slowly, the way a congested link to storage behaves.
		buf := make([]byte, 16<<10)
		for {
			n, err := r.Body.Read(buf)
			received.Add(int64(n))
			if err != nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	store := newTestStore(t, server.URL)
	body := bytes.NewReader(make([]byte, objectSize))
	if err := store.PutViaPresign(context.Background(), "agent-releases/1.0.0/wpmgr-agent.zip", body, objectSize); err != nil {
		t.Fatalf("PutViaPresign: %v", err)
	}
	if got := received.Load(); got != objectSize {
		t.Fatalf("server received %d bytes, want %d", got, objectSize)
	}
}

// TestPutViaPresign_ContextIsTheBound: the write path keeps a bound, it is just
// the caller's context now rather than a fixed 60s that a large object could
// legitimately need to exceed.
func TestPutViaPresign_ContextIsTheBound(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // never answer
	}))
	defer func() {
		close(release)
		server.Close()
	}()

	store := newTestStore(t, server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := store.PutViaPresign(ctx, "agent-releases/1.0.0/wpmgr-agent.zip", bytes.NewReader([]byte("x")), 1)
	if err == nil {
		t.Fatal("a wedged upload outlived its context; the context must bound the write")
	}
}
