package agentplugin

import "testing"

// TestIs pins both directions of the slug-only matcher. Matching too little
// would let an agent self-update advisory reach an operator; matching too much
// would hide a legitimate third-party plugin's update behind a name that merely
// looks like the agent's.
func TestIs(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		// The keys WordPress actually reports for the two distributions.
		{"wpmgr-agent/wpmgr-agent.php", true},
		{"fleet-agent-site-manager/fleet-agent-site-manager.php", true},
		// Bare directory, single file, stray leading slash, padding, case.
		{"wpmgr-agent", true},
		{"wpmgr-agent.php", true},
		{"/wpmgr-agent/wpmgr-agent.php", true},
		{"  wpmgr-agent/wpmgr-agent.php  ", true},
		{"WPMGR-Agent/WPMGR-Agent.PHP", true},
		{"fleet-agent-site-manager", true},
		// A renamed main file inside the agent's own directory still matches:
		// the directory is what identifies the plugin.
		{"wpmgr-agent/loader.php", true},
		// Everything else, including names that merely start or end with the
		// agent's slug, must not match.
		{"", false},
		{"akismet/akismet.php", false},
		{"wpmgr-agent-extras/wpmgr-agent-extras.php", false},
		{"my-wpmgr-agent/my-wpmgr-agent.php", false},
		{"agent/wpmgr-agent.php", false},
		{"twentytwentyfour", false},
		// The gap Is cannot close, and the reason IsComponent exists: a
		// directory name nobody predicted carries no usable signal at all.
		{"wpmgr-agent-0.61.88/wpmgr-agent.php", false},
	}
	for _, tc := range cases {
		if got := Is(tc.key); got != tc.want {
			t.Errorf("Is(%q) = %v, want %v", tc.key, got, tc.want)
		}
	}
}

// TestIsComponentMatchesRenamedDirectory is the fleet-outage case. Until every
// site has self-updated past the release that adds the agent's own refusal,
// this matcher is the only thing standing between a renamed agent directory and
// an operator being offered an in-process self-overwrite. WordPress derives the
// inventory key from whatever folder the archive was unpacked into, so a host
// control-panel uploader, a verbatim unzip of a versioned release asset, or a
// plain operator rename all produce keys no fixed list can enumerate. The
// plugin-header name ships inside the archive and survives all of them.
func TestIsComponentMatchesRenamedDirectory(t *testing.T) {
	cases := []struct {
		name string
		key  string
		hdr  string
	}{
		{"release asset unzipped verbatim", "wpmgr-agent-0.61.88/wpmgr-agent.php", "WPMgr Agent"},
		{"directory build unzipped verbatim", "fleet-agent-site-manager-0.61.88/fleet-agent-site-manager.php", "Fleet Agent Site Manager"},
		{"operator renamed the folder", "agent/wpmgr-agent.php", "WPMgr Agent"},
		{"uploader lowercased and suffixed", "wpmgr-agent-copy-2/wpmgr-agent.php", "WPMgr Agent"},
		{"header case differs from the archive", "whatever/plugin.php", "wpmgr agent"},
		{"header padded with whitespace", "whatever/plugin.php", "  WPMgr Agent  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !IsComponent(tc.key, tc.hdr) {
				t.Errorf("IsComponent(%q, %q) = false, want true", tc.key, tc.hdr)
			}
		})
	}
}

// TestIsComponentDoesNotOverMatch is the other direction, and the more
// dangerous one to get wrong: a false positive silently withholds a real
// security update from a legitimate third-party plugin. Both halves of the
// identity match the WHOLE value, so a neighbouring name is never swept in.
func TestIsComponentDoesNotOverMatch(t *testing.T) {
	cases := []struct {
		name string
		key  string
		hdr  string
	}{
		{"an unrelated plugin", "akismet/akismet.php", "Akismet Anti-spam"},
		{"header name is a superstring", "wpmgr-agent-pro/wpmgr-agent-pro.php", "WPMgr Agent Pro"},
		{"header name is a substring", "wpmgr/wpmgr.php", "WPMgr"},
		{"header name is a prefixed superstring", "my-wpmgr-agent/my-wpmgr-agent.php", "My WPMgr Agent"},
		{"directory build name superstring", "fleet-agent-site-manager-addon/addon.php", "Fleet Agent Site Manager Addon"},
		{"agent-ish words in another order", "agent-wpmgr/agent-wpmgr.php", "Agent WPMgr"},
		{"empty header on an unrelated slug", "hello-dolly/hello.php", ""},
		{"empty everything", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if IsComponent(tc.key, tc.hdr) {
				t.Errorf("IsComponent(%q, %q) = true, want false", tc.key, tc.hdr)
			}
		})
	}
}

// TestIsComponentFallsBackToSlug covers the inventory an older agent persisted
// (or any entry whose header name did not survive) : the slug forms still
// decide, exactly as Is does.
func TestIsComponentFallsBackToSlug(t *testing.T) {
	if !IsComponent("wpmgr-agent/wpmgr-agent.php", "") {
		t.Error("an empty header name must fall back to the slug")
	}
	if !IsComponent("fleet-agent-site-manager/fleet-agent-site-manager.php", "") {
		t.Error("an empty header name must fall back to the slug")
	}
	if IsComponent("akismet/akismet.php", "") {
		t.Error("an empty header name must not turn an ordinary plugin into the agent")
	}
}

// TestDistributionOf pins which build each identity resolves to. This is the
// signal the fleet rollup classifies "ineligible" on, so a wrong answer either
// reports a plugin-directory site outdated forever against a channel it cannot
// consume, or hides a genuinely stale self-hosted agent.
func TestDistributionOf(t *testing.T) {
	cases := []struct {
		name string
		key  string
		hdr  string
		want Distribution
	}{
		{"self hosted, canonical key", "wpmgr-agent/wpmgr-agent.php", "WPMgr Agent", DistributionSelfHosted},
		{"self hosted, slug only", "wpmgr-agent/wpmgr-agent.php", "", DistributionSelfHosted},
		{"self hosted, header only", "unpredictable/x.php", "WPMgr Agent", DistributionSelfHosted},
		{"directory, canonical key", "fleet-agent-site-manager/fleet-agent-site-manager.php", "Fleet Agent Site Manager", DistributionDirectory},
		{"directory, slug only", "fleet-agent-site-manager", "", DistributionDirectory},
		{"directory, header only", "unpredictable/x.php", "Fleet Agent Site Manager", DistributionDirectory},
		{"not the agent", "akismet/akismet.php", "Akismet Anti-spam", DistributionNone},
		// The header ships inside the archive; the folder name does not. When
		// the two disagree, the archive wins, so a self-hosted zip dropped into
		// a folder named after the other build is still the self-hosted build.
		{"header wins over a conflicting slug", "fleet-agent-site-manager/x.php", "WPMgr Agent", DistributionSelfHosted},
		{"header wins the other way too", "wpmgr-agent/x.php", "Fleet Agent Site Manager", DistributionDirectory},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DistributionOf(tc.key, tc.hdr); got != tc.want {
				t.Errorf("DistributionOf(%q, %q) = %q, want %q", tc.key, tc.hdr, got, tc.want)
			}
		})
	}
}

// TestDistributionNoneIsZeroValue keeps "not the agent" and the zero value the
// same thing, so a Distribution nobody populated can never claim a build.
func TestDistributionNoneIsZeroValue(t *testing.T) {
	var zero Distribution
	if zero != DistributionNone {
		t.Fatalf("zero Distribution = %q, want %q", zero, DistributionNone)
	}
}
