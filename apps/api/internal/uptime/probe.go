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
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/httpclient"
)

// maxProbeBody bounds how much of the response body the probe reads (we only
// need TTFB + a small drain to complete the request and free the connection).
const maxProbeBody = 64 << 10

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
	client  *httpclient.Client
	timeout time.Duration
}

// NewProber builds a Prober around the SSRF-hardened client. timeout bounds a
// single probe (defaults to 15s when non-positive).
func NewProber(client *httpclient.Client, timeout time.Duration) *Prober {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &Prober{client: client, timeout: timeout}
}

// Probe issues a GET to targetURL and measures the connection phases. A
// transport error (including an SSRF block) is NOT returned as a Go error:
// uptime treats an unreachable/blocked site as a recorded DOWN result so the
// timeline is continuous. The boolean classification lives in the result.
func (p *Prober) Probe(ctx context.Context, targetURL string) ProbeResult {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	var (
		start            = time.Now()
		dnsStart         time.Time
		dnsDone          time.Time
		connectStart     time.Time
		connectDone      time.Time
		tlsStart         time.Time
		tlsDone          time.Time
		firstByte        time.Time
		tlsExpiry        time.Time
		tlsIssuer        string
		tlsSubject       string
		gotConnReusedTLS bool
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
		TLSHandshakeDone: func(cs tls.ConnectionState, err error) {
			tlsDone = time.Now()
			if err == nil && len(cs.PeerCertificates) > 0 {
				// The leaf certificate is first; its NotAfter is the expiry the
				// dashboard surfaces, and the Issuer/Subject CommonName is what
				// the operator sees as "Let's Encrypt", "Cloudflare", etc.
				leaf := cs.PeerCertificates[0]
				tlsExpiry = leaf.NotAfter
				tlsIssuer = leaf.Issuer.CommonName
				tlsSubject = leaf.Subject.CommonName
			}
		},
		GotFirstResponseByte: func() { firstByte = time.Now() },
		GotConn: func(gci httptrace.GotConnInfo) {
			// A reused (keep-alive) connection skips DNS/connect/TLS traces; note it
			// so we don't report a misleadingly-zero handshake.
			gotConnReusedTLS = gci.Reused
		},
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
	defer func() { _, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxProbeBody)); _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxProbeBody))
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
	}
	fillTimings(&res, start, dnsStart, dnsDone, connectStart, connectDone, tlsStart, tlsDone, firstByte)
	res.TotalMs = msSince(start, end)
	_ = gotConnReusedTLS
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
