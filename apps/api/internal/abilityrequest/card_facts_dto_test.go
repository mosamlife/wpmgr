package abilityrequest

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// storedCard is a rest-write card as Postgres returns the jsonb (keys
// reordered, spaces after separators).
const storedCard = `{"live": true, "undo": "post_fields", "method": "POST", "target": {"id": 412, "post_type": "page", ` +
	`"from_the_site": {"status": "publish", "title_before": "Old title"}}, "changes": [{"key": "title", "after": "Spring sale", ` +
	`"label": "Title", "from_the_site": {"before": "Old title"}}], "route_id": "wp-v2-pages-update-fields", ` +
	`"undo_note": null, "undo_exact": true, "effect_copy": "live", "route_title": "Change a page's title or excerpt", ` +
	`"effect_label": "Published immediately"}`

func TestRequestDTOCarriesCardFacts(t *testing.T) {
	route, sum := "wp-v2-pages-update-fields", strings.Repeat("b", 64)
	digest := strings.Repeat("d", 64)
	row := sqlc.AssistantAbilityRequest{
		ID: uuid.New(), SiteID: uuid.New(), AbilityName: "wpmgr/rest-write", State: "pending",
		PresentedDigest: digest, RouteID: &route, RouteSha256: &sum, CardFacts: []byte(storedCard),
	}
	b, err := json.Marshal(toDTO(row, true, "", setterNames{}))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		RouteID         *string                    `json:"route_id"`
		RouteSHA256     *string                    `json:"route_sha256"`
		PresentedDigest string                     `json:"presented_digest"`
		CardFacts       map[string]json.RawMessage `json:"card_facts"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, b)
	}
	if got.RouteID == nil || *got.RouteID != route || got.RouteSHA256 == nil || *got.RouteSHA256 != sum {
		t.Fatalf("route facts: %s", b)
	}
	if got.PresentedDigest != digest {
		t.Fatalf("presented_digest changed in serialisation: %s", got.PresentedDigest)
	}
	for _, k := range []string{"route_title", "method", "effect_label", "live", "target", "changes", "undo", "undo_note"} {
		if _, ok := got.CardFacts[k]; !ok {
			t.Fatalf("card_facts has no %s: %s", k, b)
		}
	}
	if string(got.CardFacts["live"]) != "true" || string(got.CardFacts["effect_label"]) != `"Published immediately"` {
		t.Fatalf("card_facts values: %s", b)
	}

	// A row without a card (page-create) serialises card_facts as null.
	row.CardFacts, row.RouteID, row.RouteSha256 = nil, nil, nil
	b, _ = json.Marshal(toDTO(row, true, "", setterNames{}))
	if !strings.Contains(string(b), `"card_facts":null`) || !strings.Contains(string(b), `"route_id":null`) {
		t.Fatalf("non-rest row: %s", b)
	}
	// Anything but an object is never passed through.
	row.CardFacts = []byte(`"<script>"`)
	b, _ = json.Marshal(toDTO(row, true, "", setterNames{}))
	if !strings.Contains(string(b), `"card_facts":null`) {
		t.Fatalf("a non-object card reached the wire: %s", b)
	}
}
