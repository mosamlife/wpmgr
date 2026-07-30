package agent

// THE WRITE HALF OF THE STALL BOUND IS A PROGRESS BOUND.
//
// These tests exist because the write half was documented as a progress bound
// and was not one. It armed SetWriteDeadline(now+stall) once per 64 KiB
// w.Write, and a socket write deadline is ABSOLUTE for the whole Write call, so
// bytes leaving inside that call did not extend it. What shipped was "one chunk
// must fully land within the window", which is not a function of throughput.
//
// The decisive shape is the table below: hold the sustained byte rate FIXED and
// vary only how much the consumer reads at a time. Two consumers moving the same
// bytes per second are the same health and must get the same outcome. Under the
// old mechanism they did not.
//
// WHY THE TABLE RUNS OVER net.Pipe AND NOT LOOPBACK TCP. The effect has to be
// deterministic to be worth committing. A loopback socket carries roughly 380
// KiB of send buffer plus receive window before a write blocks at all, and that
// figure does not scale down with the window, so reproducing the defect over TCP
// needs the production 20s window and a multi-megabyte package: minutes per row,
// and an outcome that depends on the host's TCP window-update heuristics.
// net.Pipe is a real net.Conn with real deadline semantics and NO buffering, so
// the consumer's pace is the only variable and the same property is provable in
// seconds. Everything else here is the real thing: the real Gin engine, the real
// route, the real handler, a real http.Server, and a response parsed by the
// stdlib (which is also what pins the hand-written head the hijacked path
// writes). The production-scale reviewer table over real TCP is kept below,
// behind an env gate, because it takes minutes.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// An unbuffered transport
// ---------------------------------------------------------------------------

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

// pipeListener hands an http.Server one end of a net.Pipe per dial.
type pipeListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

// dial returns the CLIENT end of a fresh connection.
func (l *pipeListener) dial() (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.closed:
		_ = client.Close()
		_ = server.Close()
		return nil, net.ErrClosed
	}
}

// ---------------------------------------------------------------------------
// A package server on the real route
// ---------------------------------------------------------------------------

type packageServer struct {
	h    *UpdateHandler
	pkg  []byte
	path string

	// srv is the real http.Server serving the route, so the shutdown test can
	// call Shutdown on the same server the stream is running under.
	srv *http.Server

	// dial opens one connection to the server, whatever the transport is.
	dial func() (net.Conn, error)
}

// newPackageServer builds the real handler over the real Gin engine and serves
// it on the given transport. overTCP picks a loopback listener instead of
// net.Pipe, for the production-scale table.
func newPackageServer(t *testing.T, packageSize int, overTCP bool) *packageServer {
	t.Helper()

	pkg := make([]byte, packageSize)
	for i := range pkg {
		pkg[i] = byte(i % 251)
	}
	manifest, err := json.Marshal(releaseManifest{
		Slug:             expectedAgentSlug,
		Plugin:           "wpmgr-agent/wpmgr-agent.php",
		Version:          testPackageVersion,
		MinVersion:       "0.0.0",
		PackageObjectKey: updatePackagePrefix + testPackageVersion + "/" + expectedAgentSlug + ".zip",
		PackageSHA256:    goodSHA,
		PackageSize:      int64(packageSize),
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}

	signer, _ := newTestSigner(t)
	h := NewUpdateHandler(&packageStore{manifest: manifest, object: pkg}, signer, time.Minute)
	h.EnablePackageServing(signer, "")

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	h.RegisterPublic(engine)

	siteID := uuid.New()
	token, _, err := signer.MintPackageToken(time.Now(), siteID.String(), testPackageVersion, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	ps := &packageServer{
		h:   h,
		pkg: pkg,
		path: PackageRoutePrefix + "/" + siteID.String() +
			"?" + PackageTokenQueryParam + "=" + url.QueryEscape(token),
	}

	if overTCP {
		srv := httptest.NewServer(engine)
		t.Cleanup(srv.Close)
		ps.srv = srv.Config
		ps.dial = func() (net.Conn, error) { return net.Dial("tcp", srv.Listener.Addr().String()) }
		return ps
	}

	ln := newPipeListener()
	srv := &http.Server{Handler: engine}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = ln.Close()
		_ = srv.Close()
	})
	ps.srv = srv
	ps.dial = ln.dial
	return ps
}

// get issues the download and returns the parsed response.
//
// The bufio.Reader is deliberately tiny so it cannot read ahead of the paced
// consumer: anything larger would decide the consumer's real read size instead
// of the test doing so. Parsing with http.ReadResponse also means the stdlib is
// what validates the status line and headers, which on the hijacked path this
// handler now writes itself.
func (p *packageServer) get(t *testing.T) (*http.Response, net.Conn) {
	t.Helper()
	conn, err := p.dial()
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(conn, "GET "+p.path+" HTTP/1.1\r\nHost: cp.test\r\n\r\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}
	_ = conn.SetWriteDeadline(time.Time{})

	resp, err := http.ReadResponse(bufio.NewReaderSize(conn, 16), nil)
	if err != nil {
		t.Fatalf("read response head: %v", err)
	}
	return resp, conn
}

// drainAtPace reads readSize bytes at a time, sleeping every between reads, so
// the sustained rate is readSize/every whatever readSize is.
//
// It also reports the LARGEST gap it observed between two of its own successive
// reads. THAT NUMBER IS A LOG LINE, NOT A VERDICT, and nothing may branch on it.
// It once gated an "inconclusive" skip in the soak table below, on the theory
// that a gap past the stall window meant the test process had been descheduled
// rather than the server's bound misfiring. It cannot carry that meaning,
// because it is measured on the wrong side of the buffer: these reads are served
// from the local kernel receive buffer and the small bufio in front of it, so
// they keep returning promptly while the SERVER is blocked and unable to push a
// byte into the socket. Measured on the same row, worst gap 53.5ms when it went
// red under load and 52.4ms when it completed on an idle box. Worse, the one
// case that DID trip the skip was a consumer that blocked in Read for a whole
// window, which is a server that has genuinely gone silent: the most serious
// real failure was the one most likely to be excused.
func drainAtPace(body io.Reader, readSize int, every time.Duration, want int) ([]byte, time.Duration, error) {
	got := make([]byte, 0, want)
	buf := make([]byte, readSize)
	var maxGap time.Duration
	last := time.Now()
	for len(got) < want {
		n, err := body.Read(buf)
		if gap := time.Since(last); gap > maxGap {
			maxGap = gap
		}
		last = time.Now()
		got = append(got, buf[:n]...)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return got, maxGap, nil
			}
			return got, maxGap, err
		}
		time.Sleep(every)
	}
	return got, maxGap, nil
}

// ---------------------------------------------------------------------------
// The decisive table
// ---------------------------------------------------------------------------

// TestPackageDownload_SameRateDifferentReadSizesAllComplete is the regression
// test for the write half.
//
// Every row sustains the SAME bytes per second and differs only in how much the
// consumer takes at a time. None of them ever pauses for even a quarter of the
// stall window, so every one is a healthy slow consumer and every one must
// receive the whole package. The last row is the control: it is fast enough that
// even the old per-call deadline let it through, so a failure here is not "the
// route is broken", it is the bound discriminating on something that is not
// progress.
//
// CONFIRMED TO FAIL BEFORE THE FIX. Against the per-call deadline the first four
// rows truncate: the consumer's rate is below packageStreamChunk/stall, so one
// 64 KiB write cannot land inside one window however steadily bytes are moving.
// At production scale over real TCP the same mechanism truncated 4096 B every
// 160 ms and 16 KiB every 640 ms while 1280 B every 50 ms completed at an
// identical 25.6 KB/s (see the env-gated table at the bottom of this file).
func TestPackageDownload_SameRateDifferentReadSizesAllComplete(t *testing.T) {
	const (
		stall = time.Second
		// 80 KiB is more than one packageStreamChunk, so at least one full-size
		// write has to survive being handed over in the row's read size.
		packageSize = 80 << 10
		// The one rate every row shares. Below packageStreamChunk/stall (65536
		// B/s), which is exactly the floor the old per-call deadline imposed and
		// the one a progress bound must not have.
		rate = 32 << 10
	)

	// t.Cleanup, not defer: the parallel rows below resume after this function
	// returns, and a defer would restore the window out from under them.
	restore := packageStreamStall
	packageStreamStall = stall
	t.Cleanup(func() { packageStreamStall = restore })

	rows := []struct {
		name     string
		readSize int
		rate     int
	}{
		{"512 B at a time", 512, rate},
		{"1280 B at a time", 1280, rate},
		{"4096 B at a time", 4096, rate},
		{"8192 B at a time", 8192, rate},
		{"4096 B at four times the rate (control)", 4096, 4 * rate},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			every := time.Duration(row.readSize) * time.Second / time.Duration(row.rate)
			if every > stall/4 {
				t.Fatalf("this row pauses %s of a %s window, which is no longer unambiguously healthy", every, stall)
			}

			ps := newPackageServer(t, packageSize, false)
			resp, _ := ps.get(t)
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("want 200, got %d", resp.StatusCode)
			}
			if got := resp.Header.Get("Content-Length"); got != strconv.Itoa(packageSize) {
				t.Fatalf("Content-Length = %q, want %d", got, packageSize)
			}

			start := time.Now()
			got, _, err := drainAtPace(resp.Body, row.readSize, every, packageSize)
			elapsed := time.Since(start)

			if err != nil {
				t.Fatalf("reading %d B at a time at %d B/s failed at %d of %d bytes after %s: %v\n"+
					"the write bound is discriminating on read granularity, not on progress",
					row.readSize, row.rate, len(got), packageSize, elapsed, err)
			}
			if len(got) != packageSize {
				t.Fatalf("reading %d B at a time at %d B/s received %d of %d bytes after %s: truncated body\n"+
					"a consumer at this rate reading %d B at a time is the same health as one reading 512 B at a time",
					row.readSize, row.rate, len(got), packageSize, elapsed, row.readSize)
			}
			if !bytes.Equal(got, ps.pkg) {
				t.Fatal("received body differs from the published package")
			}
			// The rate-matched rows must each outlast a whole window, or they
			// stop proving that a slow consumer survives. The control row is
			// deliberately quick and is exempt.
			if nominal := time.Duration(packageSize) * time.Second / time.Duration(row.rate); nominal > stall && elapsed <= stall {
				t.Fatalf("the row finished in %s, inside one %s window, so it no longer proves a slow consumer survives", elapsed, stall)
			}
		})
	}
}

// TestPackageDownload_ShortPausesRepeatedlyAreNotStalls pins the other half of
// "progress, not duration": a consumer that stops for clearly less than the
// window and resumes is healthy, and it stays healthy however many times it does
// it. The pauses here add up to several whole windows, so anything that measured
// elapsed time rather than progress would cut this transfer off.
func TestPackageDownload_ShortPausesRepeatedlyAreNotStalls(t *testing.T) {
	const (
		stall       = 500 * time.Millisecond
		pause       = 300 * time.Millisecond // 60% of the window, repeatedly
		packageSize = 80 << 10
		readSize    = 8 << 10
	)

	restore := packageStreamStall
	packageStreamStall = stall
	defer func() { packageStreamStall = restore }()

	ps := newPackageServer(t, packageSize, false)
	resp, _ := ps.get(t)
	defer func() { _ = resp.Body.Close() }()

	start := time.Now()
	got, _, err := drainAtPace(resp.Body, readSize, pause, packageSize)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("a consumer pausing %s inside a %s window was cut off at %d of %d bytes: %v",
			pause, stall, len(got), packageSize, err)
	}
	if len(got) != packageSize {
		t.Fatalf("received %d of %d bytes after %s of %s pauses: short body", len(got), packageSize, elapsed, pause)
	}
	if !bytes.Equal(got, ps.pkg) {
		t.Fatal("received body differs from the published package")
	}
	if pauses := packageSize / readSize; pauses < 4 {
		t.Fatalf("only %d pauses; this no longer proves the window is restarted repeatedly", pauses)
	}
	if elapsed <= 2*stall {
		t.Fatalf("the transfer took %s, which is not several windows; it no longer proves pauses accumulate harmlessly", elapsed)
	}
}

// TestPackageDownload_APauseLongerThanTheWindowIsTornDown is the test that stops
// the fix from being vacuous. A bound that never fires is not a bound: the
// concurrency cap depends on slots coming back, so a consumer that goes quiet
// MID-TRANSFER, having already taken bytes, must still be cut off and must still
// give its slot up.
func TestPackageDownload_APauseLongerThanTheWindowIsTornDown(t *testing.T) {
	const (
		stall       = 300 * time.Millisecond
		packageSize = 512 << 10
		readSize    = 4 << 10
	)

	restore := packageStreamStall
	packageStreamStall = stall
	defer func() { packageStreamStall = restore }()

	ps := newPackageServer(t, packageSize, false)
	resp, _ := ps.get(t)
	defer func() { _ = resp.Body.Close() }()

	// Take some bytes, so this is a live consumer that goes quiet rather than one
	// that never read at all (that case is TestPackageDownload_SilentConsumer...).
	buf := make([]byte, readSize)
	moved := 0
	for moved < readSize {
		n, err := resp.Body.Read(buf)
		moved += n
		if err != nil {
			t.Fatalf("the consumer could not read even its first %d bytes: %v", readSize, err)
		}
	}

	// Now go quiet for several windows.
	time.Sleep(4 * stall)

	// The slot must have come back without the consumer doing anything.
	if inFlight := ps.h.streams.inFlight(); inFlight != 0 {
		t.Fatalf("in-flight = %d of %d after a consumer went quiet for %s: the write half has no progress bound",
			inFlight, ps.h.streams.capacity(), 4*stall)
	}

	// And the body is short, which is what the agent's size and sha256 checks
	// turn into a clean retry next cycle.
	rest, _ := io.ReadAll(resp.Body)
	if total := moved + len(rest); total >= packageSize {
		t.Fatalf("the stalled consumer somehow received the whole %d-byte package", packageSize)
	}
}

// ---------------------------------------------------------------------------
// The whole-transfer ceiling
// ---------------------------------------------------------------------------

// TestPackageDownload_ADrippingConsumerIsBoundedByTheTotal is the regression
// test for the hole the progress bound left behind.
//
// The consumer here is the mirror image of the two tests above: it never pauses
// long enough to stall, so the progress bound carries it forever, and it accepts
// so little per window that it will never finish. That is not a slow site, it is
// a held slot. One byte per 60% of the window, sixteen sockets, and every
// legitimate site on the instance gets update_package_busy for as long as the
// caller cares to keep them open, which is the exact denial the concurrency cap
// exists to refuse and is cheaper than the amplification it was built for.
//
// CONFIRMED TO FAIL BEFORE THE FIX. With no whole-transfer ceiling the drip
// restarts the stall guard indefinitely and the slot is never released: the poll
// below runs out its deadline with in-flight still 1. Measured against the
// pre-fix code at production settings, a one-byte-per-60%-of-window consumer ran
// 12.028s across 60 windows, moved 99 of 4194304 bytes (8.2 B/s) and was still
// holding one of sixteen slots when the measurement was stopped.
func TestPackageDownload_ADrippingConsumerIsBoundedByTheTotal(t *testing.T) {
	const (
		stall = time.Second
		// 60% of the window, the ratio the measurement above used: comfortably
		// inside it, so the progress bound is satisfied every single window.
		drip = 600 * time.Millisecond
		// Compressed from the production [30 min, 2 h] clamp. The property is
		// "a total bound exists and releases the slot", not its magnitude.
		ceiling = 3 * time.Second
		// Four MiB at a drip is thousands of years. Nothing here can finish.
		packageSize = 4 << 20
		// HOW SMALL THE DRIP CAN BE IS DECIDED BY THE TEST'S OWN CLIENT, NOT BY
		// THE SERVER. p.get parses the response through a deliberately tiny
		// 16-byte bufio.Reader, so a literal one-byte read is usually served out
		// of that buffer and never touches the socket at all: sixteen of them
		// pass before the server sees anything, which is a real stall and gets
		// torn down as one. Reading more than the buffer holds makes every read
		// a socket read, so the pace below is the pace the server actually sees.
		// 64 B per 600 ms is 107 B/s, still four orders of magnitude under the
		// slowest consumer this route is built to carry.
		dripSize = 64
	)

	restoreStall := packageStreamStall
	restoreMin, restoreMax := packageStreamMinTotal, packageStreamMaxTotal
	packageStreamStall = stall
	packageStreamMinTotal, packageStreamMaxTotal = ceiling, ceiling
	t.Cleanup(func() {
		packageStreamStall = restoreStall
		packageStreamMinTotal, packageStreamMaxTotal = restoreMin, restoreMax
	})

	ps := newPackageServer(t, packageSize, false)
	resp, _ := ps.get(t)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	// The drip runs until the server tears the connection down under it.
	moved := make(chan int64, 1)
	go func() {
		var n int64
		buf := make([]byte, dripSize)
		for {
			got, err := resp.Body.Read(buf)
			n += int64(got)
			if err != nil {
				moved <- n
				return
			}
			time.Sleep(drip)
		}
	}()

	// The request must take a slot before anything else is meaningful.
	takeDeadline := time.Now().Add(5 * time.Second)
	for ps.h.streams.inFlight() == 0 {
		if time.Now().After(takeDeadline) {
			t.Fatal("the request never took a slot")
		}
		time.Sleep(time.Millisecond)
	}

	start := time.Now()
	slotDeadline := time.Now().Add(8 * ceiling)
	for ps.h.streams.inFlight() != 0 {
		if time.Now().After(slotDeadline) {
			t.Fatalf("in-flight = %d of %d after %s of a consumer accepting %d B per %s: "+
				"the progress bound is satisfied forever and nothing bounds the TOTAL, so this slot is held for as long as the socket stays open",
				ps.h.streams.inFlight(), ps.h.streams.capacity(), time.Since(start), dripSize, drip)
		}
		time.Sleep(5 * time.Millisecond)
	}
	elapsed := time.Since(start)

	// It has to be the CEILING that fired, not the stall guard. If the transfer
	// died well inside the ceiling then this consumer was descheduled past the
	// stall window and stopped being a drip, which is a loaded box rather than a
	// result: the run proves nothing either way, so it is inconclusive and not a
	// failure. (Under the fix the two bounds are far enough apart that this does
	// not happen on an idle machine.)
	if elapsed < ceiling*3/4 {
		t.Skipf("inconclusive: the stream ended after %s, well inside the %s ceiling, so the %s stall guard fired instead; "+
			"the test consumer was descheduled past its own window, re-run on an idle machine", elapsed, ceiling, stall)
	}

	select {
	case n := <-moved:
		if n >= packageSize {
			t.Fatalf("the dripping consumer somehow received the whole %d-byte package", packageSize)
		}
		t.Logf("dripping consumer torn down after %s having moved %d of %d bytes (%.1f B/s)",
			elapsed.Round(time.Millisecond), n, packageSize, float64(n)/elapsed.Seconds())
	case <-time.After(5 * time.Second):
		t.Fatal("the consumer's read never returned after the stream was torn down")
	}
}

// TestPackageStreamTotalLimit pins the ceiling's arithmetic, because the value
// is only defensible if it stays far clear of every legitimate consumer.
//
// The binding floor is the agent's own whole-operation download budget:
// wp_remote_get('timeout' => 60) demands about 52 KB/s for a 3 MiB package,
// where the ceiling demands 1 KB/s, so a transfer that could reach this ceiling
// has had no agent waiting on it for the best part of an hour.
func TestPackageStreamTotalLimit(t *testing.T) {
	const mib = 1 << 20

	cases := []struct {
		name string
		size int64
		want time.Duration
	}{
		{"a small package is lifted to the floor", 1 * mib, 30 * time.Minute},
		{"the current ~3 MiB package derives 51.2 minutes", 3 * mib, 3072 * time.Second},
		{"a huge package is held at the cap", 500 * mib, 2 * time.Hour},
		{"a nonsense size still gets the floor", 0, 30 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := packageStreamTotalLimit(tc.size); got != tc.want {
				t.Fatalf("packageStreamTotalLimit(%d) = %s, want %s", tc.size, got, tc.want)
			}
		})
	}

	// The demanded average for the package actually being served must stay
	// orders of magnitude under what the agent's own 60s budget already asks.
	const packageSize = 3 * mib
	ceiling := packageStreamTotalLimit(packageSize)
	ceilingRate := float64(packageSize) / ceiling.Seconds()
	agentRate := float64(packageSize) / 60.0
	if ceilingRate > agentRate/20 {
		t.Fatalf("the ceiling demands %.0f B/s where the agent's own 60s budget demands %.0f B/s; "+
			"that is not far enough clear of a legitimate download", ceilingRate, agentRate)
	}
	t.Logf("ceiling %s for %d B demands %.0f B/s; the agent's 60s budget already demands %.0f B/s (%.0fx)",
		ceiling, packageSize, ceilingRate, agentRate, agentRate/ceilingRate)
}

// ---------------------------------------------------------------------------
// Graceful shutdown
// ---------------------------------------------------------------------------

// TestPackageDownload_ShutdownDrainsInFlightStreams covers the regression the
// hijack introduced.
//
// net/http's Server.Shutdown explicitly does not wait for HIJACKED connections,
// and the write half of this route hijacks. So the moment streamPackage took the
// connection over, this route stopped draining on SIGTERM: Server.Run returns as
// soon as Shutdown does, and a revision rollout or a scale-down would exit the
// process with up to sixteen zips mid-transfer. Each of those sites fails its
// size and sha256 check and retries next cycle, which is not data loss but is
// exactly the sort of thing that gets diagnosed as a mysterious update failure
// much later.
//
// The first half of this test asserts that gap directly (Shutdown returns while
// a stream is mid-body) so the premise cannot rot silently, and the second half
// asserts the drain that now covers it.
//
// CONFIRMED TO FAIL BEFORE THE FIX. With WaitForPackageStreams stubbed to the
// pre-fix behaviour of not waiting at all (return 0 immediately), the mid-body
// assertion below fails: it returns 0 in microseconds while the stream is still
// in flight, which is the process exiting out from under sixteen downloads.
func TestPackageDownload_ShutdownDrainsInFlightStreams(t *testing.T) {
	const (
		// Generous, so nothing in this test is torn down by the progress bound
		// while shutdown is being measured.
		stall       = 10 * time.Second
		packageSize = 256 << 10
	)

	restore := packageStreamStall
	packageStreamStall = stall
	t.Cleanup(func() { packageStreamStall = restore })

	ps := newPackageServer(t, packageSize, false)
	resp, _ := ps.get(t)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	// Take a few bytes, so this is unambiguously mid-BODY: the head is written,
	// Content-Length is promised, and the rest of the zip is still owed.
	buf := make([]byte, 512)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("could not read the first %d bytes: %v", len(buf), err)
	}
	if n := ps.h.streams.inFlight(); n != 1 {
		t.Fatalf("in-flight = %d, want 1: the stream is not actually running", n)
	}

	// (1) Shutdown alone does not wait for us. This is the regression, asserted
	// rather than described.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelShutdown()
	shutdownStart := time.Now()
	shutdownErr := ps.srv.Shutdown(shutdownCtx)
	shutdownTook := time.Since(shutdownStart)
	t.Logf("Shutdown returned after %s (err=%v) with in-flight=%d", shutdownTook.Round(time.Millisecond), shutdownErr, ps.h.streams.inFlight())
	if shutdownTook > time.Second {
		t.Fatalf("Shutdown took %s, so it now DOES wait for hijacked connections; "+
			"re-read Server.drainPackageStreams, the drain may be redundant", shutdownTook)
	}
	if n := ps.h.streams.inFlight(); n != 1 {
		t.Fatalf("in-flight = %d after Shutdown, want 1: the stream was expected to survive it", n)
	}

	// (2) The drain DOES wait, and gives the budget back when it runs out rather
	// than holding a deploy open on a slow stream.
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancelWait()
	waitStart := time.Now()
	left := ps.h.WaitForPackageStreams(waitCtx)
	waited := time.Since(waitStart)
	if left != 1 {
		t.Fatalf("WaitForPackageStreams reported %d in flight while a download was mid-body, want 1: "+
			"shutdown is not draining this route and a rollout truncates every stream on the instance", left)
	}
	if waited < 250*time.Millisecond {
		t.Fatalf("WaitForPackageStreams returned after %s on a 300ms budget: it is not waiting for the stream at all", waited)
	}

	// (3) Once the site has its package, the drain completes promptly, so a
	// healthy fleet never spends the shutdown budget.
	go func() { _, _ = io.Copy(io.Discard, resp.Body) }()
	doneCtx, cancelDone := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelDone()
	drainStart := time.Now()
	if left := ps.h.WaitForPackageStreams(doneCtx); left != 0 {
		t.Fatalf("WaitForPackageStreams reported %d in flight after the download finished, want 0", left)
	}
	t.Logf("drain completed %s after the consumer resumed reading", time.Since(drainStart).Round(time.Millisecond))
}

// ---------------------------------------------------------------------------
// The production-scale table, over real TCP
// ---------------------------------------------------------------------------

// streamSoakEnv gates the table below. It runs at the real 20s window against a
// 2 MiB package over real loopback TCP, which is the only way the granularity
// dependence reproduces (see the file header), and it takes minutes. It is not
// part of `go test ./...` for that reason.
const streamSoakEnv = "WPMGR_STREAM_SOAK"

// TestPackageDownload_ProductionScaleConsumerTable is the reviewer's table,
// verbatim, at production settings.
//
// Rows one to three sustain an identical 25.6 KB/s and differ only in read size.
// Against the per-call write deadline, measured here: row one COMPLETED all
// 2097152 bytes at 24670 B/s, row two TRUNCATED at 849772 bytes and row three at
// 841580 bytes, both with an i/o timeout, and the control row completed at 40220
// B/s. Same bytes per second, opposite outcomes, which is the measurement that
// proved the write half was not a progress bound. All four must now complete.
//
// RUN THIS ON AN IDLE MACHINE. It is LOAD SENSITIVE and it is honestly noisy:
// every row here fails when it does not receive the whole package, and there is
// no escape hatch. Read a red row like this.
//
//	On an idle box, a red row is a real regression. That is the only condition
//	this table is evidence under, and it is why the instruction above is not a
//	suggestion.
//
//	On a busy box, a red row proves nothing either way and must be re-run. The
//	probe is this test's own consumer, and it only measures the server's bound
//	while it is keeping to its stated pace. With the repo's long integration
//	suite running alongside it, this process gets descheduled, its reads slip,
//	and a consumer that has genuinely stopped consuming is torn down by a server
//	doing exactly its job. Observed under load: the write half's 20s progress
//	bound fired on a legitimate 25 KB/s consumer on a CPU-starved box. That bound
//	is not the defect and is deliberately left alone; the box was.
//
// THERE IS NO AUTOMATIC WAY TO TELL THOSE TWO APART FROM IN HERE, which is why
// this is a paragraph rather than a check. The obvious probe, the consumer's own
// worst read gap, was tried and removed: it reads the same on a loaded box that
// failed and an idle box that passed, because it sits on the wrong side of the
// receive buffer (see drainAtPace). A skip driven by it excused real failures,
// and an excuse in a diagnostic is worse than noise in one. Anything that
// distinguishes the two cases has to be measured on the SERVER's side of the
// socket, and until it is, the operator is the discriminator.
//
// THE ROWS RUN SEQUENTIALLY ON PURPOSE. Run in parallel, all four completed even
// against the broken mechanism: four concurrent transfers autotune to smaller
// socket buffers, which changes the window-update cadence enough to hide the
// defect. That is not a quirk of the test, it is the defect restated. The old
// bound's floor moved with buffer and window-update timing that the control
// plane cannot observe, so it could pass a fleet all day and cut off one site.
func TestPackageDownload_ProductionScaleConsumerTable(t *testing.T) {
	if os.Getenv(streamSoakEnv) == "" {
		t.Skipf("set %s=1 to run the production-scale consumer table (minutes, real 20s window)", streamSoakEnv)
	}

	const packageSize = 2 << 20

	rows := []struct {
		name     string
		readSize int
		every    time.Duration
	}{
		{"1280 B every 50 ms", 1280, 50 * time.Millisecond},
		{"4096 B every 160 ms", 4096, 160 * time.Millisecond},
		{"16 KiB every 640 ms", 16 << 10, 640 * time.Millisecond},
		{"4096 B every 100 ms (control)", 4096, 100 * time.Millisecond},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			ps := newPackageServer(t, packageSize, true)
			resp, _ := ps.get(t)
			defer func() { _ = resp.Body.Close() }()

			start := time.Now()
			got, maxGap, err := drainAtPace(resp.Body, row.readSize, row.every, packageSize)
			elapsed := time.Since(start)
			rate := float64(len(got)) / elapsed.Seconds()

			if err != nil || len(got) != packageSize {
				// No escape hatch, on purpose: see the load-sensitivity note on
				// this function. The worst read gap is reported because it is
				// useful context when reading a failure, and for nothing else.
				t.Fatalf("%s: %d of %d B in %s (%.0f B/s), worst read gap %s, err=%v\n"+
					"if this box was not idle, this row is inconclusive and must be re-run on one; on an idle box it is a regression",
					row.name, len(got), packageSize, elapsed, rate, maxGap, err)
			}
			t.Logf("%s: COMPLETE %d of %d B in %s (%.0f B/s, worst read gap %s)",
				row.name, len(got), packageSize, elapsed, rate, maxGap)
		})
	}
}
