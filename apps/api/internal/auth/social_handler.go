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
	c.JSON(http.StatusOK, gin.H{"providers": h.social.Enabled()})
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
	url, state, nonce, verifier, err := adapter.AuthCodeURL(h.socialRedirectURL(provider))
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
		if de, ok := domain.AsDomain(err); ok && de.Code != "" {
			// The refusals from decideSocial are the ones a person needs to act
			// on, so their codes are passed through for the SPA to explain.
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

// socialFail sends the browser back to the sign-in page carrying a code the SPA
// can turn into a sentence.
func (h *Handler) socialFail(c *gin.Context, code string) {
	q := url.Values{}
	q.Set("social_error", code)
	c.Redirect(http.StatusFound, strings.TrimRight(h.svc.baseURL, "/")+"/login?"+q.Encode())
}
