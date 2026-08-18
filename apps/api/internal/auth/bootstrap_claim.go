package auth

import (
	"crypto/subtle"
	"strings"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// BootstrapClaimHeader is the one way an installer presents the provisioning
// claim on POST /auth/register. A header rather than a body field: the claim is
// a credential, not user data, and this keeps it out of the request payload
// that validation errors and request logs echo, and out of the OpenAPI request
// schema so no generated client offers to fill it in.
const BootstrapClaimHeader = "X-Wpmgr-Bootstrap-Claim"

// BootstrapClaimEnvVar is the operator-facing name of the provisioning claim.
// It appears ONLY in operator guidance written to the server log — never in a
// response, so that the guidance cannot double as a probe.
const BootstrapClaimEnvVar = "WPMGR_BOOTSTRAP_CLAIM_SECRET"

// errRegistrationClosed is the single refusal this path ever returns.
//
// EVERY REFUSAL IS THIS ONE. Whether the install is already owned, has no claim
// configured, or was handed the wrong claim, the caller receives the same
// status, the same code and the same message. Three distinguishable answers
// would let an unauthenticated caller sort installs into "already owned" and
// "waiting to be owned", which is the one bit of state that decides whether the
// endpoint is worth attacking at all. The operator gets the detail, in the log,
// where the operator is the only reader.
func errRegistrationClosed() error {
	return domain.Forbidden("registration_closed", "open registration is closed; ask a tenant owner or admin for an invitation")
}

// SetBootstrapClaimSecret installs the provisioning claim that first-run
// ownership requires. Wired from cfg.Auth.BootstrapClaimSecret at startup.
//
// Whitespace is trimmed because the value routinely arrives through a shell, a
// .env file or a compose file, all of which are good at attaching a trailing
// newline that nobody can see. An all-whitespace value trims to empty and is
// therefore treated exactly like an unset one: no claim is configured, and
// nothing can be claimed.
func (s *Service) SetBootstrapClaimSecret(secret string) {
	s.bootstrapClaim = strings.TrimSpace(secret)
}

// bootstrapClaimAccepted reports whether presented is the configured
// provisioning claim.
//
// It fails closed on an unconfigured claim. An empty configured value returns
// false for every input INCLUDING an empty one, so "no secret set" can never
// be satisfied by "no secret sent" — that equality is precisely how a
// fail-closed check turns itself into an open door.
//
// The length is compared first so the constant-time compare only ever runs on
// equal-length inputs (ConstantTimeCompare returns 0 immediately on a length
// mismatch, which leaks length either way; making it explicit keeps the
// intent legible). Neither value is logged, echoed or recorded.
func (s *Service) bootstrapClaimAccepted(presented string) bool {
	want := s.bootstrapClaim
	if want == "" {
		return false
	}
	got := strings.TrimSpace(presented)
	if len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// BootstrapClaimConfigured reports whether this install has a provisioning
// claim at all. It is the operator-facing signal used to log actionable startup
// and refusal guidance; it is never surfaced on an HTTP response.
func (s *Service) BootstrapClaimConfigured() bool { return s.bootstrapClaim != "" }
