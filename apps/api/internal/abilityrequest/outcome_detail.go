package abilityrequest

import (
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// Outcome details of a wpmgr/page-edit refused as a conflict: the closed set
// the site answers with, and the only values outcome_detail carries.
const (
	// DetailChangedSinceRead: the page changed after the AI read it.
	DetailChangedSinceRead = "changed_since_read"
	// DetailEditorOpen: someone has the page open in Elementor.
	DetailEditorOpen = "editor_open"
	// DetailAutosavePending: someone has unsaved Elementor changes on it.
	DetailAutosavePending = "autosave_pending"
)

var pageEditConflictDetails = map[string]struct{}{
	DetailChangedSinceRead: {},
	DetailEditorOpen:       {},
	DetailAutosavePending:  {},
}

// outcomeDetailFor is outcome_detail: which conflict refused a page edit,
// or nil. The outcome recording keeps the site's detail word for a refusal
// (refusedOutcome, cleaned and capped); only a member of the closed set
// reaches the wire, so the site's own words never do.
func outcomeDetailFor(r sqlc.AssistantAbilityRequest) *string {
	if r.AbilityName != mcp.AbilityPageEdit || r.OutcomeCode == nil || *r.OutcomeCode != "conflict" ||
		r.SiteReportedText == nil {
		return nil
	}
	if _, ok := pageEditConflictDetails[*r.SiteReportedText]; !ok {
		return nil
	}
	d := *r.SiteReportedText
	return &d
}
