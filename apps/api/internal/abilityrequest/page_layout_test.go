package abilityrequest

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

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

// TestWorkerLayoutRequestOldAgentNotSent drives the dispatch check checkSite
// applies to an approved row: a layout request whose site's plugin went
// below the layout floor after approval closes not_sent/agent_outdated and
// is never sent; a text-only request on the same plugin goes on, and so
// does the layout request once the plugin is at the floor.
func TestWorkerLayoutRequestOldAgentNotSent(t *testing.T) {
	approved := func(ability, input string) sqlc.GetApprovedAbilityRequestForDispatchRow {
		return sqlc.GetApprovedAbilityRequestForDispatchRow{
			AssistantAbilityRequest: sqlc.AssistantAbilityRequest{AbilityName: ability, InputJson: input},
			EntryEnabled:            true, EntryHashCurrent: true, RouteEnabled: true, RouteHashCurrent: true,
		}
	}
	on := func(version string) sqlc.Site { return sqlc.Site{AgentVersion: version} }
	below := "0.61.158"
	cases := []struct {
		name string
		row  sqlc.GetApprovedAbilityRequestForDispatchRow
		site sqlc.Site
		want string
	}{
		{"layout page below the layout floor", approved(mcp.AbilityPageCreate, layoutInput), on(below), ReasonAgentOutdated},
		{"layout page on no reported version", approved(mcp.AbilityPageCreate, layoutInput), on(""), ReasonAgentOutdated},
		{"layout page at the layout floor", approved(mcp.AbilityPageCreate, layoutInput), on(agentcmd.MinAgentVersionForPageLayout), ""},
		{"text-only page below the layout floor", approved(mcp.AbilityPageCreate, textOnlyInput), on(below), ""},
		{"text-only page below the page-create floor", approved(mcp.AbilityPageCreate, textOnlyInput), on("0.61.155"), ReasonAgentOutdated},
		{"rest write below its floor", approved(mcp.AbilityRestWrite, `{}`), on("0.61.157"), ReasonAgentOutdated},
	}
	for _, c := range cases {
		if got := notSentBeforeReserve(c.row, c.site, true); got != c.want {
			t.Errorf("%s: reason %q, want %q", c.name, got, c.want)
		}
	}
	// The earlier checks keep their order: a site out of scope, a passed
	// deadline and a changed entry are named before the plugin version.
	row := approved(mcp.AbilityPageCreate, layoutInput)
	if got := notSentBeforeReserve(row, on(below), false); got != ReasonSiteAbsent {
		t.Errorf("site out of scope: %q", got)
	}
	row.PastDeadline = true
	if got := notSentBeforeReserve(row, on(below), true); got != ReasonDispatchDeadlinePassed {
		t.Errorf("deadline passed: %q", got)
	}
	row.PastDeadline, row.EntryHashCurrent = false, false
	if got := notSentBeforeReserve(row, on(below), true); got != ReasonEntryChanged {
		t.Errorf("entry changed: %q", got)
	}
}

// A layout write the site refused, read back from the ledger, keeps the
// agent's code, so the card shows that code's advice and not unknown.
func TestOutcomeFromStored_PageLayoutCodes(t *testing.T) {
	for _, code := range []string{"image_not_available", "image_url_unusable", "layout_needs_block_editor", "layout_invalid", "link_invalid"} {
		raw := json.RawMessage(`{"ok":false,"outcome":"refused","code":"` + code + `","detail":"attachment_id 42"}`)
		oc, ok := outcomeFromStored(raw, time.Now())
		if !ok || oc.outcome != OutcomeRefused || oc.code == nil || *oc.code != code {
			t.Errorf("%s: outcome %+v (ok %v)", code, oc, ok)
		}
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
	b, err := json.Marshal(toDTO(row, true, "", setterNames{}))
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
		b, _ := json.Marshal(toDTO(row, true, "", setterNames{}))
		if !strings.Contains(string(b), `"page_media":null`) || !strings.Contains(string(b), `"card_facts":null`) {
			t.Errorf("%s: %s", name, b)
		}
	}

	// A rest-write card is unchanged, and has no page_media.
	rest := sqlc.AssistantAbilityRequest{ID: uuid.New(), SiteID: uuid.New(), AbilityName: mcp.AbilityRestWrite,
		State: "pending", CardFacts: []byte(storedCard)}
	b, _ = json.Marshal(toDTO(rest, true, "", setterNames{}))
	if !strings.Contains(string(b), `"page_media":null`) || strings.Contains(string(b), `"card_facts":null`) {
		t.Fatalf("rest-write row: %s", b)
	}
}
