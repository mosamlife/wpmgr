package testinfra

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestClassifyDockerHealth_BoundsAWedgedDaemon proves the health probe returns
// instead of hanging when the daemon accepts the request and never answers —
// the shape testcontainers-go v0.42.0's DockerProvider.Health cannot bound on
// its own, because it hands the caller's context straight to the /info request.
//
// The fake health func blocks until its context is done, which is exactly what
// a wedged daemon does to an unbounded probe. The assertion is run under an
// independent watchdog so a regression that removes the bound fails with a
// legible message instead of hanging until the package test timeout.
func TestClassifyDockerHealth_BoundsAWedgedDaemon(t *testing.T) {
	wedged := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}

	type result struct {
		outcome dockerProbeOutcome
		err     error
	}
	done := make(chan result, 1)
	go func() {
		o, err := classifyDockerHealth(context.Background(), 50*time.Millisecond, wedged)
		done <- result{o, err}
	}()

	select {
	case got := <-done:
		if got.outcome != dockerUnresponsive {
			t.Fatalf("outcome = %d, want dockerUnresponsive (%d): a daemon that never answers must be a hard "+
				"failure, not a skip that hides it", got.outcome, dockerUnresponsive)
		}
		if got.err == nil {
			t.Fatal("err = nil, want an actionable message naming the timeout")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("classifyDockerHealth did not return within 5s against a health func that never answers: " +
			"the probe is unbounded and a wedged daemon will hang the suite until the Go test timeout")
	}
}

// TestClassifyDockerHealth_RefusedDaemonStillSkips is the over-fire twin: the
// bound must not convert a genuine "no Docker on this machine" into a hard
// failure. A health func that returns promptly with a connection error is
// still the one case that may honestly skip.
func TestClassifyDockerHealth_RefusedDaemonStillSkips(t *testing.T) {
	refused := errors.New("cannot connect to the Docker daemon at unix:///var/run/docker.sock")
	outcome, err := classifyDockerHealth(context.Background(), 50*time.Millisecond,
		func(context.Context) error { return refused })
	if outcome != dockerUnreachable {
		t.Fatalf("outcome = %d, want dockerUnreachable (%d)", outcome, dockerUnreachable)
	}
	if !errors.Is(err, refused) {
		t.Fatalf("err = %v, want the daemon's own error preserved", err)
	}
}

// TestClassifyDockerHealth_HealthyDaemonPasses is the other over-fire twin: a
// daemon that answers well inside the bound must be reported reachable with no
// error, or every Docker-backed package would fail or skip on a working
// machine.
func TestClassifyDockerHealth_HealthyDaemonPasses(t *testing.T) {
	outcome, err := classifyDockerHealth(context.Background(), DockerHealthProbeTimeout,
		func(context.Context) error { return nil })
	if outcome != dockerReachable || err != nil {
		t.Fatalf("outcome = %d, err = %v; want dockerReachable (%d) with no error", outcome, err, dockerReachable)
	}
}
