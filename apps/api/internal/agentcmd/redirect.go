package agentcmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/siteaddr"
)

// maxRedirectTargetLen caps how much of a site-supplied Location is kept. The
// value ends up in snapshot messages, audit rows and the UI, so a longer
// target is cut back to its origin rather than stored whole.
const maxRedirectTargetLen = 256

// RedirectError is returned by every signed command when the site answered
// the command POST with a 3xx redirect that was not followed. A signed command
// goes only to the site's saved address; the one exception is a redirect from
// the saved http address to the same host, port and path over https, which is
// followed once (sameHostHTTPSUpgrade). Any other redirect, and any redirect
// answering that https retry, ends the command here.
//
// A redirect is terminal whatever its status, 302 and 307 included: a
// temporary redirect that is not a same-host https upgrade fails at once with
// this error rather than having the job layer re-send a signed command into
// it, and the operator can re-run the action once the redirect is fixed.
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
	// SuggestedSiteURL is the address the saved one would become. It is set
	// only when To ends with exactly this command's route and the site address
	// that leaves is one siteaddr.PlanStrict adopts over the saved address (a
	// leading "www." toggle and/or an http to https upgrade, on the same port
	// and path, with hosts compared in their ASCII form), and it is that
	// plan's To: the value an agent push would store. Empty otherwise.
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
	target, plan := e.plan()
	switch {
	case target != "" && plan.Decision == siteaddr.Adopt:
		// plan.To, not target, is what would be stored: target may carry a
		// default port or another spelling of the host. Whether another site
		// in the workspace already holds plan.To is not known here, so the
		// copy states that condition rather than promising the update.
		return fmt.Sprintf("%s redirects to %s, so no command was sent. If WordPress on the site reports %s as its address, the saved address updates to %s automatically at a later check-in from the site. That update does not happen while another site in this workspace uses %s: if one does, remove or change the duplicate site.",
			from, target, plan.To, plan.To, plan.To)
	case e.SelfRedirect():
		return fmt.Sprintf("%s redirects its command address back to itself (HTTP %d), so no command was sent. %s",
			from, e.Status, exemptAdvice)
	case e.To != "" && e.Downgrade():
		return fmt.Sprintf("%s redirects to %s, which drops HTTPS, so no command was sent. %s",
			from, e.To, exemptAdvice)
	case e.To != "":
		return fmt.Sprintf("%s redirects to %s, so no command was sent. %s",
			from, e.To, exemptAdvice)
	default:
		return fmt.Sprintf("%s answered with a redirect (HTTP %d) that names no usable address, so no command was sent. %s",
			from, e.Status, exemptAdvice)
	}
}

// exemptAdvice is the fix for every redirect whose target is not an address
// the saved one may become.
const exemptAdvice = "Exempt /wp-json/wpmgr/ from the redirect on the site or its CDN."

// SelfRedirect reports whether the redirect target names the saved site
// address itself: the command route redirects back to the same install.
func (e *RedirectError) SelfRedirect() bool {
	target, plan := e.plan()
	return target != "" && plan.Decision == siteaddr.Same
}

// plan returns the site address the redirect target names (To with this
// command's route removed; "" when To does not end with exactly that route)
// and what siteaddr.PlanStrict decides for it against the saved address.
func (e *RedirectError) plan() (string, siteaddr.PlanResult) {
	target, ok := trimCommandSuffix(e.To, e.Command)
	if !ok || target == "" {
		return "", siteaddr.PlanResult{Decision: siteaddr.Mismatch}
	}
	return target, siteaddr.PlanStrict(e.fromSite(), target)
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

// commandRedirectProbeTimeout bounds CommandRedirectTarget's single ping.
const commandRedirectProbeTimeout = 10 * time.Second

// CommandRedirectTarget sends one signed ping to siteURL, the site's saved
// address, and reports whether it was answered with a redirect that was not
// followed. suggested is that redirect's SuggestedSiteURL: the address the
// saved one would become, or "" when the redirect names none. The ping goes
// only to the saved address (and, under the same-host upgrade rule, to its
// https form), never to the address being considered. It implements
// site.CommandRedirectProber.
func (c *Client) CommandRedirectTarget(ctx context.Context, siteID uuid.UUID, siteURL string) (suggested string, redirected bool) {
	ctx, cancel := context.WithTimeout(ctx, commandRedirectProbeTimeout)
	defer cancel()
	_, err := c.Ping(ctx, siteID, siteURL)
	re, ok := AsRedirect(err)
	if !ok {
		return "", false
	}
	return re.SuggestedSiteURL, true
}

// CommandPingOK sends one signed ping to exactly siteURL and reports whether
// the agent answered it with a 2xx carrying ok: true. A redirect, any other
// status and a transport failure are false. Called with an https address, it
// can follow no redirect at all (only an http address is ever upgraded). It
// implements site.CommandRedirectProber.
func (c *Client) CommandPingOK(ctx context.Context, siteID uuid.UUID, siteURL string) bool {
	ctx, cancel := context.WithTimeout(ctx, commandRedirectProbeTimeout)
	defer cancel()
	out, err := c.Ping(ctx, siteID, siteURL)
	return err == nil && out.OK
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
	if target, plan := re.plan(); target != "" && plan.Decision == siteaddr.Adopt {
		re.SuggestedSiteURL = plan.To
	}
	return re
}

// sameHostHTTPSUpgrade reports whether loc, the Location of a 3xx answer to a
// request for from, is the one redirect a signed command follows: from is
// http and the target is the same host, port, path and query over https. It
// returns the resolved target when it is.
//
// The host matches only when both hostnames reduce to the same ASCII form
// (siteaddr.SameHost: IDNA lookup conversion, then an ASCII-only lowercase
// compare), the form the retry is dialled by. A hostname that does not
// convert never matches.
//
// The port matches when both are the scheme defaults (from's is "" or "80",
// the target's is "" or "443"), or both are explicit, equal and not defaults.
// The target may carry no userinfo, and a fragment only when it is from's.
// Because from must be http and the target is always https, a request built
// from the target can never qualify again, so at most one redirect is ever
// followed, and a downgrade never is.
func sameHostHTTPSUpgrade(from *url.URL, loc string) (*url.URL, bool) {
	if from == nil || from.Scheme != "http" || loc == "" {
		return nil, false
	}
	target, err := from.Parse(loc)
	if err != nil || target.Scheme != "https" {
		return nil, false
	}
	if !siteaddr.SameHost(from.Hostname(), target.Hostname()) {
		return nil, false
	}
	fromPort, toPort := from.Port(), target.Port()
	fromDefault := fromPort == "" || fromPort == "80"
	toDefault := toPort == "" || toPort == "443"
	bothDefault := fromDefault && toDefault
	sameExplicit := !fromDefault && !toDefault && fromPort == toPort
	if !bothDefault && !sameExplicit {
		return nil, false
	}
	if target.User != nil {
		return nil, false
	}
	if target.EscapedPath() != from.EscapedPath() {
		return nil, false
	}
	if target.RawQuery != from.RawQuery || target.ForceQuery != from.ForceQuery {
		return nil, false
	}
	if target.Fragment != "" && target.Fragment != from.Fragment {
		return nil, false
	}
	return target, true
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
//
// The value is written as scheme://host[:port] followed by the escaped path,
// the form siteaddr.Join writes an adopted address in, so an
// internationalised host keeps its form rather than being percent-encoded. A
// host holding a space, a control or format character, or any other
// non-graphic character becomes "", so the value cannot disguise itself when
// shown.
func sanitizeRedirectURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme != "http" && scheme != "https") || u.Host == "" || u.Hostname() == "" {
		return ""
	}
	// ASCII letters only: Unicode lowercasing can turn one domain's
	// spelling into another's (a capital sharp s becomes a small one, which
	// converts to a different name), and this value is compared and shown.
	host := strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, u.Host)
	for _, r := range host {
		if !unicode.IsGraphic(r) || unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) {
			return ""
		}
	}
	origin := scheme + "://" + host
	if s := origin + escapedPath(u); len(s) <= maxRedirectTargetLen {
		return s
	}
	if len(origin) <= maxRedirectTargetLen {
		return origin
	}
	return ""
}

// escapedPath is u's path as it is written in a URL, with the leading slash a
// URL with a host requires.
func escapedPath(u *url.URL) string {
	p := u.EscapedPath()
	if p != "" && !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
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
	return u.Scheme + "://" + u.Host + escapedPath(u), true
}
