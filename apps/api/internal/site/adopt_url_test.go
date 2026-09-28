package site

import (
	"context"
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
}

func (r *adoptRepo) Get(_ context.Context, tenantID, id uuid.UUID) (Site, error) {
	return Site{ID: id, TenantID: tenantID, URL: r.url, ConnectionState: r.state}, nil
}

func (r *adoptRepo) AdoptSiteURL(_ context.Context, _, _ uuid.UUID, _, to string) (bool, error) {
	r.adoptCalls = append(r.adoptCalls, to)
	if r.adoptResult {
		r.url = to
	}
	return r.adoptResult, nil
}

// fakeProber answers both probes from fixed values and counts them.
type fakeProber struct {
	suggested     string
	redirected    bool
	pingOK        bool
	redirectCalls []string
	pingCalls     []string
}

func (p *fakeProber) CommandRedirectTarget(_ context.Context, _ uuid.UUID, siteURL string) (string, bool) {
	p.redirectCalls = append(p.redirectCalls, siteURL)
	return p.suggested, p.redirected
}

func (p *fakeProber) CommandPingOK(_ context.Context, _ uuid.UUID, siteURL string) bool {
	p.pingCalls = append(p.pingCalls, siteURL)
	return p.pingOK
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

// TestProbeLimiter_Window: one probe per key per window, measured from the
// last allowed probe.
func TestProbeLimiter_Window(t *testing.T) {
	l := newProbeLimiter(24*time.Hour, 8)
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
		if got := l.allow(k, t0.Add(s.at)); got != s.want {
			t.Errorf("allow at +%v = %v, want %v", s.at, got, s.want)
		}
	}
}

// TestProbeLimiter_Bound: the limiter never remembers more than its capacity;
// the key used least recently is forgotten first, and the others keep their
// window.
func TestProbeLimiter_Bound(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	site := uuid.New()
	key := func(i int) probeKey { return probeKey{site: site, address: fmt.Sprintf("https://www.s%d.test", i)} }

	l := newProbeLimiter(24*time.Hour, 3)
	for i := 1; i <= 4; i++ {
		if !l.allow(key(i), t0) {
			t.Fatalf("first probe of key %d refused", i)
		}
	}
	if n := l.len(); n != 3 {
		t.Fatalf("len = %d, want 3", n)
	}
	for i := 2; i <= 4; i++ {
		if l.allow(key(i), t0) {
			t.Errorf("key %d allowed again inside its window", i)
		}
	}
	if !l.allow(key(1), t0) {
		t.Error("the evicted key 1 was refused; it is no longer remembered")
	}
	if n := l.len(); n != 3 {
		t.Errorf("len after re-adding key 1 = %d, want 3", n)
	}

	svcLimiter := (&Service{}).probeLimiter()
	for i := 0; i < adoptProbeCapacity+50; i++ {
		svcLimiter.allow(key(i), t0)
	}
	if n := svcLimiter.len(); n != adoptProbeCapacity {
		t.Errorf("service limiter len = %d, want the capacity %d", n, adoptProbeCapacity)
	}
}
