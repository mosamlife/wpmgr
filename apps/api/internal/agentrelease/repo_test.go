package agentrelease

import (
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentplugin"
)

// TestAgentDistribution covers the decode step that turns ListSitesAgentVersions'
// plugin_identities projection into the signal Classify acts on. The inputs are
// written as the raw JSON the query produces, so a change to the projection's
// shape breaks this test rather than silently classifying every site as
// DistributionNone.
func TestAgentDistribution(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want agentplugin.Distribution
	}{
		{
			name: "self hosted build among ordinary plugins",
			raw: `[{"slug":"akismet/akismet.php","name":"Akismet Anti-spam"},
			       {"slug":"wpmgr-agent/wpmgr-agent.php","name":"WPMgr Agent"}]`,
			want: agentplugin.DistributionSelfHosted,
		},
		{
			name: "plugin directory build",
			raw:  `[{"slug":"fleet-agent-site-manager/fleet-agent-site-manager.php","name":"Fleet Agent Site Manager"}]`,
			want: agentplugin.DistributionDirectory,
		},
		{
			// The whole point of matching on the plugin header: a host
			// uploader that unpacked the release asset verbatim leaves a
			// directory no slug list can predict.
			name: "plugin directory build under a versioned directory",
			raw:  `[{"slug":"fleet-agent-site-manager-0.61.88/fleet-agent-site-manager.php","name":"Fleet Agent Site Manager"}]`,
			want: agentplugin.DistributionDirectory,
		},
		{
			name: "self hosted build under a versioned directory",
			raw:  `[{"slug":"wpmgr-agent-0.61.88/wpmgr-agent.php","name":"WPMgr Agent"}]`,
			want: agentplugin.DistributionSelfHosted,
		},
		{
			name: "inventory with no agent at all",
			raw:  `[{"slug":"akismet/akismet.php","name":"Akismet Anti-spam"}]`,
			want: agentplugin.DistributionNone,
		},
		{
			name: "empty projection",
			raw:  `[]`,
			want: agentplugin.DistributionNone,
		},
		{
			// A null name is what jsonb_build_object emits for an inventory
			// entry missing its name key; the slug still has to carry it.
			name: "null name falls back to the slug",
			raw:  `[{"slug":"wpmgr-agent/wpmgr-agent.php","name":null}]`,
			want: agentplugin.DistributionSelfHosted,
		},
		{
			name: "malformed projection is not a guess",
			raw:  `{"plugins":`,
			want: agentplugin.DistributionNone,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentDistribution([]byte(tc.raw)); got != tc.want {
				t.Errorf("agentDistribution(%s) = %q; want %q", tc.raw, got, tc.want)
			}
		})
	}

	if got := agentDistribution(nil); got != agentplugin.DistributionNone {
		t.Errorf("agentDistribution(nil) = %q; want %q", got, agentplugin.DistributionNone)
	}
}
