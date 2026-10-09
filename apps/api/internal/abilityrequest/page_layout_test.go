package abilityrequest

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

const (
	textOnlyInput = `{"post_type":"page","editor":"wordpress_blocks","title":"T","outline":[{"type":"paragraph","text":"x"}]}`
	layoutInput   = `{"post_type":"page","editor":"wordpress_blocks","title":"T","outline":[{"type":"image","attachment_id":42,"alt":""}]}`
)

// The dispatch floor is the stored input's: a layout request whose site's
// plugin is below the layout floor is not sent (checkSite closes it
// not_sent/agent_outdated), while a text-only request on the same plugin is.
func TestAgentFloorFor_PageCreateInput(t *testing.T) {
	page := func(input string) sqlc.AssistantAbilityRequest {
		return sqlc.AssistantAbilityRequest{AbilityName: mcp.AbilityPageCreate, InputJson: input}
	}
	cases := []struct {
		name  string
		row   sqlc.AssistantAbilityRequest
		floor string
	}{
		{"text-only page", page(textOnlyInput), agentcmd.MinAgentVersionForPageCreate},
		{"layout page", page(layoutInput), agentcmd.MinAgentVersionForPageLayout},
		{"unreadable page input", page(`not json`), agentcmd.MinAgentVersionForPageLayout},
		{"rest write", sqlc.AssistantAbilityRequest{AbilityName: mcp.AbilityRestWrite, InputJson: layoutInput}, agentcmd.MinAgentVersionForRestCall},
	}
	for _, c := range cases {
		if got := agentFloorFor(c.row); got != c.floor {
			t.Errorf("%s: floor %s, want %s", c.name, got, c.floor)
		}
	}
	below := "0.61.158"
	if agentMeetsFloor(below, agentFloorFor(page(layoutInput))) {
		t.Fatalf("a layout request would be sent to %s", below)
	}
	if !agentMeetsFloor(below, agentFloorFor(page(textOnlyInput))) {
		t.Fatalf("a text-only request would not be sent to %s", below)
	}
	if !agentMeetsFloor(agentcmd.MinAgentVersionForPageLayout, agentFloorFor(page(layoutInput))) || agentMeetsFloor("", agentcmd.MinAgentVersionForPageCreate) {
		t.Fatal("floor comparison")
	}
}

const storedPageCard = `{"kind": "page_create", "media": [{"id": 42, "mime": "image/jpeg", "width": 1200, "height": 800, ` +
	`"filename": "team-photo.jpg"}, {"id": 7, "mime": "image/png", "width": 0, "height": 0, "filename": "café.png"}]}`

// A page-create request's stored facts reach the wire as page_media, never
// as card_facts (whose wire type is the rest-write card).
func TestRequestDTOPageMedia(t *testing.T) {
	row := sqlc.AssistantAbilityRequest{
		ID: uuid.New(), SiteID: uuid.New(), AbilityName: mcp.AbilityPageCreate, State: "pending",
		InputJson: layoutInput, CardCopyVersion: mcp.AbilityCardCopyVersionLayout, CardFacts: []byte(storedPageCard),
	}
	b, err := json.Marshal(toDTO(row, true, ""))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		CardFacts json.RawMessage `json:"card_facts"`
		PageMedia []PageMediaDTO  `json:"page_media"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if string(got.CardFacts) != "null" {
		t.Fatalf("a page-create card reached card_facts: %s", b)
	}
	if len(got.PageMedia) != 2 || got.PageMedia[0] != (PageMediaDTO{ID: 42, Filename: "team-photo.jpg", Mime: "image/jpeg", Width: 1200, Height: 800}) ||
		got.PageMedia[1].ID != 7 || got.PageMedia[1].Filename != "café.png" {
		t.Fatalf("page_media: %s", b)
	}

	for name, card := range map[string][]byte{
		"no card":       nil,
		"not a card":    []byte(`"<script>"`),
		"other kind":    []byte(strings.Replace(storedPageCard, "page_create", "rest_write", 1)),
		"svg":           []byte(strings.Replace(storedPageCard, "image/png", "image/svg+xml", 1)),
		"missing field": []byte(strings.Replace(storedPageCard, `"width": 0, `, "", 1)),
		"extra field":   []byte(strings.Replace(storedPageCard, `"id": 7,`, `"id": 7, "url": "https://x.example/a.png",`, 1)),
	} {
		row.CardFacts = card
		b, _ := json.Marshal(toDTO(row, true, ""))
		if !strings.Contains(string(b), `"page_media":null`) || !strings.Contains(string(b), `"card_facts":null`) {
			t.Errorf("%s: %s", name, b)
		}
	}

	// A rest-write card is unchanged, and has no page_media.
	rest := sqlc.AssistantAbilityRequest{ID: uuid.New(), SiteID: uuid.New(), AbilityName: mcp.AbilityRestWrite,
		State: "pending", CardFacts: []byte(storedCard)}
	b, _ = json.Marshal(toDTO(rest, true, ""))
	if !strings.Contains(string(b), `"page_media":null`) || strings.Contains(string(b), `"card_facts":null`) {
		t.Fatalf("rest-write row: %s", b)
	}
}
