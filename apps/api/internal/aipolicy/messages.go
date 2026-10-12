package aipolicy

// The approval values a tool result carries.
const (
	// ApprovalNone: a read; nothing needed approving.
	ApprovalNone = "none"
	// ApprovalAuto: approved by the site's setting; it ran at once.
	ApprovalAuto = "auto"
	// ApprovalSession: approved by a session a person allowed (Merge B).
	ApprovalSession = "session"
	// ApprovalAsk: waits for a person.
	ApprovalAsk = "ask"
)

// MessageDoneBySetting is the AI's text for a change that ran under the
// site's setting. The remaining quota is not disclosed.
const MessageDoneBySetting = "Done. This ran automatically under a setting a person chose for this site. A person can undo it in WPMgr. Check it with a separate read."

// MessageRunningBySetting is the AI's text for a change the site's setting
// approved that had not finished when the tool answered.
const MessageRunningBySetting = "This change was approved automatically under a setting a person chose for this site and is running now. Call the status tool with request_id after poll_after_seconds. Do not send it again."

// The AI-facing text for each reason a change waits. Constants only: no site
// text, no person's name and no count is ever interpolated, and none says
// who chose a setting.
const (
	msgKindAlwaysAsks          = "Nothing has changed yet. This kind of change always needs a person to approve it in WPMgr. Tell the person it is waiting and give them approval_url. Do not wait in a loop, and do not try to make the same change another way."
	msgUnknownTargetState      = "Nothing has changed yet. WPMgr could not tell whether visitors would see this change, so a person must approve it. Tell the person and give them approval_url."
	msgSiteModeAsk             = "Nothing has changed yet. This site is set so a person approves every AI change. Tell the person and give them approval_url. Do not wait in a loop."
	msgKindNotInMode           = "Nothing has changed yet. This site runs this kind of change only with a person's approval. Tell the person and give them approval_url."
	msgSetterLacksPermission   = "Nothing has changed yet. This change needs a person's approval in WPMgr. Tell the person and give them approval_url."
	msgOverChangeBudget        = "Nothing has changed yet. This connection has used its automatic changes for now, so this change needs a person's approval. It will not run on its own. Tell the person and give them approval_url. Do not split the work into smaller changes to get around the limit. Automatic changes resume at auto_resumes_at."
	msgOverSiteCap             = "Nothing has changed yet. This connection has changed the most sites it may change automatically this hour, so this change needs a person's approval. Tell the person and give them approval_url. Do not retry on other sites to get around the limit."
	msgConnectionNeverAuto     = "Nothing has changed yet. A person set this connection to always ask. Tell the person and give them approval_url."
	msgConnectionSetterInvalid = "Nothing has changed yet. Every change from this connection needs a person's approval in WPMgr for now. Tell the person and give them approval_url."
	msgNotChecked              = "Nothing has changed yet. This change needs a person's approval in WPMgr. Tell the person and give them approval_url."
	msgVisibleAutoHeld         = "Nothing has changed yet. This change needs a person's approval in WPMgr. Tell the person and give them approval_url."
	msgSessionEnded            = "Nothing has changed yet. The time or changes a person allowed have run out. Tell the person and give them approval_url."
)

// Message is the constant AI-facing text for r. An unknown reason gets the
// not_checked text: the change waits, and nothing more is claimed.
func (r AskReason) Message() string {
	switch r {
	case AskKindAlwaysAsks:
		return msgKindAlwaysAsks
	case AskUnknownTargetState:
		return msgUnknownTargetState
	case AskSiteModeAsk:
		return msgSiteModeAsk
	case AskKindNotInMode:
		return msgKindNotInMode
	case AskSetterLacksPermission:
		return msgSetterLacksPermission
	case AskOverChangeBudget:
		return msgOverChangeBudget
	case AskOverSiteCap:
		return msgOverSiteCap
	case AskConnectionNeverAuto:
		return msgConnectionNeverAuto
	case AskConnectionSetterInvalid:
		return msgConnectionSetterInvalid
	case AskNotChecked:
		return msgNotChecked
	case AskVisibleAutoHeld:
		return msgVisibleAutoHeld
	case AskSessionEnded:
		return msgSessionEnded
	}
	return msgNotChecked
}
