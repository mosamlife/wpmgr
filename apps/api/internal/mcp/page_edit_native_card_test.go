package mcp

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// Both card consumers use facts produced by the verified production precheck.
func TestNativeCardFactsGolden(t *testing.T) {
	type cardCase struct {
		Name  string          `json:"name"`
		Input string          `json:"input"`
		Facts json.RawMessage `json:"facts"`
	}
	rows := []cardCase{}
	for _, row := range nativeEditGoldens(t) {
		fx := row.pageEditPreviewFixture
		checked, ok := verifyPageEditPrecheck(fixtureResponse(fx, nil), row.EntrySHA256, []byte(row.Input), fixtureFacts(t, fx))
		if !ok {
			t.Fatalf("%s: production precheck refused", row.Name)
		}
		facts, err := buildPageEditCardFacts(checked, time.Date(2026, 10, 13, 9, 52, 0, 0, time.UTC))
		if err != nil {
			t.Fatalf("%s: %v", row.Name, err)
		}
		rows = append(rows, cardCase{row.Name, row.Input, facts})
	}
	want, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	const path = "testdata/native-card-facts.json"
	if os.Getenv("UPDATE_NATIVE_CARD_FACTS") == "1" {
		if err := os.WriteFile(path, want, 0644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("native card facts differ; run UPDATE_NATIVE_CARD_FACTS=1 go test ./internal/mcp/ -run '^TestNativeCardFactsGolden$' to regenerate")
	}
}
