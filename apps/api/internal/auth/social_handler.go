package auth

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// socialProviders lists what this install offers. The sign-in page calls it so
// it can render exactly the buttons that will work: a button for an
// unconfigured provider leads to a provider error page, which reads as a broken
// product rather than an unconfigured one.
func (h *Handler) socialProviders(c *gin.Context) {
	// `sso` covers the generic operator-configured OIDC issuer, which is a
	// separate mechanism from the consumer providers but the same question for
	// the sign-in page: which buttons will actually work.
	//
	// It is reported here because the SSO button used to render unconditionally,
	// so on an install with no issuer configured (which is every install by
	// default, including production) it was a permanent dead end.
	c.JSON(http.StatusOK, gin.H{
		"providers": h.social.Enabled(),
		"sso":       h.oidc.Enabled(),
	})
}

// socialRedirectURL derives the callback the provider will send the browser
// back to. Derived, never configured: an operator-supplied value could point at
// a host they do not control, and the provider would happily deliver an
// authorization code there.
func (h *Handler) socialRedirectURL(provider string) string {
	return strings.TrimRight(h.svc.baseURL, "/") + "/auth/social/" + provider + "/callback"
}

// socialStartPerMinute caps how often one apparent client may BEGIN a
// handshake. Each start writes a session record and, for Google, can trigger an
// outbound discovery call, all from an unauthenticated GET. A person clicking a
// sign-in button does this once and occasionally retries; nothing legitimate
// needs ten in a minute, including a shared office address behind one NAT.
const socialStartPerMinute = 10

// socialStartInstancePerMinute is the ceiling that actually bounds a flood.
//
// THE PER-CLIENT KEY IS NOT A DEFENCE, because the client chooses it. It comes
// from gin's ClientIP, which reads X-Forwarded-For, and gin only ignores that
// header for hops it is told are untrusted. Nothing in this tree calls
// SetTrustedProxies, so gin trusts every proxy and takes the leftmost entry,
// which any caller can write. Rotating that header gives an attacker a fresh
// per-client budget on every request, so socialStartPerMinute is a fairness
// control between honest clients and nothing more. (Fixing the trust properly
// is a tree-wide change: the same header feeds the password-reset and 2FA
// limiters and the IPs written to the audit log, and a wrong hop count behind
// the load balancer collapses every visitor into one bucket, which turns the
// 2FA lockout into an outage. It belongs in one deliberate change, not here.)
//
// This budget is keyed on nothing the caller supplies, so it holds however the
// header is forged: an instance will begin at most this many handshakes a
// minute, which bounds both the session records written and the outbound
// discovery calls made. It is set far above human traffic (five a second, on an
// endpoint a person hits when they click a button) so it is a ceiling on abuse
// rather than a queue honest visitors ever stand in. It is per instance, like
// every other budget here, because the limiter is in-process; a fleet of N
// instances therefore admits N times this.
const socialStartInstancePerMinute = 300

// socialStart begins the handshake.
//
// LIKE THE CALLBACK, EVERY FAILURE REDIRECTS. This endpoint exists to be
// navigated to: the browser is following a button, so the address bar is where
// the response lands. It used to answer an unconfigured or stale provider with
// a JSON error body, which put `{"error":{"code":"social_provider_disabled"}}`
// on screen with no way back, at the exact moment the sign-in page one click
// away could have explained it.
func (h *Handler) socialStart(c *gin.Context) {
	provider := c.Param("provider")
	adapter := h.social.Get(provider)
	if adapter == nil {
		h.socialFail(c, "social_provider_disabled")
		return
	}
	// Ahead of both the session write and the provider call, so a flood costs
	// this instance a map lookup rather than a store record and a round trip.
	if !h.allowSocialStart(c) {
		h.socialFail(c, "social_rate_limited")
		return
	}
	url, state, nonce, verifier, err := adapter.AuthCodeURL(c.Request.Context(), h.socialRedirectURL(provider))
	if err != nil {
		// Typically provider discovery being unreachable. Coarse on purpose: the
		// detail is a server-side fact and belongs in logs.
		h.socialFail(c, "social_start_failed")
		return
	}
	h.sessions.putSocial(c.Request.Context(), provider, state, nonce, verifier)
	c.Redirect(http.StatusFound, url)
}

// allowSocialStart applies both budgets. Nil limiter (not wired) allows the
// request, matching the password-reset path: a rate limiter that is not
// configured must never become an outage.
func (h *Handler) allowSocialStart(c *gin.Context) bool {
	if h.svc.limiter == nil {
		return true
	}
	// The instance ceiling is checked FIRST, so a flood of forged client
	// addresses is refused before it can allocate a per-client bucket each. The
	// limiter keeps one bucket per distinct key for ten idle minutes, so
	// checking the spoofable key first would let the flood grow the limiter
	// itself, which is the resource this is supposed to protect.
	if ok, _ := h.svc.limiter.Allow(c.Request.Context(), "social-start:instance", socialStartInstancePerMinute); !ok {
		return false
	}
	ip := clientAddr(c)
	if !ip.IsValid() {
		// No usable address to key on. The instance ceiling above still applies.
		return true
	}
	ok, _ := h.svc.limiter.Allow(c.Request.Context(), "social-start:"+ip.String(), socialStartPerMinute)
	return ok
}

// socialCallback completes the handshake.
//
// EVERY FAILURE REDIRECTS TO THE SIGN-IN PAGE WITH A CODE, rather than
// rendering JSON. The visitor here is a browser mid-redirect from a provider,
// and answering with a JSON error body leaves a person staring at raw text with
// no way back. The SPA turns the code into a sentence and, where the answer is
// "prove you own the existing account", into the button that starts that.
func (h *Handler) socialCallback(c *gin.Context) {
	provider := c.Param("provider")
	adapter := h.social.Get(provider)
	if adapter == nil {
		h.socialFail(c, "social_provider_disabled")
		return
	}

	// The handshake is single-use, bound to the provider it was started for, and
	// consumed ONLY by the callback it belongs to. Binding the provider stops a
	// code obtained for one provider being presented to another's callback;
	// consuming conditionally stops any stray callback from popping a legitimate
	// sign-in that is still in flight. See takeSocialFor.
	nonce, verifier, ok := h.sessions.takeSocialFor(c.Request.Context(), provider, c.Query("state"))
	if !ok {
		h.socialFail(c, "social_state_mismatch")
		return
	}
	// The provider reports a user refusal as an error parameter, which is not a
	// failure worth alarming anyone about.
	if e := c.Query("error"); e != "" {
		h.socialFail(c, "social_cancelled")
		return
	}
	code := c.Query("code")
	if code == "" {
		h.socialFail(c, "social_no_code")
		return
	}

	identity, err := adapter.Exchange(c.Request.Context(), h.socialRedirectURL(provider), code, verifier, nonce)
	if err != nil {
		// Deliberately coarse. The detail belongs in logs, not in a query
		// parameter that tells an attacker which verification step failed.
		h.socialFail(c, "social_exchange_failed")
		return
	}

	res, err := h.svc.SignInWithSocial(c.Request.Context(), identity, h.newTenant)
	if err != nil {
		if de, ok := domain.AsDomain(err); ok && actionableSocialCodes[de.Code] {
			// ONLY the refusals a person can act on. An allowlist rather than
			// passing de.Code through, because the same branch also carries
			// internal codes (identity_create_failed, tenant_slug_exists) that
			// the UI renders as a generic string anyway and that would otherwise
			// travel in browser history and proxy logs.
			h.socialFail(c, de.Code)
			return
		}
		h.socialFail(c, "social_sign_in_failed")
		return
	}

	// Same 2FA invariant as the OIDC callback: an enrolled second factor must
	// not be skipped just because the first factor came from a provider.
	// issueSessionOrChallenge writes the challenge redirect itself when needed.
	if !h.issueSessionOrChallenge(c, res, h.svc.baseURL) {
		return
	}
	c.Redirect(http.StatusFound, strings.TrimRight(h.svc.baseURL, "/")+"/sites")
}

// actionableSocialCodes are the refusals the sign-in page can turn into a
// sentence with a next step. Anything not listed becomes a generic failure.
//
// WHY account_disabled AND email_not_verified STAY, having been read as an
// account-existence oracle. They are reachable on two paths:
//
//   - the identity path, where (provider, subject) is already bound to this
//     local account. The caller authenticated at the provider AS that identity,
//     so they are being told the state of their own account. Nothing is
//     disclosed to anyone but its owner.
//
//   - the linking path, which requires the provider to have vouched for the
//     address. That is NOT the same as the caller holding it, and this package
//     says so itself: see SignInWithSocial on a Workspace administrator being
//     able to obtain a validly signed token carrying an arbitrary address in
//     their own domain. So there IS a residual: someone who administers a
//     domain can learn whether an address in that domain has an account on this
//     install, and whether it is disabled or unverified.
//
// That residual is not created here and is not closed by hiding these two.
// The same linking path already answers social_link_requires_verification,
// which discloses strictly more about the same address to the same caller (an
// account exists, and it is unverified) and mails that address as well.
// Dropping account_disabled and email_not_verified would leave the channel
// open and cost a disabled user the one sentence explaining why they cannot get
// in, leaving "try again" as the advice for a state that trying again cannot
// change.
//
// Closing it for real means deciding what the LINKING path may say at all, for
// every refusal it can produce, which is a policy change in decideSocial rather
// than a display change in this list.
var actionableSocialCodes = map[string]bool{
	"social_link_requires_verification": true,
	"social_email_unverified":           true,
	"social_provider_already_linked":    true,
	"account_disabled":                  true,
	"email_not_verified":                true,
}

// socialFail sends the browser back to the sign-in page carrying a code the SPA
// can turn into a sentence.
func (h *Handler) socialFail(c *gin.Context, code string) {
	q := url.Values{}
	q.Set("social_error", code)
	c.Redirect(http.StatusFound, strings.TrimRight(h.svc.baseURL, "/")+"/login?"+q.Encode())
}
