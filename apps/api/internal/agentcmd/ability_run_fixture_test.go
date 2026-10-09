package agentcmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

// The cross-language fixture for ability_run. Go builds a real request with
// BuildAbilityRunParams from text that JSON escapes differently in Go and PHP
// (an em dash, U+2028, `<b>&amp;`), and records its body and digests. The
// agent's PHPUnit test (apps/agent/tests/AbilityRunFixtureTest.php) reads the
// same file and recomputes each digest from the bytes it decodes. If either
// side changes how it builds or hashes, one of the two goes red.
//
// Regenerate deliberately with WPMGR_UPDATE_FIXTURE=1.

const abilityRunFixturePath = "testdata/ability_run_fixture.json"

type abilityRunFixture struct {
	Body           string `json:"body"`
	PD             string `json:"pd"`
	EntrySHA256    string `json:"entry_sha256"`
	InputSHA256    string `json:"input_sha256"`
	PrecheckDigest string `json:"precheck_digest"`
}

func buildAbilityRunFixture(t *testing.T) abilityRunFixture {
	t.Helper()
	entry := []byte(`{"name":"wpmgr/site-facts","source":"wpmgr","class":"read","status":"admitted","enabled":true,` +
		`"title":"Site facts — versions` + "\u2028" + `and <b>&amp; hints","limits":{}}`)
	input := []byte(`{"plugin_slugs":["a-b"],"theme_slugs":[]}`)
	entrySum := SHA256Hex(entry)
	p, pd, err := BuildAbilityRunParams(AbilityRunCall{
		Mode: AbilityRunModePrecheck, RequestID: uuid.MustParse("7d4f7b0e-2b7a-4c55-9d7e-0f3f5e2a1c11"),
		Entry: entry, EntrySHA256: entrySum, Input: input,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(abilityRunBody{P: string(p)})
	if err != nil {
		t.Fatal(err)
	}
	inputSum := SHA256Hex(input)
	// The agent's precheck_digest: sha256 of a JSON array of hex fields
	// [entry_sha256, input_sha256, base_fingerprint, preview_digest].
	arr, _ := json.Marshal([]string{entrySum, inputSum, "", ""})
	return abilityRunFixture{
		Body: string(body), PD: pd, EntrySHA256: entrySum, InputSHA256: inputSum,
		PrecheckDigest: SHA256Hex(arr),
	}
}

func TestAbilityRunFixture_MatchesTheCommittedFile(t *testing.T) {
	got := buildAbilityRunFixture(t)
	if os.Getenv("WPMGR_UPDATE_FIXTURE") == "1" {
		b, _ := json.MarshalIndent(got, "", "  ")
		if err := os.MkdirAll(filepath.Dir(abilityRunFixturePath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abilityRunFixturePath, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(abilityRunFixturePath)
	if err != nil {
		t.Fatalf("read the committed fixture: %v", err)
	}
	var want abilityRunFixture
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("the request Go builds today differs from the committed fixture the agent tests against:\n got %+v\nwant %+v", got, want)
	}
}
