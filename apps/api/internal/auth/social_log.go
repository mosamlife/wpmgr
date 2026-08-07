package auth

import (
	"errors"
	"log/slog"

	"github.com/gin-gonic/gin"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Social sign-in used to log NOTHING. Not one line, anywhere in this package.
//
// That is what made every other failure in this path unfixable rather than
// merely broken. A social sign-in that fails answers the browser with a
// redirect to /login?social_error=<code>, deliberately coarse so the URL does
// not tell an attacker which verification step failed, and there was no second
// place the detail went. So an operator holding a support ticket that says
// "the GitHub button does nothing" had, in the entire control plane, zero
// evidence: not the status GitHub returned, not whether the token was revoked
// or the rate budget spent, not whether the policy refused the sign-in and
// why. The only way to find out was to reproduce it against production.
//
// Everything here is scoped to the social path on purpose. It is the one place
// where a third party we do not control decides whether a request succeeds,
// which is exactly where an operator cannot reason from the code alone.

// socialLog returns the logger for this path, tagged so the lines can be
// selected out of a busy request log.
func (h *Handler) socialLog() *slog.Logger {
	l := h.logger
	if l == nil {
		l = slog.Default()
	}
	return l.With(slog.String("component", "auth.social"))
}

// logSocialFailure writes one line per failed social sign-in.
//
// Warn, not Error, for all of them: a spent rate budget, a revoked token, a
// provider outage and a policy refusal are all either a third party
// misbehaving or a person being told no. None is a control-plane fault, and
// keeping Error for things the operator must actually fix is what stops these
// lines from being tuned out.
func (h *Handler) logSocialFailure(c *gin.Context, msg, provider string, err error, extra ...any) {
	attrs := make([]any, 0, 8+len(extra))
	attrs = append(attrs, slog.String("provider", provider), slog.String("client_ip", c.ClientIP()))
	attrs = append(attrs, extra...)
	attrs = append(attrs, socialErrorAttrs(err)...)
	h.socialLog().Warn(msg, attrs...)
}

// socialErrorAttrs unpacks whatever an error is actually carrying, so the four
// GitHub failures that used to arrive as one indistinguishable line arrive as
// four, and so the refusal code that no longer travels in the redirect URL is
// still recorded somewhere.
func socialErrorAttrs(err error) []any {
	if err == nil {
		return nil
	}
	attrs := []any{slog.String("error", err.Error())}

	var ge *githubAPIError
	if errors.As(err, &ge) {
		attrs = append(attrs,
			slog.String("provider_failure", string(ge.Failure)),
			slog.Int("provider_status", ge.Status),
			slog.String("provider_url", ge.URL),
		)
		if ge.RetryAfter > 0 {
			// The whole point of keeping it: "wait 43 seconds" and "wait 58
			// minutes" are different operational situations.
			attrs = append(attrs, slog.Duration("retry_after", ge.RetryAfter))
		}
		if ge.Message != "" {
			attrs = append(attrs, slog.String("provider_message", ge.Message))
		}
	}

	if de, ok := domain.AsDomain(err); ok {
		attrs = append(attrs, slog.String("code", de.Code))
	}
	return attrs
}
