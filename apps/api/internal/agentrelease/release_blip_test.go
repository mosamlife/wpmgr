package agentrelease_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentrelease"
)

// blipStore is an agentrelease.Store that fails on the listed 1-based call
// numbers and serves the manifest on every other call: one transient presign
// or storage failure, exactly as reported in production.
// failFrom, when positive, additionally fails every call from that 1-based
// number onward: storage that breaks and stays broken, i.e. a sustained outage
// rather than a blip.
type blipStore struct {
	version   string
	failCalls map[int]bool
	failFrom  int
	calls     int
}

func (b *blipStore) GetViaPresign(_ context.Context, key string) (io.ReadCloser, error) {
	b.calls++
	if key != agentrelease.ManifestKey {
		return nil, errors.New("unexpected key: " + key)
	}
	if b.failCalls[b.calls] || (b.failFrom > 0 && b.calls >= b.failFrom) {
		return nil, errors.New("simulated: transient object storage failure")
	}
	return io.NopCloser(strings.NewReader(`{"version":"` + b.version + `"}`)), nil
}

// fleetOn builds n sites all reporting the same agent version.
func fleetOn(n int, version string) []agentrelease.SiteAgentVersion {
	rows := make([]agentrelease.SiteAgentVersion, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, site(version))
	}
	return rows
}

func rollupWithReader(t *testing.T, reader agentrelease.VersionReader, rows []agentrelease.SiteAgentVersion) agentrelease.FleetSummary {
	t.Helper()
	tenantID := uuid.New()
	repo := &fakeSiteLister{byTenant: map[uuid.UUID][]agentrelease.SiteAgentVersion{tenantID: rows}}
	summary, err := agentrelease.NewService(repo, reader).FleetRollup(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("FleetRollup: %v", err)
	}
	return summary
}

// TestFleetRollup_StorageBlipDoesNotFlipTheFleetToCurrent is the production
// regression, reproduced from the reported scenario: 24 sites on 0.61.97, the
// manifest publishing 0.62.0, and object storage failing exactly once.
//
// Before the fix the failed read was cached as "" for the FULL success TTL
// (five minutes), so the rollup silently re-derived its reference from the
// fleet itself and answered "24 of 24 current" on release day, hiding the
// outdated call to action for five minutes on every replica that saw the blip.
// Truth is 24 outdated.
//
// The fix bounds that window to the much shorter negative TTL: the blip is
// retried, the published version returns, and the fleet reads outdated again.
func TestFleetRollup_StorageBlipDoesNotFlipTheFleetToCurrent(t *testing.T) {
	store := &blipStore{version: "0.62.0", failCalls: map[int]bool{1: true}}
	// A production-shaped success TTL with a test-shaped negative TTL: the
	// point of the test is precisely that the two differ.
	reader := agentrelease.NewReaderWithNegativeTTL(store, 5*time.Minute, 5*time.Millisecond)
	rows := fleetOn(24, "0.61.97")

	// During the blip nothing has ever been published in this process, so the
	// self-hosted fallback still applies and the fleet-derived answer is the
	// best available. It must not be allowed to STICK.
	during := rollupWithReader(t, reader, rows)
	if during.ReferenceSource == agentrelease.ReferenceSourcePublished {
		t.Fatalf("during the blip ReferenceSource = %q; the manifest was unreadable", during.ReferenceSource)
	}

	time.Sleep(20 * time.Millisecond) // outlive the negative TTL, not the 5m one

	after := rollupWithReader(t, reader, rows)
	assertSummary(t, after, "0.62.0", agentrelease.ReferenceSourcePublished,
		agentrelease.Counts{Outdated: 24})
	if store.calls != 2 {
		t.Errorf("store.calls = %d; want 2 (the blip was retried, not cached for the success TTL)", store.calls)
	}
}

// TestFleetRollup_BlipAfterPublishServesLastKnownGood is the same failure one
// beat later, and the harmful one: the manifest HAS been read, then a blip
// hits. Before the fix the good version was overwritten with "", the reference
// fell back to the fleet's own newest agent, and the dashboard flipped from
// "24 outdated" to "24 current" (losing the "View outdated sites" link, which
// is gated on outdated > 0). Stale but true beats fleet-derived.
func TestFleetRollup_BlipAfterPublishServesLastKnownGood(t *testing.T) {
	store := &blipStore{version: "0.62.0", failCalls: map[int]bool{2: true, 3: true}}
	reader := agentrelease.NewReaderWithNegativeTTL(store, 5*time.Millisecond, time.Millisecond)
	rows := fleetOn(24, "0.61.97")

	before := rollupWithReader(t, reader, rows)
	assertSummary(t, before, "0.62.0", agentrelease.ReferenceSourcePublished,
		agentrelease.Counts{Outdated: 24})

	time.Sleep(20 * time.Millisecond) // expire the good entry so the blip is hit

	during := rollupWithReader(t, reader, rows)
	assertSummary(t, during, "0.62.0", agentrelease.ReferenceSourcePublished,
		agentrelease.Counts{Outdated: 24})

	time.Sleep(20 * time.Millisecond)

	stillDuring := rollupWithReader(t, reader, rows)
	assertSummary(t, stillDuring, "0.62.0", agentrelease.ReferenceSourcePublished,
		agentrelease.Counts{Outdated: 24})
}

// TestFleetRollup_UnreadableAfterPublishIsNoneNotFleet pins the gate itself,
// and does so through a REAL *agentrelease.Reader rather than a hand-written
// double that simply asserts the state. Only a real reader proves the branch is
// one production can reach: a double can be made to answer "published before,
// unreadable now" whether or not any real code path produces it.
//
// The sequence is a sustained outage: storage serves the manifest once and is
// then broken permanently. While the last known good version is young it stands
// in for the live read and the fleet stays correctly measured against the
// published 0.62.0, which is the blip fix and must not regress. Once it ages
// past the bound the Reader stops offering it and the rollup answers source
// "none", because the two states are genuinely different questions:
//
//   - This install has NO release channel (self-hosted; EverPublished false).
//     The fleet-derived reference is the right answer.
//   - This install HAS a release channel and it is currently unreachable
//     (EverPublished true). Saying so is the right answer. Falling through to
//     the fleet reference would report "24 of 24 current" against the fleet's
//     own newest agent and hide any release that landed during the outage.
func TestFleetRollup_UnreadableAfterPublishIsNoneNotFleet(t *testing.T) {
	store := &blipStore{version: "0.62.0", failFrom: 2}
	reader := agentrelease.NewReaderWithLimits(store, 20*time.Millisecond, 5*time.Millisecond, 200*time.Millisecond)
	rows := fleetOn(24, "0.61.97")

	assertSummary(t, rollupWithReader(t, reader, rows), "0.62.0",
		agentrelease.ReferenceSourcePublished, agentrelease.Counts{Outdated: 24})

	time.Sleep(40 * time.Millisecond) // past the success TTL, inside the age bound
	assertSummary(t, rollupWithReader(t, reader, rows), "0.62.0",
		agentrelease.ReferenceSourcePublished, agentrelease.Counts{Outdated: 24})

	time.Sleep(220 * time.Millisecond) // past the age bound
	assertSummary(t, rollupWithReader(t, reader, rows), "",
		agentrelease.ReferenceSourceNone, agentrelease.Counts{Unknown: 24})

	// Never "fleet": the channel is proven to exist, so the honest report is
	// that WPMgr cannot determine a reference right now, not a reference
	// derived from the fleet's own peers.
	if !reader.EverPublished() {
		t.Error("EverPublished() = false after a proven publish; the gate would wrongly unlock the fleet fallback")
	}
}
