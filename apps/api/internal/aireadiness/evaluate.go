package aireadiness

import (
	"regexp"
	"strings"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/wpversion"
)

// Builder version floors: the first Elementor and Bricks releases that ship
// AI tools. They are product facts, not contract constants, so they live here.
const (
	MinElementorVersion = "4.3"
	MinBricksVersion    = "2.4"

	// abilitiesAPIWPVersion is the first WordPress release that ships the
	// Abilities API in core.
	abilitiesAPIWPVersion = "6.9"
	// bricksThemeDir is the directory name of the Bricks theme.
	bricksThemeDir = "bricks"
)

// preReleaseWord is the whole vocabulary a version suffix may use: alpha, beta,
// rc, dev or build, in any letter case. The set is closed: no other word
// matches, and only a number may follow one, so a site can put a number after
// its version but never a word of its own.
const preReleaseWord = `(?i:alpha|beta|rc|dev|build)`

// The shapes a site-reported value must have before Evaluate will compare it
// or return it. Go's $ without the m flag is the end of the text, so a
// trailing newline does not match.
var (
	// wpVersionRe admits a release of two to four numbers separated by dots
	// (7.1, 7.1.2) or a pre-release: a hyphen, a preReleaseWord and optional
	// digits, then optionally a hyphen and a build number (7.1-RC1,
	// 7.1-beta2-59000, 7.1-alpha-59000). A release or a pre-release may end in
	// -src, the tail WordPress gives a development checkout (7.1-alpha-59000-src,
	// 7.0.1-src). Only this pattern admits that tail.
	wpVersionRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+){1,3}(-` + preReleaseWord + `[0-9]*(-[0-9]+)?)?(-src)?$`)
	// agentVersionRe admits a dotted numeric release only, the shape every
	// agent floor compare in this codebase requires.
	agentVersionRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+){1,3}$`)
	// componentVersionRe admits a plugin or theme version: a release of one to
	// four numbers separated by dots (4, 4.3, 4.3.4, 1.2.3.4), optionally
	// followed by one suffix: a hyphen, plus, tilde or underscore, a
	// preReleaseWord, an optional dot and optional digits (4.3.0-beta1,
	// 2.4.1+build.5, 2.0-Beta). Nothing else is admitted, so a version never
	// carries a word of the site's own.
	componentVersionRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+){0,3}([-+~_]` + preReleaseWord + `\.?[0-9]*)?$`)
)

const (
	maxWPVersionLen        = 32
	maxComponentVersionLen = 64
)

func cleanWPVersion(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) > maxWPVersionLen || !wpVersionRe.MatchString(s) {
		return "", false
	}
	return s, true
}

func cleanAgentVersion(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if !agentVersionRe.MatchString(s) {
		return "", false
	}
	return s, true
}

func cleanComponentVersion(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) > maxComponentVersionLen || !componentVersionRe.MatchString(s) {
		return "", false
	}
	return s, true
}

// DefaultFloors derives every floor from the constant the rest of the control
// plane enforces, so the checklist and the ability engine cannot disagree
// about a number. The agent floor is the greater of the two engine floors
// builder tools need.
func DefaultFloors() Floors {
	agent := agentcmd.MinAgentVersionForRestCall
	if wpversion.Compare(agentcmd.MinAgentVersionForVendorReads, agent) > 0 {
		agent = agentcmd.MinAgentVersionForVendorReads
	}
	return Floors{
		WP:             agentcmd.MinWPVersionForVendorReads,
		Agent:          agent,
		FactsAgent:     agentcmd.MinAgentVersionForBuilderFacts,
		Elementor:      MinElementorVersion,
		Bricks:         MinBricksVersion,
		EngineAgent:    agentcmd.MinAgentVersionForAbilityEngine,
		AbilitiesAPIWP: abilitiesAPIWPVersion,
	}
}

// Evaluate computes the checklist for one site against the default floors.
// It is a pure function: no I/O, no clock, no site text in the result.
func Evaluate(f Facts) Result { return EvaluateWith(f, DefaultFloors()) }

// EvaluateWith is Evaluate against explicit floors.
//
// The rules, in one place:
//
//   - Every site-reported version is validated before it is compared or
//     returned. One that fails its shape check is "not reported", never a
//     failure.
//   - A check is unknown, not failing, whenever WPMgr could not tell: the
//     inventory never ran, it was cut short with nothing found, or the agent
//     is too old to report the fact.
//   - A builder's AI switch is on only when an ability in its namespace was
//     registered by that builder itself. A same-named ability from anything
//     else never counts (the SQL attributes by owner; Facts carries the count).
//   - A builder that is not installed contributes nothing to Status.
//   - A builder that is installed but not active is not a fix either: its
//     version row is not applicable (reason inactive), and so are the rows
//     that depend on it.
//   - A row whose pass or fail is only inferred (see Check.unconfirmed) is
//     listed with its state, and counts toward neither Status nor FixCount
//     and is not in Failing. The same row when unknown is an ordinary
//     unknown, and makes the site incomplete.
//   - Warnings are advisory and never change Status or FixCount.
func EvaluateWith(f Facts, fl Floors) Result {
	e := &evaluator{f: f, fl: fl}
	e.wp, e.wpOK = cleanWPVersion(f.WPVersion)
	e.agent, e.agentOK = cleanAgentVersion(f.AgentVersion)

	wp := e.checkWP()
	api := e.checkAbilitiesAPI()
	base := Group{
		ID: GroupBase,
		Checks: []Check{
			wp,
			api,
			e.checkAgent(),
			e.checkContentEditing(),
		},
	}
	elementor := e.elementorGroup(api)
	bricks := e.bricksGroup(api)

	groups := []Group{base, elementor, bricks}
	fixes, unknown := 0, 0
	for _, g := range groups {
		if g.ID != GroupBase && !g.Installed {
			continue // a builder that is not installed contributes nothing
		}
		for _, c := range g.Checks {
			if c.unconfirmed() {
				continue // shown, never counted
			}
			switch c.State {
			case StateFail:
				fixes++
			case StateUnknown:
				unknown++
			}
		}
	}
	status := StatusReady
	switch {
	case fixes > 0:
		status = StatusNeedsAttention
	case unknown > 0:
		status = StatusIncomplete
	}

	warnings := make([]WarningCode, 0, 2)
	if f.MCPAdapterActive {
		warnings = append(warnings, WarnMCPAdapterPluginActive)
	}
	if elementor.Installed && checkByID(elementor, CheckElementorSwitch).State == StatePass {
		warnings = append(warnings, WarnElementorEndpointOpen)
	}

	return Result{
		SiteID:        f.SiteID,
		Status:        status,
		FixCount:      fixes,
		MetadataAsOf:  f.MetadataAsOf,
		AbilitiesAsOf: f.AbilitiesAsOf,
		Warnings:      warnings,
		Floors:        fl,
		Groups:        groups,
	}
}

func checkByID(g Group, id CheckID) Check {
	for _, c := range g.Checks {
		if c.ID == id {
			return c
		}
	}
	return Check{}
}

type evaluator struct {
	f  Facts
	fl Floors

	wp      string
	wpOK    bool
	agent   string
	agentOK bool
}

func pass(id CheckID, observed string) Check {
	return Check{ID: id, State: StatePass, Observed: observed}
}

func fail(id CheckID, reason Reason, observed string) Check {
	return Check{ID: id, State: StateFail, Reason: reason, Observed: observed}
}

func unknown(id CheckID, reason Reason, observed string) Check {
	return Check{ID: id, State: StateUnknown, Reason: reason, Observed: observed}
}

func notApplicable(id CheckID, reason Reason) Check {
	return Check{ID: id, State: StateNotApplicable, Reason: reason}
}

// inactive is a builder's version row when the builder is installed but not in
// use. Choosing not to run a builder is not something to fix, so the row is
// not applicable and counts toward neither a fix nor an unknown.
func inactive(id CheckID, observed string) Check {
	return Check{ID: id, State: StateNotApplicable, Reason: ReasonInactive, Observed: observed}
}

// blocksDependents reports whether a builder's version row stops the rows
// that depend on it from being checked.
func (c Check) blocksDependents() bool {
	return c.State == StateFail || c.State == StateNotApplicable
}

// unconfirmed reports whether a row's pass or fail is only inferred from the
// site's tool list and has not been confirmed on a licensed install. Such a row
// keeps its state in Groups so it can be shown, but it is not something to fix:
// it moves neither Status nor FixCount, and Failing leaves it out. A row that
// is unknown or not applicable claims nothing, so it is an ordinary one: an
// unknown makes the site incomplete. A row stops being unconfirmed when the
// check that confirms it exists; bricks_abilities waits on the Bricks licence
// check.
func (c Check) unconfirmed() bool {
	return c.ID == CheckBricksAbilities && (c.State == StatePass || c.State == StateFail)
}

// agentAtLeast compares two well-formed agent versions.
func agentAtLeast(v, floor string) bool { return wpversion.Compare(v, floor) >= 0 }

// agentBelow reports whether the agent's version is known and below floor. An
// unknown version is not "below": we cannot say the plugin is old.
func (e *evaluator) agentBelow(floor string) bool {
	return e.agentOK && wpversion.Compare(e.agent, floor) < 0
}

// factsReason is why builder_facts is unknown for a site that did not report
// it: an agent that predates the collector needs updating; a current agent
// whose collection failed simply has not reported.
func (e *evaluator) factsReason() Reason {
	if e.agentBelow(e.fl.FactsAgent) {
		return ReasonAgentTooOldForFact
	}
	return ReasonNotReported
}

func (e *evaluator) checkWP() Check {
	if !e.wpOK {
		return unknown(CheckWPVersion, ReasonNotReported, "")
	}
	if wpversion.Compare(e.wp, e.fl.WP) >= 0 {
		return pass(CheckWPVersion, e.wp)
	}
	return fail(CheckWPVersion, ReasonNone, e.wp)
}

// checkAbilitiesAPI passes when the site's last tool-list read saw the
// Abilities API, or when WordPress itself ships it. It fails only when a read
// ran, saw no API and WordPress is known to be older than the release that
// ships it. Everything else is unknown.
func (e *evaluator) checkAbilitiesAPI() Check {
	f := e.f
	wpHasAPI := e.wpOK && wpversion.Compare(e.wp, e.fl.AbilitiesAPIWP) >= 0
	if (f.InventoryChecked && f.AbilitiesAPIPresent) || wpHasAPI {
		return pass(CheckAbilitiesAPI, "")
	}
	if f.InventoryChecked {
		if e.wpOK {
			return fail(CheckAbilitiesAPI, ReasonNone, "")
		}
		return unknown(CheckAbilitiesAPI, ReasonNotReported, "")
	}
	if e.agentBelow(e.fl.EngineAgent) {
		return unknown(CheckAbilitiesAPI, ReasonAgentTooOld, "")
	}
	return unknown(CheckAbilitiesAPI, ReasonInventoryNeverRun, "")
}

func (e *evaluator) checkAgent() Check {
	if !e.agentOK {
		return unknown(CheckAgentVersion, ReasonNotReported, "")
	}
	if wpversion.Compare(e.agent, e.fl.Agent) >= 0 {
		return pass(CheckAgentVersion, e.agent)
	}
	return fail(CheckAgentVersion, ReasonNone, e.agent)
}

func (e *evaluator) checkContentEditing() Check {
	if e.f.ContentEditingEnabled {
		return pass(CheckContentEditing, "")
	}
	return fail(CheckContentEditing, ReasonNone, "")
}

// switchCheck is a builder's AI-tools row: on when at least one ability in the
// builder's namespace was registered by the builder itself.
//
// Dependencies come first. A failing abilities_api means the site has no tool
// list to read; a failing or unknowable builder version means the switch may
// not exist yet. Then: never inventoried is unknown; a tool list read while
// the site lacked the Abilities API says nothing about the switch, so it is
// unknown too, even if WordPress has been updated since; any attributed
// ability is a pass; none on a list that was cut short is unknown; none on a
// complete list is a fail.
func (e *evaluator) switchCheck(id CheckID, needs Reason, version, api Check, attributed int64) Check {
	switch {
	case api.State == StateFail:
		return notApplicable(id, ReasonNeedsAbilities)
	case version.blocksDependents():
		return notApplicable(id, needs)
	case version.State != StatePass:
		return unknown(id, needs, "")
	case !e.f.InventoryChecked:
		return unknown(id, ReasonInventoryNeverRun, "")
	case !e.f.AbilitiesAPIPresent:
		return unknown(id, ReasonInventoryNeverRun, "")
	case attributed > 0:
		return pass(id, "")
	case e.f.AbilitiesTruncated:
		return unknown(id, ReasonInventoryTruncated, "")
	}
	return fail(id, ReasonNone, "")
}

func (e *evaluator) elementorGroup(api Check) Group {
	f := e.f
	g := Group{ID: GroupElementor, Installed: f.ElementorInstalled, Support: SupportComing, Checks: []Check{}}
	if !f.ElementorInstalled {
		return g
	}
	ver, verOK := cleanComponentVersion(f.ElementorVersion)
	g.Version = ver

	var version Check
	switch {
	case !f.ElementorActive:
		version = inactive(CheckElementorVersion, ver)
	case !verOK:
		version = unknown(CheckElementorVersion, ReasonNotReported, "")
	case wpversion.Compare(ver, e.fl.Elementor) < 0:
		version = fail(CheckElementorVersion, ReasonTooOld, ver)
	default:
		version = pass(CheckElementorVersion, ver)
	}

	sw := e.switchCheck(CheckElementorSwitch, ReasonNeedsElementor, version, api, f.ElementorAbilities)

	var atomic Check
	switch {
	case version.blocksDependents():
		atomic = notApplicable(CheckElementorAtomic, ReasonNeedsElementor)
	case version.State != StatePass:
		atomic = unknown(CheckElementorAtomic, ReasonNeedsElementor, "")
	case !f.BuilderFacts.Reported:
		atomic = unknown(CheckElementorAtomic, e.factsReason(), "")
	case f.BuilderFacts.Elementor == nil || f.BuilderFacts.Elementor.AtomicEditor == nil:
		atomic = unknown(CheckElementorAtomic, ReasonNotReported, "")
	case *f.BuilderFacts.Elementor.AtomicEditor:
		atomic = pass(CheckElementorAtomic, "")
	default:
		atomic = fail(CheckElementorAtomic, ReasonNone, "")
	}

	g.Checks = []Check{version, sw, atomic}
	return g
}

// bricksActive reports whether Bricks is the active theme or the parent of the
// active one. activeKnown is false when the site gave no way to tell: the
// theme list marks only the stylesheet theme active, so a Bricks child theme
// reads as "Bricks installed, inactive" until the agent reports the parent.
//
// The reported parent theme decides alone whenever it is known: it is read at
// the same push as the theme list, whereas an ability registered by Bricks may
// come from a tool list read before the theme was switched. Without it, Bricks
// is active when its theme row is active or the last tool-list read found an
// ability registered by Bricks.
func (e *evaluator) bricksActive() (active, activeKnown bool) {
	f := e.f
	if t := f.BuilderFacts.ThemeTemplate; t != "" {
		return t == bricksThemeDir, true
	}
	if f.BricksActive || f.BricksAbilities > 0 {
		return true, true
	}
	return false, false
}

func (e *evaluator) bricksGroup(api Check) Group {
	f := e.f
	g := Group{ID: GroupBricks, Installed: f.BricksInstalled, Support: SupportComing, Checks: []Check{}}
	if !f.BricksInstalled {
		return g
	}
	ver, verOK := cleanComponentVersion(f.BricksVersion)
	g.Version = ver

	active, activeKnown := e.bricksActive()
	var version Check
	switch {
	case !activeKnown:
		// Installed, not marked active, and no parent theme reported: a child
		// theme may be in use. An agent that predates the collector needs
		// updating; a current one simply has not reported.
		reason := ReasonNotReported
		if !f.BuilderFacts.Reported {
			reason = e.factsReason()
		}
		version = unknown(CheckBricksVersion, reason, ver)
	case !active:
		version = inactive(CheckBricksVersion, ver)
	case !verOK:
		version = unknown(CheckBricksVersion, ReasonNotReported, "")
	case wpversion.Compare(ver, e.fl.Bricks) < 0:
		version = fail(CheckBricksVersion, ReasonTooOld, ver)
	default:
		version = pass(CheckBricksVersion, ver)
	}

	sw := e.switchCheck(CheckBricksAbilities, ReasonNeedsBricks, version, api, f.BricksAbilities)
	g.Checks = []Check{version, sw}
	return g
}
