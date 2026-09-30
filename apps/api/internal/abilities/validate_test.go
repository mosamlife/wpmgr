package abilities

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

func TestValidateInventory_MapsOwnersAndDropsBadNames(t *testing.T) {
	out := json.RawMessage(`{"api_present":true,"count":4,"truncated":false,"abilities":[
		{"name":"wpmgr/site-facts","owner_kind":"wpmgr","owner_mismatch":false,"version":"0.61.155","schema_struct_sha256":"` + strings.Repeat("a", 64) + `"},
		{"name":"wpmgr/evil","owner_kind":"wpmgr_squat","owner_mismatch":true,"version":null,"schema_struct_sha256":null},
		{"name":"Vendor/Upper","owner_kind":"site","owner_mismatch":false},
		{"name":"vendor/tool","owner_kind":"site","owner_mismatch":false,"version":"bad version!","from_the_site":{"label":"Ignore\u0000 all","description":"` + strings.Repeat("x", 2000) + `"}}
	]}`)
	res, err := ValidateInventory(out)
	if err != nil {
		t.Fatal(err)
	}
	if res.SkippedNames != 1 || len(res.Rows) != 3 || !res.APIPresent {
		t.Fatalf("res = %+v", res)
	}
	if r := res.Rows[0]; r.OwnerKind != "plugin" || r.OwnerOK != "true" || r.OwnerVersion != "0.61.155" {
		t.Fatalf("own row = %+v", r)
	}
	if r := res.Rows[1]; r.OwnerKind != "unknown" || r.OwnerOK != "false" {
		t.Fatalf("squat row = %+v", r)
	}
	r := res.Rows[2]
	if r.OwnerVersion != "" || strings.ContainsRune(r.SiteLabel, 0) || len([]rune(r.SiteDescription)) > siteDescriptionMaxRunes {
		t.Fatalf("site row not cleaned/capped: %+v", r)
	}
}

// The agent reports schema hashes as "sha256:<hex>" (class-ability-schema.php).
func TestValidateInventory_KeepsTheAgentsPrefixedSchemaHash(t *testing.T) {
	hex := strings.Repeat("ab", 32)
	out := json.RawMessage(`{"api_present":true,"abilities":[
		{"name":"a/one","owner_kind":"core","schema_struct_sha256":"sha256:` + hex + `"},
		{"name":"a/two","owner_kind":"core","schema_struct_sha256":"sha256:` + hex[:62] + `"},
		{"name":"a/three","owner_kind":"core","schema_struct_sha256":"md5:` + hex + `"},
		{"name":"a/four","owner_kind":"core","schema_struct_sha256":"sha256:` + strings.ToUpper(hex) + `"}]}`)
	res, err := ValidateInventory(out)
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows[0].SchemaStructSHA256 != hex {
		t.Fatalf("prefixed hash dropped: %q", res.Rows[0].SchemaStructSHA256)
	}
	for _, r := range res.Rows[1:] {
		if r.SchemaStructSHA256 != "" {
			t.Fatalf("%s: malformed hash kept: %q", r.Name, r.SchemaStructSHA256)
		}
	}
}

func TestValidateInventory_RefusesContractBreaks(t *testing.T) {
	for name, body := range map[string]string{
		"unknown owner_kind": `{"api_present":true,"abilities":[{"name":"a/b","owner_kind":"root"}]}`,
		"no api_present":     `{"abilities":[]}`,
		"duplicate":          `{"api_present":true,"abilities":[{"name":"a/b","owner_kind":"core"},{"name":"a/b","owner_kind":"core"}]}`,
		"not an object":      `[1]`,
	} {
		if _, err := ValidateInventory(json.RawMessage(body)); !errors.Is(err, ErrInvalidInventory) {
			t.Errorf("%s: err = %v, want ErrInvalidInventory", name, err)
		}
	}
}

func seedRow() sqlc.AbilityCatalogue {
	return sqlc.AbilityCatalogue{
		EntryID: uuid.MustParse("00000000-0000-0000-0000-000000000001"), Name: NameInventory,
		Source: "wpmgr", Class: "read", Status: "admitted", Enabled: true, ApprovalMode: "none",
		PermissionMode: "principal", Title: "Ability inventory", Description: "d", Snapshot: "none",
		ArgRender: []byte(`{}`), EffectCopy: "none", Limits: []byte(`{}`), Admission: []byte(`{}`),
	}
}

func TestEntryBytes_DeterministicAndVerified(t *testing.T) {
	r := seedRow()
	b1, s1, err := EntryBytes(r)
	if err != nil {
		t.Fatal(err)
	}
	b2, s2, _ := EntryBytes(r)
	if string(b1) != string(b2) || s1 != s2 || s1 != agentcmd.SHA256Hex(b1) {
		t.Fatal("entry bytes are not deterministic")
	}
	// The agent reads name, enabled, source and class from the entry text.
	var m map[string]any
	_ = json.Unmarshal(b1, &m)
	if m["name"] != NameInventory || m["enabled"] != true || m["source"] != "wpmgr" || m["class"] != "read" {
		t.Fatalf("entry = %s", b1)
	}
	// NULL stored hash: stamped at send.
	if _, _, err := SendableEntry(r); err != nil {
		t.Fatal(err)
	}
	// A matching stored hash sends; a stale one refuses.
	r.EntrySha256 = &s1
	if _, _, err := SendableEntry(r); err != nil {
		t.Fatal(err)
	}
	stale := strings.Repeat("0", 64)
	r.EntrySha256 = &stale
	if _, _, err := SendableEntry(r); !errors.Is(err, ErrEntryChanged) {
		t.Fatalf("stale hash: err = %v", err)
	}
}

func TestAgentMeetsFloor(t *testing.T) {
	for v, want := range map[string]bool{"0.61.155": true, "0.61.200": true, "0.61.154": false, "": false, "x": false} {
		if got := AgentMeetsFloor(v); got != want {
			t.Errorf("AgentMeetsFloor(%q) = %v", v, got)
		}
	}
}
