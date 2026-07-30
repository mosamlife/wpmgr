package blobstore

// PROGRESS IS THE BOUND, NOT DURATION.
//
// The large-object client (presignStreamClient) deliberately has no
// whole-request Timeout, because http.Client.Timeout covers reading the response
// body: any fixed value turns "the peer is slow" into "the transfer died
// mid-object", which on the agent-package path reaches a WordPress site as a
// truncated zip it can never install.
//
// Removing that cap removed the only bound, and nothing replaced it. The
// transport's ResponseHeaderTimeout is already spent by the time the first body
// byte arrives and does not apply mid-body, and the Dialer keepalive only
// notices a socket whose peer has VANISHED, not one that is alive and silent. A
// storage backend that sends headers plus one chunk and then goes quiet held a
// control-plane request open indefinitely, and with the package route's
// per-instance stream cap that is a denial of the whole update channel: a
// handful of requests park on every slot and legitimate sites get 429 forever.
//
// The right dimension was never total duration. A large download is allowed to
// take a long time when it is MOVING; it must be cut off when it is not. That is
// what this file does: a watchdog that is restarted by every byte that moves and
// cancels the transfer's context when nothing moves for StreamStallTimeout.
//
// ONE WATCHDOG, BOTH HALVES. StallGuard is exported because the package-download
// route in internal/agent copies this reader straight onto a site's connection,
// and the WRITE half of that copy needs the same bound. It used to approximate
// it with a per-call socket write deadline, which is not the same mechanism and
// did not behave like one: a socket write deadline is absolute for the whole
// Write call, so bytes leaving inside that call do not extend it, and what the
// route actually enforced was "one 64 KiB chunk must land within the window"
// rather than "bytes must keep moving". Two things documented identically that
// behave differently is how that survived review, so there is now one
// implementation and both halves feed it the same way: every operation that
// moved bytes calls Progress, and the guard fires once when none has for the
// window.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// StreamStallTimeout is how long a large transfer may make NO progress at all
// before it is torn down. It is an INTER-BYTE bound, not a whole-request one: it
// is restarted by every read or write that moves bytes, so a transfer that keeps
// moving is never cut off no matter how long it takes in total.
//
// WHAT "MOVES BYTES" MEANS ON EACH HALF, because this is exactly where the write
// half used to differ. On the READ half, guardedReader calls Progress after
// every Read that returned n > 0, so the granularity is whatever the storage
// backend hands over. On the WRITE half (internal/agent, streamPackage) the
// socket write is issued under a short ATTEMPT deadline and Progress is called
// with the partial count that attempt moved, so the granularity is whatever the
// consumer accepted, NOT the copy buffer. Neither half imposes a rate: a
// consumer taking 1280 B every 50 ms and one taking 16 KiB every 640 ms are the
// same health at the same throughput and are now treated the same, which was
// measured and was not true before (see streamPackage).
//
// WHY 20 SECONDS. The behaviour it must never punish is a legitimately slow
// consumer, and a slow consumer is slow, not silent: the case this whole change
// exists for is a shared-hosting site pulling at roughly 150 KB/s, which hands
// over bytes every few milliseconds and takes ~21s for a 3 MiB package. Nothing
// healthy on either side of this transfer pauses for tens of seconds: object
// storage answers a range of an already-open body in milliseconds, and a TCP
// consumer that has not accepted a single byte for 20s has stopped consuming.
// So 20s sits about three orders of magnitude above normal inter-chunk latency
// and still bounds the damage of a wedged peer to 20s per slot instead of
// forever.
//
// WHY NOT SHORTER. A control plane under load can be descheduled, and a backend
// can hiccup on a retried range, for a few seconds; anything in the low single
// digits would start failing transfers that were about to succeed. 20s is also
// comfortably inside the agent's own 60s whole-operation download timeout (see
// the note in internal/agent/update_package_handler.go), so a stall we detect
// surfaces to the site as a clean failed download it retries next cycle, rather
// than as the agent timing out on its own.
const StreamStallTimeout = 20 * time.Second

// ErrStreamStalled is returned by a guarded transfer that made no progress for
// its stall window. It is deliberately distinct from a context cancellation so
// callers and logs can tell "the peer went quiet" apart from "the consumer went
// away".
var ErrStreamStalled = errors.New("blobstore: transfer stalled")

// StallGuard tears a transfer down when no bytes have moved for d.
//
// It is armed at construction, restarted by Progress, and stopped exactly once
// (by fire, by Stop, or by the reader hitting EOF). Stopping at EOF matters on
// the upload path: once the request body is fully written the transport is
// legitimately waiting for the response, which is what the transport's
// ResponseHeaderTimeout bounds, and a still-running progress watchdog would
// cancel a perfectly healthy upload.
//
// onStall is what actually unblocks whoever is parked on the socket, and it
// differs per half: the transfers in this package cancel the request context,
// and the package-download write half puts the connection's write deadline in
// the past. The guard does not care which; it only decides WHEN.
type StallGuard struct {
	mu      sync.Mutex
	timer   *time.Timer
	onStall func()
	d       time.Duration
	done    bool
	stalled atomic.Bool
}

// NewStallGuard arms a watchdog that calls onStall once, d after the last
// Progress. A non-positive d means the caller did not normalise its window and
// falls back to StreamStallTimeout rather than leaving the transfer unbounded.
func NewStallGuard(onStall func(), d time.Duration) *StallGuard {
	if d <= 0 {
		d = StreamStallTimeout
	}
	g := &StallGuard{onStall: onStall, d: d}
	g.timer = time.AfterFunc(d, g.fire)
	return g
}

// fire runs when the window elapsed with no progress.
func (g *StallGuard) fire() {
	g.mu.Lock()
	if g.done {
		g.mu.Unlock()
		return
	}
	g.done = true
	g.stalled.Store(true)
	g.mu.Unlock()
	g.onStall()
}

// Progress restarts the window. Called after every byte-moving operation, with
// the partial count where one is available.
func (g *StallGuard) Progress() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.done {
		return
	}
	g.timer.Reset(g.d)
}

// Stop disarms the watchdog for good. Safe to call repeatedly.
func (g *StallGuard) Stop() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.done {
		return
	}
	g.done = true
	g.timer.Stop()
}

// Restart re-arms a guard that was stopped, for a request body being REPLAYED.
// net/http replays an idempotent request whose connection died before a response
// arrived, and the previous attempt will usually have stopped this guard at EOF;
// without this the replay would run with no progress bound at all. A guard that
// has already fired stays fired: its transfer is already being torn down and
// there is nothing left to replay.
func (g *StallGuard) Restart() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stalled.Load() {
		return
	}
	g.done = false
	g.timer.Reset(g.d)
}

// Stalled reports whether this guard is the reason the transfer failed.
func (g *StallGuard) Stalled() bool { return g.stalled.Load() }

// Armed reports whether the watchdog is still running. For tests.
func (g *StallGuard) Armed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.done
}

// Window reports the configured stall window (for error messages and tests).
func (g *StallGuard) Window() time.Duration { return g.d }

// guardedReader restarts a StallGuard on every read that moves bytes, and
// translates the resulting failure into ErrStreamStalled so the cause is not
// mistaken for the caller's own cancellation.
//
// On the DOWNLOAD path the wrapped reader is the storage response body, so the
// window measures the gap between chunks arriving from storage. It also, by
// construction, measures the gap between the CONSUMER's reads: whoever is
// draining this reader has to come back for more within the window, so a
// consumer that has stopped accepting bytes is caught by the same guard.
//
// On the UPLOAD path the wrapped reader is the request body and the transport is
// the one reading it, so the window measures how long the transport has been
// unable to hand another chunk to the socket.
type guardedReader struct {
	r io.Reader
	g *StallGuard
}

func (gr *guardedReader) Read(p []byte) (int, error) {
	n, err := gr.r.Read(p)
	if n > 0 {
		gr.g.Progress()
	}
	if err == nil {
		return n, nil
	}
	if errors.Is(err, io.EOF) {
		// The body is complete. Anything still to come (the response to an
		// upload) is bounded by the transport, not by progress on this reader.
		gr.g.Stop()
		return n, err
	}
	if gr.g.Stalled() {
		return n, fmt.Errorf("%w: no progress for %s: %v", ErrStreamStalled, gr.g.Window(), err)
	}
	return n, err
}

// guardedRequestBody is a stall-guarded REQUEST body. It keeps the original
// body's Close, because net/http owns closing a request body and skipping that
// leaks whatever the caller handed us.
type guardedRequestBody struct {
	guardedReader
	rc io.ReadCloser
}

func newGuardedRequestBody(rc io.ReadCloser, g *StallGuard) *guardedRequestBody {
	return &guardedRequestBody{guardedReader: guardedReader{r: rc, g: g}, rc: rc}
}

func (b *guardedRequestBody) Close() error { return b.rc.Close() }

// guardRequestBody puts req's body under guard, and must be called AFTER the
// request is built rather than by handing http.NewRequest a wrapper.
//
// WHY THE ORDER MATTERS. http.NewRequest inspects a known body type
// (*bytes.Reader and friends) to derive ContentLength and, crucially, GetBody.
// GetBody is what lets net/http transparently REPLAY an idempotent request whose
// reused keep-alive connection died before any response arrived, which is a
// routine event against object storage. Passing an opaque wrapper suppresses
// both, silently converting those retries into upload failures. So the wrapper
// goes on afterwards, and the rewind function is wrapped too, so a replayed body
// is guarded as well and the guard is re-armed for it.
func guardRequestBody(req *http.Request, g *StallGuard) {
	if req == nil || g == nil {
		return
	}
	if req.Body != nil {
		req.Body = newGuardedRequestBody(req.Body, g)
	}
	rewind := req.GetBody
	if rewind == nil {
		return
	}
	req.GetBody = func() (io.ReadCloser, error) {
		rc, err := rewind()
		if err != nil {
			return nil, err
		}
		g.Restart()
		return newGuardedRequestBody(rc, g), nil
	}
}

// guardedBody is the stall-guarded response body handed to a streaming caller.
// Close disarms the watchdog, releases the derived context (so the transport
// reclaims the connection), and closes the underlying body.
type guardedBody struct {
	guardedReader
	rc     io.ReadCloser
	cancel context.CancelFunc
}

func newGuardedBody(rc io.ReadCloser, cancel context.CancelFunc, d time.Duration) *guardedBody {
	g := NewStallGuard(cancel, d)
	return &guardedBody{
		guardedReader: guardedReader{r: rc, g: g},
		rc:            rc,
		cancel:        cancel,
	}
}

func (b *guardedBody) Close() error {
	b.g.Stop()
	err := b.rc.Close()
	b.cancel()
	return err
}
