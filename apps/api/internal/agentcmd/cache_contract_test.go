package agentcmd

import (
	"encoding/json"
	"testing"
)

// TestDBScanResultCategoriesEmptyTolerance is the sibling of the GH #170
// per_role regression, applied to DBScanResult.Categories: the same agent
// class of bug (PHP json_encode of an empty associative array serializing as
// `[]` instead of `{}`) could in principle hit any agent-built response map.
// Categories rarely empties in practice, but the decode must stay robust
// regardless of agent version — same defense-in-depth as perRoleMap.
func TestDBScanResultCategoriesEmptyTolerance(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		wantLen int
	}{
		{
			name:    "categories empty array (PHP empty-assoc encoding)",
			payload: `{"ok":true,"job_id":"j1","categories":[]}`,
			wantLen: 0,
		},
		{
			name:    "categories empty object",
			payload: `{"ok":true,"job_id":"j1","categories":{}}`,
			wantLen: 0,
		},
		{
			name:    "categories null",
			payload: `{"ok":true,"job_id":"j1","categories":null}`,
			wantLen: 0,
		},
		{
			name:    "categories omitted",
			payload: `{"ok":true,"job_id":"j1"}`,
			wantLen: 0,
		},
		{
			name:    "categories populated (unchanged control)",
			payload: `{"ok":true,"job_id":"j1","categories":{"revisions":{"count":4,"bytes":1024}}}`,
			wantLen: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var decoded DBScanResult
			if err := json.Unmarshal([]byte(tc.payload), &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !decoded.OK {
				t.Error("decoded ok should be true")
			}
			if got := len(decoded.Categories); got != tc.wantLen {
				t.Errorf("Categories len: want %d, got %d (%+v)", tc.wantLen, got, decoded.Categories)
			}
			if tc.wantLen == 1 {
				rev := decoded.Categories["revisions"]
				if rev.Count != 4 || rev.Bytes != 1024 {
					t.Errorf("category values mismatch: %+v", rev)
				}
			}
		})
	}
}

// TestPerRoleMapUnmarshalJSON directly exercises perRoleMap's custom decode
// (white-box; same package) for the exact set of shapes an agent can emit.
func TestPerRoleMapUnmarshalJSON(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		wantLen int
		wantErr bool
	}{
		{name: "array", payload: `[]`, wantLen: 0},
		{name: "object empty", payload: `{}`, wantLen: 0},
		{name: "null", payload: `null`, wantLen: 0},
		{name: "populated", payload: `{"administrator":{"enrolled":1,"required":2,"total":3}}`, wantLen: 1},
		{name: "malformed", payload: `"not-a-map-or-array"`, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var m perRoleMap
			err := m.UnmarshalJSON([]byte(tc.payload))
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(m) != tc.wantLen {
				t.Errorf("len: want %d, got %d (%+v)", tc.wantLen, len(m), m)
			}
		})
	}
}
