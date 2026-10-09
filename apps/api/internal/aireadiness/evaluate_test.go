package aireadiness

import (
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/wpversion"
)

func tp(b bool) *bool { return &b }

// readyFacts is a site with nothing wrong and no builder installed.
func readyFacts() Facts {
	fl := DefaultFloors()
	return Facts{
		SiteID:                uuid.New(),
		WPVersion:             fl.WP,
		AgentVersion:          fl.Agent,
		ContentEditingEnabled: true,
		InventoryChecked:      true,
		AbilitiesAPIPresent:   true,
		BuilderFacts:          BuilderFacts{Reported: true},
	}
}

// withElementor installs a healthy Elementor with the AI switch on and the
// Atomic editor on.
func withElementor(f Facts) Facts {
	f.ElementorInstalled = true
	f.ElementorVersion = "4.3.4"
	f.ElementorActive = true
	f.ElementorAbilities = 3
	f.BuilderFacts.Elementor = &ElementorFacts{AtomicEditor: tp(true)}
	return f
}

// withBricks installs a healthy Bricks as the active theme with the AI switch
// on.
func withBricks(f Facts) Facts {
	f.BricksInstalled = true
	f.BricksVersion = "2.4.1"
	f.BricksActive = true
	f.BricksAbilities = 2
	return f
}

func find(t *testing.T, r Result, id CheckID) Check {
	t.Helper()
	for _, g := range r.Groups {
		for _, c := range g.Checks {
			if c.ID == id {
				return c
			}
		}
	}
	t.Fatalf("check %s is not in the result", id)
	return Check{}
}

func group(t *testing.T, r Result, id GroupID) Group {
	t.Helper()
	for _, g := range r.Groups {
		if g.ID == id {
			return g
		}
	}
	t.Fatalf("group %s is not in the result", id)
	return Group{}
}

func expect(t *testing.T, c Check, state State, reason Reason, observed string) {
	t.Helper()
	if c.State != state || c.Reason != reason || c.Observed != observed {
		t.Fatalf("%s = {state %q reason %q observed %q}, want {state %q reason %q observed %q}",
			c.ID, c.State, c.Reason, c.Observed, state, reason, observed)
	}
}

func TestEvaluateReadyBaseSite(t *testing.T) {
	r := Evaluate(readyFacts())
	if r.Status != StatusReady || r.FixCount != 0 {
		t.Fatalf("status %q fix_count %d, want ready 0: %+v", r.Status, r.FixCount, r)
	}
	if len(r.Groups) != 3 || r.Groups[0].ID != GroupBase || r.Groups[1].ID != GroupElementor || r.Groups[2].ID != GroupBricks {
		t.Fatalf("groups must be base, elementor, bricks in that order: %+v", r.Groups)
	}
	if r.Warnings == nil || len(r.Warnings) != 0 {
		t.Fatalf("warnings must be an empty, non-nil list: %#v", r.Warnings)
	}
	if len(r.Failing()) != 0 {
		t.Fatalf("a ready site has nothing failing: %v", r.Failing())
	}
	ids := []CheckID{CheckWPVersion, CheckAbilitiesAPI, CheckAgentVersion, CheckContentEditing}
	base := group(t, r, GroupBase)
	if len(base.Checks) != len(ids) {
		t.Fatalf("base group checks: %+v", base.Checks)
	}
	for i, id := range ids {
		if base.Checks[i].ID != id {
			t.Fatalf("base check %d = %s, want %s", i, base.Checks[i].ID, id)
		}
	}
}

// The floors the checklist compares against come from the constants the rest
// of the control plane enforces, never from a literal of their own.
func TestDefaultFloorsComeFromTheContractConstants(t *testing.T) {
	fl := DefaultFloors()
	wantAgent := agentcmd.MinAgentVersionForRestCall
	if wpversion.Compare(agentcmd.MinAgentVersionForVendorReads, wantAgent) > 0 {
		wantAgent = agentcmd.MinAgentVersionForVendorReads
	}
	if fl.Agent != wantAgent {
		t.Fatalf("agent floor = %q, want the greater of MinAgentVersionForRestCall and MinAgentVersionForVendorReads (%q)", fl.Agent, wantAgent)
	}
	if fl.WP != agentcmd.MinWPVersionForVendorReads {
		t.Fatalf("WP floor = %q, want %q", fl.WP, agentcmd.MinWPVersionForVendorReads)
	}
	if fl.FactsAgent != agentcmd.MinAgentVersionForBuilderFacts {
		t.Fatalf("facts floor = %q, want %q", fl.FactsAgent, agentcmd.MinAgentVersionForBuilderFacts)
	}
	if fl.EngineAgent != agentcmd.MinAgentVersionForAbilityEngine {
		t.Fatalf("engine floor = %q, want %q", fl.EngineAgent, agentcmd.MinAgentVersionForAbilityEngine)
	}
	if got := Evaluate(readyFacts()).Floors; got != fl {
		t.Fatalf("the result must report the floors it compared against: %+v vs %+v", got, fl)
	}
}

func TestWPVersionCheck(t *testing.T) {
	cases := []struct {
		in       string
		state    State
		reason   Reason
		observed string
	}{
		{"7.1", StatePass, ReasonNone, "7.1"},
		{"7.1.0", StatePass, ReasonNone, "7.1.0"},
		{"7.1.1", StatePass, ReasonNone, "7.1.1"},
		{"7.2", StatePass, ReasonNone, "7.2"},
		{"10.0", StatePass, ReasonNone, "10.0"},
		{" 7.1 ", StatePass, ReasonNone, "7.1"},
		{"7.0.9", StateFail, ReasonNone, "7.0.9"},
		{"6.9", StateFail, ReasonNone, "6.9"},
		{"7.1-RC1", StateFail, ReasonNone, "7.1-RC1"},
		{"7.1-beta2", StateFail, ReasonNone, "7.1-beta2"},
		{"7.1-alpha-59000", StateFail, ReasonNone, "7.1-alpha-59000"},
		{"", StateUnknown, ReasonNotReported, ""},
		{"   ", StateUnknown, ReasonNotReported, ""},
		{"banana", StateUnknown, ReasonNotReported, ""},
		{"7", StateUnknown, ReasonNotReported, ""},
		{"7.1\n<script>", StateUnknown, ReasonNotReported, ""},
		{"7.1; drop table", StateUnknown, ReasonNotReported, ""},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			f := readyFacts()
			f.WPVersion = c.in
			expect(t, find(t, Evaluate(f), CheckWPVersion), c.state, c.reason, c.observed)
		})
	}
}

func TestWPVersionFloorBoundaryUsesTheGivenFloor(t *testing.T) {
	fl := DefaultFloors()
	fl.WP = "9.9"
	f := readyFacts()
	f.WPVersion = "9.9"
	expect(t, find(t, EvaluateWith(f, fl), CheckWPVersion), StatePass, ReasonNone, "9.9")
	f.WPVersion = "9.8.9"
	expect(t, find(t, EvaluateWith(f, fl), CheckWPVersion), StateFail, ReasonNone, "9.8.9")
}

func TestAgentVersionCheck(t *testing.T) {
	fl := DefaultFloors()
	cases := []struct {
		in       string
		state    State
		observed string
	}{
		{fl.Agent, StatePass, fl.Agent},
		{"0.99.0", StatePass, "0.99.0"},
		{"1.0.0", StatePass, "1.0.0"},
		{"0.61.1", StateFail, "0.61.1"},
		{"0.60.999", StateFail, "0.60.999"},
		{"", StateUnknown, ""},
		{"garbage", StateUnknown, ""},
		{fl.Agent + "-beta", StateUnknown, ""},
		{fl.Agent + "\n", StatePass, fl.Agent}, // surrounding whitespace is trimmed
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			f := readyFacts()
			f.AgentVersion = c.in
			got := find(t, Evaluate(f), CheckAgentVersion)
			reason := ReasonNone
			if c.state == StateUnknown {
				reason = ReasonNotReported
			}
			expect(t, got, c.state, reason, c.observed)
		})
	}
}

func TestAgentFloorBoundary(t *testing.T) {
	fl := DefaultFloors()
	fl.Agent = "1.2.3"
	f := readyFacts()
	f.AgentVersion = "1.2.3"
	expect(t, find(t, EvaluateWith(f, fl), CheckAgentVersion), StatePass, ReasonNone, "1.2.3")
	f.AgentVersion = "1.2.2"
	expect(t, find(t, EvaluateWith(f, fl), CheckAgentVersion), StateFail, ReasonNone, "1.2.2")
}

func TestContentEditingCheck(t *testing.T) {
	f := readyFacts()
	expect(t, find(t, Evaluate(f), CheckContentEditing), StatePass, ReasonNone, "")
	f.ContentEditingEnabled = false
	r := Evaluate(f)
	expect(t, find(t, r, CheckContentEditing), StateFail, ReasonNone, "")
	// Owner ruling: AI page creation being off counts as a fix.
	if r.Status != StatusNeedsAttention || r.FixCount != 1 {
		t.Fatalf("AI page creation off must be one thing to fix, got %q %d", r.Status, r.FixCount)
	}
}

func TestAbilitiesAPICheck(t *testing.T) {
	fl := DefaultFloors()
	cases := []struct {
		name   string
		mutate func(*Facts)
		state  State
		reason Reason
	}{
		{"a read saw the API", func(f *Facts) { f.WPVersion = "6.4"; f.AbilitiesAPIPresent = true }, StatePass, ReasonNone},
		{"WordPress 6.9 ships it, no read yet", func(f *Facts) { f.InventoryChecked = false; f.AbilitiesAPIPresent = false; f.WPVersion = "6.9" }, StatePass, ReasonNone},
		{"WordPress 7.1 ships it even if a read said no", func(f *Facts) { f.AbilitiesAPIPresent = false }, StatePass, ReasonNone},
		{"a read saw no API on WordPress 6.4", func(f *Facts) { f.WPVersion = "6.4"; f.AbilitiesAPIPresent = false }, StateFail, ReasonNone},
		{"a read saw no API on 6.8.9", func(f *Facts) { f.WPVersion = "6.8.9"; f.AbilitiesAPIPresent = false }, StateFail, ReasonNone},
		{"a read saw no API and the version is unknown", func(f *Facts) { f.WPVersion = ""; f.AbilitiesAPIPresent = false }, StateUnknown, ReasonNotReported},
		{"never read, old WordPress", func(f *Facts) { f.InventoryChecked = false; f.AbilitiesAPIPresent = false; f.WPVersion = "6.4" }, StateUnknown, ReasonInventoryNeverRun},
		{"never read, agent below the engine floor", func(f *Facts) {
			f.InventoryChecked = false
			f.AbilitiesAPIPresent = false
			f.WPVersion = "6.4"
			f.AgentVersion = "0.61.1"
		}, StateUnknown, ReasonAgentTooOld},
		{"never read, agent version unknown is not called old", func(f *Facts) {
			f.InventoryChecked = false
			f.AbilitiesAPIPresent = false
			f.WPVersion = "6.4"
			f.AgentVersion = ""
		}, StateUnknown, ReasonInventoryNeverRun},
		{"never read, agent at the engine floor", func(f *Facts) {
			f.InventoryChecked = false
			f.AbilitiesAPIPresent = false
			f.WPVersion = "6.4"
			f.AgentVersion = fl.EngineAgent
		}, StateUnknown, ReasonInventoryNeverRun},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := readyFacts()
			c.mutate(&f)
			expect(t, find(t, Evaluate(f), CheckAbilitiesAPI), c.state, c.reason, "")
		})
	}
}

// ---- Elementor -----------------------------------------------------------

func TestElementorNotInstalledContributesNothing(t *testing.T) {
	f := readyFacts()
	f.ElementorInstalled = false
	// Values the group would fail on if it were evaluated.
	f.ElementorVersion = "1.0"
	f.ElementorActive = false
	f.BuilderFacts.Elementor = &ElementorFacts{AtomicEditor: tp(false)}
	f.ElementorAbilities = 0
	r := Evaluate(f)
	g := group(t, r, GroupElementor)
	if g.Installed || len(g.Checks) != 0 || g.Version != "" {
		t.Fatalf("an uninstalled builder has no checks: %+v", g)
	}
	if g.Support != SupportComing {
		t.Fatalf("support = %q, want coming", g.Support)
	}
	if r.Status != StatusReady || r.FixCount != 0 {
		t.Fatalf("a site without Elementor must not be red for it: %q %d", r.Status, r.FixCount)
	}
}

func TestElementorVersionCheck(t *testing.T) {
	cases := []struct {
		name     string
		version  string
		active   bool
		state    State
		reason   Reason
		observed string
	}{
		{"4.3.4 active", "4.3.4", true, StatePass, ReasonNone, "4.3.4"},
		{"4.3 active is the floor", "4.3", true, StatePass, ReasonNone, "4.3"},
		{"4.3.0 active", "4.3.0", true, StatePass, ReasonNone, "4.3.0"},
		{"5.0 active", "5.0", true, StatePass, ReasonNone, "5.0"},
		{"4.2.9 active is too old", "4.2.9", true, StateFail, ReasonTooOld, "4.2.9"},
		{"4.3.0-beta1 active is before 4.3", "4.3.0-beta1", true, StateFail, ReasonTooOld, "4.3.0-beta1"},
		{"installed but inactive", "4.3.4", false, StateFail, ReasonInactive, "4.3.4"},
		{"inactive wins over too old", "3.0", false, StateFail, ReasonInactive, "3.0"},
		{"no version reported is unknown", "", true, StateUnknown, ReasonNotReported, ""},
		{"a version of the wrong shape is unknown", "4.3 <b>", true, StateUnknown, ReasonNotReported, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := withElementor(readyFacts())
			f.ElementorVersion = c.version
			f.ElementorActive = c.active
			expect(t, find(t, Evaluate(f), CheckElementorVersion), c.state, c.reason, c.observed)
		})
	}
}

func TestElementorGroupReportsItsValidatedVersion(t *testing.T) {
	f := withElementor(readyFacts())
	g := group(t, Evaluate(f), GroupElementor)
	if !g.Installed || g.Version != "4.3.4" || g.Support != SupportComing || len(g.Checks) != 3 {
		t.Fatalf("elementor group = %+v", g)
	}
	f.ElementorVersion = "<script>"
	if got := group(t, Evaluate(f), GroupElementor).Version; got != "" {
		t.Fatalf("an unusable version must not be reported, got %q", got)
	}
}

func TestElementorSwitchCheck(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Facts)
		state  State
		reason Reason
	}{
		{"an ability the builder registered", func(f *Facts) {}, StatePass, ReasonNone},
		{"one is enough", func(f *Facts) { f.ElementorAbilities = 1 }, StatePass, ReasonNone},
		{"none on a complete list", func(f *Facts) { f.ElementorAbilities = 0 }, StateFail, ReasonNone},
		{"none on a list that was cut short is unknown", func(f *Facts) { f.ElementorAbilities = 0; f.AbilitiesTruncated = true }, StateUnknown, ReasonInventoryTruncated},
		{"some on a list that was cut short still passes", func(f *Facts) { f.AbilitiesTruncated = true }, StatePass, ReasonNone},
		{"never read is unknown, not failing", func(f *Facts) { f.InventoryChecked = false; f.ElementorAbilities = 0 }, StateUnknown, ReasonInventoryNeverRun},
		{"no WordPress abilities", func(f *Facts) {
			f.WPVersion = "6.4"
			f.AbilitiesAPIPresent = false
			f.ElementorAbilities = 0
		}, StateNotApplicable, ReasonNeedsAbilities},
		{"Elementor too old", func(f *Facts) { f.ElementorVersion = "4.2.0" }, StateNotApplicable, ReasonNeedsElementor},
		{"Elementor inactive", func(f *Facts) { f.ElementorActive = false }, StateNotApplicable, ReasonNeedsElementor},
		{"Elementor version unusable", func(f *Facts) { f.ElementorVersion = "" }, StateUnknown, ReasonNeedsElementor},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := withElementor(readyFacts())
			c.mutate(&f)
			expect(t, find(t, Evaluate(f), CheckElementorSwitch), c.state, c.reason, "")
		})
	}
}

func TestElementorAtomicCheck(t *testing.T) {
	fl := DefaultFloors()
	cases := []struct {
		name   string
		mutate func(*Facts)
		state  State
		reason Reason
	}{
		{"on", func(f *Facts) {}, StatePass, ReasonNone},
		{"off", func(f *Facts) { f.BuilderFacts.Elementor = &ElementorFacts{AtomicEditor: tp(false)} }, StateFail, ReasonNone},
		{"loaded but no answer", func(f *Facts) { f.BuilderFacts.Elementor = &ElementorFacts{} }, StateUnknown, ReasonNotReported},
		{"facts reported without an elementor member", func(f *Facts) { f.BuilderFacts.Elementor = nil }, StateUnknown, ReasonNotReported},
		{"facts never reported, agent below the facts floor", func(f *Facts) {
			f.BuilderFacts = BuilderFacts{}
			f.AgentVersion = "0.61.1"
		}, StateUnknown, ReasonAgentTooOldForFact},
		{"facts never reported, agent at the facts floor", func(f *Facts) {
			f.BuilderFacts = BuilderFacts{}
			f.AgentVersion = fl.FactsAgent
		}, StateUnknown, ReasonNotReported},
		{"facts never reported, agent version unknown is not called old", func(f *Facts) {
			f.BuilderFacts = BuilderFacts{}
			f.AgentVersion = ""
		}, StateUnknown, ReasonNotReported},
		{"Elementor too old", func(f *Facts) { f.ElementorVersion = "4.0.0" }, StateNotApplicable, ReasonNeedsElementor},
		{"Elementor inactive", func(f *Facts) { f.ElementorActive = false }, StateNotApplicable, ReasonNeedsElementor},
		{"Elementor version unusable", func(f *Facts) { f.ElementorVersion = "" }, StateUnknown, ReasonNeedsElementor},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := withElementor(readyFacts())
			c.mutate(&f)
			expect(t, find(t, Evaluate(f), CheckElementorAtomic), c.state, c.reason, "")
		})
	}
}

// ---- Bricks --------------------------------------------------------------

func TestBricksNotInstalledContributesNothing(t *testing.T) {
	f := readyFacts()
	f.BricksInstalled = false
	f.BricksActive = false
	f.BricksVersion = "1.0"
	r := Evaluate(f)
	g := group(t, r, GroupBricks)
	if g.Installed || len(g.Checks) != 0 {
		t.Fatalf("an uninstalled builder has no checks: %+v", g)
	}
	if r.Status != StatusReady {
		t.Fatalf("status = %q", r.Status)
	}
}

func TestBricksVersionCheck(t *testing.T) {
	fl := DefaultFloors()
	cases := []struct {
		name     string
		mutate   func(*Facts)
		state    State
		reason   Reason
		observed string
	}{
		{"active theme at 2.4.1", func(f *Facts) {}, StatePass, ReasonNone, "2.4.1"},
		{"the floor itself", func(f *Facts) { f.BricksVersion = "2.4" }, StatePass, ReasonNone, "2.4"},
		{"too old", func(f *Facts) { f.BricksVersion = "2.3.9" }, StateFail, ReasonTooOld, "2.3.9"},
		{"a child theme: the reported parent is Bricks", func(f *Facts) {
			f.BricksActive = false
			f.BricksAbilities = 0
			f.BuilderFacts.ThemeTemplate = "bricks"
		}, StatePass, ReasonNone, "2.4.1"},
		{"abilities registered by Bricks mean it is loaded", func(f *Facts) {
			f.BricksActive = false
			f.BuilderFacts = BuilderFacts{}
			f.BricksAbilities = 4
		}, StatePass, ReasonNone, "2.4.1"},
		{"installed, another theme is the parent", func(f *Facts) {
			f.BricksActive = false
			f.BricksAbilities = 0
			f.BuilderFacts.ThemeTemplate = "twentytwentyfive"
		}, StateFail, ReasonInactive, "2.4.1"},
		{"installed and inactive, parent not reported, current agent", func(f *Facts) {
			f.BricksActive = false
			f.BricksAbilities = 0
			f.BuilderFacts = BuilderFacts{Reported: true}
		}, StateUnknown, ReasonNotReported, "2.4.1"},
		{"installed and inactive, facts never reported, current agent", func(f *Facts) {
			f.BricksActive = false
			f.BricksAbilities = 0
			f.BuilderFacts = BuilderFacts{}
			f.AgentVersion = fl.FactsAgent
		}, StateUnknown, ReasonNotReported, "2.4.1"},
		{"installed and inactive, facts never reported, old agent", func(f *Facts) {
			f.BricksActive = false
			f.BricksAbilities = 0
			f.BuilderFacts = BuilderFacts{}
			f.AgentVersion = "0.61.1"
		}, StateUnknown, ReasonAgentTooOldForFact, "2.4.1"},
		{"active but no usable version", func(f *Facts) { f.BricksVersion = "" }, StateUnknown, ReasonNotReported, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := withBricks(readyFacts())
			c.mutate(&f)
			expect(t, find(t, Evaluate(f), CheckBricksVersion), c.state, c.reason, c.observed)
		})
	}
}

func TestBricksAbilitiesCheck(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Facts)
		state  State
		reason Reason
	}{
		{"an ability the theme registered", func(f *Facts) {}, StatePass, ReasonNone},
		{"none on a complete list", func(f *Facts) { f.BricksAbilities = 0 }, StateFail, ReasonNone},
		{"none on a list cut short", func(f *Facts) { f.BricksAbilities = 0; f.AbilitiesTruncated = true }, StateUnknown, ReasonInventoryTruncated},
		{"never read", func(f *Facts) { f.InventoryChecked = false; f.BricksAbilities = 0 }, StateUnknown, ReasonInventoryNeverRun},
		{"no WordPress abilities", func(f *Facts) {
			f.WPVersion = "6.4"
			f.AbilitiesAPIPresent = false
			f.BricksAbilities = 0
		}, StateNotApplicable, ReasonNeedsAbilities},
		{"Bricks too old", func(f *Facts) { f.BricksVersion = "2.0" }, StateNotApplicable, ReasonNeedsBricks},
		{"Bricks not the active theme", func(f *Facts) {
			f.BricksActive = false
			f.BricksAbilities = 0
			f.BuilderFacts.ThemeTemplate = "other"
		}, StateNotApplicable, ReasonNeedsBricks},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := withBricks(readyFacts())
			c.mutate(&f)
			expect(t, find(t, Evaluate(f), CheckBricksAbilities), c.state, c.reason, "")
		})
	}
}

// ---- status, fix count, warnings ------------------------------------------

func TestStatusRollup(t *testing.T) {
	t.Run("a failure beats an unknown", func(t *testing.T) {
		f := readyFacts()
		f.WPVersion = "6.0" // fail
		f.AgentVersion = "" // unknown
		f.ContentEditingEnabled = true
		r := Evaluate(f)
		if r.Status != StatusNeedsAttention || r.FixCount != 1 {
			t.Fatalf("got %q %d", r.Status, r.FixCount)
		}
	})
	t.Run("only unknowns is incomplete, never needs_attention", func(t *testing.T) {
		f := readyFacts()
		f.AgentVersion = ""
		f.InventoryChecked = false
		f.WPVersion = ""
		r := Evaluate(f)
		if r.Status != StatusIncomplete || r.FixCount != 0 {
			t.Fatalf("got %q %d: %+v", r.Status, r.FixCount, r.Groups)
		}
	})
	t.Run("fix_count counts every failing row in base and installed builders", func(t *testing.T) {
		f := withBricks(withElementor(readyFacts()))
		f.WPVersion = "6.0"                                                 // base fail
		f.ContentEditingEnabled = false                                     // base fail
		f.BuilderFacts.Elementor = &ElementorFacts{AtomicEditor: tp(false)} // elementor_atomic fail
		f.ElementorAbilities = 0                                            // elementor_mcp_switch fail
		f.BricksAbilities = 0                                               // bricks_abilities fail
		r := Evaluate(f)
		want := []CheckID{CheckWPVersion, CheckContentEditing, CheckElementorSwitch, CheckElementorAtomic, CheckBricksAbilities}
		got := r.Failing()
		if r.FixCount != len(want) || len(got) != len(want) {
			t.Fatalf("fix_count %d failing %v, want %v", r.FixCount, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("failing[%d] = %s, want %s (all: %v)", i, got[i], want[i], got)
			}
		}
	})
	t.Run("not_applicable rows are not fixes", func(t *testing.T) {
		f := withElementor(readyFacts())
		f.ElementorVersion = "4.0.0" // elementor_version fails; switch and atomic become n/a
		r := Evaluate(f)
		if r.FixCount != 1 {
			t.Fatalf("one failing row expected, got %d: %v", r.FixCount, r.Failing())
		}
	})
	t.Run("everything in place with both builders is ready", func(t *testing.T) {
		r := Evaluate(withBricks(withElementor(readyFacts())))
		if r.Status != StatusReady || r.FixCount != 0 {
			t.Fatalf("got %q %d: %+v", r.Status, r.FixCount, r.Groups)
		}
	})
}

func TestWarnings(t *testing.T) {
	t.Run("an active MCP Adapter warns and does not change the status", func(t *testing.T) {
		f := readyFacts()
		f.MCPAdapterActive = true
		r := Evaluate(f)
		if len(r.Warnings) != 1 || r.Warnings[0] != WarnMCPAdapterPluginActive {
			t.Fatalf("warnings = %v", r.Warnings)
		}
		if r.Status != StatusReady || r.FixCount != 0 {
			t.Fatalf("a warning must not change status or fix_count: %q %d", r.Status, r.FixCount)
		}
	})
	t.Run("an inactive MCP Adapter does not warn", func(t *testing.T) {
		f := readyFacts()
		f.MCPAdapterActive = false
		if w := Evaluate(f).Warnings; len(w) != 0 {
			t.Fatalf("warnings = %v", w)
		}
	})
	t.Run("Elementor's switch being on warns", func(t *testing.T) {
		r := Evaluate(withElementor(readyFacts()))
		if len(r.Warnings) != 1 || r.Warnings[0] != WarnElementorEndpointOpen {
			t.Fatalf("warnings = %v", r.Warnings)
		}
		if r.Status != StatusReady {
			t.Fatalf("status = %q", r.Status)
		}
	})
	t.Run("Elementor's switch being off does not warn", func(t *testing.T) {
		f := withElementor(readyFacts())
		f.ElementorAbilities = 0
		for _, w := range Evaluate(f).Warnings {
			if w == WarnElementorEndpointOpen {
				t.Fatalf("the switch is off; no endpoint warning expected")
			}
		}
	})
	t.Run("an unknown switch does not warn", func(t *testing.T) {
		f := withElementor(readyFacts())
		f.InventoryChecked = false
		f.ElementorAbilities = 0
		if w := Evaluate(f).Warnings; len(w) != 0 {
			t.Fatalf("an unknown must not be reported as open: %v", w)
		}
	})
	t.Run("both warnings, in a stable order", func(t *testing.T) {
		f := withElementor(readyFacts())
		f.MCPAdapterActive = true
		w := Evaluate(f).Warnings
		if len(w) != 2 || w[0] != WarnMCPAdapterPluginActive || w[1] != WarnElementorEndpointOpen {
			t.Fatalf("warnings = %v", w)
		}
	})
}

// A site that has never reported anything is incomplete, not broken, apart
// from the one row that is a plain fact about WPMgr (AI page creation is off).
func TestNeverReportedSite(t *testing.T) {
	r := Evaluate(Facts{SiteID: uuid.New()})
	if r.MetadataAsOf != nil || r.AbilitiesAsOf != nil {
		t.Fatalf("freshness must be null: %+v %+v", r.MetadataAsOf, r.AbilitiesAsOf)
	}
	for _, id := range []CheckID{CheckWPVersion, CheckAgentVersion, CheckAbilitiesAPI} {
		if c := find(t, r, id); c.State != StateUnknown {
			t.Fatalf("%s = %q, an unreported fact must be unknown", id, c.State)
		}
	}
	if r.Status != StatusNeedsAttention || r.FixCount != 1 || r.Failing()[0] != CheckContentEditing {
		t.Fatalf("only AI page creation can be a fix on a silent site: %q %d %v", r.Status, r.FixCount, r.Failing())
	}
}
