package aipolicy

import "time"

// Inputs is everything one decision reads. None of it comes from the AI.
type Inputs struct {
	// Class is the request's classing (Classify).
	Class Classification
	// SiteMode is sites.ai_mode as read under the policy lock.
	SiteMode Mode
	// SiteSetter is the check of the person who chose the site's mode. It is
	// ignored when the mode is ask.
	SiteSetter SetterCheck
	// ConnectionAuto is the connection's switch.
	ConnectionAuto ConnectionAuto
	// ConnectionSetter is the check of the person whose authority keeps the
	// switch on. It is ignored when the switch is never.
	ConnectionSetter SetterCheck
	// Usage is the connection's automatic changes in the class's bucket.
	Usage Usage
	// VisibleAutoHeld is true while a read whose output may not be public is
	// enabled; a live or publish change then never runs automatically.
	VisibleAutoHeld bool
}

// Outcome is what happens to the request.
type Outcome string

// The outcomes.
const (
	// OutcomeAsk: the request waits for a person.
	OutcomeAsk Outcome = "ask"
	// OutcomeAutoBySetting: the site's setting approves it.
	OutcomeAutoBySetting Outcome = "auto_by_setting"
)

// Decision is the engine's answer for one request.
type Decision struct {
	Outcome Outcome
	// Class is the effective class; empty when the target's state was
	// unknown.
	Class Class
	// Ask is why the request waits; empty for an automatic approval.
	Ask AskReason
	// ResumesAt is set with AskOverChangeBudget: when the window frees a
	// change. Zero otherwise, or when unknown.
	ResumesAt time.Time
}

func ask(c Class, r AskReason) Decision { return Decision{Outcome: OutcomeAsk, Class: c, Ask: r} }

// Evaluate decides one request. First match wins:
//
//  1. (Organisation paused, AI editing off, write tools off: refused before
//     the engine, as today.)
//  2. Effective class always_ask: kind_always_asks. Unknown target state:
//     unknown_target_state.
//  3. A setter lookup failed: not_checked.
//  4. Connection set to never: connection_never_auto. The connection's
//     setter fails the check: connection_setter_invalid.
//  5. The site's mode allows the class: the site's setter fails the check:
//     setter_lacks_permission; a live or publish change while visible
//     automatic changes are held: visible_auto_held; within the class's
//     budget: approved by the setting; over it: over_change_budget or
//     over_site_cap.
//  6. (A session covering the class: Merge B.)
//  7. Otherwise: site_mode_ask when the mode is ask, else kind_not_in_mode.
func Evaluate(in Inputs) Decision {
	c := in.Class.Class
	if in.Class.Ask != "" {
		return ask(c, in.Class.Ask)
	}
	if !c.Effective() || c == ClassAlwaysAsk {
		return ask(ClassAlwaysAsk, AskKindAlwaysAsks)
	}
	if !in.SiteMode.Known() || !in.ConnectionAuto.Known() {
		return ask(c, AskNotChecked)
	}
	if in.ConnectionSetter.LookupFailed || (in.SiteMode != ModeAsk && in.SiteSetter.LookupFailed) {
		return ask(c, AskNotChecked)
	}
	if in.ConnectionAuto == AutoNever {
		return ask(c, AskConnectionNeverAuto)
	}
	if !in.ConnectionSetter.OK || in.ConnectionSetter.Rule != RuleConnectionSetter {
		return ask(c, AskConnectionSetterInvalid)
	}
	if Allows(in.SiteMode, c) {
		if !in.SiteSetter.OK || !siteRuleMatches(in.SiteMode, in.SiteSetter.Rule) {
			return ask(c, AskSetterLacksPermission)
		}
		if in.VisibleAutoHeld && (c == ClassLive || c == ClassPublish) {
			return ask(c, AskVisibleAutoHeld)
		}
		b := BucketOf(c)
		if b == BucketNone || !in.Usage.Checked {
			return ask(c, AskNotChecked)
		}
		if r, over := overBudget(b, in.Usage); over {
			d := ask(c, r)
			if r == AskOverChangeBudget {
				d.ResumesAt = in.Usage.ResumesAt()
			}
			return d
		}
		return Decision{Outcome: OutcomeAutoBySetting, Class: c}
	}
	if in.SiteMode == ModeAsk {
		return ask(c, AskSiteModeAsk)
	}
	return ask(c, AskKindNotInMode)
}

// siteRuleMatches holds a site setter check to the rule its mode needs, so a
// check made for one mode cannot stand in for another.
func siteRuleMatches(m Mode, r SetterRule) bool {
	switch m {
	case ModeAIDrafts:
		return r == RuleSiteSetterAIDrafts
	case ModeFull:
		return r == RuleSiteSetterFull
	}
	return false
}
