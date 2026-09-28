package agentcmd

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// maxRedirectTargetLen caps how much of a site-supplied Location is kept. The
// value ends up in snapshot messages, audit rows and the UI, so a longer
// target is cut back to its origin rather than stored whole.
const maxRedirectTargetLen = 256

// RedirectError is returned by every signed command when the site answered
// the command POST with a 3xx redirect. The command was sent once, to the
// site's saved address, and was NOT re-sent to the redirect target: a signed
// command goes only to the saved address, so a redirect means the saved
// address is not the one the site serves the agent on.
//
// All URL fields are sanitised before they are stored here: http(s) only, no
// userinfo, no query, no fragment, at most maxRedirectTargetLen bytes. They
// are safe to show to an operator and to write to an audit row.
//
// The Error() text deliberately contains neither "status NNN" nor "rejected
// by agent", so string-based classifiers of the canonical agent-reject format
// cannot read a redirect as an HTTP 404 (an old agent) or a 4xx. Callers
// should still use errors.As first.
type RedirectError struct {
	// Command is the agent command that was refused (e.g. "backup").
	Command string
	// Status is the 3xx status code the site answered with.
	Status int
	// From is the command URL that was requested, built from the saved site
	// address.
	From string
	// To is the resolved redirect target. Empty when the site sent no
	// Location, or one that is not an absolute http(s) URL after resolution.
	To string
	// SuggestedSiteURL is the site address the redirect points at: To with the
	// command route suffix removed. Set only when To ends with exactly this
	// command's route and the redirect does not downgrade https to http;
	// empty otherwise.
	SuggestedSiteURL string
}

// Error implements error.
func (e *RedirectError) Error() string {
	to := e.To
	if to == "" {
		to = "no usable target"
	}
	return fmt.Sprintf("%s command: %s redirects to %s (HTTP %d); commands are sent only to the site's saved address, so the redirect was not followed",
		e.Command, e.fromSite(), to, e.Status)
}

// Downgrade reports whether the redirect goes from https to plain http.
func (e *RedirectError) Downgrade() bool {
	return strings.HasPrefix(e.From, "https://") && strings.HasPrefix(e.To, "http://")
}

// OperatorMessage is the plain-language failure text for an operator. action
// names what did not happen, capitalised (e.g. "Backup", "Restore",
// "Update"): "<action> not started. <Explanation>".
func (e *RedirectError) OperatorMessage(action string) string {
	return action + " not started. " + e.Explanation()
}

// Explanation says what the redirect was and what fixes it, naming the
// target: the part of OperatorMessage after its lead-in.
func (e *RedirectError) Explanation() string {
	from := e.fromSite()
	switch {
	case e.SuggestedSiteURL != "":
		return fmt.Sprintf("%s redirects to %s, and commands are sent only to the site's saved address. Reconnect the site to update its address to %s.",
			from, e.SuggestedSiteURL, e.SuggestedSiteURL)
	case e.Downgrade():
		return fmt.Sprintf("%s redirects to %s, which drops HTTPS, and commands are never sent over a downgraded connection. Serve the site's REST API (/wp-json/wpmgr/) over HTTPS without a redirect.",
			from, e.To)
	case e.To != "":
		return fmt.Sprintf("The site redirected its command address to %s. A redirect rule on the site or its CDN is catching /wp-json/wpmgr/; exempt it.",
			e.To)
	default:
		return fmt.Sprintf("The site answered its command address with a redirect (HTTP %d) and no usable target. A redirect rule on the site or its CDN is catching /wp-json/wpmgr/; exempt it.",
			e.Status)
	}
}

// SavedSiteURL is the site address the command was sent to: From with the
// command route removed.
func (e *RedirectError) SavedSiteURL() string { return e.fromSite() }

// fromSite is From with the command route removed: the saved site address as
// the operator knows it.
func (e *RedirectError) fromSite() string {
	if site, ok := trimCommandSuffix(e.From, e.Command); ok {
		return site
	}
	return e.From
}

// AsRedirect reports whether err is, or wraps, a *RedirectError, and returns
// it.
func AsRedirect(err error) (*RedirectError, bool) {
	var re *RedirectError
	if errors.As(err, &re) {
		return re, true
	}
	return nil, false
}

// isRedirectStatus reports whether code is a 3xx.
func isRedirectStatus(code int) bool { return code >= 300 && code < 400 }

// newRedirectError builds the typed error for a 3xx answer to a command sent
// to endpoint. The Location is resolved against the request URL (a relative
// Location is legal) and sanitised.
func newRedirectError(command, endpoint string, resp *http.Response) *RedirectError {
	re := &RedirectError{
		Command: command,
		Status:  resp.StatusCode,
		From:    sanitizeRedirectURL(mustParse(endpoint)),
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		var base *url.URL
		if resp.Request != nil && resp.Request.URL != nil {
			base = resp.Request.URL
		} else {
			base = mustParse(endpoint)
		}
		if base != nil {
			if target, err := base.Parse(loc); err == nil {
				re.To = sanitizeRedirectURL(target)
			}
		}
	}
	if re.To != "" && !re.Downgrade() {
		if site, ok := trimCommandSuffix(re.To, command); ok {
			re.SuggestedSiteURL = site
		}
	}
	return re
}

// mustParse parses raw, returning nil on error.
func mustParse(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	return u
}

// sanitizeRedirectURL reduces u to a value that is safe to store and show:
// an absolute http(s) URL with no userinfo, query or fragment, at most
// maxRedirectTargetLen bytes. A longer URL is cut back to its origin; one
// whose origin alone is too long, or that is not absolute http(s), becomes "".
func sanitizeRedirectURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme != "http" && scheme != "https") || u.Host == "" {
		return ""
	}
	clean := url.URL{
		Scheme: scheme,
		Host:   strings.ToLower(u.Host),
		Path:   u.Path,
	}
	s := clean.String()
	if len(s) <= maxRedirectTargetLen {
		return s
	}
	clean.Path = ""
	if s = clean.String(); len(s) <= maxRedirectTargetLen {
		return s
	}
	return ""
}

// trimCommandSuffix removes the command route (/wp-json/wpmgr/v1/command/<cmd>)
// from the end of a sanitised command URL, returning the site address it was
// built from. ok is false when the path does not end with exactly that route.
func trimCommandSuffix(commandURL, command string) (string, bool) {
	u, err := url.Parse(commandURL)
	if err != nil || u.Host == "" {
		return "", false
	}
	suffix := fmt.Sprintf(commandPathFormat, command)
	path := strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(path, suffix) {
		return "", false
	}
	u.Path = strings.TrimRight(strings.TrimSuffix(path, suffix), "/")
	u.RawPath = ""
	return u.String(), true
}
