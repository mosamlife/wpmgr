package auth

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
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

func (h *Handler) socialStart(c *gin.Context) {
	provider := c.Param("provider")
	adapter := h.social.Get(provider)
	if adapter == nil {
		httpx.Error(c, domain.Unavailable("social_provider_disabled", "this sign-in method is not configured"))
		return
	}
	url, state, nonce, verifier, err := adapter.AuthCodeURL(c.Request.Context(), h.socialRedirectURL(provider))
	if err != nil {
		httpx.Error(c, domain.Internal("social_url_failed", "failed to build authorization URL").WithCause(err))
		return
	}
	// Where the person was actually going. A shared link to a site, a report or
	// an invitation arrives at /login?redirect=..., and the password path has
	// always honoured it; the social path dropped it and landed everyone on
	// /sites, so following a link and signing in with Google lost the link.
	//
	// Validated here, at the only point where the value is still ours, and kept
	// in the session rather than travelling to the provider and back.
	h.sessions.putSocial(c.Request.Context(), provider, state, nonce, verifier, safeReturnPath(c.Query("redirect")))
	c.Redirect(http.StatusFound, url)
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
		h.socialFail(c, "social_provider_disabled", "")
		return
	}

	wantProvider, state, nonce, verifier, returnTo := h.sessions.takeSocial(c.Request.Context())
	// The handshake is single-use and bound to the provider it was started for.
	// Checking the provider stops a code obtained for one provider being
	// presented to another's callback.
	if state == "" || c.Query("state") != state || wantProvider != provider {
		h.socialFail(c, "social_state_mismatch", returnTo)
		return
	}
	// The provider reports a user refusal as an error parameter, which is not a
	// failure worth alarming anyone about.
	if e := c.Query("error"); e != "" {
		h.socialFail(c, "social_cancelled", returnTo)
		return
	}
	code := c.Query("code")
	if code == "" {
		h.socialFail(c, "social_no_code", returnTo)
		return
	}

	identity, err := adapter.Exchange(c.Request.Context(), h.socialRedirectURL(provider), code, verifier, nonce)
	if err != nil {
		// Deliberately coarse. The detail belongs in logs, not in a query
		// parameter that tells an attacker which verification step failed.
		h.socialFail(c, "social_exchange_failed", returnTo)
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
			h.socialFail(c, de.Code, returnTo)
			return
		}
		h.socialFail(c, "social_sign_in_failed", returnTo)
		return
	}

	h.socialComplete(c, res, returnTo)
}

// socialComplete is the ending every successful social sign-in shares: the 2FA
// gate, then the landing page.
//
// Split out of socialCallback so the landing page can be driven by a test.
// Everything above it needs a live provider and a database behind
// SignInWithSocial, so the one line that a deep link changes was the one line
// no test could reach; see social_redirect_test.go.
//
// The 2FA invariant is the OIDC callback's: an enrolled second factor must not
// be skipped just because the first factor came from a provider.
// issueSessionOrChallenge writes the challenge redirect itself when needed, and
// that shared helper owns the challenge URL, so a second factor still lands on
// /sites after the challenge rather than on the deep link.
func (h *Handler) socialComplete(c *gin.Context, res LoginResult, returnTo string) {
	if !h.issueSessionOrChallenge(c, res, h.svc.baseURL) {
		return
	}
	c.Redirect(http.StatusFound, strings.TrimRight(h.svc.baseURL, "/")+socialLandingPath(returnTo))
}

// socialLandingPath is where a completed sign-in goes: the path the browser was
// heading for before the handshake, or the default landing page.
//
// Re-validated rather than trusted, even though only safeReturnPath can put a
// value in the session: this is the function that writes a Location header, and
// the cost of checking twice is nothing next to the cost of being wrong once.
func socialLandingPath(returnTo string) string {
	if p := safeReturnPath(returnTo); p != "" {
		return p
	}
	return "/sites"
}

// safeReturnPath accepts only a path on this origin, and returns "" for
// anything else.
//
// An open redirect on a sign-in flow is worth more to an attacker than most
// bugs on the site: the link genuinely belongs to this product, the person has
// just been asked to authenticate, and the landing page is wherever the link
// says. So the rules are deliberately narrow rather than clever.
//
//   - It must start with a single "/". A bare "sites" would resolve against
//     whatever page emits it, and an absolute URL needs no discussion.
//   - "//host" and "/\host" are protocol-relative: browsers read both as
//     another origin, which is exactly what they do not look like.
//   - url.Parse then has to agree there is no scheme and no host, which also
//     rejects control characters and other malformed input.
func safeReturnPath(raw string) string {
	const maxReturnPath = 512
	if raw == "" || len(raw) > maxReturnPath {
		return ""
	}
	if !strings.HasPrefix(raw, "/") {
		return ""
	}
	if strings.HasPrefix(raw, "//") || strings.HasPrefix(raw, `/\`) {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return ""
	}
	// u.String() re-encodes, so whatever reaches the Location header is a
	// well-formed reference rather than the raw query value.
	return u.String()
}

// actionableSocialCodes are the refusals the sign-in page can turn into a
// sentence with a next step. Anything not listed becomes a generic failure.
var actionableSocialCodes = map[string]bool{
	"social_link_requires_verification": true,
	"social_email_unverified":           true,
	"account_disabled":                  true,
	"email_not_verified":                true,
}

// socialFail sends the browser back to the sign-in page carrying a code the SPA
// can turn into a sentence, and the deep link the person was following so that
// retrying with a password still lands where they were going.
func (h *Handler) socialFail(c *gin.Context, code, returnTo string) {
	q := url.Values{}
	q.Set("social_error", code)
	if p := safeReturnPath(returnTo); p != "" {
		q.Set("redirect", p)
	}
	c.Redirect(http.StatusFound, strings.TrimRight(h.svc.baseURL, "/")+"/login?"+q.Encode())
}
