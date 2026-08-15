package uptime

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/httpclient"
)

// CronKickArgs is the River job payload for the periodic cron-kick pass.
// It carries no per-run data; the cadence is set by the periodic job schedule.
type CronKickArgs struct{}

// Kind implements river.JobArgs.
func (CronKickArgs) Kind() string { return "uptime_cron_kick" }

// CronKicker fires a best-effort GET to <site_url>/wp-cron.php for every
// connected/enrolled site on a low-frequency cadence (default 5 min). The sole
// purpose is to boot PHP on fully page-cached sites so their WP-Cron queue
// drains: heartbeats, backups, transient cleanup, metadata/perf pushes all
// depend on WP-Cron running, but a fully page-cached site never boots PHP for
// organic traffic and therefore never drains cron.
//
// This is a PURE SIDE-EFFECT operation. It:
//   - Records NOTHING to metrics / ClickHouse.
//   - Does NOT update health_status or connection_state (the 0.44.0 active-verify
//     owns liveness; this only nudges the site's own cron).
//   - Reuses the same SSRF-hardened *httpclient.Client the uptime prober uses
//     (ADR-009 compliance: all outbound calls to attacker-influenced URLs go
//     through the SSRF guard).
//   - Is unconditional across all enrolled sites (cheap, harmless for sites with
//     organic traffic, necessary for sites without).
//   - Uses a bounded 5 s per-kick timeout and bounded concurrency.
//   - Logs at debug only; a kick failure is silently swallowed (fire-and-forget).
type CronKicker struct {
	river.WorkerDefaults[CronKickArgs]
	repo        Repo
	client      *httpclient.Client
	timeout     time.Duration
	concurrency int
	logger      *slog.Logger
}

// NewCronKicker builds a CronKicker.
//
//   - repo: reuses the uptime Repo for ListEnrolledForProbe (the same
//     cross-tenant enrolled-site list the probe worker uses, under app.agent GUC).
//   - client: the SAME SSRF-hardened *httpclient.Client as the probe (required;
//     not a separate unguarded client).
//   - timeout: per-kick GET timeout (defaults to 5 s when non-positive).
//   - concurrency: max simultaneous kicks (defaults to 10 when non-positive).
func NewCronKicker(repo Repo, client *httpclient.Client, timeout time.Duration, concurrency int) *CronKicker {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if concurrency <= 0 {
		concurrency = 10
	}
	return &CronKicker{
		repo:        repo,
		client:      client,
		timeout:     timeout,
		concurrency: concurrency,
		logger:      slog.Default(),
	}
}

// SetLogger wires a structured logger. Falls back to slog.Default() when nil.
func (k *CronKicker) SetLogger(l *slog.Logger) {
	if l != nil {
		k.logger = l
	}
}

// Work runs one cron-kick sweep (River entry point).
func (k *CronKicker) Work(ctx context.Context, _ *river.Job[CronKickArgs]) error {
	return k.Kick(ctx)
}

// Kick fires a best-effort GET to /wp-cron.php for every enrolled site.
// It is exposed (not just Work) so it is directly testable without River.
// Per-site failures are swallowed; the overall function never returns an error.
func (k *CronKicker) Kick(ctx context.Context) error {
	// GH #414 phase 2: this is ListEnrolledForProbe, the UNFILTERED
	// enumeration, and it must stay that way. A paused site is skipped by the
	// uptime PROBE sweep (ListEnrolledForMonitoringProbe) because monitoring
	// is what the operator paused. The cron kick is not monitoring: it records
	// nothing, alerts on nothing, and its only effect is to boot PHP on the
	// site so the site's OWN WP-Cron queue drains - which is what runs the
	// agent's heartbeats and its scheduled BACKUPS. Filtering paused sites out
	// here would silently stop backups on every fully page-cached paused site,
	// which is precisely the failure mode GH #414's design refuses: pause
	// governs monitoring, never data protection.
	sites, err := k.repo.ListEnrolledForProbe(ctx)
	if err != nil {
		// A DB failure is the only error this function propagates; it lets River
		// retry the job. A site-level kick failure (network, 4xx, 5xx) is swallowed.
		return err
	}
	if len(sites) == 0 {
		return nil
	}

	sem := make(chan struct{}, k.concurrency)
	var wg sync.WaitGroup
	for _, s := range sites {
		s := s
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			k.kickSite(ctx, s.URL)
		}()
	}
	wg.Wait()
	return nil
}

// cronKickURL returns <siteURL>/wp-cron.php?doing_wp_cron=<unix_ts> for the
// given site URL. The query parameter signals to WP that this is a cron
// execution request (WP checks it before firing spawns_cron = it prevents
// WP from recursively spawning another cron during the same request). A
// URL-parse failure falls back to a naive string join so that a malformed
// stored URL still fires a kick attempt (the SSRF guard rejects bad
// destinations at the transport layer).
func cronKickURL(siteURL string) string {
	u, err := url.Parse(strings.TrimRight(siteURL, "/"))
	if err != nil {
		return strings.TrimRight(siteURL, "/") + "/wp-cron.php"
	}
	u.Path = path.Join(u.Path, "wp-cron.php")
	q := u.Query()
	q.Set("doing_wp_cron", fmt.Sprintf("%d", time.Now().Unix()))
	u.RawQuery = q.Encode()
	return u.String()
}

// kickSite fires a single best-effort GET to the site's wp-cron.php.
// The response body is discarded; any error is logged at debug and dropped.
// The SSRF guard rejects private/loopback destinations.
func (k *CronKicker) kickSite(ctx context.Context, siteURL string) {
	kickURL := cronKickURL(siteURL)

	reqCtx, cancel := context.WithTimeout(ctx, k.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, kickURL, nil)
	if err != nil {
		k.logger.Debug("uptime cron kick: build request failed",
			slog.String("site_url", siteURL),
			slog.Any("error", err),
		)
		return
	}
	// Identify as the cron-kick path so a site's access logs can distinguish it
	// from the regular uptime probe or organic traffic.
	req.Header.Set("User-Agent", "WPMgr-CronKick/1.0")

	// Use the underlying *http.Client directly (single shot, no retries — this is
	// fire-and-forget; the SSRF transport is preserved via HTTPClient()).
	resp, err := k.client.HTTPClient().Do(req)
	if err != nil {
		k.logger.Debug("uptime cron kick: request failed",
			slog.String("site_url", siteURL),
			slog.Any("error", err),
		)
		return
	}
	// Drain + close to free the connection. Bounded read: we only care that PHP
	// booted; we never inspect the response body.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()

	k.logger.Debug("uptime cron kick: fired",
		slog.String("site_url", siteURL),
		slog.Int("status", resp.StatusCode),
	)
}
