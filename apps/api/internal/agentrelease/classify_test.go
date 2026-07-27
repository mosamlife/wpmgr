package agentrelease

import (
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentplugin"
)

// TestClassify covers the version comparison for a site whose build is either
// unidentified or the self-hosted one, i.e. every site that CAN consume the
// release channel. The plugin-directory build is covered separately below.
func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		site   string
		latest string
		want   Status
	}{
		{"older than published is outdated", "0.61.90", "0.61.95", StatusOutdated},
		{"equal to published is current", "0.61.95", "0.61.95", StatusCurrent},
		{"newer than published is current", "0.62.0", "0.61.95", StatusCurrent},
		{"empty site version is unknown, never outdated", "", "0.61.95", StatusUnknown},
		{"empty published version is unknown, never outdated", "0.61.90", "", StatusUnknown},
		{"both empty is unknown", "", "", StatusUnknown},
		{"malformed site version is unknown, never outdated", "not-a-version", "0.61.95", StatusUnknown},
		{"malformed published version is unknown, never outdated", "0.61.90", "banana", StatusUnknown},
		{"garbage on both sides is unknown", "abc", "xyz", StatusUnknown},
		{"single numeric segment is not well-formed", "5", "0.61.95", StatusUnknown},
		{"two-segment versions compare fine", "1.0", "1.1", StatusOutdated},
		{"four-segment versions compare fine", "1.2.3.4", "1.2.3.5", StatusOutdated},
		{"whitespace-padded versions are tolerated", " 0.61.90 ", " 0.61.95 ", StatusOutdated},
		{"pre-release suffix is well-formed and orders older", "0.61.95-beta", "0.61.95", StatusOutdated},
	}
	for _, dist := range []agentplugin.Distribution{
		agentplugin.DistributionNone,
		agentplugin.DistributionSelfHosted,
	} {
		for _, tc := range cases {
			t.Run(string(dist)+"/"+tc.name, func(t *testing.T) {
				got := Classify(tc.site, tc.latest, dist)
				if got != tc.want {
					t.Errorf("Classify(%q, %q, %q) = %q; want %q", tc.site, tc.latest, dist, got, tc.want)
				}
			})
		}
	}
}

// TestClassifyDirectoryBuildIsIneligible pins the classification the
// plugin-directory build has earned: that build ships without
// includes/support/class-update-checker.php and with WPMGR_WPORG_BUILD
// defined, so it cannot self-update at any version. Calling it "outdated"
// would be a permanent false alarm, so ineligibility is decided BEFORE the
// versions are compared and holds for every version pairing, including the
// ones that would otherwise read current, outdated, or unknown.
func TestClassifyDirectoryBuildIsIneligible(t *testing.T) {
	cases := []struct{ name, site, latest string }{
		{"older than published", "0.61.90", "0.61.95"},
		{"equal to published", "0.61.95", "0.61.95"},
		{"newer than published", "0.62.0", "0.61.95"},
		{"site version never reported", "", "0.61.95"},
		{"published version unknown", "0.61.90", ""},
		{"both sides malformed", "garbage", "nonsense"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.site, tc.latest, agentplugin.DistributionDirectory)
			if got != StatusIneligible {
				t.Errorf("Classify(%q, %q, directory) = %q; want %q", tc.site, tc.latest, got, StatusIneligible)
			}
		})
	}
}

// TestClassifyIneligibleOnlyForDirectoryBuild is the other direction: a site
// that can self-update must never be parked in a status the operator cannot
// act on. Only the plugin-directory build earns StatusIneligible.
func TestClassifyIneligibleOnlyForDirectoryBuild(t *testing.T) {
	versions := []struct{ site, latest string }{
		{"", ""},
		{"0.1.0", "0.1.0"},
		{"garbage", "garbage"},
		{"0.1.0", "0.2.0"},
		{"0.2.0", "0.1.0"},
	}
	for _, dist := range []agentplugin.Distribution{
		agentplugin.DistributionNone,
		agentplugin.DistributionSelfHosted,
	} {
		for _, v := range versions {
			if got := Classify(v.site, v.latest, dist); got == StatusIneligible {
				t.Errorf("Classify(%q, %q, %q) = ineligible; only the plugin-directory build is", v.site, v.latest, dist)
			}
		}
	}
}

func TestIsWellFormedVersion(t *testing.T) {
	valid := []string{"0.61.95", "1.0", "1.2.3.4", "0.0.1", " 1.2.3 ", "1.2.3-beta1", "1.2.3+build.5"}
	for _, v := range valid {
		if !isWellFormedVersion(v) {
			t.Errorf("isWellFormedVersion(%q) = false; want true", v)
		}
	}
	invalid := []string{"", "unknown", "v1.2.3", "1", "abc.def.ghi", "1.2.3.4.5", "not-a-version"}
	for _, v := range invalid {
		if isWellFormedVersion(v) {
			t.Errorf("isWellFormedVersion(%q) = true; want false", v)
		}
	}
}
