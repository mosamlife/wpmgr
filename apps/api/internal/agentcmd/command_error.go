package agentcmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/mosamlife/wpmgr/apps/api/internal/humantext"
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

	// embeddedCode is the code of a WordPress REST error envelope found inside
	// a 403 body that did not parse as JSON as a whole (see
	// embeddedRefusalCode). Read only by ForbiddenMessage, through
	// refusalCode; every other classifier sees the body exactly as before.
	embeddedCode string
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
		} else if status == http.StatusForbidden {
			ce.embeddedCode = embeddedRefusalCode(fullBody)
		}
	}
	return ce
}

// restErrorPrefix is how a WordPress REST error reply begins: wp_json_encode
// writes the WP_Error code first and adds no whitespace.
var restErrorPrefix = []byte(`{"code":`)

// embeddedRefusalCode returns the code of the first WordPress REST error
// envelope inside body, or "" when there is none. A site with display_errors
// on prints PHP notices and warnings in front of the REST reply, so a 403 the
// agent sent can arrive behind other output and fail to parse as a whole.
// Only an envelope whose data.status is 403 counts.
func embeddedRefusalCode(body []byte) string {
	i := bytes.Index(body, restErrorPrefix)
	if i < 0 {
		return ""
	}
	var env commandErrorEnvelope
	if err := json.NewDecoder(bytes.NewReader(body[i:])).Decode(&env); err != nil {
		return ""
	}
	if env.Data.Status != http.StatusForbidden {
		return ""
	}
	return env.Code
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

// maxReasonLen caps the sanitised reason, in bytes.
const maxReasonLen = 200

// sanitizeReason turns the agent's free-form message into a short, safe
// sentence fragment: the agent's generic wrapper is stripped, and the rest
// goes through humantext.Reason, the shared pipeline for prose a site
// reported (valid UTF-8, NFKC, control/format/space replacement, whitespace
// collapse, redaction of links, addresses, hostnames, IP addresses, absolute
// paths and encoded runs, and a cap of maxReasonLen bytes that redacts again
// after the cut).
func sanitizeReason(message, rawException string) string {
	return humantext.Reason(stripAgentPrefix(message, rawException), maxReasonLen)
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
	return humantext.CapBytes(msg, maxOperatorMessage)
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
	return humantext.CapBytes(msg, maxOperatorMessage)
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

// ---------------------------------------------------------------------------
// HTTP 403 refusals (GH #679)
// ---------------------------------------------------------------------------

// agentRefusalCodePattern is the allow-list for a refusal code shown to an
// operator. Every code the agent's router refuses with is "wpmgr_" plus a
// lowercase snake_case category; anything else is never echoed.
var agentRefusalCodePattern = regexp.MustCompile(`^wpmgr_[a-z_]{1,40}$`)

// reconnectRefusalCodes are the agent refusals that mean the site's
// connection to WPMgr is missing or no longer valid.
var reconnectRefusalCodes = map[string]bool{
	"wpmgr_aud_mismatch":        true,
	"wpmgr_site_not_enrolled":   true,
	"wpmgr_sig_failed":          true,
	"wpmgr_key_not_provisioned": true,
}

// Fixed copy for a 403 that did not come from the WPMgr agent.
const (
	// forbiddenFirewallAdvice: the reply carried no code at all (an HTML
	// page, plain text, an empty body), which a WordPress REST reply always
	// does.
	forbiddenFirewallAdvice = "A firewall or security rule blocked the request (HTTP 403) before it reached the WPMgr agent. " +
		"Ask the site's host to allow requests to /wp-json/wpmgr/v1/ from WPMgr, and allow them in any firewall service or security plugin the site uses."
	// forbiddenOtherAdvice: the reply had a code, but not one of the
	// agent's.
	forbiddenOtherAdvice = "Something on the site other than the WPMgr agent refused the request (HTTP 403), " +
		"such as a security plugin or a rule that restricts the WordPress REST API. Allow requests to /wp-json/wpmgr/v1/ from WPMgr there."
)

// refusalCode is the 403 reply's code: the parsed envelope's, or failing
// that, one found inside a reply that did not parse as a whole.
func (e *CommandError) refusalCode() string {
	if e.Code != "" {
		return e.Code
	}
	return e.embeddedCode
}

// ForbiddenMessage returns the operator-facing failure text for a signed
// command the site answered with HTTP 403, and false when err is not such an
// answer (a transport failure, a redirect, any other status). action names
// what did not happen, capitalised ("Update", "Dry run"), the way
// RedirectError.OperatorMessage takes it.
//
// Who refused decides the remedy. The WPMgr agent refuses a request it cannot
// verify before the command runs, with a code that is "wpmgr_" plus a
// category, and that category names the fix: the site's clock, its
// connection, or a header the host strips. A reply with no code came from
// something in front of the agent, such as a web server rule, a host or CDN
// firewall, or a security plugin's block page.
//
// Every sentence is fixed copy. The only text from the reply that can appear
// is an agent code that matches agentRefusalCodePattern. The raw reply stays
// in err.Error(), which callers keep in the task's error log.
func ForbiddenMessage(err error, action string) (string, bool) {
	ce, ok := AsCommandError(err)
	if !ok || ce.Status != http.StatusForbidden {
		return "", false
	}
	lead := action + " not started. "
	again := "then run the " + strings.ToLower(action) + " again."

	raw := ce.refusalCode()
	code := sanitizeAttemptCode(raw)
	if !agentRefusalCodePattern.MatchString(code) {
		code = ""
	}

	switch {
	case raw == "":
		return lead + forbiddenFirewallAdvice, true
	case code == "":
		return lead + forbiddenOtherAdvice, true
	case code == "wpmgr_token_expired" || code == "wpmgr_token_skew":
		return lead + fmt.Sprintf("The WPMgr agent refused the request (HTTP 403, %s) because the site's server clock differs from WPMgr's, "+
			"and WPMgr's signed requests are valid for less than a minute. Ask the site's host to sync the server time (NTP), %s", code, again), true
	case reconnectRefusalCodes[code]:
		return lead + fmt.Sprintf("The WPMgr agent refused the request (HTTP 403, %s) because the site's connection to WPMgr is missing or no longer valid. "+
			"Reconnect the site to WPMgr, %s", code, again), true
	case code == "wpmgr_missing_token":
		// WPMgr always sends the header, so its absence at the agent means a
		// layer between the two dropped or replaced it. Reconnecting does not
		// change that.
		return lead + fmt.Sprintf("The request reached the WPMgr agent without its Authorization header (HTTP 403, %s): "+
			"the site's web server, or a proxy in front of it, did not pass the header on. "+
			"Ask the site's host to forward the Authorization header to PHP for requests to /wp-json/wpmgr/v1/, %s", code, again), true
	case code == "wpmgr_invalid_token":
		// This code covers several causes; the two a site owner can fix are
		// the clock and the connection.
		return lead + fmt.Sprintf("The WPMgr agent could not verify the request (HTTP 403, %s). "+
			"Check the site's server clock first and ask the host to sync it (NTP) if it is off; "+
			"if the clock is correct, reconnect the site to WPMgr. Then run the %s again.", code, strings.ToLower(action)), true
	default:
		return lead + fmt.Sprintf("The WPMgr agent refused the request (HTTP 403, %s).", code), true
	}
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
