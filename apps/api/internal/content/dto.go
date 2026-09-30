package content

import (
	"encoding/json"
	"time"
)

// Wire shapes. They match the schemas of the same names in
// packages/openapi/openapi.yaml; dto_contract_test.go decodes them through the
// generated types so the two cannot drift.

type editorDTO struct {
	IntegrationID string  `json:"integration_id"`
	DisplayName   *string `json:"display_name"`
	Version       *string `json:"version"`
}

type inventoryRowDTO struct {
	PostID      int64      `json:"post_id"`
	PostType    string     `json:"post_type"`
	PostStatus  string     `json:"post_status"`
	Verdict     string     `json:"verdict"`
	RouteNumber int        `json:"route_number"`
	RouteReason string     `json:"route_reason"`
	Editor      *editorDTO `json:"editor"`
	Title       *string    `json:"title"`
	CheckedAt   time.Time  `json:"checked_at"`
}

type inventoryPageDTO struct {
	State           string            `json:"state"`
	AgentVersion    string            `json:"agent_version"`
	MinAgentVersion string            `json:"min_agent_version"`
	LastCheckedAt   *time.Time        `json:"last_checked_at"`
	TitlesIncluded  bool              `json:"titles_included"`
	NextAfterPostID *int64            `json:"next_after_post_id"`
	Pages           []inventoryRowDTO `json:"pages"`
}

func toInventoryPageDTO(p InventoryPage) inventoryPageDTO {
	out := inventoryPageDTO{
		State: p.State, AgentVersion: p.AgentVersion, MinAgentVersion: p.MinAgent,
		LastCheckedAt: p.LastCheckedAt, TitlesIncluded: p.TitlesIncluded,
		NextAfterPostID: p.NextAfterPost, Pages: make([]inventoryRowDTO, 0, len(p.Rows)),
	}
	for _, r := range p.Rows {
		row := inventoryRowDTO{
			PostID: r.PostID, PostType: r.PostType, PostStatus: r.PostStatus,
			Verdict: r.Verdict, RouteNumber: int(r.RouteNumber), RouteReason: r.RouteReason,
			Title: r.Title, CheckedAt: r.CheckedAt,
		}
		if r.OwnerIntegrationID != nil {
			row.Editor = &editorDTO{
				IntegrationID: *r.OwnerIntegrationID, DisplayName: r.OwnerDisplayName, Version: r.OwnerVersion,
			}
		}
		out.Pages = append(out.Pages, row)
	}
	return out
}

type fleetVerdictDTO struct {
	Verdict     string `json:"verdict"`
	RouteNumber int    `json:"route_number"`
	Pages       int64  `json:"pages"`
	Sites       int64  `json:"sites"`
}

type fleetBuilderDTO struct {
	IntegrationID string  `json:"integration_id"`
	Version       *string `json:"version"`
	Pages         int64   `json:"pages"`
	Sites         int64   `json:"sites"`
}

type fleetReportDTO struct {
	Pages     int64             `json:"pages"`
	ByVerdict []fleetVerdictDTO `json:"by_verdict"`
	ByBuilder []fleetBuilderDTO `json:"by_builder"`
}

func toFleetReportDTO(r FleetReport) fleetReportDTO {
	out := fleetReportDTO{Pages: r.Pages, ByVerdict: make([]fleetVerdictDTO, 0, len(r.ByVerdict)), ByBuilder: make([]fleetBuilderDTO, 0, len(r.ByBuilder))}
	for _, v := range r.ByVerdict {
		out.ByVerdict = append(out.ByVerdict, fleetVerdictDTO{Verdict: v.Verdict, RouteNumber: int(v.RouteNumber), Pages: v.Pages, Sites: v.Sites})
	}
	for _, b := range r.ByBuilder {
		out.ByBuilder = append(out.ByBuilder, fleetBuilderDTO{IntegrationID: b.IntegrationID, Version: b.Version, Pages: b.Pages, Sites: b.Sites})
	}
	return out
}

type integrationDTO struct {
	IntegrationID          string          `json:"integration_id"`
	DisplayName            string          `json:"display_name"`
	Enabled                bool            `json:"enabled"`
	Status                 string          `json:"status"`
	Descriptor             json.RawMessage `json:"descriptor"`
	Abilities              json.RawMessage `json:"abilities"`
	MinVersion             *string         `json:"min_version"`
	MaxTestedVersion       *string         `json:"max_tested_version"`
	MinWPVersion           *string         `json:"min_wp_version"`
	IntegrationEntrySHA256 *string         `json:"integration_entry_sha256"`
	UpdatedAt              time.Time       `json:"updated_at"`
}

func toIntegrationDTO(r IntegrationRecord) integrationDTO {
	abil := json.RawMessage(`null`)
	if len(r.Abilities) > 0 {
		abil = r.Abilities
	}
	desc := json.RawMessage(`{}`)
	if len(r.Descriptor) > 0 {
		desc = r.Descriptor
	}
	return integrationDTO{
		IntegrationID: r.IntegrationID, DisplayName: r.DisplayName, Enabled: r.Enabled,
		Status: r.Status, Descriptor: desc, Abilities: abil, MinVersion: r.MinVersion,
		MaxTestedVersion: r.MaxTestedVersion, MinWPVersion: r.MinWPVersion,
		IntegrationEntrySHA256: r.IntegrationEntrySHA256, UpdatedAt: r.UpdatedAt,
	}
}

// integrationInput is the PUT body. There is deliberately no actor field: the
// acting user is the authenticated session, and the decoder refuses unknown
// fields, so a body that names one is refused.
type integrationInput struct {
	DisplayName      string          `json:"display_name"`
	Enabled          bool            `json:"enabled"`
	Status           string          `json:"status"`
	Descriptor       json.RawMessage `json:"descriptor"`
	Abilities        json.RawMessage `json:"abilities"`
	MinVersion       *string         `json:"min_version"`
	MaxTestedVersion *string         `json:"max_tested_version"`
	MinWPVersion     *string         `json:"min_wp_version"`
}
