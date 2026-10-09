package agentcmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
)

// GH #367: the agent (apps/agent/includes/commands/class-update-command.php,
// its SKIP_* constants) adds skip_reason to a skipped update row. These tests
// hold the control plane's decode of that field to the closed set, through
// json.Unmarshal into UpdateResponse, which is what Client.Update does with
// the agent's reply body.

// decodeSkipRow decodes one agent update reply whose single row carries the
// given raw JSON as its skip_reason, or no skip_reason when raw is "".
func decodeSkipRow(t *testing.T, raw string) ItemResult {
	t.Helper()
	field := ""
	if raw != "" {
		field = `,"skip_reason":` + raw
	}
	body := `{"ok":true,"results":[{"type":"core","slug":"core","from_version":"","to_version":"",` +
		`"status":"skipped","snapshot_id":"","log":"Core was not changed."` + field + `}]}`
	var resp UpdateResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("decoded %d results, want 1", len(resp.Results))
	}
	return resp.Results[0]
}

func TestItemResult_SkipReason_DecodesEachKnownValue(t *testing.T) {
	cases := []struct {
		wire string
		want SkipReason
	}{
		{`"not_installed"`, SkipNotInstalled},
		{`"self_target"`, SkipSelfTarget},
		{`"core_managed"`, SkipCoreManaged},
		{`"file_mods_disallowed"`, SkipFileModsDisallowed},
	}
	for _, tc := range cases {
		t.Run(tc.wire, func(t *testing.T) {
			if got := decodeSkipRow(t, tc.wire).SkipReason; got != tc.want {
				t.Errorf("skip_reason %s decoded as %q, want %q", tc.wire, got, tc.want)
			}
		})
	}
}

// An unknown or malformed skip_reason reads as no reason. Its text is never
// kept, and it never fails the decode of the row around it.
func TestItemResult_SkipReason_UnknownOrMalformedIsNoReason(t *testing.T) {
	cases := []struct {
		name string
		wire string
	}{
		{"absent", ""},
		{"null", `null`},
		{"empty string", `""`},
		{"a value this build does not know", `"composer_v2"`},
		{"a known value in another case", `"CORE_MANAGED"`},
		{"a known value with padding", `" core_managed"`},
		{"a number", `7`},
		{"a boolean", `true`},
		{"an object", `{"reason":"core_managed"}`},
		{"an array", `["core_managed"]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := decodeSkipRow(t, tc.wire)
			if row.SkipReason != SkipNone {
				t.Errorf("skip_reason %s decoded as %q, want no reason", tc.wire, row.SkipReason)
			}
			// The rest of the row still decodes.
			if row.Status != ItemSkipped || row.Type != TargetCore || row.Log != "Core was not changed." {
				t.Errorf("row = %+v, want the skipped core row with its log", row)
			}
		})
	}
}

// A row with no reason marshals exactly as it did before the field existed,
// so a test agent that encodes ItemResult sends the old bytes; a known reason
// is sent under its wire name.
func TestItemResult_SkipReason_WireShape(t *testing.T) {
	cases := []struct {
		name string
		row  ItemResult
		want string
	}{
		{
			name: "no reason omits the key",
			row:  ItemResult{Type: "plugin", Slug: "akismet", FromVersion: "5.0", ToVersion: "5.0", Status: ItemSkipped},
			want: `{"type":"plugin","slug":"akismet","from_version":"5.0","to_version":"5.0","status":"skipped"}`,
		},
		{
			name: "a reason is sent as skip_reason",
			row:  ItemResult{Type: "core", Slug: CoreSlug, Status: ItemSkipped, SkipReason: SkipCoreManaged},
			want: `{"type":"core","slug":"core","from_version":"","to_version":"","status":"skipped","skip_reason":"core_managed"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.row)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(raw) != tc.want {
				t.Errorf("item result = %s, want %s", raw, tc.want)
			}
		})
	}
}

// The closed set is the agent's: this reads the SKIP_* constants out of the
// agent's UpdateCommand and requires the two sets to be equal. A value the
// agent adds without a constant here would otherwise read as no reason, which
// is safe but silent; a constant here the agent never sends is dead copy.
func TestItemResult_SkipReason_MatchesTheAgentConstants(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate the repo root")
	}
	// apps/api/internal/agentcmd/<file>_test.go is four levels below the root.
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..",
		"apps", "agent", "includes", "commands", "class-update-command.php")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the agent's update command: %v", err)
	}
	re := regexp.MustCompile(`private const SKIP_[A-Z_]+\s*=\s*'([^']*)';`)
	matches := re.FindAllStringSubmatch(string(src), -1)
	if len(matches) == 0 {
		t.Fatal("no SKIP_* constants found in class-update-command.php; if they moved, point this test at their new home")
	}
	agent := map[SkipReason]bool{}
	for _, m := range matches {
		agent[SkipReason(m[1])] = true
		if got := decodeSkipRow(t, `"`+m[1]+`"`).SkipReason; string(got) != m[1] {
			t.Errorf("the agent sends skip_reason %q, which decodes here as %q; add it to SkipReason", m[1], got)
		}
	}
	for _, known := range []SkipReason{SkipNotInstalled, SkipSelfTarget, SkipCoreManaged, SkipFileModsDisallowed} {
		if !agent[known] {
			t.Errorf("SkipReason %q is not one of the agent's SKIP_* values", known)
		}
	}
}
