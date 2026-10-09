// Package uptime implements M5 active uptime monitoring: a periodic probe of
// every enrolled site's URL over the SSRF-hardened client (ADR-009), recording
// a per-check timing breakdown + TLS expiry into the ClickHouse metrics store,
// refreshing the site's Postgres health_status, and a downtime/recovery alert
// evaluator (email via go-mail + signed webhook over the SSRF client).
//
// Site URLs are user-controlled, so ALL probes go through the SSRF guard, which
// blocks private/loopback/link-local destinations — exactly the desired posture
// for uptime (we only ever probe public sites).
package uptime

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"strings"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/httpclient"
	"github.com/mosamlife/wpmgr/apps/api/internal/wpfatal"
)

// maxProbeBody bounds how much of the response body the probe reads (we only
// need TTFB + a small drain to complete the request and free the connection).
// It also bounds how much scanFatal ever buffers/inspects — see the Probe
// body-buffering comment below (issue #132 S1).
const maxProbeBody = 64 << 10

// wpFatalDetectEnv is the kill-switch env var for the wp-fatal-page scan.
// Detection defaults ON; only "0" or "false" (case-insensitive) disables it.
const wpFatalDetectEnv = "WPMGR_UPTIME_WPFATAL_DETECT"

// ProbeResult is the measured outcome of a single site probe. All durations are
// milliseconds. Up is the classification (2xx/3xx = up; 5xx/timeout/conn-error
// = down). A transport/SSRF error sets Up=false and Error.
type ProbeResult struct {
	Up         bool
	HTTPStatus int
	DNSMs      float64
	ConnectMs  float64
	TLSMs      float64
	TTFBMs     float64
	TotalMs    float64
	TLSExpiry  time.Time
	// TLSIssuer is the leaf certificate's Issuer.CommonName ("Let's Encrypt
	// Authority X3", "Google Trust Services LLC", etc.). Empty when the probe
	// was not HTTPS or the cert could not be read.
	TLSIssuer string
	// TLSSubject is the leaf certificate's Subject.CommonName (usually the host).
	TLSSubject string
	Error      string
}

// Prober performs a single timed HTTP(S) GET via the SSRF-hardened client. It
// uses the client's underlying *http.Client (SSRF transport preserved) with a
// per-probe httptrace to break down DNS/connect/TLS/TTFB.
type Prober struct {
	client        *httpclient.Client
	timeout       time.Duration
	wpFatalDetect bool
}

// NewProber builds a Prober around the SSRF-hardened client. timeout bounds a
// single probe (defaults to 15s when non-positive). The wp-fatal-page scan
// kill-switch (WPMGR_UPTIME_WPFATAL_DETECT) is read once here, not per-probe.
func NewProber(client *httpclient.Client, timeout time.Duration) *Prober {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &Prober{client: client, timeout: timeout, wpFatalDetect: wpFatalDetectEnabled()}
}

// wpFatalDetectEnabled reads the WPMGR_UPTIME_WPFATAL_DETECT kill-switch.
// Detection defaults ON; only "0" or "false" (case-insensitive) disables it.
func wpFatalDetectEnabled() bool {
	v := strings.TrimSpace(os.Getenv(wpFatalDetectEnv))
	if v == "" {
		return true
	}
	return v != "0" && !strings.EqualFold(v, "false")
}

// Probe issues a GET to targetURL and measures the connection phases. A
// transport error (including an SSRF block) is NOT returned as a Go error:
// uptime treats an unreachable/blocked site as a recorded DOWN result so the
// timeline is continuous. The boolean classification lives in the result.
func (p *Prober) Probe(ctx context.Context, targetURL string) ProbeResult {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	var (
		start        = time.Now()
		dnsStart     time.Time
		dnsDone      time.Time
		connectStart time.Time
		connectDone  time.Time
		tlsStart     time.Time
		tlsDone      time.Time
		firstByte    time.Time
		tlsExpiry    time.Time
		tlsIssuer    string
		tlsSubject   string
	)

	trace := &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone:  func(httptrace.DNSDoneInfo) { dnsDone = time.Now() },
		ConnectStart: func(_, _ string) {
			if connectStart.IsZero() {
				connectStart = time.Now()
			}
		},
		ConnectDone:       func(_, _ string, _ error) { connectDone = time.Now() },
		TLSHandshakeStart: func() { tlsStart = time.Now() },
		// This callback only measures the handshake PHASE TIMING (TLSMs). It
		// NEVER fires on a reused keep-alive connection — the shared client
		// (90s IdleConnTimeout, probes every 60s) reuses its connection to a
		// site after the first probe, so relying on this callback for the
		// certificate itself left tls_expiry permanently NULL from the second
		// probe onward. The certificate fields are instead read from
		// resp.TLS below, which is populated on both fresh AND reused
		// connections.
		TLSHandshakeDone: func(_ tls.ConnectionState, _ error) {
			tlsDone = time.Now()
		},
		GotFirstResponseByte: func() { firstByte = time.Now() },
	}

	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodGet, targetURL, nil)
	if err != nil {
		return ProbeResult{Up: false, Error: fmt.Sprintf("build request: %v", err)}
	}
	req.Header.Set("User-Agent", "WPMgr-UptimeProbe/1.0")

	resp, err := p.client.HTTPClient().Do(req)
	if err != nil {
		res := ProbeResult{Up: false, Error: err.Error()}
		if httpclient.IsSSRFBlocked(err) {
			res.Error = "ssrf_blocked: " + err.Error()
		}
		// Fill in whatever phase timings we captured before the failure.
		fillTimings(&res, start, dnsStart, dnsDone, connectStart, connectDone, tlsStart, tlsDone, firstByte)
		return res
	}
	defer func() { _ = resp.Body.Close() }()
	// resp.TLS is populated on the FINAL response — after following any
	// redirects — for both a fresh handshake and a reused keep-alive
	// connection, on HTTP/1.1 and HTTP/2 alike. It is the authoritative
	// source for the certificate fields (TLSHandshakeDone above only ever
	// fires on a fresh handshake, so a reused connection would otherwise
	// never report an expiry). Plain-HTTP sites leave resp.TLS nil, which
	// keeps TLSExpiry zero (recorded as NULL), unchanged from before.
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		leaf := resp.TLS.PeerCertificates[0]
		tlsExpiry = leaf.NotAfter
		tlsIssuer = leaf.Issuer.CommonName
		tlsSubject = leaf.Subject.CommonName
	}
	// Buffer up to maxProbeBody bytes for the wp-fatal-page scan below. A
	// WordPress fatal error only reaches the client as HTTP 200 once headers
	// have already been sent (WP can no longer switch to a 500 at that
	// point), which means the wp_die/db-error document is appended AFTER
	// whatever partial page had already flushed — a modern block theme can
	// flush 30-80 KB of inline global-styles CSS in <head> alone. A small
	// head-only scan window would miss the appended document entirely, so we
	// keep everything already read up to the existing maxProbeBody read
	// budget (nothing beyond it is ever read, so memory stays bounded) rather
	// than discarding all but a small head fraction of it (issue #132 S1).
	var bodyBuf bytes.Buffer
	_, _ = io.Copy(&bodyBuf, io.LimitReader(resp.Body, maxProbeBody))
	end := time.Now()

	res := ProbeResult{
		HTTPStatus: resp.StatusCode,
		TLSExpiry:  tlsExpiry,
		TLSIssuer:  tlsIssuer,
		TLSSubject: tlsSubject,
		// 2xx/3xx = up; 4xx is "reachable but not OK" — for uptime we treat <500
		// as up (the site responded), 5xx as down. This matches the brief
		// (2xx/3xx up; 5xx/timeout/conn-error down) while not flapping on a 404.
		Up: resp.StatusCode > 0 && resp.StatusCode < 500,
	}
	if !res.Up {
		res.Error = fmt.Sprintf("http status %d", resp.StatusCode)
	} else if p.wpFatalDetect {
		// A WordPress critical-error or database-connection page is served
		// with HTTP 200, so a status-only classification never catches it;
		// scan the buffered window for its structural signature and
		// reclassify as DOWN when it matches (issue #132).
		if down, reason := scanFatal(bodyBuf.Bytes(), resp.Header.Get("Content-Type")); down {
			res.Up = false
			res.Error = reason
		}
	}
	fillTimings(&res, start, dnsStart, dnsDone, connectStart, connectDone, tlsStart, tlsDone, firstByte)
	res.TotalMs = msSince(start, end)
	return res
}

// fillTimings populates the per-phase millisecond fields from the captured trace
// timestamps. A phase that never fired (e.g. TLS on a plain-HTTP site, or DNS on
// an IP literal) stays zero.
func fillTimings(res *ProbeResult, start, dnsStart, dnsDone, connectStart, connectDone, tlsStart, tlsDone, firstByte time.Time) {
	if !dnsStart.IsZero() && !dnsDone.IsZero() {
		res.DNSMs = msSince(dnsStart, dnsDone)
	}
	if !connectStart.IsZero() && !connectDone.IsZero() {
		res.ConnectMs = msSince(connectStart, connectDone)
	}
	if !tlsStart.IsZero() && !tlsDone.IsZero() {
		res.TLSMs = msSince(tlsStart, tlsDone)
	}
	if !firstByte.IsZero() {
		res.TTFBMs = msSince(start, firstByte)
	}
}

func msSince(start, end time.Time) float64 {
	if start.IsZero() || end.IsZero() || !end.After(start) {
		return 0
	}
	return float64(end.Sub(start).Microseconds()) / 1000.0
}

// fatalProximityBytes is the proximity bound the shared recognizer applies
// (wpfatal.ProximityBytes). This package's tests size their fixtures by it.
const fatalProximityBytes = wpfatal.ProximityBytes

// scanFatal inspects the buffered response body (at most maxProbeBody bytes,
// see Probe) for an error document WordPress itself rendered, through the
// recognizer this monitor shares with the post-update health probe
// (wpfatal.Scan). It reports down with reason "wp_fatal_error" for the
// critical-error screen or "wp_db_error" for the database connection failure
// screen, and only ever for a text/html response (issue #132).
func scanFatal(body []byte, contentType string) (down bool, reason string) {
	return wpfatal.Scan(body, contentType)
}
