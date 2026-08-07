package auth

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

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
	h.sessions.putSocial(c.Request.Context(), provider, state, nonce, verifier)
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
		h.socialFail(c, "social_provider_disabled")
		return
	}

	wantProvider, state, nonce, verifier := h.sessions.takeSocial(c.Request.Context())
	// The handshake is single-use and bound to the provider it was started for.
	// Checking the provider stops a code obtained for one provider being
	// presented to another's callback.
	if state == "" || c.Query("state") != state || wantProvider != provider {
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
	if !h.issueProviderSessionOrChallenge(c, res) {
		return
	}
	c.Redirect(http.StatusFound, strings.TrimRight(h.svc.baseURL, "/")+"/sites")
}

// issueProviderSessionOrChallenge wraps issueSessionOrChallenge for the two
// provider-redirect callbacks (the consumer providers and the generic OIDC
// issuer), and is the ONLY place a link approved by SignInWithSocial gets
// written.
//
// A provider handshake is one factor. An account with a second factor must not
// have a new way in bound to it on the strength of the first alone, so the
// approved link is parked in the server-side session and applied by the
// factor-completion handlers instead. Both callbacks go through here because
// both reach the same policy, and a callback that forgot to write the link
// would silently never link that provider at all.
//
// Returns false when the caller must write nothing further, exactly like
// issueSessionOrChallenge.
func (h *Handler) issueProviderSessionOrChallenge(c *gin.Context, res LoginResult) bool {
	// Parked from inside the challenge hook, which is the only moment that works
	// for both halves of the binding: the challenge id does not exist any earlier,
	// and the session save rides the response write, so any later is too late.
	//
	// The link is bound to THIS challenge and not merely to this user. A browser
	// can hold more than one live challenge for one account, and a link approved
	// by a provider handshake must not be applied by a challenge that handshake
	// did not produce.
	park := func(challengeID uuid.UUID) {
		if res.PendingSocialLink == nil {
			return
		}
		h.sessions.putPendingSocialLink(c.Request.Context(), res.User.ID, challengeID, *res.PendingSocialLink)
	}
	if !h.issueSessionOrChallengeThen(c, res, h.svc.baseURL, park) {
		return false
	}
	// A session exists, so the login is complete and the link can be written.
	if res.PendingSocialLink != nil {
		if err := h.svc.CompleteSocialLink(c.Request.Context(), res.User.ID, *res.PendingSocialLink); err != nil {
			// The person IS signed in; only the convenience of not repeating the
			// handshake next time is lost. Sending them back to the sign-in page
			// over that would be worse than the failure.
			slog.ErrorContext(c.Request.Context(), "social link write failed after sign-in",
				slog.String("provider", res.PendingSocialLink.Provider),
				slog.String("user_id", res.User.ID.String()), slog.Any("error", err))
		}
	}
	return true
}

// completePendingSocialLink writes a link parked by socialCallback, now that
// the second factor has been proven. Called by every factor-completion handler
// immediately after a session is established, passing the challenge that was
// just completed: only the challenge the handshake produced may apply its link.
//
// Silent on failure by design: the caller has authenticated with both factors
// and their session is already valid, so the only thing a failure costs is one
// repeated provider handshake at the next sign-in.
func (h *Handler) completePendingSocialLink(c *gin.Context, userID, challengeID uuid.UUID) {
	ctx := c.Request.Context()
	link, ok := h.sessions.takePendingSocialLink(ctx, userID, challengeID)
	if !ok {
		return
	}
	if err := h.svc.CompleteSocialLink(ctx, userID, link); err != nil {
		slog.ErrorContext(ctx, "parked social link write failed after second factor",
			slog.String("provider", link.Provider),
			slog.String("user_id", userID.String()), slog.Any("error", err))
	}
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
// can turn into a sentence.
func (h *Handler) socialFail(c *gin.Context, code string) {
	q := url.Values{}
	q.Set("social_error", code)
	c.Redirect(http.StatusFound, strings.TrimRight(h.svc.baseURL, "/")+"/login?"+q.Encode())
}
