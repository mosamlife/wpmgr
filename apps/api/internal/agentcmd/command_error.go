package agentcmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// maxCommandErrorBody bounds how large a non-2xx agent response body postRaw
// will try to parse as a JSON command-failure envelope. The agent caps its own
// failure bodies at 512 bytes (fitFailureBody on the agent side); anything
// past 8 KiB is not agent-shaped, so parsing it further is wasted work on a
// possibly hostile or truncated body.
const maxCommandErrorBody = 8 << 10 // 8 KiB

// commandFailedCode is the WP_Error code the agent's router post-auth
// catch(\Throwable) block has used since before agent 0.61.150 for a command
// that threw during dispatch (class-router.php's catch-all handler).
const commandFailedCode = "wpmgr_command_failed"

// wordPressFatalCode is the error code WordPress's fatal-error handler puts
// in its JSON body when a REST request crashes PHP.
const wordPressFatalCode = "internal_server_error"

// CommandError is the typed result of a non-2xx response to a signed CP->agent
// command (see postRaw). It always carries the byte-identical legacy message
// (Error()) the pre-existing regex-based classifiers depend on, plus, when the
// body parsed as a JSON object within maxCommandErrorBody, the individual
// envelope fields that AgentFailed, OperatorMessage and DescribeAttemptError
// use to tell a genuine agent-side failure from a proxy's or a host's own 5xx.
type CommandError struct {
	// Command is the command name that was sent (e.g. "backup").
	Command string
	// Status is the HTTP status code the response carried.
	Status int
	// Code is the response body's top-level "code" field, when the body
	// parsed as a JSON object (e.g. "wpmgr_command_failed"). Empty when the
	// body did not parse, or parsed but had no such field.
	Code string
	// Message is the response body's top-level "message" field.
	Message string
	// DataCommand is body.data.command. Present only on agent 0.61.150+; an
	// older agent's equivalent failure body omits it entirely.
	DataCommand string
	// Exception is body.data.exception (e.g. "RuntimeException"). Present
	// only on agent 0.61.150+. NOT sanitised — display it only through
	// OperatorMessage, which sanitises before formatting.
	Exception string
	// At is body.data.at (e.g. "includes/commands/class-backup-command.php:239").
	// Present only on agent 0.61.150+. NOT sanitised.
	At string
	// DataStatus is body.data.status.
	DataStatus int

	// snippet is the exact, already-clamped body snippet postRaw builds today
	// (up to 512 bytes, with "…(truncated)" appended past that). Error()
	// replays it verbatim so every existing string/regex matcher keeps working
	// unchanged.
	snippet string
}

// commandErrorEnvelope is the shape of the agent's WP_Error-derived JSON body
// on a command failure, both the 0.61.150+ form (command/exception/at) and the
// older, pre-#756 form (status only).
type commandErrorEnvelope struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Status    int    `json:"status"`
		Command   string `json:"command"`
		Exception string `json:"exception"`
		At        string `json:"at"`
	} `json:"data"`
}

// newCommandError builds the typed error postRaw returns for a non-2xx
// response. snippet is the already-512-byte-clamped body text used verbatim by
// Error(); fullBody is the complete (up to maxRespBody) response body, parsed
// as a JSON command-failure envelope only when it is maxCommandErrorBody or
// smaller. A body that fails to parse, or is too large, leaves every envelope
// field at its zero value, which makes AgentFailed() report false — exactly
// the "stay retryable" behaviour an ambiguous response should get.
func newCommandError(command string, status int, snippet string, fullBody []byte) *CommandError {
	ce := &CommandError{
		Command: command,
		Status:  status,
		snippet: snippet,
	}
	if len(fullBody) <= maxCommandErrorBody {
		var env commandErrorEnvelope
		if err := json.Unmarshal(fullBody, &env); err == nil {
			ce.Code = env.Code
			ce.Message = env.Message
			ce.DataCommand = env.Data.Command
			ce.Exception = env.Data.Exception
			ce.At = env.Data.At
			ce.DataStatus = env.Data.Status
		}
	}
	return ce
}

// Error implements error. The format is BYTE-IDENTICAL to the untyped error
// postRaw returned before this type existed: existing regex classifiers
// depend on it verbatim — httpStatusPattern (client.go), agentStatusRE
// (internal/update/refresh.go:134), and the "status 404"/"status NNN"
// substring checks in internal/scan/worker.go:193 and
// internal/email/service.go:1185. Do not change this format without updating
// every one of those.
func (e *CommandError) Error() string {
	return fmt.Sprintf("%s command rejected by agent: status %d body=%s", e.Command, e.Status, e.snippet)
}

// AsCommandError unwraps err into a *CommandError, or returns (nil, false)
// when err is not one — a transport-level failure (timeout, TLS, connection
// refused, DNS, JWT-mint failure) or a redirect never wraps a CommandError.
func AsCommandError(err error) (*CommandError, bool) {
	var ce *CommandError
	if errors.As(err, &ce) {
		return ce, true
	}
	return nil, false
}

// AgentFailed reports whether this response is a genuine, terminal failure
// FROM THE AGENT ITSELF — as opposed to a transient failure a proxy, a
// misconfigured host, or WordPress's own fatal-error handler could equally
// produce. It requires every one of:
//   - HTTP status 500;
//   - the body parsed as a JSON object;
//   - code == "wpmgr_command_failed";
//   - data.status == 500;
//   - data.command is either absent (agents before 0.61.150) or equal to the
//     command that was actually sent.
//
// This can't be done cryptographically: the agent never signs its reply (see
// the #791 design doc). The rule is deliberately narrow so an ambiguous
// response stays retryable; only a response that positively matches this
// agent-produced shape is treated as final.
func (e *CommandError) AgentFailed() bool {
	if e == nil {
		return false
	}
	if e.Status != 500 {
		return false
	}
	if e.Code != commandFailedCode {
		return false
	}
	if e.DataStatus != 500 {
		return false
	}
	if e.DataCommand != "" && e.DataCommand != e.Command {
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Sanitiser
// ---------------------------------------------------------------------------
//
// Nothing below ever shows a caller the raw response body or headers. The
// agent's exception name, source location and message are free-form strings
// that anything able to answer for the site could influence. Every field is
// validated against an allow-list shape before display, and the whole
// composed message is length-capped independent of any single field. The
// failure email never carries the free-form reason at all (see
// NotificationMessage); the reason is for the dashboard, which renders it as
// plain text.

// maxAtLen bounds the source location shown to an operator.
const maxAtLen = 200

var (
	// exceptionPattern allows a PHP class name, optionally namespaced with a
	// leading backslash and backslash separators (e.g. "RuntimeException" or
	// "\Wpmgr\Backup\Exception").
	exceptionPattern = regexp.MustCompile(`^\\?[A-Za-z_][A-Za-z0-9_\\]{0,127}$`)
	// atPattern allows only a relative "dir/dir/file.php:123" location:
	// directory segments carry no dot, so neither "." nor ".." can appear as
	// a segment, the first character can never be "/", and the file must be
	// a .php file.
	atPattern = regexp.MustCompile(`^(?:[A-Za-z0-9_-]+/)*[A-Za-z0-9_.-]+\.php:\d{1,6}$`)
	// attemptCodePattern is the allow-list for a machine code shown verbatim
	// on a still-retrying attempt (e.g. a 403 "wpmgr_token_expired").
	attemptCodePattern = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
	// leadingClassPattern matches a PHP class name at the start of the
	// agent's message, followed by ": " or the end of the string. The agent
	// marks a class name it had to shorten with a leading "...".
	leadingClassPattern = regexp.MustCompile(`^(?:\.\.\.)?\\?[A-Za-z_][A-Za-z0-9_\\]*(?:: |$)`)

	// schemeURLPattern matches anything with a "//" authority, with or
	// without a scheme in front of it: http, https, ftp, a defanged "hxxps",
	// or a protocol-relative "//host".
	schemeURLPattern = regexp.MustCompile(`(?i)(?:\b[a-z][a-z0-9+.-]*:)?//\S*`)
	// emailPattern matches anything shaped like user@host.
	emailPattern = regexp.MustCompile(`\S+@\S+`)
	// uncPathPattern matches a Windows UNC path (\\server\share\...).
	uncPathPattern = regexp.MustCompile(`\\\\\S+`)
	// windowsPathPattern matches a drive-letter path with either separator.
	windowsPathPattern = regexp.MustCompile(`(?i)\b[a-z]:[\\/]\S*`)
	// posixPathPattern matches an ABSOLUTE POSIX path: a "/" at the start of
	// the text or after a character that cannot be part of a relative path.
	// The preceding character is captured and put back, so a relative path
	// such as "includes/commands/x.php" is left intact.
	posixPathPattern = regexp.MustCompile(`(^|[^A-Za-z0-9._/\-])/[A-Za-z0-9._-]+(?:/[A-Za-z0-9._-]*)*`)
	// hostPattern matches a dotted name whose last label starts with a
	// letter (a hostname shape, in any script, so an internationalised name
	// is covered as well as its punycode form), optionally followed by a
	// path. Group 1 is the character before the name (or the start of the
	// text), which is put back: Go's \b is ASCII-only, so the boundary is
	// spelled out. Any character that cannot be part of a name is a
	// boundary, "_" included, so "_evil.com" loses its host.
	// Group 2 is the name and its path.
	hostPattern = regexp.MustCompile(`(^|[^\p{L}\p{M}\p{N}])((?:[\p{L}\p{M}\p{N}](?:[\p{L}\p{M}\p{N}-]*[\p{L}\p{M}\p{N}])?\.)+\p{L}[\p{L}\p{M}\p{N}-]*[\p{L}\p{M}\p{N}](?:/\S*)?)`)
	// ipv4Pattern matches a dotted-quad address, with an optional port and
	// path. Like hostPattern, group 1 is the boundary in front of it (the
	// start of the text, or a character that is not an ASCII letter or
	// digit, "_" included) and is put back; group 2 is the address. A
	// letter directly in front ("v1.2.3.4") keeps the text as it is.
	ipv4Pattern = regexp.MustCompile(`(^|[^0-9A-Za-z])(\d{1,3}(?:\.\d{1,3}){3}\b(?::\d{1,5})?(?:/\S*)?)`)
	// encodedRunPattern matches a long token-shaped run; redactEncoded
	// decides whether it is one.
	encodedRunPattern = regexp.MustCompile(`[A-Za-z0-9+/=_-]{32,}`)
	whitespaceRun     = regexp.MustCompile(`\s+`)
)

// fileExtensions are the final labels that make a dotted name a file name,
// not a hostname, when nothing else about it looks like a link. None of them
// is a top-level domain.
var fileExtensions = map[string]struct{}{
	"php": {}, "phar": {}, "js": {}, "json": {}, "css": {}, "htm": {}, "html": {},
	"xml": {}, "sql": {}, "txt": {}, "log": {}, "ini": {}, "lock": {}, "yml": {},
	"yaml": {}, "gz": {}, "tar": {}, "bak": {}, "tmp": {}, "csv": {},
}

// sanitizeException returns exc unchanged when it matches the PHP-class-name
// allow-list, or "" when it does not (a forged or malformed value is dropped
// entirely, never partially shown).
func sanitizeException(exc string) string {
	if exceptionPattern.MatchString(exc) {
		return exc
	}
	return ""
}

// sanitizeAt returns at unchanged when it is a relative "dir/file.php:line"
// location of at most maxAtLen bytes, or "" otherwise.
func sanitizeAt(at string) string {
	if len(at) > maxAtLen {
		return ""
	}
	if !atPattern.MatchString(at) {
		return ""
	}
	return at
}

// sanitizeAttemptCode returns code unchanged when it matches the machine-code
// allow-list, or "" otherwise.
func sanitizeAttemptCode(code string) string {
	if attemptCodePattern.MatchString(code) {
		return code
	}
	return ""
}

// stripAgentPrefix removes the agent's generic wrapper from its message:
// "Command execution failed: <Class>: <reason>" becomes "<reason>", and a
// message that carries no reason ("Command execution failed: <Class>" or
// "Command execution failed.") becomes "". The class in the message is
// matched by shape, not only by equality with data.exception, because the
// agent may shorten one and not the other when it fits its body budget.
func stripAgentPrefix(message, rawException string) string {
	const generic = "Command execution failed"
	if message == generic+"." {
		return ""
	}
	rest, ok := strings.CutPrefix(message, generic+": ")
	if !ok {
		return message
	}
	if rawException != "" {
		if r, ok := strings.CutPrefix(rest, rawException+": "); ok {
			return r
		}
		if rest == rawException {
			return ""
		}
	}
	if loc := leadingClassPattern.FindStringIndex(rest); loc != nil {
		return rest[loc[1]:]
	}
	return rest
}

// redactHosts applies redactHost to every hostPattern match, keeping the
// boundary character each match captured in front of the name.
func redactHosts(s string) string {
	matches := hostPattern.FindAllStringSubmatchIndex(s, -1)
	if matches == nil {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	prev := 0
	for _, m := range matches {
		nameStart, nameEnd := m[4], m[5]
		b.WriteString(s[prev:nameStart])
		b.WriteString(redactHost(s[nameStart:nameEnd]))
		prev = nameEnd
	}
	b.WriteString(s[prev:])
	return b.String()
}

// redactHost replaces a hostname-shaped match with "[link]" unless it is a
// plain file name: no "www." in front, no path after it, and a final label
// that is a known file extension.
func redactHost(m string) string {
	if strings.Contains(m, "/") || strings.HasPrefix(strings.ToLower(m), "www.") {
		return "[link]"
	}
	ext := strings.ToLower(m[strings.LastIndexByte(m, '.')+1:])
	if _, ok := fileExtensions[ext]; ok {
		return m
	}
	return "[link]"
}

// redactEncoded replaces a long run with "[redacted]" when it carries a digit
// or a base64 symbol, the marks of a token, key or hash. A run of letters,
// "-", "_" and "/" alone is a relative path or a slug, and is kept.
func redactEncoded(m string) string {
	if strings.ContainsAny(m, "0123456789+=") {
		return "[redacted]"
	}
	return m
}

// maxReasonLen caps the sanitised reason, in bytes.
const maxReasonLen = 200

// sanitizeReason turns the agent's free-form message into a short, safe
// sentence fragment: the agent's generic wrapper is stripped, the text is
// forced to valid UTF-8 and NFKC-normalised (so full-width letters, "@",
// "/" and dots become their ASCII forms, and an ideographic full stop becomes
// "."), control, format and every kind of Unicode space or line/paragraph
// separator become a plain space, whitespace is collapsed, links, addresses,
// hostnames, IP addresses and absolute paths are replaced with neutral
// placeholders, long encoded runs are redacted, and the result is capped at
// maxReasonLen bytes on a rune boundary and redacted again, so the cut
// cannot leave a bare hostname behind.
func sanitizeReason(message, rawException string) string {
	reason := stripAgentPrefix(message, rawException)
	reason = strings.ToValidUTF8(reason, "")
	reason = norm.NFKC.String(reason)
	reason = strings.Map(asciiFullStop, reason)
	reason = replaceControlAndFormatChars(reason)
	reason = collapseWhitespace(reason)
	reason = redactReason(reason)
	return capAndRedact(reason, maxReasonLen)
}

// redactReason replaces every link, address, path, hostname, IP address and
// encoded run in s with its placeholder. It is idempotent: no placeholder
// matches any pattern.
func redactReason(s string) string {
	// Links first, so a token in a query string is swallowed by "[link]"
	// rather than fragmented by a later pass, and so the "//host/path" of a
	// URL is never mistaken for a POSIX path.
	s = schemeURLPattern.ReplaceAllString(s, "[link]")
	s = emailPattern.ReplaceAllString(s, "[address]")
	s = uncPathPattern.ReplaceAllString(s, "[path]")
	s = windowsPathPattern.ReplaceAllString(s, "[path]")
	s = posixPathPattern.ReplaceAllString(s, "${1}[path]")
	s = ipv4Pattern.ReplaceAllString(s, "${1}[link]")
	s = redactHosts(s)
	s = encodedRunPattern.ReplaceAllStringFunc(s, redactEncoded)
	return collapseWhitespace(s)
}

// capAndRedact caps an already-redacted s at limit bytes. Cutting can turn a
// kept file name into a bare hostname ("help.com.js" cut to "help.com"), so
// the capped text is redacted again. A placeholder can be a few bytes longer
// than what it replaced, so this repeats until the text fits; only the tail
// the cut created can change on each pass, and a cut placeholder matches
// nothing.
func capAndRedact(s string, limit int) string {
	for i := 0; i < 4; i++ {
		if len(s) <= limit {
			return s
		}
		s = redactReason(capBytes(s, limit))
	}
	if len(s) <= limit {
		return s
	}
	// Did not settle: drop the partial last word rather than show it.
	s = capBytes(s, limit)
	if k := strings.LastIndexByte(s, ' '); k > 0 {
		return s[:k]
	}
	return ""
}

// asciiFullStop maps the ideographic full stop (U+3002, which NFKC also
// produces from the halfwidth form) to ".", as hostname parsers do. NFKC
// already maps the full-width and small full stops.
func asciiFullStop(r rune) rune {
	if r == '\u3002' {
		return '.'
	}
	return r
}

// replaceControlAndFormatChars replaces every Unicode control character (Cc),
// format character (Cf: bidi overrides, zero-width characters) and space
// character (unicode.IsSpace, which includes U+2028 LINE SEPARATOR, U+2029
// PARAGRAPH SEPARATOR and U+00A0) with a single ASCII space, so the ASCII-only
// whitespace collapse that follows sees every one of them.
func replaceControlAndFormatChars(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) || unicode.IsSpace(r) {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// collapseWhitespace replaces every run of whitespace with a single space and
// trims the ends.
func collapseWhitespace(s string) string {
	return strings.TrimSpace(whitespaceRun.ReplaceAllString(s, " "))
}

// capBytes truncates s to at most limit bytes, backing off byte-by-byte until
// the result is valid UTF-8 so a multi-byte rune is never split.
func capBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	b := []byte(s)[:limit]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// Operator-facing copy
// ---------------------------------------------------------------------------

// maxOperatorMessage is the cap on a composed final failure reason, the same
// limit agentFailReason uses on the wire.
const maxOperatorMessage = 512

// agentErrorHead builds "Agent error: <Class> at <file.php:line>" from the
// sanitised fields, or "" when neither survived the sanitiser.
func agentErrorHead(exc, at string) string {
	switch {
	case exc != "" && at != "":
		return "Agent error: " + exc + " at " + at
	case exc != "":
		return "Agent error: " + exc
	case at != "":
		return "Agent error at " + at
	default:
		return ""
	}
}

// isOldAgentShape reports whether the body carried none of the fields agents
// 0.61.150 and later add, so there is nothing more to show.
func (e *CommandError) isOldAgentShape() bool {
	return e.Exception == "" && e.At == "" && e.DataCommand == ""
}

func stoppedSentence(action, lower string) string {
	return fmt.Sprintf("%s failed: the WPMgr agent on this site stopped with an error, so the %s was not retried.", action, lower)
}

func oldAgentMessage(action, lower string) string {
	return stoppedSentence(action, lower) +
		" This agent version does not report the error; update the WPMgr agent on this site to see it."
}

// OperatorMessage builds the FINAL failure reason shown to an operator in the
// dashboard, the schedule run and the audit log when AgentFailed() is true.
// action is the capitalised verb naming the command family ("Backup",
// "Restore", "Scan"); the message lower-cases it where English grammar wants
// a noun.
//
// Every agent field is sanitised before it is folded in, and the composed
// message is capped at 512 bytes. It includes the sanitised reason, so it is
// never the text of an outbound email: use NotificationMessage there.
func (e *CommandError) OperatorMessage(action string) string {
	lower := strings.ToLower(action)
	if e == nil {
		return stoppedSentence(action, lower)
	}
	if e.isOldAgentShape() {
		return oldAgentMessage(action, lower)
	}

	head := agentErrorHead(sanitizeException(e.Exception), sanitizeAt(e.At))
	reason := sanitizeReason(e.Message, e.Exception)

	var agentErr string
	switch {
	case head != "" && reason != "":
		agentErr = head + ": " + reason + "."
	case head != "":
		agentErr = head + "."
	case reason != "":
		agentErr = "Agent error: " + reason + "."
	}

	msg := stoppedSentence(action, lower) + " "
	if agentErr != "" {
		msg += agentErr + " "
	}
	msg += fmt.Sprintf("Fix the cause on the site, then run the %s again.", lower)
	return capBytes(msg, maxOperatorMessage)
}

// NotificationMessage is the failure reason for an outbound plain-text email.
// It carries the control plane's own wording plus, when they pass the
// sanitiser, the exception class and the source location, and never the
// agent's free-form reason: text the site supplied does not go into an email.
// It points the reader at the dashboard, which shows the full reason.
func (e *CommandError) NotificationMessage(action string) string {
	lower := strings.ToLower(action)
	if e == nil {
		return stoppedSentence(action, lower)
	}
	if e.isOldAgentShape() {
		return oldAgentMessage(action, lower)
	}
	msg := stoppedSentence(action, lower) + " "
	if head := agentErrorHead(sanitizeException(e.Exception), sanitizeAt(e.At)); head != "" {
		msg += head + ". "
	}
	msg += fmt.Sprintf("Fix the cause on the site, then run the %s again. The WPMgr dashboard shows the agent's full message.", lower)
	return capBytes(msg, maxOperatorMessage)
}

// DescribeAttemptError classifies a non-final agentcmd error (one the caller
// will retry) into a short, operator-facing sentence with no raw agent or
// transport text — never a body snippet, a stack trace, or free-form agent
// prose. It is meant for the "last error while retrying" field
// (attempt_error on a running backup and its schedule run), which any
// operator watching the run can see.
func DescribeAttemptError(err error) string {
	if err == nil {
		return ""
	}
	if ce, ok := AsCommandError(err); ok {
		switch {
		case ce.Status == 403:
			code := sanitizeAttemptCode(ce.Code)
			switch {
			case strings.HasPrefix(code, "wpmgr_"):
				return fmt.Sprintf("The WPMgr agent refused the request (%s).", code)
			case code != "":
				return fmt.Sprintf("The site refused the request (HTTP 403, %s).", code)
			default:
				return "The site refused the request (HTTP 403)."
			}
		case ce.Status == 404:
			return "The site did not find the WPMgr agent (HTTP 404). Check that the plugin is active."
		case ce.Status == 500 && ce.Code == wordPressFatalCode:
			// WordPress's fatal-error handler answers a REST request with
			// this code: the site's PHP crashed (out of memory, a time
			// limit) before the agent could answer.
			return "WordPress on the site hit a critical error (HTTP 500)."
		case ce.Status == 500:
			// Any other 500: a web server's own error page, a body with
			// output in front of the JSON, or an agent-shaped body that
			// failed AgentFailed(). None of these can be attributed.
			return "The site returned a server error (HTTP 500)."
		case ce.Status >= 500:
			return fmt.Sprintf("The site returned a server error (HTTP %d) that did not come from the WPMgr agent.", ce.Status)
		default:
			return fmt.Sprintf("The site answered HTTP %d.", ce.Status)
		}
	}
	if isDecodeErr(err) {
		return "The site answered, but the reply was not the WPMgr agent's response. A PHP notice or another plugin's output may be in the way."
	}
	if IsTimeoutErr(err) {
		return "The site did not answer in time."
	}
	if isTLSErr(err) {
		return "The site's HTTPS certificate was not accepted."
	}
	return "Could not connect to the site."
}

// isDecodeErr reports whether err is a JSON decode failure on a 2xx reply:
// the site answered, but not with the agent's JSON.
func isDecodeErr(err error) bool {
	var syn *json.SyntaxError
	if errors.As(err, &syn) {
		return true
	}
	var typ *json.UnmarshalTypeError
	return errors.As(err, &typ)
}
