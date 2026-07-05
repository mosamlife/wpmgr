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
	"regexp"
	"strings"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/httpclient"
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

// fatalProximityBytes bounds the byte distance allowed between a structural
// marker and its companion phrase for scanFatal to treat them as the same
// rendered error document rather than two unrelated mentions scattered
// across an unrelated page (issue #132 S2). WordPress's own templates place
// the marker and the message immediately adjacent to one another — for the
// critical-error screen, <body id="error-page"> is followed straight away by
// <div class="wp-die-message">{message}</div> with no chrome in between; for
// the db-error screen, the <title>Database Error</title> and the lone <h1>
// message are only a few dozen bytes apart in a page that is otherwise just
// a bare <html><head>...</head><body>...</body></html> skeleton. 1 KB gives
// generous margin for that real shape while staying far too tight to bridge
// an article's separate code sample and prose paragraphs.
const fatalProximityBytes = 1024

// criticalErrorPhraseEN is the hard-coded, non-translated English phrase WP
// core's fatal-error catch emits (wp-includes/load.php) before it has had a
// chance to load translations.
const criticalErrorPhraseEN = "there has been a critical error on this website"

// criticalErrorPhrases is the SUPPLEMENTARY, precise detection layer for the
// wp_die() critical-error screen (issue #132): each entry is matched near a
// structural marker exactly like criticalErrorPhraseEN always was. It is not
// the primary defense against non-English sites — bodyErrorPageRe below is,
// because it keys on markup WP core hard-codes and never translates — but it
// stays as a belt-and-suspenders layer for a hypothetical page that kept a
// recognizable message string while altering the body-tag structure enough to
// dodge bodyErrorPageRe. English plus the two German wp_die variants WP core
// ships (informal "deiner"/formal "Ihrer" address) are listed because the
// reporter's fleet is ~90% German; any other locale is still caught by the
// structural signal without needing an entry here.
var criticalErrorPhrases = []string{
	criticalErrorPhraseEN,
	"es gab einen kritischen fehler auf deiner website",
	"es gab einen kritischen fehler auf ihrer website",
}

// dbConnectionPhrase is the hard-coded, non-translated English phrase
// wpdb::bail()/dead_db() emits before WordPress has loaded translations.
const dbConnectionPhrase = "error establishing a database connection"

// bodyErrorPageRe matches WordPress core's _default_wp_die_handler() document
// by its actual opening <body> tag: `id="error-page"` (single or double
// quotes), allowing any other attributes before/around it. This tag, along
// with the `class="wp-die-message"` wrapper around the message (checked
// alongside it in scanFatal), is hard-coded in wp-includes/functions.php and
// is NEVER translated — only the visible message text is — so this is the
// locale-independent, primary signal for the critical-error screen: it
// catches German, French, or any other locale with zero phrase-table
// maintenance (issue #132).
//
// Keying on the BODY TAG specifically (not "error-page" appearing anywhere)
// is what keeps this safe from false positives: a healthy themed page's real
// <body> tag is body_class()-generated (e.g. `<body class="home page ...">`)
// and never carries id="error-page", and a tutorial page that quotes this
// exact markup in a code sample does so HTML-escaped (`&lt;body ...&gt;`),
// which does not contain a literal "<body" and so cannot match this pattern.
//
// The attribute is required to be WHITESPACE-separated (`\s`, not a bare word
// boundary `\b`): a real id attribute is always preceded by whitespace —
// `<body id=...>` or `<body class="x" id=...>` — whereas `\b` also matches at
// a hyphen (a non-word byte), so it let a contrived `<body data-id="error-page">`
// or `<body my-id='error-page'>` match even though neither is WP core's
// error-page body tag (issue #132 M2 regex-tightening fix).
var bodyErrorPageRe = regexp.MustCompile(`<body[^>]*\sid=["']error-page["']`)

// wpDieMessageClassLiteral is the companion structural marker checked near a
// bodyErrorPageRe match (trigger 1 only): WP core wraps the (translated)
// message in `<div class="wp-die-message">`, immediately following the
// <body> tag with no theme chrome in between. A bare `class="wp-die-message"`
// literal is safe here specifically because trigger 1 only ever checks it
// alongside an ALREADY-matched real, unescaped bodyErrorPageRe tag — an
// escaped code sample never gets this far, since bodyErrorPageRe itself
// cannot match escaped markup (see its comment).
const wpDieMessageClassLiteral = `class="wp-die-message"`

// wpDieMessageDivLiteral is the companion structural marker for the
// phrase-table path (trigger 2 in scanFatal): unlike wpDieMessageClassLiteral
// above, trigger 2 has no already-matched real <body> tag to lean on, so its
// own marker must itself require the REAL (unescaped) opening tag prefix,
// `<div class="wp-die-message"`, not the bare `class="wp-die-message"`
// attribute text. HTML-escaping rewrites `<`/`>` to `&lt;`/`&gt;` but leaves
// `class="..."` byte sequences untouched inside a quoted code sample, so the
// bare attribute text alone survives escaping and a tutorial article that
// shows the wp_die markup as an escaped `<pre><code>` sample can supply it —
// this is the exact false positive found in verification: a healthy German
// how-to page whose code sample renders `&lt;body id="error-page"&gt;` /
// `&lt;div class="wp-die-message"&gt;` within proximity of the German phrase
// in its prose was misclassified DOWN. Requiring the `<div` tag prefix closes
// that gap the same way bodyErrorPageRe already closes it for the body tag:
// escaping the `<` breaks the literal, so an escaped sample can never match.
const wpDieMessageDivLiteral = `<div class="wp-die-message"`

// dbErrorTitleRe matches WP core's dead_db() default <title> element
// verbatim: just "Database Error", with no site branding around it. A real
// page's own <title> almost always carries the site name/tagline around any
// prose mention of a database error (e.g. a "How to Fix the Error
// Establishing a Database Connection in WordPress - My Blog" support
// article), so requiring the tag's entire content to be exactly this phrase
// is a structural signal a legitimate page will not produce incidentally.
var dbErrorTitleRe = regexp.MustCompile(`<title[^>]*>\s*database error\s*</title>`)

// scanFatal inspects the buffered response body (at most maxProbeBody bytes,
// see Probe) for the signature of a WordPress fatal-error page served with
// HTTP 200 — the wp_die()-rendered critical-error screen, or the classic
// database connection failure screen (issue #132).
//
// It only scans text/html responses (JSON/XML/binary bodies are skipped
// outright). The critical-error screen is detected by TWO independent
// triggers, either of which reclassifies the probe as DOWN with reason
// "wp_fatal_error":
//
//  1. The locale-independent structural signal (primary): bodyErrorPageRe's
//     <body id="error-page"> matched near the wp-die-message class wrapper.
//     Both are hard-coded in WP core and never translated, so this alone
//     catches the error screen on every locale — including the reporter's
//     ~90%-German fleet — with no phrase-table maintenance.
//  2. The localized phrase table (supplementary, precise layer): a REAL,
//     unescaped structural marker — bodyErrorPageRe's <body id="error-page">
//     tag, or the wpDieMessageDivLiteral <div class="wp-die-message"> tag
//     prefix — co-occurring with one of criticalErrorPhrases. This exists for
//     a hypothetical page that kept a recognizable message string while
//     altering the body-tag structure enough to dodge trigger 1. Both markers
//     require the real, unescaped tag prefix (not a bare attribute literal)
//     for the same reason bodyErrorPageRe does: HTML-escaping rewrites `<`/`>`
//     but leaves `id="..."`/`class="..."` byte sequences untouched, so a bare
//     attribute literal alone would still match inside an escaped code-sample
//     quote of the markup (issue #132 false-positive fix — a healthy how-to
//     page's escaped `<pre><code>` sample was tripping this trigger).
//
// Every check requires a structural marker AND its companion signal to
// co-occur WITHIN fatalProximityBytes of each other — true DOM co-location,
// not mere presence anywhere in the buffered response. That is what makes a
// false-DOWN on a healthy site (one that merely discusses the error, quotes
// its markup HTML-escaped in a code sample, or renders its own themed <body>
// tag) effectively impossible, while still catching the real chrome-less
// error document wherever it lands in the buffered window (including one
// appended after tens of KB of already-flushed theme output — issue #132 S1).
//
// This is intentionally stricter than internal/agentcmd/client.go's
// fatalSignatures ("fatal error", "uncaught error", ...): that loose list is
// fine for the post-update self-probe of a site we just changed, but far too
// loose for monitoring arbitrary public sites, whose legitimate content may
// contain those substrings.
func scanFatal(body []byte, contentType string) (down bool, reason string) {
	if !isHTMLContentType(contentType) {
		return false, ""
	}
	window := bytes.ToLower(body)

	if markerRegexNearLiteral(window, bodyErrorPageRe, wpDieMessageClassLiteral, fatalProximityBytes) {
		return true, "wp_fatal_error"
	}

	for _, phrase := range criticalErrorPhrases {
		if markerRegexNearLiteral(window, bodyErrorPageRe, phrase, fatalProximityBytes) ||
			markerNearPhrase(window, wpDieMessageDivLiteral, phrase, fatalProximityBytes) {
			return true, "wp_fatal_error"
		}
	}

	if loc := dbErrorTitleRe.FindIndex(window); loc != nil {
		lo, hi := windowAround(loc[0], loc[1], fatalProximityBytes, len(window))
		if bytes.Contains(window[lo:hi], []byte(dbConnectionPhrase)) {
			return true, "wp_db_error"
		}
	}

	return false, ""
}

// markerNearPhrase reports whether any occurrence of the literal marker and
// any occurrence of phrase fall within proximity bytes of each other in
// window (see fatalProximityBytes for why proximity, not mere presence,
// matters here).
func markerNearPhrase(window []byte, marker, phrase string, proximity int) bool {
	markerBytes, phraseBytes := []byte(marker), []byte(phrase)
	for cursor := 0; ; {
		idx := bytes.Index(window[cursor:], markerBytes)
		if idx < 0 {
			return false
		}
		pos := cursor + idx
		lo, hi := windowAround(pos, pos+len(markerBytes), proximity, len(window))
		if bytes.Contains(window[lo:hi], phraseBytes) {
			return true
		}
		cursor = pos + 1
	}
}

// markerRegexNearLiteral reports whether any match of re and any occurrence
// of literal fall within proximity bytes of each other in window. This is
// markerNearPhrase's counterpart for a structural marker that needs a regex
// (bodyErrorPageRe's <body> tag can carry arbitrary other attributes) rather
// than a fixed literal. literal is not necessarily another structural
// marker — scanFatal's trigger 2 also calls this with a criticalErrorPhrases
// entry as literal, to check a phrase's proximity to the regex-matched body
// tag.
func markerRegexNearLiteral(window []byte, re *regexp.Regexp, literal string, proximity int) bool {
	literalBytes := []byte(literal)
	for _, loc := range re.FindAllIndex(window, -1) {
		lo, hi := windowAround(loc[0], loc[1], proximity, len(window))
		if bytes.Contains(window[lo:hi], literalBytes) {
			return true
		}
	}
	return false
}

// windowAround returns the [lo, hi) slice bounds spanning [start, end)
// expanded by proximity bytes on each side, clamped to [0, length).
func windowAround(start, end, proximity, length int) (lo, hi int) {
	lo = start - proximity
	if lo < 0 {
		lo = 0
	}
	hi = end + proximity
	if hi > length {
		hi = length
	}
	return lo, hi
}

// isHTMLContentType reports whether the response's Content-Type header names
// text/html, ignoring parameters (charset, etc.) and case.
func isHTMLContentType(contentType string) bool {
	ct := contentType
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.EqualFold(strings.TrimSpace(ct), "text/html")
}
