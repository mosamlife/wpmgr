package agentcmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
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
// an attacker who can make the agent (or something posing as it) answer could
// influence; these are shown to operators and, for a failed backup, emailed in
// plain text (see internal/backup's #753 phishing-surface note). Every field
// is validated against an allow-list shape before display, and the whole
// composed message is length-capped independent of any single field.

var (
	// exceptionPattern allows a PHP class name, optionally namespaced with a
	// leading backslash and backslash separators (e.g. "RuntimeException" or
	// "\Wpmgr\Backup\Exception").
	exceptionPattern = regexp.MustCompile(`^\\?[A-Za-z_][A-Za-z0-9_\\]{0,127}$`)
	// atPattern allows a relative "path/to/file.php:123"-shaped location. The
	// character class alone already rejects a Windows drive letter + backslash
	// path (backslash is not in it); the prefix/".." checks below reject a
	// leading-slash absolute path and path traversal, which the class permits.
	atPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}:\d{1,6}$`)
	// attemptCodePattern is the allow-list for a machine code shown verbatim
	// on a still-retrying attempt (e.g. a 403 "wpmgr_token_expired").
	attemptCodePattern = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

	urlPattern         = regexp.MustCompile(`(?i)\bhttps?://\S+`)
	posixPathPattern   = regexp.MustCompile(`/[A-Za-z0-9._-]+(?:/[A-Za-z0-9._-]+)+`)
	windowsPathPattern = regexp.MustCompile(`(?i)\b[a-z]:\\[^\s]*`)
	encodedRunPattern  = regexp.MustCompile(`[A-Za-z0-9+/=_-]{32,}`)
	whitespaceRun      = regexp.MustCompile(`\s+`)
)

// sanitizeException returns exc unchanged when it matches the PHP-class-name
// allow-list, or "" when it does not (a forged or malformed value is dropped
// entirely, never partially shown).
func sanitizeException(exc string) string {
	if exceptionPattern.MatchString(exc) {
		return exc
	}
	return ""
}

// sanitizeAt returns at unchanged when it is a safe relative "file:line"
// location, or "" otherwise. Beyond the character-class match, an absolute
// path (leading "/") and any ".." path-traversal segment are rejected even
// though the character class alone would allow them.
func sanitizeAt(at string) string {
	if !atPattern.MatchString(at) {
		return ""
	}
	if strings.HasPrefix(at, "/") {
		return ""
	}
	if strings.Contains(at, "..") {
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

// sanitizeReason turns the agent's free-form message into a short, safe
// sentence fragment: the generic "Command execution failed: <exception>: "
// wrapper is stripped, the text is forced to valid UTF-8, control and format
// characters (including bidi overrides and zero-width characters) become
// spaces, whitespace is collapsed, absolute paths and URLs are replaced with
// neutral placeholders, long encoded/opaque runs are redacted, and the result
// is capped at 200 bytes on a rune boundary.
func sanitizeReason(message, rawException string) string {
	reason := message
	if rawException != "" {
		reason = strings.TrimPrefix(reason, "Command execution failed: "+rawException+": ")
	}
	reason = strings.ToValidUTF8(reason, "")
	reason = replaceControlAndFormatChars(reason)
	reason = collapseWhitespace(reason)
	// URLs first, so a long token embedded in a query string is swallowed by
	// "[link]" rather than left for the encoded-run pass to fragment, and so
	// the "//host/path" shape of a URL is never mistaken for a POSIX path.
	reason = urlPattern.ReplaceAllString(reason, "[link]")
	reason = windowsPathPattern.ReplaceAllString(reason, "[path]")
	reason = posixPathPattern.ReplaceAllString(reason, "[path]")
	reason = encodedRunPattern.ReplaceAllString(reason, "[redacted]")
	reason = collapseWhitespace(reason)
	return capBytes(reason, 200)
}

// replaceControlAndFormatChars replaces every Unicode control character (Cc:
// C0/C1 controls) and format character (Cf: bidi overrides U+202A-U+202E and
// U+2066-U+2069, zero-width space/joiners U+200B-U+200D, etc.) with a single
// space.
func replaceControlAndFormatChars(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
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

// OperatorMessage builds the FINAL failure reason shown to an operator (and,
// for backups, emailed in plain text) when AgentFailed() is true. action is
// the capitalised verb naming the command family ("Backup", "Restore",
// "Scan"); the message lower-cases it where English grammar wants a noun.
//
// Every agent field is sanitised before it is folded in; the composed message
// is capped at 512 bytes, the same limit agentFailReason uses on the wire.
func (e *CommandError) OperatorMessage(action string) string {
	lower := strings.ToLower(action)
	if e == nil {
		return fmt.Sprintf("%s failed: the WPMgr agent on this site stopped with an error, so the %s was not retried.", action, lower)
	}

	// Agents before 0.61.150 send only {"code":..., "data":{"status":500}} —
	// no exception, no location, no echoed command. There is nothing more to
	// show, so say so and point at the fix (an agent update) rather than
	// showing an empty "Agent error: ." line.
	if e.Exception == "" && e.At == "" && e.DataCommand == "" {
		return fmt.Sprintf(
			"%s failed: the WPMgr agent on this site stopped with an error, so the %s was not retried. "+
				"This agent version does not report the error; update the WPMgr agent on this site to see it.",
			action, lower)
	}

	exc := sanitizeException(e.Exception)
	at := sanitizeAt(e.At)
	reason := sanitizeReason(e.Message, e.Exception)

	var agentErr string
	switch {
	case exc != "" && at != "":
		agentErr = fmt.Sprintf("Agent error: %s at %s: %s.", exc, at, reason)
	case exc != "":
		agentErr = fmt.Sprintf("Agent error: %s: %s.", exc, reason)
	default:
		agentErr = fmt.Sprintf("Agent error: %s.", reason)
	}

	msg := fmt.Sprintf(
		"%s failed: the WPMgr agent on this site stopped with an error, so the %s was not retried. %s "+
			"Fix the cause on the site, then run the %s again.",
		action, lower, agentErr, lower)
	return capBytes(msg, 512)
}

// DescribeAttemptError classifies a non-final agentcmd error (one the caller
// will retry) into a short, operator-facing sentence with no raw agent or
// transport text — never a body snippet, a stack trace, or free-form agent
// prose. It is meant for the "last error while retrying" column (e.g.
// backup_snapshots.error while status='running'), which is visible to any
// operator watching the run, not just the one who triggered it.
func DescribeAttemptError(err error) string {
	if err == nil {
		return ""
	}
	if ce, ok := AsCommandError(err); ok {
		switch {
		case ce.Status == 403:
			if code := sanitizeAttemptCode(ce.Code); code != "" {
				return fmt.Sprintf("The WPMgr agent refused the request (%s).", code)
			}
			return fmt.Sprintf("The site answered HTTP %d.", ce.Status)
		case ce.Status == 404:
			return "The site did not find the WPMgr agent (HTTP 404). Check that the plugin is active."
		case ce.Status == 500:
			// Covers both an agent-shaped 500 that failed AgentFailed() (a
			// data.command mismatch, say) and a plain WordPress fatal
			// (out-of-memory, time limit) with no wpmgr body at all — from the
			// HTTP status alone the two are indistinguishable, and both are a
			// WordPress-level crash rather than a proxy or host in front of it.
			return "WordPress on the site hit a critical error (HTTP 500)."
		case ce.Status >= 500:
			return fmt.Sprintf("The site returned a server error (HTTP %d) that did not come from the WPMgr agent.", ce.Status)
		default:
			return fmt.Sprintf("The site answered HTTP %d.", ce.Status)
		}
	}
	if IsTimeoutErr(err) {
		return "The site did not answer in time."
	}
	if isTLSErr(err) {
		return "The site's HTTPS certificate was not accepted."
	}
	return "Could not connect to the site."
}
