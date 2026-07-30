package blobstore

// The progress bound on large transfers.
//
// Dropping the whole-request Timeout from presignStreamClient fixed the
// truncation of a slow-but-moving transfer and, on its own, left an
// alive-but-silent peer able to hold a transfer open forever. These tests pin
// both halves of the replacement: a transfer that keeps moving is never cut off,
// and a transfer that stops moving is torn down.
//
// Every one of them uses a REAL HTTP server and a REAL body, because the
// property lives in the client and the guard, not in the store's bookkeeping.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newStallTestStore is newTestStore with a compressed stall window, so these
// tests measure the mechanism in a fraction of a second instead of sleeping
// through the production 20s.
func newStallTestStore(t *testing.T, endpoint string, stall time.Duration) *Store {
	t.Helper()
	store, err := New(Config{
		Bucket:             "test-bucket",
		Endpoint:           endpoint,
		Region:             "us-east-1",
		AccessKey:          "test-access-key",
		SecretKey:          "test-secret-key",
		ForcePathStyle:     true,
		StreamStallTimeout: stall,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return store
}

// TestStreamStallTimeoutDefault pins the default window and that Config can
// compress it. The value is the one bound standing between a wedged peer and an
// indefinitely held control-plane request.
func TestStreamStallTimeoutDefault(t *testing.T) {
	if StreamStallTimeout <= 0 {
		t.Fatal("StreamStallTimeout must be positive: it is the only bound on a body with no whole-request cap")
	}
	s := newStallTestStore(t, "http://127.0.0.1:9000", 0)
	if s.stallTimeout != StreamStallTimeout {
		t.Errorf("zero Config.StreamStallTimeout gave %s, want the %s default", s.stallTimeout, StreamStallTimeout)
	}
	s = newStallTestStore(t, "http://127.0.0.1:9000", 250*time.Millisecond)
	if s.stallTimeout != 250*time.Millisecond {
		t.Errorf("stallTimeout = %s, want the configured 250ms", s.stallTimeout)
	}
}

// TestGetStreamViaPresign_SilentPeerIsTornDown is the reproduction of the defect
// the uncapped streaming client introduced.
//
// The storage peer here is ALIVE: it completes the TLS-less handshake, answers
// with headers and one chunk, and then never sends another byte while keeping
// the socket open. ResponseHeaderTimeout is already spent, the Dialer keepalive
// sees a perfectly healthy socket, and the caller's context is not cancelled
// because on the real path it belongs to a site that has not disconnected.
// Before the progress bound, this read never returned.
func TestGetStreamViaPresign_SilentPeerIsTornDown(t *testing.T) {
	const stall = 300 * time.Millisecond

	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1048576")
		_, _ = w.Write(make([]byte, 16<<10))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release // alive, connected, and silent forever
	}))
	defer func() {
		close(release)
		server.Close()
	}()

	store := newStallTestStore(t, server.URL, stall)

	// Deliberately context.Background(): the point is that NOTHING outside the
	// transfer bounds it.
	rc, err := store.GetStreamViaPresign(context.Background(), "agent-releases/1.0.0/wpmgr-agent.zip")
	if err != nil {
		t.Fatalf("GetStreamViaPresign: %v", err)
	}
	defer func() { _ = rc.Close() }()

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, rerr := io.ReadAll(rc)
		done <- rerr
	}()

	select {
	case rerr := <-done:
		if rerr == nil {
			t.Fatal("a silent peer's body read succeeded; this test no longer reproduces the stall")
		}
		if !errors.Is(rerr, ErrStreamStalled) {
			t.Fatalf("read failed with %v, want ErrStreamStalled so the cause is not mistaken for a caller cancellation", rerr)
		}
		if elapsed := time.Since(start); elapsed > 10*stall {
			t.Fatalf("stall detected after %s, want roughly %s", elapsed, stall)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a silent peer held the read open indefinitely: the transfer has no progress bound")
	}
}

// TestGetStreamViaPresign_SteadyTrickleIsNeverCutOff is the other half. The peer
// here is slower than the stall window is long in TOTAL, but it never pauses for
// a whole window, which is exactly the shape of the shared-hosting site this
// whole change exists to keep working. Every byte must arrive.
func TestGetStreamViaPresign_SteadyTrickleIsNeverCutOff(t *testing.T) {
	const (
		stall      = 200 * time.Millisecond
		objectSize = 128 << 10
		chunk      = 8 << 10
	)
	object := make([]byte, objectSize)
	for i := range object {
		object[i] = byte(i % 251)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "131072")
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
			// Well inside the window, but 16 of these add up to more than one
			// whole window: a duration cap would have killed this transfer.
			time.Sleep(40 * time.Millisecond)
		}
	}))
	defer server.Close()

	store := newStallTestStore(t, server.URL, stall)
	rc, err := store.GetStreamViaPresign(context.Background(), "agent-releases/1.0.0/wpmgr-agent.zip")
	if err != nil {
		t.Fatalf("GetStreamViaPresign: %v", err)
	}
	defer func() { _ = rc.Close() }()

	start := time.Now()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("a steadily trickling transfer was cut off at %d of %d bytes: %v", len(got), objectSize, err)
	}
	if !bytes.Equal(got, object) {
		t.Fatalf("read %d bytes, want the whole %d-byte object", len(got), objectSize)
	}
	if elapsed := time.Since(start); elapsed <= stall {
		t.Fatalf("the transfer took %s, which is inside one %s window; it no longer proves a slow transfer survives", elapsed, stall)
	}
}

// TestPutViaPresign_SilentPeerIsTornDown covers the WRITE path, which the same
// change moved onto the uncapped client. Four of its callers run under River
// jobs that carry a job timeout, but the RUCSS ingest stash runs on the agent's
// request path, so without this bound a storage backend that accepts a
// connection and then stops reading pins a request-path goroutine indefinitely.
func TestPutViaPresign_SilentPeerIsTornDown(t *testing.T) {
	const (
		stall      = 300 * time.Millisecond
		objectSize = 8 << 20 // larger than any socket buffer, so the write blocks
	)

	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // accept the connection, never read the body, never answer
	}))
	defer func() {
		close(release)
		server.Close()
	}()

	store := newStallTestStore(t, server.URL, stall)

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- store.PutViaPresign(context.Background(),
			"agent-releases/1.0.0/wpmgr-agent.zip", bytes.NewReader(make([]byte, objectSize)), objectSize)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an upload to a peer that never read the body reported success")
		}
		if !errors.Is(err, ErrStreamStalled) {
			t.Fatalf("upload failed with %v, want ErrStreamStalled", err)
		}
		if elapsed := time.Since(start); elapsed > 10*stall {
			t.Fatalf("stall detected after %s, want roughly %s", elapsed, stall)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a silent peer held the upload open indefinitely: the write path has no progress bound")
	}
}

// TestPutViaPresign_SteadyTrickleIsNeverCutOff: an upload that is slow but
// moving keeps its full run. This is the property the 60s whole-request cap
// broke and that a naive re-add of any duration cap would break again.
func TestPutViaPresign_SteadyTrickleIsNeverCutOff(t *testing.T) {
	const (
		stall      = 200 * time.Millisecond
		objectSize = 256 << 10
	)

	var received int64
	drained := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 16<<10)
		for {
			n, err := r.Body.Read(buf)
			received += int64(n)
			if err != nil {
				break
			}
			time.Sleep(40 * time.Millisecond)
		}
		close(drained)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	store := newStallTestStore(t, server.URL, stall)
	start := time.Now()
	if err := store.PutViaPresign(context.Background(),
		"agent-releases/1.0.0/wpmgr-agent.zip", bytes.NewReader(make([]byte, objectSize)), objectSize); err != nil {
		t.Fatalf("a steadily progressing upload was cut off: %v", err)
	}
	<-drained
	if received != objectSize {
		t.Fatalf("server received %d bytes, want %d", received, objectSize)
	}
	if elapsed := time.Since(start); elapsed <= stall {
		t.Fatalf("the upload took %s, inside one %s window; it no longer proves a slow upload survives", elapsed, stall)
	}
}

// TestGuardRequestBody_KeepsTheRequestReplayable pins the ordering trap in
// guarding an upload.
//
// net/http replays an idempotent request whose reused keep-alive connection died
// before any response arrived, which is routine against object storage, and it
// can only do that when GetBody survives. Handing http.NewRequest a wrapper
// instead of wrapping afterwards suppresses GetBody (and ContentLength) and
// quietly turns those transparent retries into upload failures.
func TestGuardRequestBody_KeepsTheRequestReplayable(t *testing.T) {
	const payload = "the object bytes"

	req, err := http.NewRequest(http.MethodPut, "http://storage.example.test/o", bytes.NewReader([]byte(payload)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if req.GetBody == nil {
		t.Fatal("precondition: a *bytes.Reader body should give the stdlib a GetBody")
	}
	if req.ContentLength != int64(len(payload)) {
		t.Fatalf("precondition: ContentLength = %d, want %d", req.ContentLength, len(payload))
	}

	guard := NewStallGuard(func() {}, time.Minute)
	defer guard.Stop()
	guardRequestBody(req, guard)

	if req.GetBody == nil {
		t.Fatal("guarding the body dropped GetBody: an idempotent PUT can no longer be replayed on a stale connection")
	}
	if req.ContentLength != int64(len(payload)) {
		t.Fatalf("ContentLength = %d after guarding, want %d", req.ContentLength, len(payload))
	}
	if _, ok := req.Body.(*guardedRequestBody); !ok {
		t.Fatalf("req.Body is %T, want the guarded wrapper", req.Body)
	}

	// The first attempt runs the body to EOF, which stops the guard. A replay
	// must come back guarded AND re-armed, or the retry would upload with no
	// progress bound at all.
	if _, err := io.ReadAll(req.Body); err != nil {
		t.Fatalf("read guarded body: %v", err)
	}
	replay, err := req.GetBody()
	if err != nil {
		t.Fatalf("GetBody: %v", err)
	}
	if _, ok := replay.(*guardedRequestBody); !ok {
		t.Fatalf("replayed body is %T, want the guarded wrapper", replay)
	}
	if !guard.Armed() {
		t.Fatal("the guard was not re-armed for the replayed body")
	}
	got, err := io.ReadAll(replay)
	if err != nil {
		t.Fatalf("read replayed body: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("replayed body = %q, want %q", got, payload)
	}
}

// TestPutViaPresign_SmallObjectWaitingForAResponseIsNotAStall: once the body is
// fully written the guard must stand down, because the transport is then
// legitimately waiting for the response and that wait is ResponseHeaderTimeout's
// job. Without the EOF handoff, every small upload to a slow-to-answer backend
// would fail one stall window after its last byte.
func TestPutViaPresign_SmallObjectWaitingForAResponseIsNotAStall(t *testing.T) {
	const stall = 150 * time.Millisecond

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		// Answer well after the stall window: a healthy backend under load.
		time.Sleep(4 * stall)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	store := newStallTestStore(t, server.URL, stall)
	if err := store.PutViaPresign(context.Background(),
		"agent-releases/latest.json", bytes.NewReader([]byte(`{"version":"1.0.0"}`)), 19); err != nil {
		t.Fatalf("a slow RESPONSE was treated as a stalled BODY: %v", err)
	}
}
