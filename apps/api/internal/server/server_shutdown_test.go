package server

// THE SHUTDOWN DRAIN IS EXERCISED THROUGH Run, DELIBERATELY.
//
// The drain itself has a test next to the handler it belongs to
// (internal/agent, TestPackageDownload_ShutdownDrainsInFlightStreams). What that
// test cannot see is whether anything CALLS it. Run's single
// s.drainPackageStreams(shutdownCtx) line is the whole of the fix, and deleting
// it leaves every package-stream test in the repo green, because
// WaitForPackageStreams keeps working perfectly while nobody waits on it.
//
// The failure that reintroduces is silent and delayed. net/http's
// Server.Shutdown does not wait for HIJACKED connections and the package route
// hijacks, so without the drain the process exits the moment Shutdown returns:
// every revision rollout truncates whatever agent downloads are mid-transfer,
// each of those sites fails its size and sha256 check, and months later the
// symptom reads as "agent updates fail sometimes" with nothing pointing at a
// deploy.
//
// So the test below drives the real Server.Run, over a real listener, with a
// real download mid-body, and pins the shutdown timing in BOTH directions: an
// in-flight stream holds the return, and an idle instance is not held at all. A
// drain that made every rollout wait out the full budget would be a worse defect
// than the one being guarded against, which is why the idle bound is asserted
// just as tightly as the busy one.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agent"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentplugin"
	"github.com/mosamlife/wpmgr/apps/api/internal/config"
)

// probeVersion is the published release the probe serves. It has to be a usable
// object-key segment or the download route refuses it before any byte moves.
const probeVersion = "0.10.6-test"

// probeReleaseStore is the agent.ManifestStore the probe serves from. It holds
// both objects in memory and imposes no delay of its own, so the ONLY thing
// pacing a transfer is the consumer in the test.
type probeReleaseStore struct {
	manifest []byte
	object   []byte
}

func (s *probeReleaseStore) GetViaPresign(_ context.Context, key string) (io.ReadCloser, error) {
	if strings.HasSuffix(key, ".json") {
		return io.NopCloser(bytes.NewReader(s.manifest)), nil
	}
	return io.NopCloser(bytes.NewReader(s.object)), nil
}

func (s *probeReleaseStore) GetStreamViaPresign(ctx context.Context, key string) (io.ReadCloser, error) {
	return s.GetViaPresign(ctx, key)
}

func (s *probeReleaseStore) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://storage.invalid/" + key, nil
}

// shutdownProbe is one Server running its real Run loop on a loopback listener,
// serving the real package download route.
type shutdownProbe struct {
	h    *agent.UpdateHandler
	addr string
	path string
	size int

	done      chan error
	cancel    context.CancelFunc
	signalled time.Time
	awaited   bool
}

// freeLoopbackAddr reserves a loopback port and immediately gives it back.
//
// Run calls ListenAndServe on the address it was configured with and there is no
// way to hand it a listener, so the port has to be known before Run starts. The
// window between releasing the port here and Run binding it is the only source
// of flake, it is microseconds wide, and if something does take the port first
// Run returns the listen error at once and waitListening reports THAT rather
// than hanging.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a loopback port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release the reserved port %s: %v", addr, err)
	}
	return addr
}

// newShutdownProbe wires the real UpdateHandler onto a real Gin engine, hands it
// to a real Server, and starts Run. budget becomes the shutdown budget, so each
// case can pick a window it can measure against.
func newShutdownProbe(t *testing.T, packageSize int, budget time.Duration) *shutdownProbe {
	t.Helper()

	pkg := make([]byte, packageSize)
	for i := range pkg {
		pkg[i] = byte(i % 251)
	}
	manifest, err := json.Marshal(map[string]any{
		"slug":               agentplugin.SlugSelfHosted,
		"plugin":             agentplugin.SlugSelfHosted + "/" + agentplugin.SlugSelfHosted + ".php",
		"version":            probeVersion,
		"min_version":        "0.0.0",
		"package_object_key": "agent-releases/" + probeVersion + "/" + agentplugin.SlugSelfHosted + ".zip",
		"package_sha256":     strings.Repeat("ab", 32),
		"package_size":       packageSize,
	})
	if err != nil {
		t.Fatalf("marshal release manifest: %v", err)
	}

	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate signing key: %v", err)
	}
	signer, err := agentcmd.NewSigner(base64.StdEncoding.EncodeToString(priv))
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}

	h := agent.NewUpdateHandler(&probeReleaseStore{manifest: manifest, object: pkg}, signer, time.Minute)
	h.EnablePackageServing(signer, "")

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	h.RegisterPublic(engine)

	siteID := uuid.New()
	token, _, err := signer.MintPackageToken(time.Now(), siteID.String(), probeVersion, time.Minute)
	if err != nil {
		t.Fatalf("mint package token: %v", err)
	}

	// Built by hand rather than through New: New wants the whole dependency
	// graph (pool, sessions, every handler), and none of it participates in the
	// property under test. Everything Run actually touches on the way down is
	// real: the http.Server newHTTPServer builds, the shutdown budget from
	// config, and the UpdateHandler the drain consults.
	srv := &Server{
		deps: Deps{
			Config:       config.Config{Shutdown: config.ShutdownConfig{Timeout: budget}},
			UpdateAgentH: h,
		},
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		http: newHTTPServer(freeLoopbackAddr(t), engine),
	}

	ctx, cancel := context.WithCancel(context.Background())
	p := &shutdownProbe{
		h:    h,
		addr: srv.http.Addr,
		path: agent.PackageRoutePrefix + "/" + siteID.String() +
			"?" + agent.PackageTokenQueryParam + "=" + url.QueryEscape(token),
		size:   packageSize,
		done:   make(chan error, 1),
		cancel: cancel,
	}
	go func() { p.done <- srv.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		_ = srv.http.Close()
		if !p.awaited {
			select {
			case <-p.done:
			case <-time.After(5 * time.Second):
			}
		}
	})

	p.waitListening(t)
	return p
}

// waitListening blocks until Run's listener is accepting, so a case can never
// mistake "the server has not started yet" for anything else.
func (p *shutdownProbe) waitListening(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", p.addr, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			select {
			case runErr := <-p.done:
				p.awaited = true
				t.Fatalf("Run returned before the server was listening on %s: %v", p.addr, runErr)
			default:
			}
			t.Fatalf("the server never accepted a connection on %s: %v", p.addr, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// get issues the package download on a raw connection and parses the response
// head with the stdlib, which is also what validates the hand-written head the
// hijacked path writes. The bufio is deliberately tiny so this test's own reads
// reach the socket instead of being served out of a client-side buffer.
func (p *shutdownProbe) get(t *testing.T) *http.Response {
	t.Helper()
	conn, err := net.DialTimeout("tcp", p.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", p.addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, "GET "+p.path+" HTTP/1.1\r\nHost: cp.test\r\n\r\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}
	_ = conn.SetWriteDeadline(time.Time{})

	resp, err := http.ReadResponse(bufio.NewReaderSize(conn, 16), nil)
	if err != nil {
		t.Fatalf("read response head: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("package download returned %d, want 200", resp.StatusCode)
	}
	return resp
}

// signalShutdown is the SIGTERM: it cancels Run's context and starts the clock
// the timing assertions are measured against.
func (p *shutdownProbe) signalShutdown() {
	p.signalled = time.Now()
	p.cancel()
}

// awaitRun returns how long Run took to come back after signalShutdown.
func (p *shutdownProbe) awaitRun(t *testing.T, limit time.Duration) time.Duration {
	t.Helper()
	p.awaited = true
	select {
	case err := <-p.done:
		waited := time.Since(p.signalled)
		if err != nil {
			t.Fatalf("Run returned %v after %s; the shutdown path is expected to complete cleanly here", err, waited)
		}
		return waited
	case <-time.After(limit):
		t.Fatalf("Run did not return within %s of the shutdown signal, with %d package stream(s) in flight",
			limit, p.h.PackageStreamsInFlight())
		return 0
	}
}

// TestRun_ShutdownDrainsInFlightPackageStreams drives the REAL Run shutdown
// path. See the file header for why the drain's own unit test is not enough.
//
// CONFIRMED TO FAIL WITHOUT THE FIX. With the s.drainPackageStreams(shutdownCtx)
// line deleted from Run, the first two cases fail: Run comes back in tens of
// microseconds (measured: 74.125µs against a 600ms budget) while a download is
// still mid-body, which is the process exiting out from under every stream on
// the instance. The third case passes either way by design, because it exists to
// stop the drain being "fixed" into a blanket wait that slows every rollout.
//
// A PASSING RUN STILL PRINTS ONE HANDLER ERROR. The first case deliberately
// abandons its download, so when the test closes that connection the handler
// logs "package download stream aborted ... broken pipe" on the default logger.
// That is the abandoned consumer being noticed, not a failure.
func TestRun_ShutdownDrainsInFlightPackageStreams(t *testing.T) {
	// Far larger than the loopback send buffer plus receive window, so a consumer
	// that takes a few bytes and stops leaves the server genuinely blocked
	// mid-body rather than parked with the whole package already buffered.
	const packageSize = 4 << 20

	t.Run("a stream mid-body holds the return until the budget expires", func(t *testing.T) {
		// Short enough to measure quickly, long enough that the microseconds an
		// undrained shutdown takes cannot be confused with it.
		const budget = 600 * time.Millisecond

		p := newShutdownProbe(t, packageSize, budget)
		resp := p.get(t)
		defer func() { _ = resp.Body.Close() }()

		head := make([]byte, 512)
		if _, err := io.ReadFull(resp.Body, head); err != nil {
			t.Fatalf("could not read the first %d body bytes: %v", len(head), err)
		}
		if n := p.h.PackageStreamsInFlight(); n != 1 {
			t.Fatalf("in-flight = %d after the body started, want 1: this consumer is not holding a stream open, "+
				"so the case proves nothing (a package smaller than the socket buffers would do this)", n)
		}

		// The consumer now goes quiet for good, so the drain can only end on the
		// budget. That is the deploy-safety bound: a slow stream can consume time
		// Shutdown did not, and never a second past Shutdown.Timeout.
		p.signalShutdown()
		waited := p.awaitRun(t, budget+5*time.Second)

		if waited < budget*3/4 {
			t.Fatalf("Run returned %s after the shutdown signal with a package download mid-body on a %s budget: "+
				"Shutdown does not wait for hijacked connections, so this means nothing is draining them; "+
				"check that Run still calls drainPackageStreams before it returns", waited, budget)
		}
		if waited > budget+2*time.Second {
			t.Fatalf("Run returned %s after the shutdown signal on a %s budget: the drain is outliving the budget "+
				"and would hold a rollout open on a slow stream", waited, budget)
		}
		t.Logf("Run returned %s after the signal on a %s budget with a stream mid-body", waited.Round(time.Millisecond), budget)
	})

	t.Run("the return comes back when the stream finishes, not on the clock", func(t *testing.T) {
		// Generous, so anything that waits out the budget instead of waiting for
		// the streams is unmistakable in the timing.
		const (
			budget = 10 * time.Second
			pause  = 200 * time.Millisecond
		)

		p := newShutdownProbe(t, packageSize, budget)
		resp := p.get(t)
		defer func() { _ = resp.Body.Close() }()

		head := make([]byte, 512)
		if _, err := io.ReadFull(resp.Body, head); err != nil {
			t.Fatalf("could not read the first %d body bytes: %v", len(head), err)
		}

		p.signalShutdown()

		// While the site is still mid-download, Run must be held.
		time.Sleep(pause)
		select {
		case err := <-p.done:
			p.awaited = true
			t.Fatalf("Run returned %s after the shutdown signal (err=%v) while a package download was mid-body: "+
				"the site is left with a truncated zip that fails its size and sha256 check",
				time.Since(p.signalled).Round(time.Millisecond), err)
		default:
		}

		// The site now takes the rest of its package, and the drain must end with
		// it rather than sitting out the remaining budget.
		rest, err := io.Copy(io.Discard, resp.Body)
		if err != nil {
			t.Fatalf("the download did not survive the shutdown: %v", err)
		}
		if got := int64(len(head)) + rest; got != int64(p.size) {
			t.Fatalf("the site received %d of %d bytes across the shutdown: it was cut off mid-package", got, p.size)
		}

		waited := p.awaitRun(t, budget+5*time.Second)
		if waited >= budget/4 {
			t.Fatalf("Run took %s of its %s budget after a download that completed in well under it: "+
				"the drain is waiting out the clock instead of waiting for the streams", waited, budget)
		}
		t.Logf("Run returned %s after the signal, having carried a %d-byte download to completion first",
			waited.Round(time.Millisecond), p.size)
	})

	t.Run("an idle shutdown is not slowed", func(t *testing.T) {
		// A finished download, not an untouched server: this way the case also
		// covers a stream that HAS completed, so a slot that failed to come back
		// shows up here as a rollout that suddenly takes the whole budget.
		const budget = 10 * time.Second

		p := newShutdownProbe(t, packageSize, budget)
		resp := p.get(t)
		defer func() { _ = resp.Body.Close() }()

		got, err := io.Copy(io.Discard, resp.Body)
		if err != nil {
			t.Fatalf("download failed: %v", err)
		}
		if got != int64(p.size) {
			t.Fatalf("received %d of %d bytes", got, p.size)
		}

		// The handler releases its slot just after the last byte, so give it that
		// moment before calling the instance idle.
		idleBy := time.Now().Add(2 * time.Second)
		for p.h.PackageStreamsInFlight() != 0 {
			if time.Now().After(idleBy) {
				t.Fatalf("in-flight = %d two seconds after the download completed: the stream slot was never released",
					p.h.PackageStreamsInFlight())
			}
			time.Sleep(2 * time.Millisecond)
		}

		p.signalShutdown()
		waited := p.awaitRun(t, budget+5*time.Second)

		// Measured at about 1 ms. The bound is loose enough for a busy box and far
		// tighter than any drain that waited on an empty semaphore, which is the
		// mistake this case exists to catch: a shutdown made slow for everybody in
		// the name of protecting the few instances that are mid-stream.
		if waited > 250*time.Millisecond {
			t.Fatalf("an idle shutdown took %s on a %s budget: nothing was in flight, so the drain must return at once; "+
				"anything else adds this to EVERY revision rollout", waited, budget)
		}
		t.Logf("idle shutdown returned %s after the signal", waited.Round(time.Millisecond))
	})
}
