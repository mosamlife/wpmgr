package agentcmd

import (
	"encoding/json"
	"testing"
)

// TestRollbackRequest_WireShape pins the bytes of the rollback body.
//
// apps/agent/includes/commands/class-rollback-command.php runs a core rollback
// only when allow_core_downgrade is the JSON boolean true. A string, a number
// or a missing key is a refusal (GH #415), so a tag change here that sends
// "true" or 1 would make every core rollback a refusal without either side's
// own tests noticing. A plugin or theme body must not carry the key at all.
func TestRollbackRequest_WireShape(t *testing.T) {
	cases := []struct {
		name string
		req  RollbackRequest
		want string
	}{
		{
			name: "core rollback carries the JSON boolean true",
			req:  RollbackRequest{Type: "core", Slug: CoreSlug, ToVersion: "7.0", AllowCoreDowngrade: true},
			want: `{"type":"core","slug":"core","to_version":"7.0","allow_core_downgrade":true}`,
		},
		{
			name: "plugin rollback omits the key",
			req:  RollbackRequest{Type: "plugin", Slug: "suremail", SnapshotID: "snap-1", ToVersion: "1.9.9"},
			want: `{"type":"plugin","slug":"suremail","snapshot_id":"snap-1","to_version":"1.9.9"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.req)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(raw) != tc.want {
				t.Errorf("rollback request body = %s, want %s", raw, tc.want)
			}
		})
	}
}
