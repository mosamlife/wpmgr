package agentcmd

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// Codes DescribeSiteFailure assigns. The dashboard keys on them, so a code is
// never renamed once released.
const (
	// CodeSiteURLRedirects: the site answered the command address with a
	// redirect, so no command was sent.
	CodeSiteURLRedirects = "site_url_redirects"
	// CodeSiteRefused: the site answered HTTP 403, whether the WPMgr agent
	// refused the request or something in front of it did.
	CodeSiteRefused = "site_refused"
	// CodeAgentCommandFailed: the WPMgr agent ran the command and stopped with
	// an error.
	CodeAgentCommandFailed = "agent_command_failed"
	// CodeSiteError: the site answered with any other HTTP error status.
	CodeSiteError = "site_error"
	// CodeSiteTimeout: the site did not answer in time.
	CodeSiteTimeout = "site_timeout"
	// CodeSiteUnreachable: the connection to the site failed.
	CodeSiteUnreachable = "site_unreachable"
	// CodeSiteBadReply: the site answered with success, but the reply was not
	// the agent's JSON.
	CodeSiteBadReply = "site_bad_reply"
)

// SiteFailure is how a signed command the site did not answer usefully is told
// to an operator.
type SiteFailure struct {
	// Code is one of the Code constants above.
	Code string
	// Message is fixed copy for the operator. It never contains err.Error() or
	// any other text the site supplied; see DescribeSiteFailure for the three
	// values taken from the site's answer after they were reduced to a safe
	// shape.
	Message string
	// Status is the HTTP status the site answered with when it answered with
	// an error or a redirect, and 0 when no such status applies: a failure to
	// connect, a timeout, or a success reply that was not JSON.
	Status int
}

// DescribeSiteFailure classifies the error of a signed command, or of reading
// its reply, into a code and operator copy that a caller can return to the
// dashboard as it is. action names what did not happen, capitalised
// ("Diagnostics check"), the way ForbiddenMessage and
// RedirectError.OperatorMessage take it.
//
//   - a redirect: CodeSiteURLRedirects;
//   - HTTP 403: CodeSiteRefused, worded by ForbiddenMessage;
//   - the WPMgr agent's own failure (CommandError.AgentFailed):
//     CodeAgentCommandFailed;
//   - any other HTTP error status: CodeSiteError;
//   - a timeout: CodeSiteTimeout;
//   - a success reply that does not decode: CodeSiteBadReply;
//   - any other failure of the exchange with the site: CodeSiteUnreachable.
//
// Every sentence is fixed copy, or the fixed copy of a classifier in this
// package. The raw reply stays in err.Error(), which is never used here. The
// only values taken from the site's answer are the ones those classifiers
// already allow: an agent refusal code that matches wpmgr_<category>, the HTTP
// status number, and a redirect target after it was reduced to a plain http(s)
// address (see RedirectError). The wording for the agent's own failure carries
// none of the agent's message, class or source location.
//
// ok is false when err is not a failure to get an answer from the site, and
// the caller keeps treating it as its own error: a nil error, a command its
// caller cancelled, a failure before any request could be made (the saved
// address cannot be used, the command token could not be minted), and a
// success reply the agent itself marked ok:false.
func DescribeSiteFailure(err error, action string) (SiteFailure, bool) {
	if err == nil {
		return SiteFailure{}, false
	}
	if re, ok := AsRedirect(err); ok {
		return SiteFailure{Code: CodeSiteURLRedirects, Message: re.OperatorMessage(action), Status: re.Status}, true
	}
	if ce, ok := AsCommandError(err); ok {
		switch {
		case ce.Status == http.StatusForbidden:
			msg, _ := ForbiddenMessage(err, action)
			return SiteFailure{Code: CodeSiteRefused, Message: msg, Status: ce.Status}, true
		case ce.AgentFailed():
			return SiteFailure{Code: CodeAgentCommandFailed, Message: agentFailedMessage(action), Status: ce.Status}, true
		default:
			return SiteFailure{Code: CodeSiteError, Message: failedMessage(action, err), Status: ce.Status}, true
		}
	}
	if isDecodeErr(err) {
		return SiteFailure{Code: CodeSiteBadReply, Message: failedMessage(action, err)}, true
	}
	if isTimeoutErr(err) {
		return SiteFailure{Code: CodeSiteTimeout, Message: failedMessage(action, err)}, true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, ErrAgentReportedFailure) || failedInsideControlPlane(err) {
		return SiteFailure{}, false
	}
	return SiteFailure{Code: CodeSiteUnreachable, Message: failedMessage(action, err)}, true
}

// failedMessage is "<action> failed." followed by DescribeAttemptError's fixed
// sentence for err.
func failedMessage(action string, err error) string {
	return action + " failed. " + DescribeAttemptError(err)
}

// agentFailedMessage is the fixed copy for the WPMgr agent's own failure. The
// agent's message, exception class and source location are not shown here:
// they are text the site supplied.
func agentFailedMessage(action string) string {
	return action + " failed: the WPMgr agent on this site stopped with an error while running it. " +
		"Check the site's PHP error log for the cause, fix it, then run the " + strings.ToLower(action) + " again."
}

// failedInsideControlPlane reports whether err happened before any request
// could be made, so the site had no part in it: the saved address could not be
// turned into a command URL, the body could not be encoded, the command token
// could not be minted, or the request could not be built. A connection that
// failed before the request was written (a refused or unresolvable address, a
// failed certificate check) is marked not sent too, but it carries the
// transport's *url.Error, and it is the site that did not answer.
func failedInsideControlPlane(err error) bool {
	if !errors.Is(err, ErrCommandNotSent) {
		return false
	}
	var transport *url.Error
	return !errors.As(err, &transport)
}
