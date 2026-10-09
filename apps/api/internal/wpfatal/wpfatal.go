// Package wpfatal recognises the error documents WordPress itself renders, by
// their structure: the wp_die() screen, which carries WordPress's
// critical-error screen, and the database connection failure screen.
//
// It is shared by the uptime monitor and the post-update health probe, so
// both apply one rule: a response is WordPress's error document only when it
// carries the markup WordPress hard-codes and never translates, close
// together, as WordPress renders it. A healthy page is never recognised as
// one. The package uses the standard library only.
package wpfatal

import (
	"bytes"
	"regexp"
	"strings"
)

// ReasonFatalError is Scan's reason for the wp_die() critical-error screen.
const ReasonFatalError = "wp_fatal_error"

// ReasonDBError is Scan's reason for the database connection failure screen.
const ReasonDBError = "wp_db_error"

// ProximityBytes bounds the byte distance allowed between a structural
// marker and its companion phrase for Scan to treat them as the same
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
const ProximityBytes = 1024

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
// alongside it in Scan), is hard-coded in wp-includes/functions.php and
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
// phrase-table path (trigger 2 in Scan): unlike wpDieMessageClassLiteral
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

// Scan inspects body for the signature of an error document WordPress itself
// rendered: the wp_die()-rendered critical-error screen, or the classic
// database connection failure screen (issue #132). The caller decides how
// much of the response to read and which responses to scan.
//
// It only scans text/html responses (JSON/XML/binary bodies are skipped
// outright). The critical-error screen is detected by TWO independent
// triggers, either of which reports found with reason
// ReasonFatalError:
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
// co-occur WITHIN ProximityBytes of each other — true DOM co-location,
// not mere presence anywhere in the buffered response. That is what makes a
// false-DOWN on a healthy site (one that merely discusses the error, quotes
// its markup HTML-escaped in a code sample, or renders its own themed <body>
// tag) effectively impossible, while still catching the real chrome-less
// error document wherever it lands in the buffered window (including one
// appended after tens of KB of already-flushed theme output — issue #132 S1).
//
// The uptime monitor and the post-update health probe both rely on this one
// recognizer, so a response counts as WordPress's error document only by the
// structure WordPress renders, never by what a page says.
func Scan(body []byte, contentType string) (found bool, reason string) {
	if !isHTMLContentType(contentType) {
		return false, ""
	}
	window := bytes.ToLower(body)

	if markerRegexNearLiteral(window, bodyErrorPageRe, wpDieMessageClassLiteral, ProximityBytes) {
		return true, ReasonFatalError
	}

	for _, phrase := range criticalErrorPhrases {
		if markerRegexNearLiteral(window, bodyErrorPageRe, phrase, ProximityBytes) ||
			markerNearPhrase(window, wpDieMessageDivLiteral, phrase, ProximityBytes) {
			return true, ReasonFatalError
		}
	}

	if loc := dbErrorTitleRe.FindIndex(window); loc != nil {
		lo, hi := windowAround(loc[0], loc[1], ProximityBytes, len(window))
		if bytes.Contains(window[lo:hi], []byte(dbConnectionPhrase)) {
			return true, ReasonDBError
		}
	}

	return false, ""
}

// markerNearPhrase reports whether any occurrence of the literal marker and
// any occurrence of phrase fall within proximity bytes of each other in
// window (see ProximityBytes for why proximity, not mere presence,
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
// marker — Scan's trigger 2 also calls this with a criticalErrorPhrases
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
