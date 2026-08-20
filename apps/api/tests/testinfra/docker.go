// Package testinfra holds small, dependency-free helpers shared by the
// apps/api integration test packages (tests, tests/gh458, and others as they
// adopt it) for telling "Docker is not reachable at all" apart from "this
// container failed to start despite Docker being up".
//
// Routing the second case to a skip is this project's signature defect: a
// package that reports "ok" over a suite that asserted nothing. See
// test(api): fail closed when a testcontainer fails to start with Docker up.
package testinfra

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
)

// DockerHealthProbeTimeout bounds the daemon-reachability probe below.
//
// testcontainers-go v0.42.0's DockerProvider.Health passes the caller's
// context straight to the daemon's /info request and adds no deadline of its
// own, so an unbounded context turns a wedged or unresponsive daemon into a
// hang: the probe blocks until the whole package trips the Go test timeout,
// and the resulting panic names whichever test happened to be running rather
// than the daemon.
//
// A reachable daemon answers /info in single-digit milliseconds. The slow
// legitimate case is a cold Docker Desktop VM whose API socket is accepting
// connections while the engine is still coming up, which still answers well
// inside a couple of seconds. 10s clears both comfortably while failing two
// orders of magnitude faster than the 10m default test timeout.
const DockerHealthProbeTimeout = 10 * time.Second

// SetupFatalf fails the test with a message that cannot be mistaken for an
// assertion made by the test body itself. A full-package run starts many
// testcontainers over several minutes, and under that load an infrastructure
// hiccup at a setup stage (container start, connection string, migrate,
// connect, role provisioning) otherwise surfaces as a bare "%v" attached to
// whatever test happened to be running — which reads exactly like that
// test's own assertion failed.
func SetupFatalf(t *testing.T, err error, stage string) {
	t.Helper()
	t.Fatalf("SETUP FAILURE (infrastructure, not the test's own assertion) at stage=%q: %v", stage, err)
}

// SetupSkipf is SetupFatalf's skip counterpart, used only for "Docker is not
// available on this machine at all" — the one setup failure that is not a
// mid-run flake and that every test depending on Docker would hit
// identically, so skipping is the honest signal.
func SetupSkipf(t *testing.T, err error, stage string) {
	t.Helper()
	t.Skipf("SETUP SKIP (infrastructure, not the test's own assertion) at stage=%q: %v", stage, err)
}

// SkipIfDockerUnavailable positively probes whether the Docker/OCI provider
// testcontainers will use is reachable at all, and skips (via SetupSkipf)
// only if it is not. This is the ONLY thing that may resolve to a skip:
// "the daemon cannot be reached" is checked directly, up front, before any
// container is asked to start, rather than being inferred from whatever
// error a later container-start call happens to return.
//
// That distinction matters because a container that fails to START despite
// Docker being reachable (bad image tag, no space left, registry pull
// failure, resource limits) is a real setup failure, not "Docker is not
// installed here" — callers must route that to SetupFatalf, never infer a
// skip from it. Routing a start failure to Skip is exactly the failure mode
// this helper exists to eliminate: a package-level "ok" over a suite that
// asserted nothing.
//
// The probe itself is bounded by DockerHealthProbeTimeout — see
// classifyDockerHealth for the three outcomes and why an unresponsive daemon
// is a hard failure rather than a skip.
func SkipIfDockerUnavailable(t *testing.T, ctx context.Context, stage string) {
	t.Helper()
	provider, err := testcontainers.ProviderDocker.GetProvider()
	if err != nil {
		SetupSkipf(t, err, stage+" (docker provider unavailable)")
		return
	}
	outcome, herr := classifyDockerHealth(ctx, DockerHealthProbeTimeout, provider.Health)
	switch outcome {
	case dockerReachable:
		return
	case dockerUnresponsive:
		SetupFatalf(t, herr, stage+" (docker daemon unresponsive)")
	default:
		SetupSkipf(t, herr, stage+" (docker daemon unreachable)")
	}
}

// dockerProbeOutcome is what a bounded health probe concluded.
type dockerProbeOutcome int

const (
	// dockerReachable: the daemon answered, and answered in time.
	dockerReachable dockerProbeOutcome = iota
	// dockerUnreachable: the daemon refused or could not be dialled at all —
	// the "Docker is not available on this machine" case, and the only one
	// that may resolve to a skip.
	dockerUnreachable
	// dockerUnresponsive: the probe ran out of time instead of getting an
	// answer. The daemon is there but wedged, which is a broken machine, not
	// a machine without Docker. Skipping would hide it behind a green
	// package, so this is routed to SetupFatalf.
	dockerUnresponsive
)

// classifyDockerHealth runs health under an explicit timeout and classifies
// the result. It is separated from SkipIfDockerUnavailable so the bound can be
// exercised against an injected health func that never returns — proving the
// probe returns rather than hangs without touching the real daemon.
func classifyDockerHealth(ctx context.Context, timeout time.Duration, health func(context.Context) error) (dockerProbeOutcome, error) {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	err := health(probeCtx)
	if err == nil {
		return dockerReachable, nil
	}
	switch {
	case ctx.Err() != nil:
		// The CALLER's context ended first; the daemon never got its chance.
		// Not a skip either: the run is being torn down or misconfigured, and
		// saying "Docker is unavailable" would be a false diagnosis.
		return dockerUnresponsive, fmt.Errorf(
			"docker health probe context ended before the daemon answered (caller ctx: %v): %w", ctx.Err(), err)
	case probeCtx.Err() != nil:
		return dockerUnresponsive, fmt.Errorf(
			"docker daemon did not answer a health probe within %s: the daemon is wedged or unresponsive, "+
				"not absent -- restart it and re-run: %w", timeout, err)
	default:
		return dockerUnreachable, err
	}
}
