package tests

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
)

// TestMeasureFirstRunRefusalTiming reports the two durations a refusal on this
// path could otherwise be told apart by. It is a MEASUREMENT, printed with -v,
// not a threshold assertion: wall-clock ratios on a laptop under a container
// are too noisy to gate a build on, and a flaky timing gate gets switched off,
// after which it guards nothing.
//
// What it is for: the claim that "every refusal is indistinguishable" is a
// claim about duration as well as bytes, and this is the command that checks
// it. Run it after any change to Service.Bootstrap or Service.RegisterSelfServe
// that adds a branch, and compare the medians.
//
//	go test -count=1 -v -run TestMeasureFirstRunRefusalTiming ./tests/
func TestMeasureFirstRunRefusalTiming(t *testing.T) {
	const samples = 15

	median := func(ds []time.Duration) time.Duration {
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		return ds[len(ds)/2]
	}
	timeN := func(fn func()) time.Duration {
		out := make([]time.Duration, 0, samples)
		for i := 0; i < samples; i++ {
			start := time.Now()
			fn()
			out = append(out, time.Since(start))
		}
		return median(out)
	}

	// --- Oracle A: claim-correctness against an already-owned install -------
	ownedPool := startPostgres(t)
	ctx := context.Background()
	ownedSvc, _ := newAuthStack(ownedPool)
	ownedSvc.SetBootstrapClaimSecret(testClaim)
	if _, err := ownedSvc.Bootstrap(ctx, auth.RegisterInput{
		Email: "owner@example.com", Password: "a-very-strong-password", Name: "Owner",
	}, testClaim); err != nil {
		t.Fatalf("claim the install: %v", err)
	}

	probe := func(claim string) func() {
		return func() {
			_, _ = ownedSvc.Bootstrap(ctx, auth.RegisterInput{
				Email: "probe@example.com", Password: "a-very-strong-password", Name: "Probe",
			}, claim)
		}
	}
	wrongLen := timeN(probe("short"))
	sameLen := timeN(probe("test-provisioning-claim-valuX"))
	correct := timeN(probe(testClaim))

	t.Logf("ORACLE A (claim-correctness, owned install) MEDIAN over %d: wrong-length=%v same-length-wrong=%v CORRECT-claim(spent)=%v",
		samples, wrongLen, sameLen, correct)
	assertRatioUnder(t, "oracle A (claim-correctness)", wrongLen, correct)
	assertRatioUnder(t, "oracle A (claim-correctness)", sameLen, correct)

	// --- Oracle B: install state on the no-header register path -------------
	unclaimedPool := startPostgres(t)
	unclaimedSvc, _ := newAuthStack(unclaimedPool)
	unclaimedSvc.SetBootstrapClaimSecret(testClaim)

	// The error is checked rather than discarded: RegisterSelfServe returns nil
	// on every intended outcome here, so a non-nil one means the call did not do
	// the work being timed and the sample measures nothing. A median over
	// samples that all failed early would look exactly like a fast path.
	selfServe := func(svc *auth.Service, pool *db.Pool, base int) func() {
		i := base
		return func() {
			i++
			if err := svc.RegisterSelfServe(ctx, auth.RegisterInput{
				Email:    uniqueEmail(i),
				Password: "a-very-strong-password",
				Name:     "Probe",
			}, makeCreateTenant(t, pool)); err != nil {
				t.Fatalf("self-serve sample: %v", err)
			}
		}
	}
	unclaimed := timeN(selfServe(unclaimedSvc, unclaimedPool, 0))

	claimedPool := startPostgres(t)
	claimedSvc, _ := newAuthStack(claimedPool)
	claimedSvc.SetBootstrapClaimSecret(testClaim)
	if _, err := claimedSvc.Bootstrap(ctx, auth.RegisterInput{
		Email: "owner@example.com", Password: "a-very-strong-password", Name: "Owner",
	}, testClaim); err != nil {
		t.Fatalf("claim the install: %v", err)
	}
	claimed := timeN(selfServe(claimedSvc, claimedPool, 1000))

	t.Logf("ORACLE B (install state, self-serve register, no claim header) MEDIAN over %d: UNCLAIMED=%v CLAIMED=%v",
		samples, unclaimed, claimed)
	assertRatioUnder(t, "oracle B (install state)", unclaimed, claimed)
}

// maxRefusalRatio is the ceiling this test holds the two paths to.
//
// WHY 4x, WHICH IS NOWHERE NEAR TIGHT. The property being defended is that
// neither pair is separated by ORDERS of magnitude: before the equalising work
// these ratios were about 112000x and 100x, and afterwards they measure about
// 1.04x and 1.13x. Anything that reintroduces a skipped argon2 hash — the whole
// mechanism, and the only way back to a usable difference — lands far above 4x,
// so the ceiling catches the regression it exists to catch.
//
// It cannot flake, and that is deliberate rather than hopeful. The gap between
// the measured 1.13x and the 4x ceiling is not a safety margin, it is roughly
// three times the whole measurement. For a green run to go red on noise alone,
// one median of fifteen would have to move by more than 3x — on a path whose
// cost is dominated by a fixed argon2 computation that runs identically on both
// sides. A tight bound (say 1.5x) would have been the flaky one; this project
// switches off tests that redden correct work, and then they guard nothing.
//
// If this fires, do not raise the number. Read the medians in the log line
// above it: something stopped doing equal work on both paths.
const maxRefusalRatio = 4.0

func assertRatioUnder(t *testing.T, label string, a, b time.Duration) {
	t.Helper()
	lo, hi := a, b
	if lo > hi {
		lo, hi = hi, lo
	}
	if lo <= 0 {
		t.Fatalf("%s: a median of %v is not a usable measurement", label, lo)
	}
	ratio := float64(hi) / float64(lo)
	if ratio > maxRefusalRatio {
		t.Fatalf("%s: the two paths differ by %.1fx (%v vs %v), ceiling is %.1fx — one of them stopped doing the work the other does",
			label, ratio, a, b, maxRefusalRatio)
	}
	t.Logf("%s: %.2fx separation (ceiling %.1fx)", label, ratio, maxRefusalRatio)
}

func uniqueEmail(i int) string {
	return "probe-" + time.Now().Format("150405.000000000") + "-" + itoa(i) + "@example.com"
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
