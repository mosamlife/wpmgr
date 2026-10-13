package abilityrequest

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

func TestClearedRequestDTO(t *testing.T) {
	at := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	row := sqlc.AssistantAbilityRequest{AbilityName: "wpmgr/page-create", State: "done", InputJson: "{}", GrantLabel: "Laptop", SiteLabel: "Shop", ContentClearedAt: pgtype.Timestamptz{Time: at, Valid: true}}
	b, err := json.Marshal(toDTO(row, true, "", false, setterNames{}))
	if err != nil {
		t.Fatal(err)
	}
	var dto map[string]any
	if err := json.Unmarshal(b, &dto); err != nil {
		t.Fatal(err)
	}
	if dto["content_cleared_at"] != at.Format(time.RFC3339) {
		t.Fatalf("content_cleared_at = %v", dto["content_cleared_at"])
	}
	if dto["input_json"] != "{}" || dto["grant_label"] != "Laptop" || dto["state"] != "done" {
		t.Fatalf("record = %s", b)
	}
	if _, ok := dto["retained_draft"]; ok {
		t.Fatal("draft design must not be sent to the dashboard")
	}
}
