package aitrust

import (
	"encoding/base64"
	"strings"
	"time"

	"github.com/google/uuid"
)

// SiteAIModeDTO is GET/PUT /sites/{siteId}/ai/mode on the wire. set_by_name
// is a person's name: the client renders it as plain text.
type SiteAIModeDTO struct {
	SiteID              uuid.UUID       `json:"site_id"`
	Mode                string          `json:"mode"`
	Source              string          `json:"source"`
	Version             int64           `json:"version"`
	SetByUserID         *uuid.UUID      `json:"set_by_user_id"`
	SetByName           *string         `json:"set_by_name"`
	SetByAccountDeleted bool            `json:"set_by_account_deleted"`
	SetAt               *time.Time      `json:"set_at"`
	SetterValid         bool            `json:"setter_valid"`
	AIPaused            bool            `json:"ai_paused"`
	MinAgentVersion     string          `json:"min_agent_version"`
	Options             []ModeOptionDTO `json:"options"`
	Kinds               []ChangeKindDTO `json:"kinds"`
}

// ModeOptionDTO is one offered mode.
type ModeOptionDTO struct {
	Mode      string  `json:"mode"`
	Choosable bool    `json:"choosable"`
	Reason    *string `json:"reason"`
}

// ChangeKindDTO is one row of what each mode covers.
type ChangeKindDTO struct {
	ChangeClass string            `json:"change_class"`
	Name        string            `json:"name"`
	Abilities   []KindAbilityDTO  `json:"abilities"`
	Decisions   []ModeDecisionDTO `json:"decisions"`
}

// KindAbilityDTO is one reviewed tool.
type KindAbilityDTO struct {
	Name  string `json:"name"`
	Title string `json:"title"`
}

// ModeDecisionDTO is what one mode does with one kind.
type ModeDecisionDTO struct {
	Mode    string `json:"mode"`
	Outcome string `json:"outcome"`
}

// PutSiteAIModeBody is the PUT body. Version is a pointer so a missing one is
// refused rather than read as zero.
type PutSiteAIModeBody struct {
	Mode    string `json:"mode"`
	Version *int64 `json:"version"`
}

// AIConnectionAutoDTO is a connection's switch on the wire.
type AIConnectionAutoDTO struct {
	GrantID                 uuid.UUID  `json:"grant_id"`
	AIAuto                  string     `json:"ai_auto"`
	AutoSetByUserID         *uuid.UUID `json:"auto_set_by_user_id"`
	AutoSetByName           *string    `json:"auto_set_by_name"`
	AutoSetByAccountDeleted bool       `json:"auto_set_by_account_deleted"`
	AutoSetAt               *time.Time `json:"auto_set_at"`
	AutoSetterValid         bool       `json:"auto_setter_valid"`
	CreatedWithAPIKey       bool       `json:"created_with_api_key"`
}

// AIUsageBucketDTO is one count against one fixed limit.
type AIUsageBucketDTO struct {
	Used  int32 `json:"used"`
	Limit int32 `json:"limit"`
}

// AIConnectionUsageDTO is GET /ai/connections/{grantId}/usage.
type AIConnectionUsageDTO struct {
	AIConnectionAutoDTO
	WindowMinutes int32            `json:"window_minutes"`
	DraftChanges  AIUsageBucketDTO `json:"draft_changes"`
	DraftSites    AIUsageBucketDTO `json:"draft_sites"`
}

// PutAIConnectionAutoBody is the PUT body.
type PutAIConnectionAutoBody struct {
	AIAuto string `json:"ai_auto"`
}

// AIActivityItemDTO is one item; Request is the queue's own object.
type AIActivityItemDTO struct {
	Kind    string `json:"kind"`
	Request any    `json:"request"`
}

// AIActivityPageDTO is GET /ai/activity.
type AIActivityPageDTO struct {
	Items      []AIActivityItemDTO `json:"items"`
	NextCursor *string             `json:"next_cursor"`
}

func toSiteAIModeDTO(m SiteMode) SiteAIModeDTO {
	out := SiteAIModeDTO{
		SiteID: m.SiteID, Mode: string(m.Mode), Source: m.Source, Version: m.Version,
		SetByUserID: m.SetByUserID, SetByName: m.SetByName, SetByAccountDeleted: m.SetByAccountDeleted,
		SetAt: m.SetAt, SetterValid: m.SetterValid, AIPaused: m.AIPaused, MinAgentVersion: m.MinAgentVersion,
		Options: make([]ModeOptionDTO, 0, len(m.Options)),
		Kinds:   make([]ChangeKindDTO, 0, len(m.Kinds)),
	}
	for _, o := range m.Options {
		d := ModeOptionDTO{Mode: string(o.Mode), Choosable: o.Choosable}
		if o.Reason != "" {
			r := o.Reason
			d.Reason = &r
		}
		out.Options = append(out.Options, d)
	}
	for _, k := range m.Kinds {
		d := ChangeKindDTO{
			ChangeClass: string(k.Class), Name: k.Name,
			Abilities: make([]KindAbilityDTO, 0, len(k.Abilities)),
			Decisions: make([]ModeDecisionDTO, 0, len(k.Decisions)),
		}
		for _, a := range k.Abilities {
			d.Abilities = append(d.Abilities, KindAbilityDTO{Name: a.Name, Title: a.Title})
		}
		for _, dec := range k.Decisions {
			d.Decisions = append(d.Decisions, ModeDecisionDTO{Mode: string(dec.Mode), Outcome: dec.Outcome})
		}
		out.Kinds = append(out.Kinds, d)
	}
	return out
}

func toConnectionAutoDTO(a ConnectionAuto) AIConnectionAutoDTO {
	return AIConnectionAutoDTO{
		GrantID: a.GrantID, AIAuto: string(a.AIAuto),
		AutoSetByUserID: a.SetByUserID, AutoSetByName: a.SetByName,
		AutoSetByAccountDeleted: a.SetByAccountDeleted, AutoSetAt: a.SetAt,
		AutoSetterValid: a.SetterValid, CreatedWithAPIKey: a.CreatedWithAPIKey,
	}
}

func toConnectionUsageDTO(u ConnectionUsage) AIConnectionUsageDTO {
	return AIConnectionUsageDTO{
		AIConnectionAutoDTO: toConnectionAutoDTO(u.ConnectionAuto),
		WindowMinutes:       u.WindowMinutes,
		DraftChanges:        AIUsageBucketDTO{Used: u.DraftChanges.Used, Limit: u.DraftChanges.Limit},
		DraftSites:          AIUsageBucketDTO{Used: u.DraftSites.Used, Limit: u.DraftSites.Limit},
	}
}

func toActivityPageDTO(p ActivityPage) AIActivityPageDTO {
	out := AIActivityPageDTO{Items: make([]AIActivityItemDTO, 0, len(p.Items))}
	for _, it := range p.Items {
		out.Items = append(out.Items, AIActivityItemDTO{Kind: it.Kind, Request: it.Request})
	}
	if p.Next != nil {
		c := EncodeCursor(*p.Next)
		out.NextCursor = &c
	}
	return out
}

// EncodeCursor makes the opaque next_cursor: the keyset position as
// RFC 3339 time with nanoseconds and the id, base64url without padding.
func EncodeCursor(c Cursor) string {
	raw := c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor reads a cursor EncodeCursor made. ok is false for anything
// else.
func DecodeCursor(s string) (Cursor, bool) {
	if s == "" || len(s) > 128 {
		return Cursor{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, false
	}
	at, id, found := strings.Cut(string(raw), "|")
	if !found {
		return Cursor{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return Cursor{}, false
	}
	u, err := uuid.Parse(id)
	if err != nil {
		return Cursor{}, false
	}
	return Cursor{CreatedAt: t, ID: u}, true
}
