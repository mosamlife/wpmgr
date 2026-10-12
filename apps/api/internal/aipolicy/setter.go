package aipolicy

import (
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// AccountActive is the users.status value of an account that may act.
const AccountActive = "active"

// Setter is one person an automatic approval stands for, as they are now:
// the principal the session authenticator would build for them in the
// tenant, and their account's status. It is built before the approving
// transaction, never inside it.
type Setter struct {
	// UserID is the person on record (the site's setter, or the
	// connection's).
	UserID uuid.UUID
	// Principal is their current principal in the tenant. A non-member, or a
	// member of a deleted organisation, has TenantID == uuid.Nil.
	Principal domain.Principal
	// AccountStatus is users.status; empty when the account no longer
	// exists.
	AccountStatus string
	// LookupFailed is true when any read that built this failed. A failed
	// lookup is a failed check: the request asks with not_checked.
	LookupFailed bool
}

// SetterRule names the rule a setter check applied, for the audit row.
type SetterRule string

// The rules.
const (
	RuleSiteSetterAIDrafts SetterRule = "site_setter_ai_drafts"
	RuleSiteSetterFull     SetterRule = "site_setter_full"
	RuleConnectionSetter   SetterRule = "connection_setter"
)

// SetterCheck is the outcome of one setter check.
type SetterCheck struct {
	Rule SetterRule
	// OK is true only when every requirement held.
	OK bool
	// Failed names the first requirement that did not hold; empty when OK.
	Failed string
	// LookupFailed is true when the check could not be made.
	LookupFailed bool
}

// The requirement names a failed check records.
const (
	FailedNoSetter       = "no_setter"
	FailedLookup         = "lookup_failed"
	FailedNotMember      = "not_a_member"
	FailedAccountStatus  = "account_not_active"
	FailedAuthority      = "authority_to_choose"
	FailedOrgScope       = "full_membership"
	FailedSiteAccess     = "site_access"
	FailedRowPermission  = "row_permission"
	FailedUnknownSetting = "unknown_setting"
)

// base checks what every setter check requires: a person on record, a
// lookup that succeeded, membership in the tenant and an active account.
func base(rule SetterRule, s Setter, tenantID uuid.UUID) (SetterCheck, bool) {
	c := SetterCheck{Rule: rule}
	switch {
	case s.UserID == uuid.Nil:
		c.Failed = FailedNoSetter
	case s.LookupFailed:
		c.Failed, c.LookupFailed = FailedLookup, true
	case s.Principal.TenantID == uuid.Nil || s.Principal.TenantID != tenantID || s.Principal.UserID != s.UserID:
		c.Failed = FailedNotMember
	case s.AccountStatus != AccountActive:
		c.Failed = FailedAccountStatus
	default:
		return c, true
	}
	return c, false
}

// CheckSiteSetter checks the person who chose the site's mode against the
// authority that mode needs now, and against this row (B1): they must still
// be able to approve it by hand.
//
//   - ai_drafts: site.content.edit and access to the site.
//   - full: a full member of the organisation holding apikey:manage.
//
// In both, the setter must also hold the row's operator_permission (a
// permission this build does not know refuses) and have access to the row's
// site.
func CheckSiteSetter(s Setter, tenantID, siteID uuid.UUID, mode Mode, operatorPermission string) SetterCheck {
	rule := RuleSiteSetterAIDrafts
	if mode == ModeFull {
		rule = RuleSiteSetterFull
	}
	c, ok := base(rule, s, tenantID)
	if !ok {
		return c
	}
	p := s.Principal
	switch mode {
	case ModeAIDrafts:
		if !authz.PrincipalAllows(p, authz.PermSiteContentEdit) {
			c.Failed = FailedAuthority
			return c
		}
	case ModeFull:
		// authz.PrincipalAllows does not apply the organisation-scope guard,
		// and a site collaborator's role is clamped, not refused, so full
		// membership is tested on its own.
		if p.IsSiteConstrained() {
			c.Failed = FailedOrgScope
			return c
		}
		if !authz.PrincipalAllows(p, authz.PermAPIKeyManage) {
			c.Failed = FailedAuthority
			return c
		}
	default:
		c.Failed = FailedUnknownSetting
		return c
	}
	if !p.CanAccessSite(siteID) {
		c.Failed = FailedSiteAccess
		return c
	}
	perm := authz.Permission(operatorPermission)
	if !authz.KnownPermission(perm) || !authz.PrincipalAllows(p, perm) {
		c.Failed = FailedRowPermission
		return c
	}
	c.OK = true
	return c
}

// CheckConnectionSetter checks the person whose authority keeps the
// connection's switch on (D1): a full member of the organisation holding
// apikey:manage, the permission the switch, minting and consent require,
// with an active account.
func CheckConnectionSetter(s Setter, tenantID uuid.UUID) SetterCheck {
	c, ok := base(RuleConnectionSetter, s, tenantID)
	if !ok {
		return c
	}
	if s.Principal.IsSiteConstrained() {
		c.Failed = FailedOrgScope
		return c
	}
	if !authz.PrincipalAllows(s.Principal, authz.PermAPIKeyManage) {
		c.Failed = FailedAuthority
		return c
	}
	c.OK = true
	return c
}
