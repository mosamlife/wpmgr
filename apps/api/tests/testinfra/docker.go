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
	"testing"

	"github.com/testcontainers/testcontainers-go"
)

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
func SkipIfDockerUnavailable(t *testing.T, ctx context.Context, stage string) {
	t.Helper()
	provider, err := testcontainers.ProviderDocker.GetProvider()
	if err != nil {
		SetupSkipf(t, err, stage+" (docker provider unavailable)")
		return
	}
	if err := provider.Health(ctx); err != nil {
		SetupSkipf(t, err, stage+" (docker daemon unreachable)")
	}
}
