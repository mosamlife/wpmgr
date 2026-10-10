package mcp

// The answer the AI gets when the site refuses a wpmgr/page-create or
// wpmgr/page-edit precheck. Everything in it is ours or a member of a closed
// set: the agent's refusal code (agentcmd.AbilityRunRefusalCodes, anything
// else is unknown), for a page-edit conflict the reason (the closed set
// below), and the fixed hint for them. The site's own detail text is
// untrusted and never reaches the answer.

import (
	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// The reasons a wpmgr/page-edit is refused as a conflict: the closed set the
// agent answers with in a conflict's detail. They are the only values a
// conflict's reason, and a request's outcome_detail, carry.
const (
	// PageEditConflictChangedSinceRead: the page changed after the AI read it.
	PageEditConflictChangedSinceRead = "changed_since_read"
	// PageEditConflictEditorOpen: someone has the page open in Elementor.
	PageEditConflictEditorOpen = "editor_open"
	// PageEditConflictAutosavePending: someone has unsaved Elementor changes
	// on it.
	PageEditConflictAutosavePending = "autosave_pending"
)

// pageEditCodeConflict is the agent's refusal code for a page edit that
// conflicts with the page as it is now; its detail names the reason.
const pageEditCodeConflict = "conflict"

// unknownRefusalCode is the code of a refusal outside the agent's closed set.
const unknownRefusalCode = "unknown"

// Fixed hints for a page-edit conflict, one per reason and one for a
// conflict that names none.
const (
	hintPageEditConflictEditorOpen = "Someone has this page open in Elementor. Wait three minutes, then send the " +
		"same call again: WordPress frees a page about two and a half minutes after its editor is closed. If it " +
		"is refused for this reason three times, stop and ask the person to close the page in Elementor."
	hintPageEditConflictChangedSinceRead = "The page changed after you read it. Read the page again with " +
		"wpmgr/page-structure, then send the edit with the base_fingerprint and refs that read gives."
	hintPageEditConflictAutosavePending = "Someone has unsaved Elementor changes on this page. Ask the person to " +
		"save or discard their changes in Elementor, then read the page again with wpmgr/page-structure before " +
		"you edit it."
	hintPageEditConflict = "The page is open in Elementor or changed after you read it. Read the page again with " +
		"wpmgr/page-structure before you edit it, and if someone has it open in Elementor, ask them to close it."
)

// pageEditConflictHints maps each conflict reason to its fixed hint.
var pageEditConflictHints = map[string]string{
	PageEditConflictChangedSinceRead: hintPageEditConflictChangedSinceRead,
	PageEditConflictEditorOpen:       hintPageEditConflictEditorOpen,
	PageEditConflictAutosavePending:  hintPageEditConflictAutosavePending,
}

// PageEditConflictReason is detail as a page-edit conflict reason: ok only
// for a member of the closed set, compared exactly.
func PageEditConflictReason(detail string) (string, bool) {
	if _, ok := pageEditConflictHints[detail]; !ok {
		return "", false
	}
	return detail, true
}

// msgSitePrecheckRefused opens the message of every site precheck refusal.
const msgSitePrecheckRefused = "the site refused the call, so nothing was asked"

// sitePrecheckRefusal is the refusal for a wpmgr/page-create or
// wpmgr/page-edit precheck the site refused. Its details are the code,
// whether a retry can help and the fixed hint when there is one; a page-edit
// conflict adds its reason, and only editor_open is retryable, whatever the
// agent's own flag says. The message names the code and the reason and
// carries the hint, so a client that shows only the message still has them.
func sitePrecheckRefusal(ability string, refusal *agentcmd.AbilityRunRefusal) *toolRefusal {
	code := refusal.Code
	if _, ok := agentcmd.AbilityRunRefusalCodes[code]; !ok {
		code = unknownRefusalCode
	}
	details := map[string]any{"code": code, "retryable": refusal.Retryable}
	reason, hint := "", precheckRefusalHint(code)
	if ability == AbilityPageEdit && code == pageEditCodeConflict {
		reason, _ = PageEditConflictReason(refusal.Detail)
		hint = hintPageEditConflict
		if reason != "" {
			hint = pageEditConflictHints[reason]
			details["reason"] = reason
		}
		details["retryable"] = reason == PageEditConflictEditorOpen
	}
	if hint != "" {
		details["hint"] = hint
	}
	msg := msgSitePrecheckRefused + ": code " + code
	if reason != "" {
		msg += ", reason " + reason
	}
	if hint != "" {
		msg += ". " + hint
	}
	return &toolRefusal{
		reason: reasonAbilityPrecheckRefused,
		err:    domain.Validation(ErrCodeInvalidToolArguments, msg).WithDetails(details),
		meta:   map[string]any{"code": code},
	}
}
