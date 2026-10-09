package abilities

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

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
	ownerDirRe    = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

// Caps, in runes, matching the m155 column CHECKs.
const (
	siteLabelMaxRunes       = 200
	siteDescriptionMaxRunes = 1000
	// maxInventoryRows bounds a reply; the agent caps at 500.
	maxInventoryRows = 500
)

// ownerKindMap maps the agent's closed owner_kind set onto the column's.
// ok is the owner_ok value to store before the C2 fields are read. A kind
// outside this map fails the reply.
//
//	core, plugin, theme, mu-plugin, unknown  the resolved owner (C2, agent 0.61.157+)
//	wpmgr        our own ability, registered by the agent plugin
//	wpmgr_squat  another plugin registered a name in our namespace
//	site         an older agent that did not resolve the owner; stored as unknown
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
	OwnerDir           *string `json:"owner_dir"`
	OwnerVersion       *string `json:"owner_version"`
	OwnerSplit         *bool   `json:"owner_split"`
	AbilityClassOK     *bool   `json:"ability_class_ok"`
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
		// C2: an agent that resolved ownership reports owner_split and
		// ability_class_ok; the owner is verified only when both are clean.
		if a.OwnerSplit != nil && a.AbilityClassOK != nil && mapped.kind != "unknown" && mapped.ok == "" {
			if !*a.OwnerSplit && *a.AbilityClassOK {
				row.OwnerOK = "true"
			} else {
				row.OwnerOK = "false"
			}
		}
		if a.OwnerMismatch {
			row.OwnerOK = "false"
		}
		version := a.OwnerVersion
		if version == nil {
			version = a.Version
		}
		if version != nil && versionRe.MatchString(*version) {
			row.OwnerVersion = *version
		}
		if a.OwnerDir != nil && mapped.kind != "core" && mapped.kind != "unknown" && ownerDirRe.MatchString(*a.OwnerDir) {
			row.OwnerDir = *a.OwnerDir
		}
		// The agent reports "sha256:<hex>"; the column holds bare hex.
		if a.SchemaStructSHA256 != nil {
			if h := strings.TrimPrefix(*a.SchemaStructSHA256, "sha256:"); hex64Re.MatchString(h) {
				row.SchemaStructSHA256 = h
			}
		}
		if a.FromTheSite != nil {
			// CapRunes(s, n) yields up to n+1 runes (it appends an ellipsis),
			// so cap at max-1 to stay inside the column CHECK.
			row.SiteLabel = humantext.CapRunes(humantext.Clean(a.FromTheSite.Label), siteLabelMaxRunes-1)
			row.SiteDescription = humantext.CapRunes(humantext.Clean(a.FromTheSite.Description), siteDescriptionMaxRunes-1)
		}
		res.Rows = append(res.Rows, row)
	}
	return res, nil
}
