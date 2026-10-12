// Package aipolicy decides whether one AI write request runs at once under
// the site's setting or waits for a person (ADR-065, approval tiers).
//
// It is pure: no database, no clock, no network. Every input is a fact the
// control plane recorded from its own catalogue, its own checked precheck and
// its own settings tables. Nothing the AI sent, and no text from a site,
// reaches an input: a hostile post title gives the same decision and the same
// constant message as any other title.
//
// The mode matrix lives here and in the SQL function ai_mode_allows(mode,
// class); a test compares every pair, so the two copies cannot drift.
package aipolicy

// Mode is how much AI connections may do on a site without a person.
type Mode string

// The site modes.
const (
	// ModeAsk: every change waits for a person.
	ModeAsk Mode = "ask"
	// ModeAIDrafts: the AI's own drafts, and changes to them, run at once.
	ModeAIDrafts Mode = "ai_drafts"
	// ModeFull: everything except the Ask list runs at once. No route in
	// this build sets it.
	ModeFull Mode = "full"
)

// Modes is every mode, in display order.
func Modes() []Mode { return []Mode{ModeAsk, ModeAIDrafts, ModeFull} }

// Known reports whether m is one of the three modes.
func (m Mode) Known() bool {
	switch m {
	case ModeAsk, ModeAIDrafts, ModeFull:
		return true
	}
	return false
}

// Class is the kind of a change. It comes from the catalogue entry or route
// row a superadmin wrote, and from the checked precheck; never from the AI.
type Class string

// The effective classes ("the seven").
const (
	ClassAIDraft     Class = "ai_draft"
	ClassOperational Class = "operational"
	ClassUnpublished Class = "unpublished"
	ClassLive        Class = "live"
	ClassPublish     Class = "publish"
	ClassUpdate      Class = "update"
	ClassAlwaysAsk   Class = "always_ask"
)

// StoredByTargetStatus is a STORED class only. A route or ability that edits
// an existing post stores it, and the effective class comes from the checked
// status of the target. It is never an effective class: Allows is false for
// it in every mode.
const StoredByTargetStatus Class = "by_target_status"

// Classes is the seven effective classes, in display order.
func Classes() []Class {
	return []Class{ClassAIDraft, ClassOperational, ClassUnpublished, ClassLive, ClassPublish, ClassUpdate, ClassAlwaysAsk}
}

// StoredClasses is every value a catalogue entry, a route row or a request's
// base_change_class may hold: the seven plus StoredByTargetStatus.
func StoredClasses() []Class { return append(Classes(), StoredByTargetStatus) }

// Effective reports whether c is one of the seven.
func (c Class) Effective() bool {
	for _, k := range Classes() {
		if c == k {
			return true
		}
	}
	return false
}

// Stored reports whether c is a value a catalogue may store.
func (c Class) Stored() bool { return c.Effective() || c == StoredByTargetStatus }

// KindName is WPMgr's name for an effective class as copy uses it. It is
// empty for always_ask, which is named by its list, and for anything unknown.
func (c Class) KindName() string {
	switch c {
	case ClassAIDraft:
		return "changes to the AI's own drafts"
	case ClassOperational:
		return "cache clears"
	case ClassUnpublished:
		return "changes to unpublished content"
	case ClassLive:
		return "edits to published pages"
	case ClassPublish:
		return "publishing and scheduling"
	case ClassUpdate:
		return "plugin and theme updates"
	}
	return ""
}

// Allows is the mode matrix. The SQL copy is ai_mode_allows(mode, class).
//
//	              ask   ai_drafts   full
//	ai_draft      no    yes         yes
//	operational   no    yes         yes
//	unpublished   no    no          yes
//	live          no    no          yes
//	publish       no    no          yes
//	update        no    no          yes
//	always_ask    no    no          no
//
// Anything else, by_target_status and unknown values included, is no.
func Allows(m Mode, c Class) bool {
	switch m {
	case ModeAIDrafts:
		return c == ClassAIDraft || c == ClassOperational
	case ModeFull:
		switch c {
		case ClassAIDraft, ClassOperational, ClassUnpublished, ClassLive, ClassPublish, ClassUpdate:
			return true
		}
	}
	return false
}

// ConnectionAuto is a connection's switch for automatic changes.
type ConnectionAuto string

// The switch values.
const (
	// AutoSiteSetting: the connection runs a change wherever the site's
	// mode allows it.
	AutoSiteSetting ConnectionAuto = "site_setting"
	// AutoNever: every change from the connection waits for a person.
	AutoNever ConnectionAuto = "never"
)

// Known reports whether a is one of the two switch values.
func (a ConnectionAuto) Known() bool { return a == AutoSiteSetting || a == AutoNever }

// AskReason is why a request waits for a person. The set is closed and
// matches the request tables' ask_reason CHECK.
type AskReason string

// The reasons a request waits for a person.
const (
	AskKindAlwaysAsks          AskReason = "kind_always_asks"
	AskUnknownTargetState      AskReason = "unknown_target_state"
	AskSiteModeAsk             AskReason = "site_mode_ask"
	AskKindNotInMode           AskReason = "kind_not_in_mode"
	AskSetterLacksPermission   AskReason = "setter_lacks_permission"
	AskOverChangeBudget        AskReason = "over_change_budget"
	AskOverSiteCap             AskReason = "over_site_cap"
	AskConnectionNeverAuto     AskReason = "connection_never_auto"
	AskConnectionSetterInvalid AskReason = "connection_setter_invalid"
	AskNotChecked              AskReason = "not_checked"
	AskVisibleAutoHeld         AskReason = "visible_auto_held"
	AskSessionEnded            AskReason = "session_ended"
)

// AskReasons is the closed set, in the order of the ask_reason CHECK.
func AskReasons() []AskReason {
	return []AskReason{
		AskKindAlwaysAsks, AskUnknownTargetState, AskSiteModeAsk, AskKindNotInMode,
		AskSetterLacksPermission, AskOverChangeBudget, AskOverSiteCap, AskConnectionNeverAuto,
		AskConnectionSetterInvalid, AskNotChecked, AskVisibleAutoHeld, AskSessionEnded,
	}
}

// Known reports whether r is in the closed set.
func (r AskReason) Known() bool {
	for _, k := range AskReasons() {
		if r == k {
			return true
		}
	}
	return false
}
