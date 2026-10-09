// Package aireadiness computes whether an AI assistant connected to WPMgr can
// work on a site, from what the site last reported.
//
// The result is ADVICE FOR A PERSON. Nothing is allowed or refused because of
// it: no tool call, approval, dispatch or agent command reads it, and
// advisory_test.go fails the build if any package outside the wiring layer
// imports this one.
//
// Every value in a Result is either a constant of ours or a version string
// that passed a strict shape check. No label, description or other text a
// site chose is read, stored in a Facts value or returned.
package aireadiness

import (
	"time"

	"github.com/google/uuid"
)

// Status is the roll-up of a Result.
type Status string

// Statuses.
const (
	StatusReady          Status = "ready"
	StatusNeedsAttention Status = "needs_attention"
	StatusIncomplete     Status = "incomplete"
)

// State is the outcome of one check.
type State string

// States. Unknown is never a failure: it means WPMgr could not tell.
const (
	StatePass          State = "pass"
	StateFail          State = "fail"
	StateUnknown       State = "unknown"
	StateNotApplicable State = "not_applicable"
)

// CheckID names one row. The set is closed.
type CheckID string

// Check ids.
const (
	CheckWPVersion        CheckID = "wp_version"
	CheckAbilitiesAPI     CheckID = "abilities_api"
	CheckAgentVersion     CheckID = "agent_version"
	CheckContentEditing   CheckID = "content_editing"
	CheckElementorVersion CheckID = "elementor_version"
	CheckElementorSwitch  CheckID = "elementor_mcp_switch"
	CheckElementorAtomic  CheckID = "elementor_atomic"
	CheckBricksVersion    CheckID = "bricks_version"
	CheckBricksAbilities  CheckID = "bricks_abilities"
)

// Reason says why a check is unknown or not applicable, or (for the version
// checks) why one failed. The set is closed. The empty Reason means none.
// ReasonInactive is carried by a not-applicable version row: a builder that is
// installed but not in use is not a fix.
type Reason string

// Reasons.
const (
	ReasonNone               Reason = ""
	ReasonNotReported        Reason = "not_reported"
	ReasonInventoryNeverRun  Reason = "inventory_never_run"
	ReasonInventoryTruncated Reason = "inventory_truncated"
	ReasonAgentTooOld        Reason = "agent_too_old"
	ReasonAgentTooOldForFact Reason = "agent_too_old_for_fact"
	ReasonNeedsAbilities     Reason = "needs_abilities"
	ReasonNeedsElementor     Reason = "needs_elementor"
	ReasonNeedsBricks        Reason = "needs_bricks"
	ReasonInactive           Reason = "inactive"
	ReasonTooOld             Reason = "too_old"
	// ReasonPrereleaseBuild is the WordPress row's reason when the site runs a
	// development or pre-release build whose release number reaches the floor.
	// WPMgr's AI tools run on a released WordPress only.
	ReasonPrereleaseBuild Reason = "prerelease_build"
)

// GroupID names a group of checks.
type GroupID string

// Group ids, in the order a Result lists them.
const (
	GroupBase      GroupID = "base"
	GroupElementor GroupID = "elementor"
	GroupBricks    GroupID = "bricks"
)

// WPMgrSupport says whether WPMgr can build pages with a builder yet.
type WPMgrSupport string

// Support levels. Every builder is "coming" until its builder slice ships;
// the checks then show whether the site WILL be ready.
const (
	SupportComing    WPMgrSupport = "coming"
	SupportAvailable WPMgrSupport = "available"
)

// WarningCode names an advisory notice. The set is closed. A warning never
// changes Status or FixCount.
type WarningCode string

// Warning codes.
const (
	// WarnMCPAdapterPluginActive: the WordPress MCP Adapter plugin is active,
	// which lets any logged-in user of the site connect an AI tool directly.
	WarnMCPAdapterPluginActive WarningCode = "mcp_adapter_plugin_active"
	// WarnElementorEndpointOpen: Elementor's AI tools switch is on, which also
	// opens Elementor's own AI connection point on the site.
	WarnElementorEndpointOpen WarningCode = "elementor_mcp_endpoint_open"
)

// Check is one row of the checklist.
type Check struct {
	ID    CheckID
	State State
	// Reason is ReasonNone for a pass, and for a fail with a single way to
	// fail. See the OpenAPI description of AIReadinessCheck for the reasons
	// each row can carry.
	Reason Reason
	// Observed is the version that was compared, or "" when there is none. It
	// has passed a strict shape check; it is never free text.
	Observed string
}

// Group is a set of checks. Builder groups also say whether the builder is
// installed; a group whose builder is not installed has no checks and
// contributes nothing to Status.
type Group struct {
	ID GroupID
	// Installed is meaningful for builder groups only.
	Installed bool
	// Version is the validated installed version, or "".
	Version string
	// Support is empty for the base group.
	Support WPMgrSupport
	Checks  []Check
}

// Floors are the versions the checks compare against. DefaultFloors derives
// them from the contract constants so a number cannot drift from the one the
// ability engine enforces.
type Floors struct {
	// WP is the WordPress floor for builder tools.
	WP string
	// Agent is the WPMgr agent floor for builder tools.
	Agent string
	// FactsAgent is the first agent that reports builder_facts.
	FactsAgent string
	// Elementor and Bricks are the first builder versions with AI tools.
	Elementor string
	Bricks    string

	// EngineAgent is the first agent that can read a site's tool list. It is
	// used to decide whether a refresh can ask for one; it is not part of the
	// response.
	EngineAgent string
	// AbilitiesAPIWP is the first WordPress release that ships the Abilities
	// API in core. It is not part of the response.
	AbilitiesAPIWP string
}

// BuilderFacts is what the site's agent reported in builder_facts. The zero
// value means the agent reported none (an agent too old, or a collection that
// failed): "not reported", never "off".
type BuilderFacts struct {
	// Reported is true when the stored inventory holds a builder_facts object.
	Reported bool
	// ThemeTemplate is the parent theme directory, validated; "" when absent
	// or unusable.
	ThemeTemplate string
	// Elementor is nil when the agent sent no elementor member.
	Elementor *ElementorFacts
}

// ElementorFacts is the elementor member of BuilderFacts.
type ElementorFacts struct {
	// AtomicEditor is nil when Elementor is loaded but gave no answer.
	AtomicEditor *bool
}

// Facts is everything Evaluate reads about one site. The version fields are
// RAW site-reported text: Evaluate validates them and never returns one that
// failed.
type Facts struct {
	SiteID uuid.UUID

	WPVersion    string
	AgentVersion string
	// MetadataAsOf is when the site last reported its plugin and theme lists.
	MetadataAsOf          *time.Time
	ContentEditingEnabled bool

	ElementorInstalled bool
	ElementorVersion   string
	ElementorActive    bool
	// MCPAdapterActive is true when a plugin in the mcp-adapter directory is
	// active.
	MCPAdapterActive bool
	BricksInstalled  bool
	BricksVersion    string
	BricksActive     bool

	BuilderFacts BuilderFacts

	// InventoryChecked is true when the site's ability inventory has run at
	// least once. AbilitiesAsOf is when.
	InventoryChecked    bool
	AbilitiesAsOf       *time.Time
	AbilitiesAPIPresent bool
	AbilitiesTruncated  bool
	// ElementorAbilities and BricksAbilities count inventoried abilities whose
	// registering owner is that builder itself (never merely the namespace).
	ElementorAbilities int64
	BricksAbilities    int64
}

// Result is the checklist for one site.
type Result struct {
	SiteID        uuid.UUID
	Status        Status
	FixCount      int
	MetadataAsOf  *time.Time
	AbilitiesAsOf *time.Time
	Warnings      []WarningCode
	Floors        Floors
	// Groups is always base, elementor, bricks, in that order.
	Groups []Group
}

// Failing lists the ids of the checks that count toward FixCount, in group
// order: those whose state is fail, less the unconfirmed rows, which stay in
// Groups with their state but are not fixes. For a Result from EvaluateWith it
// has FixCount entries.
func (r Result) Failing() []CheckID {
	out := make([]CheckID, 0, r.FixCount)
	for _, g := range r.Groups {
		for _, c := range g.Checks {
			if c.State == StateFail && !c.unconfirmed() {
				out = append(out, c.ID)
			}
		}
	}
	return out
}
