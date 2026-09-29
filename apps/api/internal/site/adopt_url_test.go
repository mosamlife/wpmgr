package site

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

// adoptRepo is fakeRepo with a stored address and state, recording every
// address write.
type adoptRepo struct {
	*fakeRepo
	url         string
	state       ConnectionState
	adoptResult bool
	adoptCalls  []string
	// failWrites is how many AdoptSiteURL calls, from the next, fail with a
	// database error before the writes succeed.
	failWrites int
}

func (r *adoptRepo) Get(_ context.Context, tenantID, id uuid.UUID) (Site, error) {
	return Site{ID: id, TenantID: tenantID, URL: r.url, ConnectionState: r.state}, nil
}

// UpdateMetadata returns the stored address with the row, as the real
// query's RETURNING * does: the metadata push reads the saved address from it.
func (r *adoptRepo) UpdateMetadata(_ context.Context, tenantID, siteID uuid.UUID, _ Metadata, _ []byte) (Site, error) {
	return Site{ID: siteID, TenantID: tenantID, URL: r.url, ConnectionState: r.state}, nil
}

func (r *adoptRepo) AdoptSiteURL(_ context.Context, _, _ uuid.UUID, _, to string) (bool, error) {
	r.adoptCalls = append(r.adoptCalls, to)
	if r.failWrites > 0 {
		r.failWrites--
		return false, errors.New("write failed: connection reset")
	}
	if r.adoptResult {
		r.url = to
	}
	return r.adoptResult, nil
}

// fakeProber answers both probes from fixed values and counts them. Every
// answer is definitive unless unanswered is set, which models a transport
// failure or a timeout.
type fakeProber struct {
	suggested     string
	redirected    bool
	pingOK        bool
	unanswered    bool
	redirectCalls []string
	pingCalls     []string
}

func (p *fakeProber) CommandRedirectTarget(_ context.Context, _ uuid.UUID, siteURL string) (string, bool, bool) {
	p.redirectCalls = append(p.redirectCalls, siteURL)
	if p.unanswered {
		return "", false, false
	}
	return p.suggested, p.redirected, true
}

func (p *fakeProber) CommandPingOK(_ context.Context, _ uuid.UUID, siteURL string) (bool, bool) {
	p.pingCalls = append(p.pingCalls, siteURL)
	if p.unanswered {
		return false, false
	}
	return p.pingOK, true
}

type manualClock struct{ t time.Time }

func (c *manualClock) Now() time.Time { return c.t }

func newAdoptService(saved string, prober *fakeProber, clk *manualClock) (*Service, *adoptRepo) {
	repo := &adoptRepo{fakeRepo: &fakeRepo{}, url: saved, state: StateConnected, adoptResult: true}
	svc := NewService(repo, nil, clk)
	if prober != nil {
		svc.SetCommandRedirectProber(prober)
	}
	return svc, repo
}

// TestAdoptReportedURL_SchemeUpgradeNeedsAnHTTPSPing: an http to https
// upgrade on the same host is written only after a signed ping to the https
// form of the saved host answers 2xx; no prober, or a failed ping, adopts
// nothing.
func TestAdoptReportedURL_SchemeUpgradeNeedsAnHTTPSPing(t *testing.T) {
	ctx := context.Background()
	clk := &manualClock{t: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}

	failing := &fakeProber{pingOK: false}
	svc, repo := newAdoptService("http://example.com", failing, clk)
	adopted, err := svc.adoptReportedURL(ctx, uuid.New(), uuid.New(), "https://example.com/", "test", "")
	if err != nil || adopted {
		t.Fatalf("failed ping: adopted=%v err=%v, want not adopted", adopted, err)
	}
	if len(repo.adoptCalls) != 0 {
		t.Errorf("failed ping: address written %v", repo.adoptCalls)
	}
	if len(failing.pingCalls) != 1 || failing.pingCalls[0] != "https://example.com" {
		t.Errorf("ping calls = %v, want one to https://example.com", failing.pingCalls)
	}
	if len(failing.redirectCalls) != 0 {
		t.Errorf("redirect probe sent for a scheme-only upgrade: %v", failing.redirectCalls)
	}

	ok := &fakeProber{pingOK: true}
	svc, repo = newAdoptService("http://example.com", ok, clk)
	adopted, err = svc.adoptReportedURL(ctx, uuid.New(), uuid.New(), "https://example.com", "test", "")
	if err != nil || !adopted {
		t.Fatalf("2xx ping: adopted=%v err=%v, want adopted", adopted, err)
	}
	if len(repo.adoptCalls) != 1 || repo.adoptCalls[0] != "https://example.com" {
		t.Errorf("address writes = %v, want one to https://example.com", repo.adoptCalls)
	}

	svc, repo = newAdoptService("http://example.com", nil, clk)
	if adopted, _ := svc.adoptReportedURL(ctx, uuid.New(), uuid.New(), "https://example.com", "test", ""); adopted || len(repo.adoptCalls) != 0 {
		t.Errorf("no prober: adopted=%v writes=%v, want nothing", adopted, repo.adoptCalls)
	}
}

// TestAdoptReportedURL_ProbesAtMostOncePerDay: a reported address the site
// does not serve is probed once, then not again for 24h, for both the host
// change probe and the https probe; another site or another address has its
// own window.
func TestAdoptReportedURL_ProbesAtMostOncePerDay(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	clk := &manualClock{t: t0}
	tenant, site := uuid.New(), uuid.New()

	www := &fakeProber{redirected: false}
	svc, _ := newAdoptService("https://example.com", www, clk)
	push := func() {
		t.Helper()
		if adopted, err := svc.adoptReportedURL(ctx, tenant, site, "https://www.example.com", "test", ""); adopted || err != nil {
			t.Fatalf("adopted=%v err=%v, want not adopted", adopted, err)
		}
	}
	push()
	push()
	clk.t = t0.Add(24*time.Hour - time.Nanosecond)
	push()
	if n := len(www.redirectCalls); n != 1 {
		t.Fatalf("www probes inside 24h = %d, want 1", n)
	}
	clk.t = t0.Add(24 * time.Hour)
	push()
	push()
	if n := len(www.redirectCalls); n != 2 {
		t.Fatalf("www probes after 24h = %d, want 2", n)
	}

	// Another site reporting the same kind of address is probed on its own.
	if _, err := svc.adoptReportedURL(ctx, tenant, uuid.New(), "https://www.example.com", "test", ""); err != nil {
		t.Fatal(err)
	}
	if n := len(www.redirectCalls); n != 3 {
		t.Errorf("probes after another site's push = %d, want 3", n)
	}

	clk.t = t0
	https := &fakeProber{pingOK: false}
	svc, _ = newAdoptService("http://example.com", https, clk)
	for i := 0; i < 3; i++ {
		if adopted, _ := svc.adoptReportedURL(ctx, tenant, site, "https://example.com", "test", ""); adopted {
			t.Fatal("adopted on a failed https ping")
		}
	}
	if n := len(https.pingCalls); n != 1 {
		t.Errorf("https probes inside 24h = %d, want 1", n)
	}
	clk.t = t0.Add(25 * time.Hour)
	_, _ = svc.adoptReportedURL(ctx, tenant, site, "https://example.com", "test", "")
	if n := len(https.pingCalls); n != 2 {
		t.Errorf("https probes after 24h = %d, want 2", n)
	}
}

// probeOnce is one whole probe through the limiter: begin at now and, when
// allowed, finish at now with hold.
func probeOnce(l *probeLimiter, k probeKey, now time.Time, hold time.Duration) bool {
	if !l.begin(k, now) {
		return false
	}
	l.finish(k, now, hold)
	return true
}

// TestProbeLimiter_Window: after a probe, a key is refused for the hold its
// outcome recorded, measured from the end of that probe.
func TestProbeLimiter_Window(t *testing.T) {
	l := newProbeLimiter(8)
	k := probeKey{site: uuid.New(), address: "https://www.example.com"}
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	steps := []struct {
		at   time.Duration
		want bool
	}{
		{0, true},
		{time.Hour, false},
		{24*time.Hour - time.Nanosecond, false},
		{24 * time.Hour, true},
		{25 * time.Hour, false},
		{48 * time.Hour, true},
	}
	for _, s := range steps {
		if got := probeOnce(l, k, t0.Add(s.at), adoptProbeWindow); got != s.want {
			t.Errorf("probe at +%v allowed = %v, want %v", s.at, got, s.want)
		}
	}
}

// TestProbeLimiter_OneProbeInFlight: while a key is being probed, it is
// refused, whatever its last hold; the hold is recorded only by finish.
func TestProbeLimiter_OneProbeInFlight(t *testing.T) {
	l := newProbeLimiter(8)
	k := probeKey{site: uuid.New(), address: "https://www.example.com"}
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if !l.begin(k, t0) {
		t.Fatal("first probe refused")
	}
	if l.begin(k, t0.Add(48*time.Hour)) {
		t.Fatal("a second probe was allowed while the first is in flight")
	}
	l.finish(k, t0.Add(time.Minute), adoptProbeBackoff)
	if l.begin(k, t0.Add(time.Minute+adoptProbeBackoff-time.Nanosecond)) {
		t.Fatal("allowed inside the hold finish recorded")
	}
	if !l.begin(k, t0.Add(time.Minute+adoptProbeBackoff)) {
		t.Fatal("refused once the hold finish recorded had ended")
	}
}

// TestProbeLimiter_Bound: the limiter never remembers more than its capacity;
// the key used least recently is forgotten first, and the others keep their
// window.
func TestProbeLimiter_Bound(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	site := uuid.New()
	key := func(i int) probeKey { return probeKey{site: site, address: fmt.Sprintf("https://www.s%d.test", i)} }

	l := newProbeLimiter(3)
	for i := 1; i <= 4; i++ {
		if !probeOnce(l, key(i), t0, adoptProbeWindow) {
			t.Fatalf("first probe of key %d refused", i)
		}
	}
	if n := l.len(); n != 3 {
		t.Fatalf("len = %d, want 3", n)
	}
	for i := 2; i <= 4; i++ {
		if probeOnce(l, key(i), t0, adoptProbeWindow) {
			t.Errorf("key %d allowed again inside its window", i)
		}
	}
	if !probeOnce(l, key(1), t0, adoptProbeWindow) {
		t.Error("the evicted key 1 was refused; it is no longer remembered")
	}
	if n := l.len(); n != 3 {
		t.Errorf("len after re-adding key 1 = %d, want 3", n)
	}

	svcLimiter := (&Service{}).probeLimiter()
	for i := 0; i < adoptProbeCapacity+50; i++ {
		probeOnce(svcLimiter, key(i), t0, adoptProbeWindow)
	}
	if n := svcLimiter.len(); n != adoptProbeCapacity {
		t.Errorf("service limiter len = %d, want the capacity %d", n, adoptProbeCapacity)
	}
}

// TestAdoptReportedURL_TimeoutHoldsForAnHour: a probe that got no answer (a
// timeout or a transport failure) holds the address back for an hour, not a
// day: refused at 30m, probed again at 1h. It covers both probes.
func TestAdoptReportedURL_TimeoutHoldsForAnHour(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name, saved, reported string
		calls                 func(*fakeProber) int
	}{
		{"https ping", "http://example.com", "https://example.com", func(p *fakeProber) int { return len(p.pingCalls) }},
		{"www redirect probe", "https://example.com", "https://www.example.com", func(p *fakeProber) int { return len(p.redirectCalls) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clk := &manualClock{t: t0}
			prober := &fakeProber{unanswered: true}
			svc, repo := newAdoptService(c.saved, prober, clk)
			tenant, site := uuid.New(), uuid.New()
			push := func(at time.Duration) {
				t.Helper()
				clk.t = t0.Add(at)
				if adopted, err := svc.adoptReportedURL(ctx, tenant, site, c.reported, "test", ""); adopted || err != nil {
					t.Fatalf("at +%v: adopted=%v err=%v, want not adopted", at, adopted, err)
				}
			}
			push(0)
			push(30 * time.Minute)
			if n := c.calls(prober); n != 1 {
				t.Fatalf("probes by +30m after a timeout = %d, want 1", n)
			}
			push(time.Hour)
			if n := c.calls(prober); n != 2 {
				t.Fatalf("probes by +1h after a timeout = %d, want 2", n)
			}
			if len(repo.adoptCalls) != 0 {
				t.Errorf("address written: %v", repo.adoptCalls)
			}
		})
	}
}

// TestAdoptReportedURL_DefinitiveNoHoldsForADay: a definitive answer that
// adopts nothing (the saved address answers a 2xx, so it redirects nowhere)
// holds the address back for 24h, including after an earlier timeout's
// shorter hold.
func TestAdoptReportedURL_DefinitiveNoHoldsForADay(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	clk := &manualClock{t: t0}
	prober := &fakeProber{unanswered: true}
	svc, _ := newAdoptService("https://example.com", prober, clk)
	tenant, site := uuid.New(), uuid.New()
	push := func(at time.Duration) {
		t.Helper()
		clk.t = t0.Add(at)
		if adopted, err := svc.adoptReportedURL(ctx, tenant, site, "https://www.example.com", "test", ""); adopted || err != nil {
			t.Fatalf("at +%v: adopted=%v err=%v, want not adopted", at, adopted, err)
		}
	}
	push(0)
	// The site answers from here on: a 2xx, no redirect.
	prober.unanswered = false
	push(time.Hour)
	if n := len(prober.redirectCalls); n != 2 {
		t.Fatalf("probes by +1h = %d, want 2 (the timeout held for an hour)", n)
	}
	push(2 * time.Hour)
	push(time.Hour + 24*time.Hour - time.Nanosecond)
	if n := len(prober.redirectCalls); n != 2 {
		t.Fatalf("probes inside the day after a definitive no = %d, want 2", n)
	}
	push(time.Hour + 24*time.Hour)
	if n := len(prober.redirectCalls); n != 3 {
		t.Fatalf("probes once the day had passed = %d, want 3", n)
	}
}

// TestAdoptReportedURL_OneHostKeyUpgradePingsTheSavedHost: a site saved as
// http://BÜCHER.de whose agent reports https://bücher.de is a scheme-only
// upgrade: it sends one https ping to the saved host, no redirect probe, and
// writes the saved spelling over https only when that ping answers 2xx.
func TestAdoptReportedURL_OneHostKeyUpgradePingsTheSavedHost(t *testing.T) {
	ctx := context.Background()
	clk := &manualClock{t: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	const saved, reported, want = "http://BÜCHER.de", "https://bücher.de", "https://bÜcher.de"

	failing := &fakeProber{pingOK: false}
	svc, repo := newAdoptService(saved, failing, clk)
	if adopted, err := svc.adoptReportedURL(ctx, uuid.New(), uuid.New(), reported, "test", ""); adopted || err != nil {
		t.Fatalf("failed ping: adopted=%v err=%v, want not adopted", adopted, err)
	}
	if len(repo.adoptCalls) != 0 {
		t.Errorf("failed ping: address written %v", repo.adoptCalls)
	}

	ok := &fakeProber{pingOK: true}
	svc, repo = newAdoptService(saved, ok, clk)
	if adopted, err := svc.adoptReportedURL(ctx, uuid.New(), uuid.New(), reported, "test", ""); !adopted || err != nil {
		t.Fatalf("2xx ping: adopted=%v err=%v, want adopted", adopted, err)
	}
	for _, p := range []*fakeProber{failing, ok} {
		if len(p.pingCalls) != 1 || p.pingCalls[0] != want {
			t.Errorf("ping calls = %v, want one to %s", p.pingCalls, want)
		}
		if len(p.redirectCalls) != 0 {
			t.Errorf("redirect probe sent for a scheme-only upgrade: %v", p.redirectCalls)
		}
	}
	if len(repo.adoptCalls) != 1 || repo.adoptCalls[0] != want {
		t.Errorf("address writes = %v, want one to %s", repo.adoptCalls, want)
	}
}
