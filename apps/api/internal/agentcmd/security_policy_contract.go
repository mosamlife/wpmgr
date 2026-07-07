package agentcmd

import (
	"bytes"
	"encoding/json"
)

// This file is the AUTHORITATIVE CP->agent command contract for the Security
// Suite Phase 3 (ADR-059): per-site user 2FA + password policy + hide-backend.
// The wp-agent-engineer mirrors these shapes in the agent's command handler.
// Field names are JSON wire names; do not rename without updating both sides.
//
// Transport — sync_security_policy:
//   POST {site_url}/wp-json/wpmgr/v1/command/sync_security_policy
//   Header: Authorization: Bearer <minted EdDSA JWT>
//           cmd="sync_security_policy", aud=<siteId>
//   Body:   application/json — SecurityPolicyRequest below.
//   Response: 200 with SecurityPolicyResult; ok=false = semantic failure (HTTP
//             transport error OR ok=false at 200 is treated as an error by the
//             CP, same as sync_security_hardening).
//
// The agent applies the FULL policy snapshot atomically on every push.
// It does not diff against a previous state — the CP sends the canonical current
// snapshot and the agent replaces its local copy in the wpmgr_security_policy
// wp-option. Every apply path is try/catch-guarded (a malformed push never
// breaks WP login).

// SecurityPolicy is the per-site policy toggle block the CP sends to the agent.
// Each field maps to one enforced behaviour in the agent's login/password flow.
//
// All toggles default false/0 (off) so enabling is opt-in. The agent treats a
// missing field as the zero value to remain backward-compatible when new fields
// are added without a simultaneous agent update.
//
// 2FA knobs:
//
//	two_factor_enabled              — master switch; when false all 2FA enforcement is inert.
//	two_factor_methods              — allowed provider slugs: "totp", "email", "backup".
//	two_factor_required_roles       — WP role slugs that must use 2FA; empty = optional for all.
//	two_factor_grace_logins         — logins before required-but-unenrolled users hit onboarding (0=force immediately).
//	two_factor_remember_device_days — trusted-device TTL in days; 0 = feature disabled.
//	block_xmlrpc_for_2fa_users      — reject password-only XML-RPC for any user with 2FA configured.
//
// Password knobs:
//
//	password_min_zxcvbn_score       — minimum zxcvbn score on set/change (0=disabled, 1-4).
//	password_min_zxcvbn_roles       — roles the strength rule applies to; empty = all.
//	password_block_compromised      — reject HIBP-breached passwords on set/change.
//	password_reuse_block_count      — block reusing the last N passwords; 0=off.
//	password_max_age_days           — force change after N days; 0=off.
//	password_expiry_roles           — roles the expiry rule applies to; empty = all.
//
// Hide-backend knobs:
//
//	hide_backend_enabled            — master switch for the secret login slug.
//	hide_backend_slug               — the secret slug (e.g. "my-login"); validated ^[a-z0-9-]{4,64}$.
//	hide_backend_redirect           — where to send logged-out hits on canonical wp-login; "" = 404.
type SecurityPolicy struct {
	// 2FA
	TwoFactorEnabled            bool     `json:"two_factor_enabled"`
	TwoFactorMethods            []string `json:"two_factor_methods"`
	TwoFactorRequiredRoles      []string `json:"two_factor_required_roles"`
	TwoFactorGraceLogins        int      `json:"two_factor_grace_logins"`
	TwoFactorRememberDeviceDays int      `json:"two_factor_remember_device_days"`
	BlockXMLRPCFor2FAUsers      bool     `json:"block_xmlrpc_for_2fa_users"`
	// Password
	PasswordMinZxcvbnScore    int      `json:"password_min_zxcvbn_score"`
	PasswordMinZxcvbnRoles    []string `json:"password_min_zxcvbn_roles"`
	PasswordBlockCompromised  bool     `json:"password_block_compromised"`
	PasswordReuseBlockCount   int      `json:"password_reuse_block_count"`
	PasswordMaxAgeDays        int      `json:"password_max_age_days"`
	PasswordExpiryRoles       []string `json:"password_expiry_roles"`
	// Hide-backend
	HideBackendEnabled  bool   `json:"hide_backend_enabled"`
	HideBackendSlug     string `json:"hide_backend_slug"`
	HideBackendRedirect string `json:"hide_backend_redirect"`
}

// SecurityPolicyGroup is one per-role policy override entry.
// Nullable fields use pointer types so the agent can distinguish "not set" from
// the zero value. The agent applies the strictest matching group when a user
// holds multiple roles.
//
//	role              — WP role slug this override applies to.
//	require_2fa       — when non-nil, overrides whether 2FA is required for this role.
//	allowed_methods   — when non-nil, restricts the allowed provider set for this role.
//	min_zxcvbn_score  — when non-nil, overrides the minimum password-strength score.
//	block_compromised — when non-nil, overrides the HIBP-breach check for this role.
//	max_age_days      — when non-nil, overrides the password-expiry window (0 = off).
type SecurityPolicyGroup struct {
	Role             string   `json:"role"`
	Require2FA       *bool    `json:"require_2fa,omitempty"`
	AllowedMethods   []string `json:"allowed_methods,omitempty"`
	MinZxcvbnScore   *int     `json:"min_zxcvbn_score,omitempty"`
	BlockCompromised *bool    `json:"block_compromised,omitempty"`
	MaxAgeDays       *int     `json:"max_age_days,omitempty"`
}

// ForcePasswordChangeEntry is one entry in the optional force_password_change
// list. When the CP sends this list, the agent sets a "must change password"
// flag in user-meta for each user_login on next login.
//
//	user_login — the WP user_login of the user to flag.
//	reason     — short reason code recorded in user-meta for audit purposes
//	             (e.g. "admin_reset", "policy_breach").
type ForcePasswordChangeEntry struct {
	UserLogin string `json:"user_login"`
	Reason    string `json:"reason"`
}

// SecurityPolicyRequest is the POST body for the `sync_security_policy` command.
//
//	policy                — the full site-level policy knob snapshot.
//	groups                — the full current per-role group override list. The agent
//	                        REPLACES its local groups on every push; an empty slice
//	                        means "no overrides" (clear any local groups).
//	force_password_change — optional list of WP users to flag for a forced password
//	                        change on next login. The agent applies these flags
//	                        atomically with the policy. Omitting the field (nil) means
//	                        "no forced changes" — the agent must not clear existing flags.
type SecurityPolicyRequest struct {
	Policy             SecurityPolicy             `json:"policy"`
	Groups             []SecurityPolicyGroup      `json:"groups"`
	ForcePasswordChange []ForcePasswordChangeEntry `json:"force_password_change,omitempty"`
}

// SecurityPolicyResult is the agent's response to the `sync_security_policy`
// command.
//
//	ok                — whether the agent successfully applied the policy.
//	detail            — short human-readable note ("applied", or an error description).
//	enrollment_summary — optional; piggybacks the current 2FA enrollment counts per
//	                     WP role so the CP can update the dashboard coverage card
//	                     without waiting for the next diagnostics push. The agent
//	                     SHOULD include this on every successful apply.
type SecurityPolicyResult struct {
	OK               bool                         `json:"ok"`
	Detail           string                       `json:"detail"`
	EnrollmentSummary *EnrollmentSummary           `json:"enrollment_summary,omitempty"`
}

// EnrollmentSummary carries per-role 2FA enrollment counts. No user identity or
// secret leaves the site; only aggregate counts are transmitted.
type EnrollmentSummary struct {
	PerRole perRoleMap `json:"per_role"`
}

// perRoleMap tolerates a JSON array [] (PHP json_encode of an empty associative
// array) or null, decoding either as an empty map — defense-in-depth alongside
// the agent-side object-cast emitter fix (GH #170, same class as #148).
type perRoleMap map[string]RoleEnrollment

func (m *perRoleMap) UnmarshalJSON(data []byte) error {
	t := bytes.TrimSpace(data)
	if len(t) == 0 || string(t) == "null" || string(t) == "[]" {
		*m = perRoleMap{}
		return nil
	}
	var raw map[string]RoleEnrollment
	if err := json.Unmarshal(t, &raw); err != nil {
		return err
	}
	*m = raw
	return nil
}

// RoleEnrollment is the 2FA enrollment count for one WP role.
//
//	enrolled — number of users with at least one 2FA method configured.
//	required — number of users who are required to use 2FA (by policy or group).
//	total    — total number of users with this role.
type RoleEnrollment struct {
	Enrolled int `json:"enrolled"`
	Required int `json:"required"`
	Total    int `json:"total"`
}
