package config

import (
	"net/url"
	"os"
	"strings"

	"github.com/mosamlife/wpmgr/apps/api/internal/riverutil"
)

// validateWebAuthnOrigins checks that every WPMGR_AUTH_WEBAUTHN_RP_ORIGINS entry
// uses HTTPS and is not a loopback/localhost origin. Called by Validate in
// production only. Self-hosted operators who deploy HTTP or use localhost must
// set WPMGR_ENV != "production".
//
// N3: mirrors the project's existing insecure-TLS loud-warn/fail pattern.
func validateWebAuthnOrigins(origins string) *Issue {
	if origins == "" {
		return nil
	}
	for _, o := range strings.Split(origins, ",") {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		lower := strings.ToLower(o)
		if strings.HasPrefix(lower, "http://") {
			return &Issue{
				Name:   "WPMGR_AUTH_WEBAUTHN_RP_ORIGINS",
				Reason: "contains an http:// origin (" + o + ") — WebAuthn requires HTTPS in production; use https://",
			}
		}
		// Detect loopback / localhost in the origin host.
		host := lower
		if idx := strings.Index(host, "://"); idx >= 0 {
			host = host[idx+3:]
		}
		// Strip port.
		if idx := strings.LastIndex(host, ":"); idx >= 0 {
			host = host[:idx]
		}
		// Strip path.
		if idx := strings.IndexByte(host, '/'); idx >= 0 {
			host = host[:idx]
		}
		if host == "localhost" || host == "127.0.0.1" || host == "::1" || strings.HasSuffix(host, ".localhost") {
			return &Issue{
				Name:   "WPMGR_AUTH_WEBAUTHN_RP_ORIGINS",
				Reason: "contains a loopback/localhost origin (" + o + ") — not permitted in production; use your public domain",
			}
		}
	}
	return nil
}

// Issue describes a single configuration problem detected by Validate. Name is
// the environment-variable name (safe to log and surface to operators); Reason
// is a short, human-readable explanation that NEVER contains a secret value.
type Issue struct {
	Name   string
	Reason string
}

// Validate aggregates ALL boot-critical configuration problems and returns them
// as a slice of Issues. An empty slice means the configuration is valid.
//
// Checks performed (in order):
//  1. WPMGR_SESSION_SECRET — empty, placeholder, or too short.
//  2. WPMGR_AGENT_SIGNING_PRIVATE_KEY — production-only: known committed dev key.
//  3. WPMGR_SITE_DEST_AGE_SECRET — production-only: must be present.
//  4. WPMGR_AUTH_WEBAUTHN_RP_ORIGINS — production-only: https, no loopback.
//  5. WPMGR_RIVER_MEDIA_SCHEMA — must be a simple Postgres identifier.
//  6. WPMGR_BILLING_* — hosted-only: no partially configured payment provider.
//
// EVERY ISSUE RETURNED HERE STOPS THE CONTROL PLANE FROM SERVING (main parks in
// serveDegraded, where /readyz 503s and nothing else answers at all). That is
// the bar for adding a check to this function: the configuration must be one
// under which the product cannot be trusted to run, not merely one under which
// some optional feature will not work. A problem confined to a single feature
// belongs in Advisories, which degrades that feature and leaves the rest of the
// control plane serving.
//
// The production guard mirrors the exact condition used during full boot: checks
// 2 and 3 are skipped in non-production environments so the function is safe to
// call in development without any secrets configured.
//
// SECRET-LEAK INVARIANT: every Reason string contains only the env-var name and
// a short human description. Raw errors from crypto parsing, DSN construction,
// or any other credential-wrapping path are NEVER included.
func Validate(cfg Config) []Issue {
	var issues []Issue

	// 1. Session secret.
	s := cfg.Auth.SessionSecret
	switch {
	case s == "":
		issues = append(issues, Issue{
			Name:   "WPMGR_SESSION_SECRET",
			Reason: "empty — set a random secret of at least 32 bytes",
		})
	case strings.HasPrefix(s, "change-me"):
		issues = append(issues, Issue{
			Name:   "WPMGR_SESSION_SECRET",
			Reason: "still holds the placeholder value — set a real random secret of at least 32 bytes",
		})
	case len(s) < 32:
		issues = append(issues, Issue{
			Name:   "WPMGR_SESSION_SECRET",
			Reason: "too short — use at least 32 bytes",
		})
	}

	// 2. Agent signing private key (production-only: known committed dev key).
	if cfg.IsProduction() {
		k := cfg.Agent.SigningPrivateKey
		if k != "" {
			for _, dev := range devAgentSigningPrivateKeys {
				if k == dev {
					issues = append(issues, Issue{
						Name:   "WPMGR_AGENT_SIGNING_PRIVATE_KEY",
						Reason: "holds a known committed dev key — generate a fresh control-plane Ed25519 keypair for production",
					})
					break
				}
			}
		}
	}

	// 3. Site-destination age secret (production-only: must be present).
	if cfg.IsProduction() {
		if strings.TrimSpace(os.Getenv("WPMGR_SITE_DEST_AGE_SECRET")) == "" {
			issues = append(issues, Issue{
				Name:   "WPMGR_SITE_DEST_AGE_SECRET",
				Reason: "required in production — an empty value uses an ephemeral key that orphans stored secrets on restart",
			})
		}
	}

	// 4. WebAuthn RP origins (production-only: must not be http:// or loopback).
	// N3: mirrors the insecure-TLS loud-fail pattern for other production guards.
	if cfg.IsProduction() {
		if issue := validateWebAuthnOrigins(cfg.Auth.WebAuthnRPOrigins); issue != nil {
			issues = append(issues, *issue)
		}
	}

	// 5. River media schema must be a simple Postgres identifier when set.
	// Reported here so an invalid value parks in readyz-degraded alongside the
	// other config problems, rather than crash-looping later at River bootstrap.
	if _, err := riverutil.NormalizeSchema(cfg.River.MediaSchema); err != nil {
		issues = append(issues, Issue{
			Name:   "WPMGR_RIVER_MEDIA_SCHEMA",
			Reason: "must be a simple Postgres identifier: letters, digits, and underscores, not starting with a digit",
		})
	}

	// 6. M16 Phase B Stripe config — required ONLY when hosted billing is on
	// AND the operator has started configuring Stripe (signaled by ANY one of
	// the five fields being set). Hosted-with-zero-billing-env (the Phase A
	// behavior — every entitlement check no-ops) and hosted-with-a-fully-set
	// Stripe config are both legal; a PARTIAL Stripe config is refused so an
	// operator never boots into a half-wired provider that registers, then
	// fails confusingly on first checkout/webhook.
	if cfg.Hosted.Enabled {
		issues = append(issues, validateStripeConfig(cfg.Billing.Stripe)...)
		issues = append(issues, validateRazorpayConfig(cfg.Billing.Razorpay)...)
	}

	return issues
}

// Advisories reports configuration problems that must NOT stop the control
// plane from serving.
//
// The distinction from Validate is the whole point of this function. An
// operator who half-enters a Google client id has made a mistake about ONE
// sign-in button. Parking the process in degraded boot over it would take down
// backups, updates, uptime monitoring and every dashboard for every tenant, and
// it would do so on the upgrade path, where the operator did not change
// anything and has no reason to suspect the variable they set months ago. That
// is the same trade that was already rejected for Google's discovery call:
// nothing about one sign-in method is worth the control plane.
//
// So the FEATURE degrades, never the process. Callers log these, surface them
// in wpmgr-cli validate-env, and switch off the affected feature (see
// Config.SocialSignInUsable) so no button renders that would fail at the
// provider.
func Advisories(cfg Config) []Issue {
	issues := validateSocialConfig(cfg)
	if cfg.IsProduction() {
		if issue := insecurePublicBaseURLIssue(cfg.PublicBaseURL); issue != nil {
			issues = append(issues, *issue)
		}
	}
	return issues
}

// SocialSignInUsable reports whether the derived OAuth redirect_uri would be an
// absolute URL, which is the condition under which social sign-in can work at
// all.
//
// Callers use it to switch the feature OFF rather than to refuse to boot: a
// provider whose redirect_uri is the relative path /auth/social/google/callback
// renders a button that dead-ends at a provider error page, so not rendering it
// is the honest degradation.
func (c Config) SocialSignInUsable() bool {
	return validatePublicBaseURL(c.PublicBaseURL) == nil
}

// validateSocialConfig reports a social configuration that cannot work.
//
// It reports two distinct mistakes.
//
// A HALF-ENTERED CREDENTIAL. Only one of client id and client secret is set.
// The provider stays correctly off (a button that fails at the provider is
// worse than no button), but staying off is not the same as staying quiet: the
// operator typed one of the two variables, so they meant to switch this on, and
// the install owes them the reason it did not happen.
//
// A MISSING OR RELATIVE PUBLIC BASE URL. The OAuth redirect_uri is derived,
// never configured, as <public base URL>/auth/social/<provider>/callback. That
// derivation is a security property, but it makes the sign-in flow depend on a
// variable nothing else refuses to start without, so an unset value yields the
// relative path /auth/social/google/callback. Every provider rejects it, and
// the operator sees only a provider error page with no hint that the fault is
// in their own environment.
func validateSocialConfig(cfg Config) []Issue {
	var issues []Issue

	providers := []struct {
		idName, idValue         string
		secretName, secretValue string
	}{
		{
			idName: "WPMGR_SOCIAL_GOOGLE_CLIENT_ID", idValue: cfg.Social.Google.ClientID,
			secretName: "WPMGR_SOCIAL_GOOGLE_CLIENT_SECRET", secretValue: cfg.Social.Google.ClientSecret,
		},
		{
			idName: "WPMGR_SOCIAL_GITHUB_CLIENT_ID", idValue: cfg.Social.GitHub.ClientID,
			secretName: "WPMGR_SOCIAL_GITHUB_CLIENT_SECRET", secretValue: cfg.Social.GitHub.ClientSecret,
		},
	}
	for _, p := range providers {
		switch {
		case p.idValue == "" && p.secretValue != "":
			issues = append(issues, Issue{
				Name:   p.idName,
				Reason: "missing while its client secret is set: both halves of a social provider credential are required, and this provider stays switched off until it has both",
			})
		case p.secretValue == "" && p.idValue != "":
			issues = append(issues, Issue{
				Name:   p.secretName,
				Reason: "missing while its client id is set: both halves of a social provider credential are required, and this provider stays switched off until it has both",
			})
		}
	}

	if cfg.Social.Configured() {
		if issue := validatePublicBaseURL(cfg.PublicBaseURL); issue != nil {
			issues = append(issues, *issue)
		}
	}

	return issues
}

// publicBaseURLName is the env var the derived social redirect_uri is built
// from.
const publicBaseURLName = "WPMGR_PUBLIC_BASE_URL"

// validatePublicBaseURL requires an absolute origin: scheme plus host.
//
// It judges the string EXACTLY as given, with no trimming of its own. Load
// normalizes the value once (NormalizePublicBaseURL) and every consumer reads
// cfg.PublicBaseURL, so the string judged here is the string that will be
// concatenated into a redirect_uri. A check that quietly trimmed first would be
// approving a value nobody uses.
//
// It deliberately does NOT require https, which is the separate, production-only
// question insecurePublicBaseURLIssue asks: a self-hosted operator testing on
// http://localhost is doing something legitimate, and the failure this check
// exists for is the one the operator cannot see, a value that is not an
// absolute URL at all and silently produces a relative redirect_uri.
func validatePublicBaseURL(raw string) *Issue {
	if raw == "" {
		return &Issue{
			Name:   publicBaseURLName,
			Reason: "required once a social sign-in provider is configured: the OAuth redirect_uri is derived from it, so an empty value produces the relative path /auth/social/<provider>/callback, which every provider rejects. Social sign-in stays switched off until it is set",
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return &Issue{
			Name:   publicBaseURLName,
			Reason: "must be an absolute URL with an http or https scheme and a host, for example https://manage.example.com, because the social sign-in redirect_uri is derived from it. Social sign-in stays switched off until it is",
		}
	}
	return nil
}

// insecurePublicBaseURLIssue reports a plain-http public base URL in production.
//
// Narrow on purpose. Loopback is exempt (an operator testing production config
// locally is not exposing anything), and the whole check is production-only,
// because a self-hosted install on a private network over http is a choice its
// operator has already made. What it is not narrow about is the reason: this
// value is not only the social redirect_uri, it is the origin of every password
// reset and invitation link, and those carry single-use tokens.
func insecurePublicBaseURLIssue(raw string) *Issue {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || !strings.EqualFold(u.Scheme, "http") {
		return nil
	}
	if isLoopbackHost(u.Hostname()) {
		return nil
	}
	return &Issue{
		Name:   publicBaseURLName,
		Reason: "uses plain http in production: reset and invitation links, and the social sign-in redirect_uri, are all built from this origin, so their single-use tokens travel in clear text. Set the https origin your users reach (terminate TLS at your proxy and point this at it)",
	}
}

// isLoopbackHost reports whether a hostname names this machine.
func isLoopbackHost(host string) bool {
	h := strings.ToLower(host)
	return h == "localhost" || h == "127.0.0.1" || h == "::1" || h == "[::1]" || strings.HasSuffix(h, ".localhost")
}

// validateStripeConfig checks internal consistency of the five Stripe
// fields: either all empty (Stripe simply is not this instance's provider —
// legal) or all five present.
func validateStripeConfig(s StripeConfig) []Issue {
	fields := map[string]string{
		"WPMGR_BILLING_STRIPE_SECRET_KEY":     s.SecretKey,
		"WPMGR_BILLING_STRIPE_WEBHOOK_SECRET": s.WebhookSecret,
		"WPMGR_BILLING_STRIPE_PRICE_STARTER":  s.PriceStarter,
		"WPMGR_BILLING_STRIPE_PRICE_AGENCY":   s.PriceAgency,
		"WPMGR_BILLING_STRIPE_PRICE_SCALE":    s.PriceScale,
	}
	anySet := false
	for _, v := range fields {
		if v != "" {
			anySet = true
			break
		}
	}
	if !anySet {
		return nil
	}

	var issues []Issue
	for name, v := range fields {
		if v == "" {
			issues = append(issues, Issue{
				Name:   name,
				Reason: "Stripe billing is partially configured — every WPMGR_BILLING_STRIPE_* variable is required once any one of them is set",
			})
		}
	}
	return issues
}

// validateRazorpayConfig checks internal consistency of the nine Razorpay
// fields (3 credentials + 6 dual-currency plan ids): either all empty
// (Razorpay simply is not configured on this instance — legal) or all nine
// present. A partial config (e.g. INR plans set but USD plans missing) is
// refused at boot rather than registering a Razorpay provider that would
// fail confusingly on the first checkout in the unconfigured currency.
func validateRazorpayConfig(r RazorpayConfig) []Issue {
	fields := map[string]string{
		"WPMGR_BILLING_RAZORPAY_KEY_ID":           r.KeyID,
		"WPMGR_BILLING_RAZORPAY_KEY_SECRET":       r.KeySecret,
		"WPMGR_BILLING_RAZORPAY_WEBHOOK_SECRET":   r.WebhookSecret,
		"WPMGR_BILLING_RAZORPAY_PLAN_STARTER_USD": r.PlanStarterUSD,
		"WPMGR_BILLING_RAZORPAY_PLAN_STARTER_INR": r.PlanStarterINR,
		"WPMGR_BILLING_RAZORPAY_PLAN_AGENCY_USD":  r.PlanAgencyUSD,
		"WPMGR_BILLING_RAZORPAY_PLAN_AGENCY_INR":  r.PlanAgencyINR,
		"WPMGR_BILLING_RAZORPAY_PLAN_SCALE_USD":   r.PlanScaleUSD,
		"WPMGR_BILLING_RAZORPAY_PLAN_SCALE_INR":   r.PlanScaleINR,
	}
	anySet := false
	for _, v := range fields {
		if v != "" {
			anySet = true
			break
		}
	}
	if !anySet {
		return nil
	}

	var issues []Issue
	for name, v := range fields {
		if v == "" {
			issues = append(issues, Issue{
				Name:   name,
				Reason: "Razorpay billing is partially configured — every WPMGR_BILLING_RAZORPAY_* variable is required once any one of them is set",
			})
		}
	}
	return issues
}
