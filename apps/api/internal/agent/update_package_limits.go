package agent

// GH #302 hardening (H3): the four bounds that stop the unauthenticated package
// download route from being a bandwidth and goroutine amplifier: how many
// streams may be in flight, how many times one token may be redeemed, how long a
// stream may make no progress before it is torn down, and how long one stream
// may run in total no matter how much progress it claims.
//
// THE SHAPE OF THE PROBLEM. The signed token bounds WHO may download, not HOW
// MANY TIMES. It is deliberately not single use (a download retried after a
// network drop must still work inside its window) and it lives five minutes, and
// the route is mounted on the bare root engine with no rate limiting anywhere in
// the middleware stack. Anyone who controls one enrolled site can take the token
// out of a normal manifest fetch and then issue unbounded concurrent GETs. Each
// one pins a goroutine, opens a storage connection, and moves the package twice:
// storage to control plane, control plane to caller. The request costs a few
// hundred bytes and the response costs megabytes.
//
// Under the older presigned-URL design the same token exposure existed, but the
// load landed on object storage. Serving the package from the control plane moves
// that load onto the API process, where a few hundred concurrent slow reads on a
// small maxScale deployment take the dashboard down with them.
//
// PER INSTANCE, NOT GLOBAL. The two counting limits below live in this process's
// memory and are enforced per API instance. With N instances behind a load balancer the
// effective ceilings are N times these numbers. That is deliberate: a shared
// store would put a network round trip (and a new failure mode) on the hot path
// of a route whose entire point is to be cheap, and the per-instance bound
// already achieves what matters, which is that no single instance can be driven
// into resource exhaustion by this route. Nobody should read these as a
// fleet-wide guarantee.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mosamlife/wpmgr/apps/api/internal/blobstore"
)

const (
	// maxConcurrentPackageStreams caps how many package downloads one API
	// instance will have in flight at once. Above it the route answers 429
	// immediately rather than queueing, because queueing is the amplification:
	// a queued request still holds a goroutine and a connection.
	//
	// WHY 16. The cost that matters is not memory (16 copy buffers is
	// nothing) but the instance's request-concurrency budget and its egress: at
	// a container concurrency of 80, 16 slots leaves 80% of the instance free
	// for the dashboard and the rest of the API even while this route is
	// saturated.
	//
	// WHY A LEGITIMATE ROLLOUT CANNOT TRIP IT. Take a pessimistic 200 KB/s site
	// and a ~2 MiB package: one download occupies a slot for about 10s, so 16
	// slots retire roughly 1.6 downloads/s, about 5,700 per hour per instance.
	// Agents check for an update every six hours on their own WP-cron schedule,
	// so a 5,000-site fleet offers about 5,000 downloads spread over a six-hour
	// window, roughly 0.23/s, which is about 15% of ONE instance's capacity even
	// with zero help from the other instances. A refused request is not a failed
	// update either: the site simply downloads on its next check.
	maxConcurrentPackageStreams = 16

	// maxPackageTokenRedemptions caps how many times ONE token may be redeemed
	// inside its five-minute life, so a captured token cannot be replayed
	// indefinitely.
	//
	// WHY 5. A healthy install downloads once. The budget exists for the retry
	// cases the token is deliberately not single-use for: a dropped connection
	// part-way through the zip, a WordPress upgrade attempt that restarts, an
	// agent that re-reads the manifest and re-downloads within the same window.
	// Five covers all of those with room to spare, while cutting an unbounded
	// replay down to at most five package-sized responses per token. Minting a
	// sixth response requires a fresh manifest fetch, which is on the
	// Ed25519-authenticated agent route.
	maxPackageTokenRedemptions = 5

	// maxTrackedPackageTokens bounds the redemption table so it can never become
	// a memory-exhaustion vector of its own. Each entry is a jti plus a count and
	// an expiry, and entries are swept once they pass their token's exp, so the
	// steady-state size is bounded by the mint rate over five minutes. Reaching
	// this ceiling takes tens of thousands of AUTHENTICATED manifest fetches
	// inside one token lifetime.
	maxTrackedPackageTokens = 20000

	// packageRedemptionSweepEvery is how often the table is swept for expired
	// entries. Sweeping on every request would make a cheap route O(n) in the
	// number of live tokens.
	packageRedemptionSweepEvery = 30 * time.Second
)

// packageStreamStall is how long ONE package stream may make no progress before
// it is torn down and its slot released.
//
// WHY THIS BOUND EXISTS AT ALL. The concurrency cap above only helps if slots
// come BACK. Once the route moved onto the uncapped streaming client, nothing in
// the process bounded a stream that had stopped moving: the transport's
// ResponseHeaderTimeout is spent before the first body byte, the Dialer
// keepalive only notices a peer that has vanished rather than one that is alive
// and silent, the server sets no WriteTimeout (deliberately, see
// internal/server/server.go), and the request context belongs to a client that
// has not disconnected. A storage backend that answered with headers and one
// chunk and then went quiet held its slot indefinitely. Composed with the cap
// that is a denial of the entire update channel: a handful of requests, minted
// from a few ordinary manifest fetches, park on all 16 slots and every
// legitimate site on the instance gets update_package_busy for as long as the
// attacker keeps the sockets open. That is cheaper and more effective than the
// amplification the cap was built to refuse.
//
// WHY IT IS A PROGRESS BOUND AND NOT A DURATION. A duration cap is what caused
// the original defect: a legitimately slow site was cut off mid-zip. A large
// download must be allowed to take a long time while it is MOVING and must be
// cut off when it is not, so the window is restarted by every chunk that moves
// in either direction.
//
// WHY THE VALUE. It matches blobstore.StreamStallTimeout (20s) so the read half
// and the write half of the same copy cannot disagree; the reasoning for the
// number is documented there. Both halves now run the same watchdog
// (blobstore.StallGuard), so "cannot disagree" is a property of the code rather
// than of two comments. A var rather than a const only so this package's own
// tests can compress the window instead of sleeping through it; nothing in
// production assigns it.
var packageStreamStall = blobstore.StreamStallTimeout

// packageStreamChunk is the copy buffer: how much is read from storage and
// offered to the socket at a time.
//
// IT DOES NOT DECIDE THE BOUND, AND THAT IS THE WHOLE POINT. It used to. The
// write side armed one socket deadline per w.Write(buf[:n]) with n up to this
// size, and a socket write deadline is ABSOLUTE for the whole Write call:
// bytes that leave inside the call do not extend it. So the shipped guarantee
// was "one 64 KiB chunk must fully land within 20s", which is a throughput
// floor of 64 KiB / 20s = 3.28 KB/s on paper and, measured, was not a rate at
// all. streamPackage below now writes under a short attempt deadline and reports
// the PARTIAL count to the watchdog, so this constant is back to being what its
// name says: a buffer size, traded off against syscalls, with no bearing on when
// a transfer is torn down.
const packageStreamChunk = 64 << 10

// packageStreamAttempt is how long ONE write attempt may block before the copy
// loop comes back to look at the watchdog. It is the OVERSHOOT of the stall
// window, not the window itself: a stalled consumer is detected within
// stall+attempt of its last byte, and a moving one is never affected because a
// write that completes returns long before this fires.
//
// WHY IT IS DERIVED AND CLAMPED. stall/8 keeps the overshoot proportional when a
// test compresses the window to milliseconds; the 1s ceiling keeps a production
// stream from waking more than once a second while it is blocked, which is the
// only case that costs an extra syscall at all. A healthy 64 KiB write to a
// consumer that is keeping up completes inside the first attempt, so the steady
// state is one SetWriteDeadline and one Write per chunk, exactly as before.
func packageStreamAttempt(stall time.Duration) time.Duration {
	attempt := stall / 8
	if attempt > time.Second {
		attempt = time.Second
	}
	if attempt < time.Millisecond {
		attempt = time.Millisecond
	}
	return attempt
}

// ---------------------------------------------------------------------------
// THE WHOLE-TRANSFER CEILING
// ---------------------------------------------------------------------------

// packageStreamMinRate is the average throughput a whole transfer has to sustain
// to fit inside its ceiling, and is what the ceiling is derived from.
//
// WHY A TOTAL BOUND EXISTS, WHEN REMOVING ONE IS THE POINT OF THIS WORK. The
// stall guard bounds PROGRESS, which is the right bound for a slow site and is
// what lets a legitimately slow download finish. It does not bound the TOTAL,
// and those are not the same protection. A consumer that accepts the smallest
// amount its TCP stack registers as progress, once per window, restarts the
// guard forever and holds its slot for as long as it keeps the socket open.
// Measured over net.Pipe against this exact handler, with a consumer taking one
// byte per 60% of the window: 12.028s (60 windows) moved 99 of 4194304 bytes,
// 8.2 B/s, with the stream STILL in flight and its slot STILL held. Over real
// TCP the practical floor is one window update per window, roughly 2 to 64 KiB
// per 20s, so the slot-hold time available to a caller who wants one grew by up
// to about 30x versus the mechanism this replaced.
//
// Composed with the concurrency cap that is precisely the denial this file
// exists to refuse, and it is cheaper than the amplification: one enrolled site
// fetches four manifests, which nothing rate limits, for 20 redemptions against
// a 16 slot cap; it opens 16 connections and drips; every legitimate site on the
// instance gets update_package_busy for as long as those sockets stay open.
// Nothing else ends it. There is no server WriteTimeout (deliberately, see
// internal/server/server.go), the request context belongs to a client that has
// not disconnected, and the token's expiry is checked once at request start and
// never revalidated mid-stream (see packageStreamTotalLimit for why that stays
// true).
//
// WHY THIS IS NOT THE DURATION CAP THAT CAUSED THE ORIGINAL DEFECT. That cap was
// 15s and it cut off sites that were moving bytes the whole time. This one is
// three orders of magnitude above it and is derived from the package size, so it
// demands 1 KB/s averaged over the whole transfer and nothing more. The
// arithmetic is on packageStreamTotalLimit.
const packageStreamMinRate = 1 << 10 // bytes per second

// packageStreamMinTotal is the floor under the derived ceiling, so a SMALL
// package never turns the ceiling into a rate demand. A 1 MiB package derives
// 17 minutes; the floor lifts it to 30, at which point the demanded average is
// 582 B/s. Shrinking the package can therefore never tighten this bound.
//
// packageStreamMaxTotal caps it from the other side, so the worst-case
// slot-hold time can be STATED however large a future package gets. Without a
// cap a hypothetical 100 MiB package would derive a 28 hour ceiling, which is
// not a bound anybody can reason about. At the cap the demanded average is
// size/7200s: 2.8 KB/s for a 20 MiB package, still an order of magnitude under
// the slowest consumer band this route is built to carry.
//
// Both are vars only so this package's own tests can compress them, exactly as
// packageStreamStall is. Nothing in production assigns either.
var (
	packageStreamMinTotal = 30 * time.Minute
	packageStreamMaxTotal = 2 * time.Hour
)

// errPackageStreamTotal is returned by a transfer torn down for exceeding its
// whole-transfer ceiling. Deliberately distinct from blobstore.ErrStreamStalled:
// "it kept dripping for an hour" and "it went silent for 20s" are different
// behaviours and a log that conflates them cannot tell abuse from a wedged peer.
var errPackageStreamTotal = errors.New("agent: package stream exceeded its whole-transfer ceiling")

// packageStreamTotalLimit is how long ONE package stream may run in total,
// whatever it is doing, derived from the size it was asked to serve.
//
// THE VALUE, AND WHY NO LEGITIMATE TRANSFER CAN REACH IT. For the current agent
// package of about 3 MiB the ceiling is 3145728 B / 1024 B/s = 3072s, 51.2
// minutes, inside the [30 min, 2 h] clamp. Three independent floors sit far
// above the 1 KB/s that ceiling demands:
//
//   - THE AGENT'S OWN BUDGET, which is the binding one. It downloads with
//     wp_remote_get('timeout' => 60), a WHOLE-operation cap, so a 3 MiB package
//     has to average 3145728/60 = 52428 B/s, about 52 KB/s. That is 51x the rate
//     this ceiling asks for, and in wall clock the agent has abandoned the
//     request 51 minutes before the ceiling could fire. A transfer that reaches
//     this ceiling has no agent waiting on the other end of it.
//   - THE MEASURED SLOW-CONSUMER BAND. The population this work exists to
//     protect sat between 25 and 40 KB/s (see streamPackage's table), which is
//     25x to 40x the ceiling's floor.
//   - THE PESSIMISTIC SITE used to size the concurrency cap, 200 KB/s, which is
//     200x it.
//
// And the behaviour it is built to stop measured 8.2 B/s, about 1/125th of the
// ceiling's floor. The gap between the slowest thing that must survive and the
// fastest thing that must not is two orders of magnitude wide, which is why a
// total bound can be added here without reintroducing the defect.
//
// WHY NOT REVALIDATE THE TOKEN'S EXPIRY MID-STREAM INSTEAD. It was considered
// and deliberately not done. The token's exp bounds when a download may START,
// not how long one may take, and its TTL is presignTTL, at most five minutes.
// Enforcing it mid-body would cap every response at five minutes of wall clock
// with 200 and Content-Length already flushed, which is the original defect
// verbatim, just with a five minute cap instead of a 15s one. It would also bind
// the control plane to one agent's timeout: the 60s budget above is documented
// as movable, and an operator who raises it for genuinely slow sites would find
// this route truncating them for a reason no log on either side names. The
// presigned-storage design this replaced checked its expiry at request start and
// never mid-body either, so revalidating would be a behaviour change against the
// baseline this route is a drop-in for. What actually needed bounding was the
// SLOT, and the slot is bounded here, by size and not by credential lifetime.
func packageStreamTotalLimit(size int64) time.Duration {
	if size <= 0 {
		return packageStreamMinTotal
	}
	d := time.Duration(size/packageStreamMinRate) * time.Second
	if d < packageStreamMinTotal {
		return packageStreamMinTotal
	}
	if d > packageStreamMaxTotal {
		return packageStreamMaxTotal
	}
	return d
}

// warnHijackFailed and warnNotHijackable report ONCE per process, each for its
// own reason, that this response could not give up its connection and the write
// half is therefore running on the degraded per-call deadline instead of the
// progress bound. Two separate Onces because the two reasons mean different
// things to whoever reads the log: Hijack returning an error is a runtime
// failure on a connection that should have been hijackable, while a writer that
// is not an http.Hijacker at all is a deployment that has moved to HTTP/2. Both
// are silent degradations, so neither may return without saying so.
var (
	warnHijackFailed  sync.Once
	warnNotHijackable sync.Once
)

// streamPackage copies at most limit bytes from src to w under a PROGRESS bound:
// the transfer is torn down when no bytes move for stall, whatever size the
// consumer reads in.
//
// WHY THIS IS NOT A PER-CALL DEADLINE ANY MORE. The previous shape armed
// SetWriteDeadline(now+stall) once per w.Write of up to packageStreamChunk. That
// reads like a progress bound and is not one: a socket write deadline is
// absolute for the whole Write call, and bytes that leave inside that call do
// not extend it, so nothing on this side was ever restarted by progress. What it
// enforced was "one 64 KiB chunk must fully land within the window", whose cost
// is not a rate. Measured on the real serving path, with a real 20s window, real
// blobstore semantics, a 2 MiB package and IDENTICAL sustained throughput in
// every row, varying only the consumer's read size:
//
//	1280 B every 50 ms   24670 B/s  COMPLETE  2097152 of 2097152 B in 85.0s
//	4096 B every 160 ms  25122 B/s  TRUNCATED  849772 of 2097152 B, i/o timeout
//	16 KiB every 640 ms  24739 B/s  TRUNCATED  841580 of 2097152 B, i/o timeout
//	4096 B every 100 ms  40220 B/s  COMPLETE  2097152 of 2097152 B in 52.1s
//
// That table is TestPackageDownload_ProductionScaleConsumerTable, which is kept
// and can be re-run against either mechanism.
//
// Same bytes per second, opposite outcomes, because how long one 64 KiB write
// blocks depends on the consumer's read granularity and on TCP window-update
// timing, neither of which the control plane can observe or predict. The real
// floor sat between 25 and 40 KB/s rather than the 3.28 KB/s the chunk size
// reasons to, which for a small package is inside the range the agent's own 60s
// budget still allows: a 30 KB/s site pulling a 1.2 MiB package has 41s of agent
// budget and would be cut off here at about 840 KB, with 200 and Content-Length
// already flushed, so it fails its size and sha256 check and never self-updates.
// That is the original defect, for a narrower population.
//
// THE MECHANISM NOW. The response gives up its connection (Hijack) and the body
// is written to the socket directly, because that is the only place a PARTIAL
// write count is observable: net/http's ResponseWriter closes the connection on
// any write error, so a timed-out write there cannot be resumed and progress
// inside a call can never be seen. Each attempt is issued under a short
// packageStreamAttempt deadline and reports what it moved to a
// blobstore.StallGuard, the same watchdog the READ half of this copy runs on. An
// attempt that timed out having moved bytes is progress, not death; the guard
// fires only when a whole window passes with none, and it fires by putting the
// connection's deadline in the past, which unblocks the parked write at once.
// Shrinking packageStreamChunk was the other option and is not a fix: it moves
// the same bug to a smaller size and costs syscalls.
//
// WHY NOT A SERVER-LEVEL WriteTimeout. http.Server's WriteTimeout is global and
// is a whole-response deadline measured from the end of the request headers, so
// any value large enough for a multi-megabyte package on a slow link is useless
// as a bound, and any value small enough to be a bound would kill the SSE
// streams (update runs, backup runs, the connection-lifecycle bus) that are
// supposed to stay open for minutes. This bound lives on one response and leaves
// every other handler alone. See internal/server/server.go.
//
// Its return contract is io.CopyN's: fewer than limit bytes copied is io.EOF, so
// the caller's existing short-body handling is unchanged.
func streamPackage(w gin.ResponseWriter, src io.Reader, limit int64, stall time.Duration) (int64, error) {
	// The progress bound and the total bound are both applied, because neither
	// implies the other: progress carries a slow site to the end, total stops a
	// caller that drips forever from holding the slot it is carried in.
	total := packageStreamTotalLimit(limit)

	conn, brw, ok := hijackStreamConn(w)
	if !ok {
		return streamPackageViaWriter(w, src, limit, stall, total)
	}
	// We own the connection now, including closing it. The Connection: close
	// written with the head says so, so no client is waiting to reuse it.
	defer func() { _ = conn.Close() }()

	if err := writePackageHead(conn, brw, w, stall); err != nil {
		return 0, err
	}
	return copyToConnGuarded(conn, src, limit, stall, total)
}

// copyToConnGuarded is the progress-bounded copy itself, under a whole-transfer
// ceiling.
//
// HOW THE CEILING IS ENFORCED, AND WHY NO SECOND TIMER. It is a clock comparison
// at the top of each loop rather than another watchdog poking the socket,
// because every way this loop can block is ALREADY bounded and short: a write is
// issued under packageStreamAttempt (at most 1s) and a read from storage is
// under blobstore's own stall guard (the same 20s window). So the ceiling is
// noticed within one attempt of a dripping consumer and within one stall window
// of a silent storage peer, an overshoot of seconds on a ceiling of tens of
// minutes. A second timer would buy that back and add a second thing racing the
// same deadline.
func copyToConnGuarded(conn net.Conn, src io.Reader, limit int64, stall, total time.Duration) (int64, error) {
	attempt := packageStreamAttempt(stall)

	// The watchdog fires once, stall after the last byte moved. Putting the
	// deadline in the past is what unblocks a write parked in the kernel; the
	// attempt deadline alone would only end the current attempt, not the copy.
	guard := blobstore.NewStallGuard(func() {
		_ = conn.SetWriteDeadline(time.Now())
	}, stall)
	defer guard.Stop()

	stalled := func(written int64) (int64, error) {
		return written, fmt.Errorf("%w: no progress for %s", blobstore.ErrStreamStalled, guard.Window())
	}

	start := time.Now()
	hardStop := start.Add(total)
	overTotal := func() bool { return !time.Now().Before(hardStop) }
	exceeded := func(written int64) (int64, error) {
		return written, fmt.Errorf("%w: %s of %s elapsed having served %d of %d bytes",
			errPackageStreamTotal, time.Since(start).Round(time.Second), total, written, limit)
	}

	buf := make([]byte, packageStreamChunk)
	var written int64
	for written < limit {
		if overTotal() {
			return exceeded(written)
		}
		want := int64(len(buf))
		if rem := limit - written; rem < want {
			want = rem
		}
		n, rerr := src.Read(buf[:want])

		for rem := buf[:n]; len(rem) > 0; {
			if guard.Stalled() {
				// Fired between attempts, so re-arming the deadline below would
				// quietly undo the teardown.
				return stalled(written)
			}
			// Checked here and not only in the outer loop because a dripping
			// consumer barely leaves this one: handing a single 64 KiB chunk over
			// at a few bytes per window keeps it spinning for a very long time,
			// and every timed-out attempt continues straight back to the top of
			// it. An outer-loop-only check would let the ceiling slip by a whole
			// chunk's worth of dripping.
			if overTotal() {
				return exceeded(written)
			}
			if err := conn.SetWriteDeadline(time.Now().Add(attempt)); err != nil {
				return written, err
			}
			nw, werr := conn.Write(rem)
			rem = rem[nw:]
			written += int64(nw)
			if nw > 0 {
				// Bytes moved, whatever the consumer's read size was. THIS is the
				// bound, and it is the same call the read half makes.
				guard.Progress()
			}
			if werr == nil {
				continue
			}
			if guard.Stalled() {
				return stalled(written)
			}
			var nerr net.Error
			if errors.As(werr, &nerr) && nerr.Timeout() {
				// The ATTEMPT deadline, not the window. The consumer is slow, not
				// silent: re-arm and carry on with what is left of this chunk.
				continue
			}
			return written, werr
		}

		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return written, rerr
		}
	}

	if written < limit {
		return written, io.EOF
	}
	return written, nil
}

// hijackStreamConn takes the connection out from under the response, or reports
// that this response cannot give one up.
//
// WHY THE UNWRAP DANCE. gin's ResponseWriter always satisfies http.Hijacker, and
// its Hijack does an unchecked type assertion on the writer underneath, so
// calling it on an httptest.ResponseRecorder panics rather than returning an
// error. The only safe test is to unwrap to the real writer first. HTTP/2 lands
// here too: its response writer is not a Hijacker, so an h2 deployment falls
// back rather than failing. Gin's own Hijack is still what we call once the test
// passes, because it also marks the response written, which stops the engine
// from trying to send a header onto a connection it no longer owns.
//
// ONE COSMETIC CONSEQUENCE, so nobody chases it later: bytes written to a
// hijacked connection do not go through gin's writer, so gin's Size() stays 0
// for this route and the otelgin ResponseSize attribute reads 0 with it. The
// request log is unaffected (it does not record size) and the handler logs the
// byte count it actually served on the success path.
func hijackStreamConn(w gin.ResponseWriter) (net.Conn, *bufio.ReadWriter, bool) {
	var inner http.ResponseWriter = w
	for {
		u, ok := inner.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			break
		}
		inner = u.Unwrap()
	}
	if _, ok := inner.(http.Hijacker); !ok {
		// This branch used to return silently, which meant the ONE deployment
		// change that reaches it, moving this route to HTTP/2, would degrade the
		// write bound with nothing in the log to say so. It says so now.
		warnNotHijackable.Do(func() {
			slog.Warn("GH #302 package stream: this response is not hijackable, so the write half falls back to a per-call deadline and the weaker, read-granularity-dependent bound that implies; an HTTP/2 response writer is not an http.Hijacker",
				slog.String("writer_type", fmt.Sprintf("%T", inner)))
		})
		return nil, nil, false
	}
	conn, brw, err := w.Hijack()
	if err != nil {
		warnHijackFailed.Do(func() {
			slog.Warn("GH #302 package stream: this response would not give up its connection; the write half falls back to a per-call deadline",
				slog.String("err", err.Error()))
		})
		return nil, nil, false
	}
	return conn, brw, true
}

// writePackageHead sends the status line and headers that net/http would have
// sent, built from the headers the handler already set so the two cannot drift.
// Connection: close is added because we own the socket and close it below, and
// Date because net/http adds one and nothing downstream should notice the
// difference.
func writePackageHead(conn net.Conn, brw *bufio.ReadWriter, w gin.ResponseWriter, stall time.Duration) error {
	status := w.Status()
	if status == 0 {
		status = http.StatusOK
	}
	h := w.Header()
	h.Set("Connection", "close")
	if h.Get("Date") == "" {
		h.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	}

	// The head is a few hundred bytes into an empty socket buffer, so one whole
	// window is a generous bound and no consumer pace can be punished by it.
	if err := conn.SetWriteDeadline(time.Now().Add(stall)); err != nil {
		return err
	}
	// THE PROTOCOL IN THE STATUS LINE IS FIXED AT 1.1, WHICH NET/HTTP WOULD NOT
	// DO, AND THAT IS DELIBERATE. net/http echoes the request's version
	// (writeStatusLine takes an is11 flag and emits "HTTP/1.0 " for a 1.0
	// request), and WordPress's HTTP API defaults httpversion to 1.0, so the
	// agent can present as 1.0 and receive a 1.1 status line from this path where
	// every other route in the process would answer 1.0. Verified inert and left
	// alone on purpose: the response carries an explicit Content-Length and
	// Connection: close, which are exactly the two things a 1.0 client would
	// otherwise have to infer from the version, so there is no framing ambiguity
	// for it to get wrong; and in front of this route both proxies re-issue the
	// request as 1.1 anyway, so the 1.0 case does not arise in production. Do not
	// "fix" this to match the request version without re-testing the framing: the
	// value here is that ONE literal produces the head, not that it agrees with
	// net/http.
	if _, err := fmt.Fprintf(brw, "HTTP/1.1 %d %s\r\n", status, http.StatusText(status)); err != nil {
		return err
	}
	if err := h.Write(brw); err != nil {
		return err
	}
	if _, err := brw.WriteString("\r\n"); err != nil {
		return err
	}
	return brw.Flush()
}

// streamPackageViaWriter is the fallback for a response that cannot give up its
// connection: an httptest.ResponseRecorder in this package's own tests, or an
// HTTP/2 response.
//
// IT KEEPS THE OLD, WEAKER BOUND, DELIBERATELY AND VISIBLY. A per-call deadline
// still releases the slot held by a consumer that has gone completely silent,
// which is what the concurrency cap needs, but it is NOT a progress bound and it
// reimposes the granularity-dependent floor described on streamPackage. The
// control plane serves this route over HTTP/1.1 (see newHTTPServer: no TLS, no
// h2c), so in production this path is not taken; if that ever changes, this is
// the comment that says what changed with it.
func streamPackageViaWriter(w gin.ResponseWriter, src io.Reader, limit int64, stall, total time.Duration) (int64, error) {
	ctrl := http.NewResponseController(w)
	buf := make([]byte, packageStreamChunk)

	start := time.Now()
	hardStop := start.Add(total)

	var written int64
	// deadlines goes false if this ResponseWriter cannot carry one (an
	// httptest.ResponseRecorder). The read half of the bound still applies.
	deadlines := true

	for written < limit {
		// The whole-transfer ceiling applies here too. This path's per-call
		// deadline is the weaker of the two progress mechanisms, so it is if
		// anything MORE important that the total is bounded on it.
		if !time.Now().Before(hardStop) {
			return written, fmt.Errorf("%w: %s of %s elapsed having served %d of %d bytes",
				errPackageStreamTotal, time.Since(start).Round(time.Second), total, written, limit)
		}
		want := int64(len(buf))
		if rem := limit - written; rem < want {
			want = rem
		}
		n, rerr := src.Read(buf[:want])
		if n > 0 {
			if deadlines {
				if err := ctrl.SetWriteDeadline(time.Now().Add(stall)); err != nil {
					deadlines = false
				}
			}
			nw, werr := w.Write(buf[:n])
			written += int64(nw)
			if werr != nil {
				return written, werr
			}
			if nw != n {
				return written, io.ErrShortWrite
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return written, rerr
		}
	}

	if deadlines {
		// Clear it: the deadline belongs to this copy, not to whatever the
		// connection is reused for next.
		_ = ctrl.SetWriteDeadline(time.Time{})
	}
	if written < limit {
		return written, io.EOF
	}
	return written, nil
}

// packageStreamSemaphore is a non-blocking counting semaphore over in-flight
// package streams. tryAcquire never waits: the caller either gets a slot or
// returns 429.
type packageStreamSemaphore struct {
	slots chan struct{}
}

func newPackageStreamSemaphore(n int) *packageStreamSemaphore {
	if n <= 0 {
		n = 1
	}
	return &packageStreamSemaphore{slots: make(chan struct{}, n)}
}

// tryAcquire takes a slot if one is free. On success the caller MUST release.
func (s *packageStreamSemaphore) tryAcquire() bool {
	select {
	case s.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *packageStreamSemaphore) release() {
	select {
	case <-s.slots:
	default:
	}
}

// inFlight reports how many slots are currently held. For logging and tests.
func (s *packageStreamSemaphore) inFlight() int { return len(s.slots) }

// waitIdle blocks until no slot is held or ctx is done, and reports how many
// were still held when it returned.
//
// THIS EXISTS FOR GRACEFUL SHUTDOWN, AND THE REASON IS SPECIFIC TO THIS ROUTE.
// http.Server.Shutdown explicitly does NOT wait for hijacked connections, and
// this route hijacks. Measured against this handler mid-body: the hijacked path
// let Shutdown return after 0s with a download in flight, where the fallback
// path held it for the full 5s budget. So once the write half took over the
// connection, a SIGTERM (a revision rollout, a scale-down) stopped draining
// these streams: up to 16 sites take a truncated zip, fail their size and sha256
// check, and retry next cycle. No data is lost, but "every in-flight agent
// update truncates on deploy" is exactly the kind of thing that gets diagnosed
// as a mysterious update failure months later, so the server waits for these
// explicitly. See Server.Run.
//
// POLLED, NOT SIGNALLED. A sync.WaitGroup is the obvious shape and is the wrong
// one: Add on a counter that is at zero must not race Wait, and during shutdown
// a request that slips in between the listener closing and the last release does
// exactly that. A 20 ms poll over a bound that is already capped by the shutdown
// budget costs at most a few hundred wakeups on a process that is exiting.
func (s *packageStreamSemaphore) waitIdle(ctx context.Context) int {
	const tick = 20 * time.Millisecond
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		if n := s.inFlight(); n == 0 {
			return 0
		}
		select {
		case <-ctx.Done():
			return s.inFlight()
		case <-t.C:
		}
	}
}

// capacity reports the configured ceiling. For logging and tests.
func (s *packageStreamSemaphore) capacity() int { return cap(s.slots) }

// packageRedemption is the outcome of counting one redemption of a token.
type packageRedemption struct {
	// Count is how many redemptions this token has now had, including this one.
	// Zero when the redemption was not tracked.
	Count int
	// Allowed is false once a token has passed maxPackageTokenRedemptions.
	Allowed bool
	// Tracked is false when the table was full and this token could not be
	// recorded. The request is still allowed (see redeem), but the caller should
	// log it: it means the redemption ceiling is not being enforced right now.
	Tracked bool
}

// packageTokenRedemptions counts redemptions per token jti within the token's
// own lifetime.
type packageTokenRedemptions struct {
	mu        sync.Mutex
	max       int
	seen      map[string]packageRedemptionEntry
	nextSweep time.Time
}

type packageRedemptionEntry struct {
	count     int
	expiresAt time.Time
}

func newPackageTokenRedemptions(max int) *packageTokenRedemptions {
	if max <= 0 {
		max = 1
	}
	return &packageTokenRedemptions{max: max, seen: make(map[string]packageRedemptionEntry)}
}

// redeem records one redemption of jti and reports whether it is allowed.
// expiresAt is the token's own exp, which is when its entry becomes sweepable:
// an entry never has to outlive the token it counts.
//
// FAIL OPEN, DELIBERATELY. If the table is at maxTrackedPackageTokens and this
// is a token it has never seen, the request is allowed and left uncounted rather
// than refused. Failing closed would let a flood of freshly minted tokens deny
// service to legitimate agents, which is the outcome this whole file exists to
// prevent; and the concurrency cap, which is the bound that actually protects
// the instance, is unaffected either way.
func (r *packageTokenRedemptions) redeem(now time.Time, jti string, expiresAt time.Time) packageRedemption {
	if jti == "" {
		// No id to count against. Nothing to enforce, and nothing to leak.
		return packageRedemption{Allowed: true}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if now.After(r.nextSweep) {
		for k, e := range r.seen {
			if !now.Before(e.expiresAt) {
				delete(r.seen, k)
			}
		}
		r.nextSweep = now.Add(packageRedemptionSweepEvery)
	}

	entry, ok := r.seen[jti]
	if !ok {
		if len(r.seen) >= maxTrackedPackageTokens {
			return packageRedemption{Allowed: true}
		}
		entry = packageRedemptionEntry{expiresAt: expiresAt}
	}
	if entry.count >= r.max {
		return packageRedemption{Count: entry.count, Allowed: false, Tracked: true}
	}
	entry.count++
	r.seen[jti] = entry
	return packageRedemption{Count: entry.count, Allowed: true, Tracked: true}
}

// packageTableFullLogEvery throttles the "the redemption table is full" warning.
//
// WHY IT IS THROTTLED. That line reports a real loss of enforcement, so it must
// not be dropped to debug, but the condition that produces it is a flood of
// freshly minted tokens, and it fires once per REQUEST while the flood lasts.
// An unthrottled warning would turn a token flood into a log flood, which costs
// ingestion budget on the instance that is already under load and buries the
// signal in copies of itself. One line a minute is enough to see the state and
// to see when it clears.
const packageTableFullLogEvery = time.Minute

// logThrottle rate limits one log site to at most one line per interval.
type logThrottle struct {
	mu   sync.Mutex
	next time.Time
}

// allow reports whether this log site may emit now, and if so arms the next
// window.
func (l *logThrottle) allow(now time.Time, every time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Before(l.next) {
		return false
	}
	l.next = now.Add(every)
	return true
}

// warnPackageTableFull throttles the untracked-redemption warning in
// packageDownload.
var warnPackageTableFull logThrottle

// tracked reports how many tokens are currently held. For tests.
func (r *packageTokenRedemptions) tracked() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seen)
}
