package abilityrequest

import (
	"strings"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// Outcome details of a wpmgr/page-edit refused as a conflict: the closed set
// the site answers with (mcp's page-edit conflict reasons, the one set the AI
// and this outcome both use), and the only values outcome_detail carries.
const (
	// DetailChangedSinceRead: the page changed after the AI read it.
	DetailChangedSinceRead = mcp.PageEditConflictChangedSinceRead
	// DetailEditorOpen: someone has the page open in Elementor.
	DetailEditorOpen = mcp.PageEditConflictEditorOpen
	// DetailAutosavePending: someone has unsaved Elementor changes on it.
	DetailAutosavePending = mcp.PageEditConflictAutosavePending
)

// outcomeDetailFor is outcome_detail: which conflict refused a page edit,
// or nil. The outcome recording keeps the site's detail word for a refusal
// (refusedOutcome, cleaned and capped); only a member of the closed set
// reaches the wire, so the site's own words never do.
func outcomeDetailFor(r sqlc.AssistantAbilityRequest) *string {
	if r.AbilityName != mcp.AbilityPageEdit || r.OutcomeCode == nil || *r.OutcomeCode != "conflict" ||
		r.SiteReportedText == nil {
		return nil
	}
	d, ok := mcp.PageEditConflictReason(*r.SiteReportedText)
	if !ok {
		return nil
	}
	return &d
}

// Outside changes of a wpmgr/page-edit refused with side_effect_detected:
// the kind of thing Elementor's save changed outside the page, and the only
// values outside_change carries.
const (
	// OutsideChangeActiveKit: the site's active Elementor kit.
	OutsideChangeActiveKit = "active_kit"
	// OutsideChangeOtherPosts: another post or page, or its data, written,
	// trashed or deleted.
	OutsideChangeOtherPosts = "other_posts"
	// OutsideChangeTerms: categories or tags.
	OutsideChangeTerms = "terms"
	// OutsideChangeSiteSettings: a site setting.
	OutsideChangeSiteSettings = "site_settings"
	// OutsideChangeUsers: a user account, a role, or the site's
	// administrators.
	OutsideChangeUsers = "users"
)

// The agent's detail for an outside change, as the outcome recording keeps
// it: the kit sentence, or the scope sentence followed by the write scope's
// fixed labels, sorted and comma separated.
const (
	outsideKitDetail   = "the active kit changed during the save"
	outsideScopeDetail = "the save wrote outside the page: "
)

// outsideChangeKinds maps each fixed label the agent's write scope reports
// to the kind of change it names. A label not here names no kind.
var outsideChangeKinds = map[string]string{
	"other_post_written":      OutsideChangeOtherPosts,
	"other_post_meta_written": OutsideChangeOtherPosts,
	"post_deleted":            OutsideChangeOtherPosts,
	"post_trashed":            OutsideChangeOtherPosts,
	"term_changed":            OutsideChangeTerms,
	"option_written":          OutsideChangeSiteSettings,
	"role_changed":            OutsideChangeUsers,
	"user_changed":            OutsideChangeUsers,
	"user_capabilities_meta":  OutsideChangeUsers,
	"user_level_meta":         OutsideChangeUsers,
	"user_meta_unknown":       OutsideChangeUsers,
	"user_roles_option":       OutsideChangeUsers,
	"site_admins":             OutsideChangeUsers,
}

// outsideChangeFor is outside_change: what kind of thing outside the page
// Elementor's save changed when a page edit was refused with
// side_effect_detected, or nil. It names one kind only: a detail whose
// labels name more than one kind, a label that names none, or any other
// text gives nil, so the site's own words never reach the wire.
func outsideChangeFor(r sqlc.AssistantAbilityRequest) *string {
	if r.AbilityName != mcp.AbilityPageEdit || r.OutcomeCode == nil || *r.OutcomeCode != "side_effect_detected" ||
		r.SiteReportedText == nil {
		return nil
	}
	if *r.SiteReportedText == outsideKitDetail {
		kind := OutsideChangeActiveKit
		return &kind
	}
	labels, ok := strings.CutPrefix(*r.SiteReportedText, outsideScopeDetail)
	if !ok {
		return nil
	}
	kind := ""
	for _, label := range strings.Split(labels, ", ") {
		k, known := outsideChangeKinds[label]
		if !known || (kind != "" && k != kind) {
			return nil
		}
		kind = k
	}
	return &kind
}
