package site

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

// pingRoute is the signed ping's command route, the one CommandRedirectTarget
// sends to.
const pingRoute = "/wp-json/wpmgr/v1/command/ping"

// redirectingDoer answers every command with a 301 to location, as a site
// behind an apex-to-www rule does. It never touches the network, so the
// suggestion below is the one the real agentcmd client computes from a 3xx.
type redirectingDoer struct {
	location string
	sent     []string
}

func (d *redirectingDoer) Do(req *http.Request) (*http.Response, error) { return d.DoOnce(req) }

func (d *redirectingDoer) DoOnce(req *http.Request) (*http.Response, error) {
	d.sent = append(d.sent, req.URL.String())
	return &http.Response{
		StatusCode: http.StatusMovedPermanently,
		Header:     http.Header{"Location": {d.location}},
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}, nil
}

// redirectingClient is the production agentcmd.Client with a real Signer over
// a redirectingDoer.
func redirectingClient(t *testing.T, location string) (*agentcmd.Client, *redirectingDoer) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	signer, err := agentcmd.NewSigner(base64.StdEncoding.EncodeToString(priv))
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	d := &redirectingDoer{location: location}
	return agentcmd.NewClient(d, signer), d
}

// TestAdoptReportedURL_TrailingSlashFollowsTheRedirect: the suggestion a
// refused redirect makes and the address a push adopts agree whether or not
// the saved address, the redirect or the reported address carries a trailing
// slash, and an adopted address keeps the saved address's path form. A path
// that differs by more than a slash is neither suggested nor adopted.
func TestAdoptReportedURL_TrailingSlashFollowsTheRedirect(t *testing.T) {
	cases := []struct {
		name, saved, location, reported string
		suggested                       string // what the real client suggests
		adopted                         string // the address written; "" when nothing is
	}{
		{"saved with a slash, the reported shape", "https://x.test/", "https://www.x.test" + pingRoute, "https://www.x.test",
			"https://www.x.test", "https://www.x.test/"},
		{"saved with a slash, reported with one", "https://x.test/", "https://www.x.test" + pingRoute, "https://www.x.test/",
			"https://www.x.test", "https://www.x.test/"},
		{"saved without a slash, redirect with one", "https://x.test", "https://www.x.test" + pingRoute + "/", "https://www.x.test/",
			"https://www.x.test", "https://www.x.test"},
		{"www saved with a slash to the apex", "https://www.x.test/", "https://x.test" + pingRoute, "https://x.test",
			"https://x.test", "https://x.test/"},
		{"a real path difference is refused", "https://x.test/", "https://www.x.test/blog" + pingRoute, "https://www.x.test/",
			"", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			client, doer := redirectingClient(t, c.location)

			suggested, redirected := client.CommandRedirectTarget(ctx, uuid.New(), c.saved)
			if !redirected || suggested != c.suggested {
				t.Fatalf("CommandRedirectTarget(%q) = %q %v, want %q true", c.saved, suggested, redirected, c.suggested)
			}

			clk := &manualClock{t: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
			repo := &adoptRepo{fakeRepo: &fakeRepo{}, url: c.saved, state: StateConnected, adoptResult: true}
			svc := NewService(repo, nil, clk)
			svc.SetCommandRedirectProber(client)
			doer.sent = nil

			adopted, err := svc.adoptReportedURL(ctx, uuid.New(), uuid.New(), c.reported, "test", "")
			if err != nil {
				t.Fatalf("adoptReportedURL: %v", err)
			}
			if c.adopted == "" {
				if adopted || len(repo.adoptCalls) != 0 {
					t.Fatalf("adopted=%v writes=%v, want nothing written", adopted, repo.adoptCalls)
				}
				return
			}
			if !adopted || len(repo.adoptCalls) != 1 || repo.adoptCalls[0] != c.adopted {
				t.Fatalf("adopted=%v writes=%v, want one write of %q", adopted, repo.adoptCalls, c.adopted)
			}
			if len(doer.sent) != 1 || !strings.HasSuffix(doer.sent[0], pingRoute) {
				t.Errorf("sent %v, want one ping to the saved address", doer.sent)
			}
		})
	}
}

// TestAdoptReportedURL_SuggestionOnAnotherPathIsRefused: the adoption compares
// the suggestion with the planned address as an address, so a trailing slash
// is ignored and any other path difference still refuses.
func TestAdoptReportedURL_SuggestionOnAnotherPathIsRefused(t *testing.T) {
	ctx := context.Background()
	clk := &manualClock{t: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	for _, suggested := range []string{"https://www.x.test/blog", "https://www.x.test/blog/", "http://www.x.test/", "https://www.x.test:8443/"} {
		prober := &fakeProber{suggested: suggested, redirected: true}
		svc, repo := newAdoptService("https://x.test/", prober, clk)
		adopted, err := svc.adoptReportedURL(ctx, uuid.New(), uuid.New(), "https://www.x.test", "test", "")
		if err != nil || adopted || len(repo.adoptCalls) != 0 {
			t.Errorf("suggested %q: adopted=%v err=%v writes=%v, want nothing written", suggested, adopted, err, repo.adoptCalls)
		}
	}
	prober := &fakeProber{suggested: "https://www.x.test", redirected: true}
	svc, repo := newAdoptService("https://x.test/", prober, clk)
	if adopted, err := svc.adoptReportedURL(ctx, uuid.New(), uuid.New(), "https://www.x.test", "test", ""); err != nil || !adopted ||
		len(repo.adoptCalls) != 1 || repo.adoptCalls[0] != "https://www.x.test/" {
		t.Errorf("control: adopted=%v err=%v writes=%v, want one write of https://www.x.test/", adopted, err, repo.adoptCalls)
	}
}

// TestAdoptReportedURL_StoresAndPingsTheComparedHost: a push never moves the
// saved address to the name a host's Unicode lowercase spells, and the https
// ping of a scheme-only upgrade goes to the saved host as written.
func TestAdoptReportedURL_StoresAndPingsTheComparedHost(t *testing.T) {
	ctx := context.Background()
	clk := &manualClock{t: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}

	upgrades := []struct{ saved, reported, to string }{
		{"http://İstanbul.test", "https://İstanbul.test", "https://İstanbul.test"},
		{"http://STRAẞE.test", "https://STRAẞE.test", "https://straẞe.test"},
		{"http://my_site.test", "https://my_site.test", "https://my_site.test"},
	}
	for _, c := range upgrades {
		prober := &fakeProber{pingOK: true}
		svc, repo := newAdoptService(c.saved, prober, clk)
		adopted, err := svc.adoptReportedURL(ctx, uuid.New(), uuid.New(), c.reported, "test", "")
		if err != nil || !adopted || len(repo.adoptCalls) != 1 || repo.adoptCalls[0] != c.to {
			t.Errorf("%q -> %q: adopted=%v err=%v writes=%v, want one write of %q", c.saved, c.reported, adopted, err, repo.adoptCalls, c.to)
		}
		if len(prober.pingCalls) != 1 || prober.pingCalls[0] != c.to {
			t.Errorf("%q -> %q: pinged %v, want one ping to %q", c.saved, c.reported, prober.pingCalls, c.to)
		}
	}

	refused := [][2]string{
		{"http://İstanbul.test", "https://istanbul.test"},
		{"https://İstanbul.test", "https://www.istanbul.test"},
		{"http://STRAẞE.test", "https://straße.test"},
		{"https://STRAẞE.test", "https://www.straße.test"},
	}
	for _, c := range refused {
		prober := &fakeProber{pingOK: true, suggested: c[1], redirected: true}
		svc, repo := newAdoptService(c[0], prober, clk)
		adopted, err := svc.adoptReportedURL(ctx, uuid.New(), uuid.New(), c[1], "test", "")
		if err != nil || adopted || len(repo.adoptCalls) != 0 {
			t.Errorf("%q -> %q: adopted=%v err=%v writes=%v, want nothing written", c[0], c[1], adopted, err, repo.adoptCalls)
		}
		if len(prober.pingCalls)+len(prober.redirectCalls) != 0 {
			t.Errorf("%q -> %q: probed %v %v, want no probe", c[0], c[1], prober.pingCalls, prober.redirectCalls)
		}
	}
}
