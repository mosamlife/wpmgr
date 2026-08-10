package auth

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

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

// SocialRedirectURL derives the callback the provider will send the browser
// back to. Derived, never configured: an operator-supplied value could point at
// a host they do not control, and the provider would happily deliver an
// authorization code there.
//
// Exported because an operator has to register this exact string at the
// provider, so .env.example documents it, and a documented URL that differs
// from the produced one by a character is a support ticket with no symptom to
// search for. The documentation is checked against THIS function
// (TestEnvExampleDocumentsTheDerivedSocialCallback), not against a second copy
// of the rule.
func SocialRedirectURL(baseURL, provider string) string {
	return strings.TrimRight(strings.TrimSpace(baseURL), "/") + "/auth/social/" + provider + "/callback"
}

func (h *Handler) socialRedirectURL(provider string) string {
	return SocialRedirectURL(h.svc.baseURL, provider)
}

// socialStart begins a handshake.
//
// LIKE THE CALLBACK, EVERY FAILURE REDIRECTS TO THE SIGN-IN PAGE WITH A CODE.
// The caller is a browser doing a full-page navigation (the buttons set
// window.location), so a JSON error body here leaves a person staring at raw
// text on a blank page with no way back, which is exactly what an unreachable
// provider used to produce.
//
// It writes NO server-side state. See social_handshake.go for why that is the
// property that matters on an endpoint anybody can call.
func (h *Handler) socialStart(c *gin.Context) {
	provider := c.Param("provider")
	returnTo := safeReturnPath(c.Query("redirect"))
	adapter := h.social.Get(provider)
	if adapter == nil {
		h.socialFail(c, "social_provider_disabled", returnTo)
		return
	}
	url, state, nonce, verifier, err := adapter.AuthCodeURL(c.Request.Context(), h.socialRedirectURL(provider))
	if err != nil {
		// For Google this is the OIDC discovery call failing, which is now the
		// only symptom of an unreachable issuer: discovery moved off the boot
		// path, so nothing else anywhere reports it.
		h.logSocialFailure(c, "social authorization URL failed", provider, err)
		h.socialFail(c, "social_url_failed", returnTo)
		return
	}
	// Where the person was actually going. A shared link to a site, a report or
	// an invitation arrives at /login?redirect=..., and the password path has
	// always honoured it; the social path dropped it and landed everyone on
	// /sites, so following a link and signing in with Google lost the link.
	//
	// Validated above, at the only point where the value is still ours, and
	// carried in the sealed handshake rather than travelling to the provider and
	// back: a value returned by the provider is a value an attacker can choose.
	if err := h.putHandshake(c, handshake{
		Provider: provider, State: state, Nonce: nonce, Verifier: verifier, Return: returnTo,
	}); err != nil {
		// Only reachable on an instance with no handshake key, which the boot
		// wiring and the session-secret validation between them do not allow.
		// It fails closed rather than starting a handshake nobody sealed.
		h.logSocialFailure(c, "social handshake could not be sealed", provider, err)
		h.socialFail(c, "social_start_failed", returnTo)
		return
	}
	// A new handshake supersedes a link approved by an abandoned one. This used
	// to ride along with the handshake's own session write; it is now the one
	// thing on this path that touches the session at all, and it writes nothing
	// unless this browser really is carrying a parked link.
	h.sessions.clearPendingSocialLink(c.Request.Context())
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
		// Take rather than merely clear. The flow cannot be completed on an
		// instance where the provider is off, but the handshake is the only
		// carrier of where this person was trying to go, and dropping it landed
		// them on a bare sign-in page having lost the deep link they started
		// from. Every other failure branch below hands the return path to
		// socialFail; there is no reason this one is the exception. takeHandshake
		// clears the cookie either way, so the handshake stays single-use, and
		// socialFail re-validates the path through safeReturnPath, so a sealed
		// value is not trusted merely because it was sealed.
		hs, _ := h.takeHandshake(c)
		h.socialFail(c, "social_provider_disabled", hs.Return)
		return
	}

	// The handshake this browser started, read back out of its own sealed
	// cookie and cleared in the same call, so it is single-use however this ends.
	hs, ok := h.takeHandshake(c)
	returnTo := hs.Return
	// The handshake is bound to the provider it was started for. Checking the
	// provider stops a code obtained for one provider being presented to
	// another's callback. The state comparison is constant-time because state is
	// the value that binds this callback to the browser that started it, and a
	// comparison that returns early is a comparison that can be measured.
	if !ok || hs.State == "" || !constantTimeEqual(c.Query("state"), hs.State) || hs.Provider != provider {
		// Usually a stale tab or a handshake that expired mid-flow, occasionally
		// somebody replaying a callback. Both are worth being able to count.
		h.logSocialFailure(c, "social callback state mismatch", provider, nil,
			slog.Bool("had_state", ok && hs.State != ""),
			slog.String("started_for_provider", hs.Provider))
		h.socialFail(c, "social_state_mismatch", returnTo)
		return
	}
	nonce, verifier := hs.Nonce, hs.Verifier
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
		// The redirect stays deliberately coarse: a query parameter naming the
		// step that failed tells an attacker which one to work on. This is
		// where the detail goes instead, and until this line existed it went
		// nowhere at all.
		h.logSocialFailure(c, "social code exchange failed", provider, err)
		h.socialFail(c, "social_exchange_failed", returnTo)
		return
	}

	res, err := h.svc.SignInWithSocial(c.Request.Context(), identity, h.newTenant)
	if err != nil {
		// Refusals included, and especially. A refusal is the policy working,
		// but the operator fielding "it will not let me in" needs to see which
		// rule said no and for which identity.
		h.logSocialFailure(c, "social sign-in failed", provider, err,
			slog.String("subject", identity.Subject),
			slog.String("email", identity.Email),
			slog.Bool("provider_email_verified", identity.EmailVerified),
			slog.Bool("provider_email_unreachable", identity.EmailUnreachable))
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
// issueProviderSessionOrChallenge writes the challenge redirect itself when
// needed, and the shared helper underneath it owns the challenge URL, so a
// second factor still lands on /sites after the challenge rather than on the
// deep link.
//
// It has to be the PROVIDER variant, not the bare issueSessionOrChallenge: that
// wrapper is the only place an approved-but-unwritten identity link is parked
// across the second factor and then written. Calling the plain helper here
// would keep the redirect behaviour and silently stop linking altogether.
func (h *Handler) socialComplete(c *gin.Context, res LoginResult, returnTo string) {
	if !h.issueProviderSessionOrChallenge(c, res) {
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

// constantTimeEqual compares two strings without returning early on the first
// differing byte. Used for the handshake state, which is the value that binds a
// callback to the browser that started it.
//
// The length difference is still observable, as it is for every comparison of
// this shape; the states this server mints are all the same length, so a length
// oracle says nothing an attacker did not already know.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
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
	"social_email_unreachable":          true,
	"account_disabled":                  true,
	"email_not_verified":                true,
}

// Whether the sign-in page is meant to answer a code with its own sentence or
// with the generic one. Named so the table below reads as the decision it is.
const (
	namedSentence  = false
	coarseSentence = true
)

// socialErrorCodes IS THE CONTRACT for ?social_error=: every value this server
// can put there, and nothing else.
//
// IT EXISTS BECAUSE THE OTHER END WAS PINNING A CONTRACT NOBODY IMPLEMENTED.
// apps/web/src/features/auth/social-errors.test.ts named this file as its source
// of truth and listed three codes (social_rate_limited, social_start_failed,
// social_provider_already_linked) that no server path emitted. The test passed
// by describing an agreement with itself, and one of those inventions
// (social_rate_limited) read as evidence that the start endpoint was rate
// limited when it was not. A list of strings in a test file cannot detect that;
// this table plus the two tests that check it can:
//
//   - TestSocialErrorCodesAreExactlyWhatTheHandlerEmits reads this file's
//     socialFail call sites and actionableSocialCodes and refuses any drift
//     from this table, in either direction.
//   - social-errors.test.ts reads THIS table and refuses any web copy for a
//     code that is not in it, or any missing copy for a code that is.
//
// coarseSentence marks the failures that are deliberately vague. Naming the
// verification step that failed tells whoever caused it which one to work on,
// and the code travels in browser history and proxy logs, so those three get the
// generic sentence on purpose and the web tests assert that they keep it.
var socialErrorCodes = map[string]bool{
	// Start.
	"social_provider_disabled": namedSentence,
	"social_url_failed":        namedSentence,
	"social_start_failed":      namedSentence,
	// Callback, before any identity exists.
	"social_state_mismatch":  namedSentence,
	"social_cancelled":       namedSentence,
	"social_no_code":         coarseSentence,
	"social_exchange_failed": coarseSentence,
	// Sign-in policy.
	"social_sign_in_failed":             coarseSentence,
	"social_link_requires_verification": namedSentence,
	"social_email_unverified":           namedSentence,
	"social_email_unreachable":          namedSentence,
	"account_disabled":                  namedSentence,
	"email_not_verified":                namedSentence,
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
