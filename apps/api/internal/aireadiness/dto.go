package aireadiness

import (
	"bytes"
	"encoding/json"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// ---------------------------------------------------------------------------
// Wire shapes. packages/openapi/openapi.yaml is the contract.
// ---------------------------------------------------------------------------

type siteReadinessDTO struct {
	SiteID        uuid.UUID    `json:"site_id"`
	Status        string       `json:"status"`
	FixCount      int          `json:"fix_count"`
	MetadataAsOf  *time.Time   `json:"metadata_as_of"`
	AbilitiesAsOf *time.Time   `json:"abilities_as_of"`
	Warnings      []warningDTO `json:"warnings"`
	Floors        floorsDTO    `json:"floors"`
	Groups        []any        `json:"groups"`
}

type warningDTO struct {
	Code string `json:"code"`
}

type floorsDTO struct {
	WP         string `json:"wp"`
	Agent      string `json:"agent"`
	FactsAgent string `json:"facts_agent"`
	Elementor  string `json:"elementor"`
	Bricks     string `json:"bricks"`
}

// baseGroupDTO is the base group: no installed, version or support members.
type baseGroupDTO struct {
	ID     string     `json:"id"`
	Checks []checkDTO `json:"checks"`
}

// builderGroupDTO is a builder group. Version is always written: null when
// the builder is not installed or reported no usable version.
type builderGroupDTO struct {
	ID           string     `json:"id"`
	Installed    bool       `json:"installed"`
	Version      *string    `json:"version"`
	WPMgrSupport string     `json:"wpmgr_support"`
	Checks       []checkDTO `json:"checks"`
}

// checkDTO always writes reason and observed, as null when there is none.
type checkDTO struct {
	ID       string  `json:"id"`
	State    string  `json:"state"`
	Reason   *string `json:"reason"`
	Observed *string `json:"observed"`
}

type fleetReadinessDTO struct {
	Sites []fleetSiteDTO `json:"sites"`
}

type fleetSiteDTO struct {
	SiteID   uuid.UUID `json:"site_id"`
	Status   string    `json:"status"`
	FixCount int       `json:"fix_count"`
	Failing  []string  `json:"failing"`
	Warnings []string  `json:"warnings"`
}

type refreshResultDTO struct {
	Metadata  bool `json:"metadata"`
	Abilities bool `json:"abilities"`
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func toCheckDTO(c Check) checkDTO {
	return checkDTO{
		ID:       string(c.ID),
		State:    string(c.State),
		Reason:   strPtr(string(c.Reason)),
		Observed: strPtr(c.Observed),
	}
}

func toChecksDTO(cs []Check) []checkDTO {
	out := make([]checkDTO, 0, len(cs))
	for _, c := range cs {
		out = append(out, toCheckDTO(c))
	}
	return out
}

func toSiteDTO(r Result) siteReadinessDTO {
	groups := make([]any, 0, len(r.Groups))
	for _, g := range r.Groups {
		if g.ID == GroupBase {
			groups = append(groups, baseGroupDTO{ID: string(g.ID), Checks: toChecksDTO(g.Checks)})
			continue
		}
		groups = append(groups, builderGroupDTO{
			ID:           string(g.ID),
			Installed:    g.Installed,
			Version:      strPtr(g.Version),
			WPMgrSupport: string(g.Support),
			Checks:       toChecksDTO(g.Checks),
		})
	}
	warnings := make([]warningDTO, 0, len(r.Warnings))
	for _, w := range r.Warnings {
		warnings = append(warnings, warningDTO{Code: string(w)})
	}
	return siteReadinessDTO{
		SiteID:        r.SiteID,
		Status:        string(r.Status),
		FixCount:      r.FixCount,
		MetadataAsOf:  r.MetadataAsOf,
		AbilitiesAsOf: r.AbilitiesAsOf,
		Warnings:      warnings,
		Floors: floorsDTO{
			WP: r.Floors.WP, Agent: r.Floors.Agent, FactsAgent: r.Floors.FactsAgent,
			Elementor: r.Floors.Elementor, Bricks: r.Floors.Bricks,
		},
		Groups: groups,
	}
}

func toFleetDTO(rs []Result) fleetReadinessDTO {
	out := fleetReadinessDTO{Sites: make([]fleetSiteDTO, 0, len(rs))}
	for _, r := range rs {
		failing := make([]string, 0, r.FixCount)
		for _, id := range r.Failing() {
			failing = append(failing, string(id))
		}
		warnings := make([]string, 0, len(r.Warnings))
		for _, w := range r.Warnings {
			warnings = append(warnings, string(w))
		}
		out.Sites = append(out.Sites, fleetSiteDTO{
			SiteID:   r.SiteID,
			Status:   string(r.Status),
			FixCount: r.FixCount,
			Failing:  failing,
			Warnings: warnings,
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// Database rows -> Facts.
// ---------------------------------------------------------------------------

// themeTemplateRe is the shape a stored parent-theme directory must have. The
// write path validates it already; reading re-validates because the stored
// document is site-influenced JSON.
var themeTemplateRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

func tsPtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

func deref(b *bool) bool { return b != nil && *b }

// decodeStoredBuilderFacts reads the builder_facts object out of the stored
// inventory document. It never fails: anything that is not the expected shape
// is dropped on its own. raw is nil when the document holds no builder_facts
// object, which is "not reported" and never "off".
func decodeStoredBuilderFacts(raw []byte) BuilderFacts {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return BuilderFacts{}
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return BuilderFacts{}
	}
	bf := BuilderFacts{Reported: true}
	if v, ok := obj["theme_template"]; ok {
		var s string
		if trimmed := bytes.TrimSpace(v); len(trimmed) > 0 && trimmed[0] == '"' &&
			json.Unmarshal(trimmed, &s) == nil && themeTemplateRe.MatchString(s) {
			bf.ThemeTemplate = s
		}
	}
	if v, ok := obj["elementor"]; ok {
		var eobj map[string]json.RawMessage
		if json.Unmarshal(bytes.TrimSpace(v), &eobj) == nil && eobj != nil {
			ef := &ElementorFacts{}
			if a, ok := eobj["atomic_editor"]; ok {
				switch string(bytes.TrimSpace(a)) {
				case "true":
					t := true
					ef.AtomicEditor = &t
				case "false":
					f := false
					ef.AtomicEditor = &f
				}
			}
			bf.Elementor = ef
		}
	}
	return bf
}

// assembleFacts maps the combined facts and ability-count rows to Facts. The
// builder ability counts are the attributed ones: abilities registered by the
// builder itself. The in-namespace counts are for diagnostics and never reach
// Facts. When only is set, rows for any other site are dropped: the SQL
// already narrows to it, and this is the second check.
func assembleFacts(rows []sqlc.ListAIReadinessSiteFactsWithAbilityCountsRow, only *uuid.UUID) []Facts {
	out := make([]Facts, 0, len(rows))
	for _, r := range rows {
		if only != nil && r.SiteID != *only {
			continue
		}
		out = append(out, Facts{
			SiteID:                r.SiteID,
			WPVersion:             r.WpVersion,
			AgentVersion:          r.AgentVersion,
			MetadataAsOf:          tsPtr(r.ComponentsUpdatedAt),
			ContentEditingEnabled: r.ContentEditingEnabledAt.Valid,
			ElementorInstalled:    r.ElementorInstalled,
			ElementorVersion:      r.ElementorVersion,
			ElementorActive:       r.ElementorActive,
			MCPAdapterActive:      r.McpAdapterActive,
			BricksInstalled:       r.BricksInstalled,
			BricksVersion:         r.BricksVersion,
			BricksActive:          r.BricksActive,
			BuilderFacts:          decodeStoredBuilderFacts(r.BuilderFacts),
			InventoryChecked:      r.AbilitiesCheckedAt.Valid,
			AbilitiesAsOf:         tsPtr(r.AbilitiesCheckedAt),
			AbilitiesAPIPresent:   deref(r.AbilitiesApiPresent),
			AbilitiesTruncated:    deref(r.AbilitiesTruncated),
			ElementorAbilities:    r.ElementorAbilitiesAttributed,
			BricksAbilities:       r.BricksAbilitiesAttributed,
		})
	}
	return out
}
