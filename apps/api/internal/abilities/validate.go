package abilities

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/mosamlife/wpmgr/apps/api/internal/humantext"
)

// ErrInvalidInventory marks an agent reply that broke the contract. The job
// does not retry it: the same request would get the same answer.
var ErrInvalidInventory = errors.New("abilities: inventory reply failed validation")

// ErrEntryChanged marks a catalogue row whose stored entry_sha256 no longer
// matches its reproduced bytes.
var ErrEntryChanged = errors.New("abilities: catalogue entry changed")

// ErrNotSendable marks a refresh declined without contacting the site.
var ErrNotSendable = errors.New("abilities: refresh not sendable")

var (
	abilityNameRe = regexp.MustCompile(`^[a-z0-9-]{1,64}/[a-z0-9-]{1,64}$`)
	hex64Re       = regexp.MustCompile(`^[0-9a-f]{64}$`)
	versionRe     = regexp.MustCompile(`^[0-9A-Za-z.+-]{1,32}$`)
)

// Caps, in runes, matching the m155 column CHECKs.
const (
	siteLabelMaxRunes       = 200
	siteDescriptionMaxRunes = 1000
	// maxInventoryRows bounds a reply; the agent caps at 500.
	maxInventoryRows = 500
)

// ownerKindMap maps the agent's closed owner_kind set onto the column's.
// ok is the owner_ok value to store. A kind outside this map fails the reply.
//
//	wpmgr        our own ability, registered by the agent plugin
//	wpmgr_squat  another plugin registered a name in our namespace
//	core         WordPress core
//	site         anything else; the agent does not yet resolve the owner
var ownerKindMap = map[string]struct{ kind, ok string }{
	"wpmgr":       {"plugin", "true"},
	"wpmgr_squat": {"unknown", "false"},
	"core":        {"core", ""},
	"site":        {"unknown", ""},
	"plugin":      {"plugin", ""},
	"theme":       {"theme", ""},
	"mu-plugin":   {"mu-plugin", ""},
	"unknown":     {"unknown", ""},
}

type wireInventory struct {
	APIPresent *bool         `json:"api_present"`
	Count      int           `json:"count"`
	Truncated  bool          `json:"truncated"`
	Abilities  []wireAbility `json:"abilities"`
}

type wireAbility struct {
	Name               string  `json:"name"`
	OwnerKind          string  `json:"owner_kind"`
	OwnerMismatch      bool    `json:"owner_mismatch"`
	Version            *string `json:"version"`
	SchemaStructSHA256 *string `json:"schema_struct_sha256"`
	FromTheSite        *struct {
		Label       string `json:"label"`
		Description string `json:"description"`
	} `json:"from_the_site"`
}

// ValidateInventory checks the wpmgr/abilities-inventory output against the
// contract. A name outside the pattern is dropped and counted; an owner_kind
// outside the closed set, a missing api_present, too many rows, or a duplicate
// name fails the whole reply. Site text is cleaned and capped.
func ValidateInventory(output json.RawMessage) (InventoryResult, error) {
	var w wireInventory
	if err := json.Unmarshal(output, &w); err != nil {
		return InventoryResult{}, fmt.Errorf("%w: %v", ErrInvalidInventory, err)
	}
	if w.APIPresent == nil {
		return InventoryResult{}, fmt.Errorf("%w: api_present missing", ErrInvalidInventory)
	}
	if len(w.Abilities) > maxInventoryRows {
		return InventoryResult{}, fmt.Errorf("%w: %d abilities exceeds %d", ErrInvalidInventory, len(w.Abilities), maxInventoryRows)
	}
	res := InventoryResult{APIPresent: *w.APIPresent, Truncated: w.Truncated}
	seen := make(map[string]struct{}, len(w.Abilities))
	for _, a := range w.Abilities {
		mapped, ok := ownerKindMap[a.OwnerKind]
		if !ok {
			return InventoryResult{}, fmt.Errorf("%w: owner_kind outside the closed set", ErrInvalidInventory)
		}
		if !abilityNameRe.MatchString(a.Name) {
			res.SkippedNames++
			continue
		}
		if _, dup := seen[a.Name]; dup {
			return InventoryResult{}, fmt.Errorf("%w: duplicate ability %s", ErrInvalidInventory, a.Name)
		}
		seen[a.Name] = struct{}{}
		row := InventoryRow{Name: a.Name, OwnerKind: mapped.kind, OwnerOK: mapped.ok}
		if a.OwnerMismatch {
			row.OwnerOK = "false"
		}
		if a.Version != nil && versionRe.MatchString(*a.Version) {
			row.OwnerVersion = *a.Version
		}
		if a.SchemaStructSHA256 != nil && hex64Re.MatchString(*a.SchemaStructSHA256) {
			row.SchemaStructSHA256 = *a.SchemaStructSHA256
		}
		if a.FromTheSite != nil {
			row.SiteLabel = humantext.CapRunes(humantext.Clean(a.FromTheSite.Label), siteLabelMaxRunes)
			row.SiteDescription = humantext.CapRunes(humantext.Clean(a.FromTheSite.Description), siteDescriptionMaxRunes)
		}
		res.Rows = append(res.Rows, row)
	}
	return res, nil
}
