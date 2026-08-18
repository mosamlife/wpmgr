package tests

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
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

	// --- Oracle B: install state on the no-header register path -------------
	unclaimedPool := startPostgres(t)
	unclaimedSvc, _ := newAuthStack(unclaimedPool)
	unclaimedSvc.SetBootstrapClaimSecret(testClaim)

	selfServe := func(svc *auth.Service, pool interface{}) func() {
		i := 0
		return func() {
			i++
			_ = svc.RegisterSelfServe(ctx, auth.RegisterInput{
				Email:    uniqueEmail(i),
				Password: "a-very-strong-password",
				Name:     "Probe",
			}, makeCreateTenant(t, unclaimedPool))
		}
	}
	unclaimed := timeN(selfServe(unclaimedSvc, unclaimedPool))

	claimedPool := startPostgres(t)
	claimedSvc, _ := newAuthStack(claimedPool)
	claimedSvc.SetBootstrapClaimSecret(testClaim)
	if _, err := claimedSvc.Bootstrap(ctx, auth.RegisterInput{
		Email: "owner@example.com", Password: "a-very-strong-password", Name: "Owner",
	}, testClaim); err != nil {
		t.Fatalf("claim the install: %v", err)
	}
	j := 0
	claimed := timeN(func() {
		j++
		_ = claimedSvc.RegisterSelfServe(ctx, auth.RegisterInput{
			Email:    uniqueEmail(1000 + j),
			Password: "a-very-strong-password",
			Name:     "Probe",
		}, makeCreateTenant(t, claimedPool))
	})

	t.Logf("ORACLE B (install state, self-serve register, no claim header) MEDIAN over %d: UNCLAIMED=%v CLAIMED=%v",
		samples, unclaimed, claimed)
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
