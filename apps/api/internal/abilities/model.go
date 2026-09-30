// Package abilities is the control-plane half of the ability engine (Track B
// engine, slice E1): the global catalogue of reviewed abilities, each site's
// cached inventory of registered abilities, and the refresh job that fills it
// through the agent's `ability_run` command.
package abilities

import (
	"encoding/json"
	"fmt"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// NameInventory is the own read ability that lists a site's abilities.
const NameInventory = "wpmgr/abilities-inventory"

// Entry is the catalogue entry exactly as it is sent to the agent. Its JSON
// encoding (EntryBytes) is the text the agent hashes, so the field order here
// IS the wire order and must not be reshuffled without re-stamping every
// stored entry_sha256.
type Entry struct {
	EntryID            string          `json:"entry_id"`
	Name               string          `json:"name"`
	Source             string          `json:"source"`
	Class              string          `json:"class"`
	Status             string          `json:"status"`
	Enabled            bool            `json:"enabled"`
	ApprovalMode       string          `json:"approval_mode"`
	PermissionMode     string          `json:"permission_mode"`
	IntegrationID      *string         `json:"integration_id"`
	OwnerDir           *string         `json:"owner_dir"`
	VersionMin         *string         `json:"version_min"`
	VersionMaxTested   *string         `json:"version_max_tested"`
	MinWPVersion       *string         `json:"min_wp_version"`
	MinAgentVersion    *string         `json:"min_agent_version"`
	SchemaStructSHA256 *string         `json:"schema_struct_sha256"`
	DynamicEnumPaths   []string        `json:"dynamic_enum_paths"`
	Title              string          `json:"title"`
	Description        string          `json:"description"`
	Usage              *string         `json:"usage"`
	OperatorPermission *string         `json:"operator_permission"`
	Target             json.RawMessage `json:"target"`
	Snapshot           string          `json:"snapshot"`
	Preview            *string         `json:"preview"`
	ArgRender          json.RawMessage `json:"arg_render"`
	EffectCopy         string          `json:"effect_copy"`
	Limits             json.RawMessage `json:"limits"`
	NestedAllow        []string        `json:"nested_allow"`
	GlobalOptionKeys   []string        `json:"global_option_keys"`
	IntegrationBlock   json.RawMessage `json:"integration_block"`
	Admission          json.RawMessage `json:"admission"`
}

func rawOrNull(b []byte) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("null")
	}
	return json.RawMessage(b)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// EntryFromRow projects a catalogue row onto the wire entry.
func EntryFromRow(r sqlc.AbilityCatalogue) Entry {
	return Entry{
		EntryID: r.EntryID.String(), Name: r.Name, Source: r.Source, Class: r.Class,
		Status: r.Status, Enabled: r.Enabled, ApprovalMode: r.ApprovalMode,
		PermissionMode: r.PermissionMode, IntegrationID: r.IntegrationID, OwnerDir: r.OwnerDir,
		VersionMin: r.VersionMin, VersionMaxTested: r.VersionMaxTested,
		MinWPVersion: r.MinWpVersion, MinAgentVersion: r.MinAgentVersion,
		SchemaStructSHA256: r.SchemaStructSha256, DynamicEnumPaths: nonNil(r.DynamicEnumPaths),
		Title: r.Title, Description: r.Description, Usage: r.Usage,
		OperatorPermission: r.OperatorPermission, Target: rawOrNull(r.Target),
		Snapshot: r.Snapshot, Preview: r.Preview, ArgRender: rawOrNull(r.ArgRender),
		EffectCopy: r.EffectCopy, Limits: rawOrNull(r.Limits), NestedAllow: nonNil(r.NestedAllow),
		GlobalOptionKeys: nonNil(r.GlobalOptionKeys), IntegrationBlock: rawOrNull(r.IntegrationBlock),
		Admission: rawOrNull(r.Admission),
	}
}

// EntryBytes is the canonical entry text: encoding/json over Entry, whose
// field order is fixed and whose JSON-typed members are Postgres's own jsonb
// output (deterministic for a stored value), compacted by encoding/json.
//
// THE CONTROL PLANE DOES NOT STORE THESE BYTES; IT REPRODUCES THEM. The same
// row always yields the same bytes, and entry_sha256 is the sha256 of them.
// The admin write stamps entry_sha256 from these bytes of the row it is about
// to store; a row whose stored entry_sha256 is NULL (the m155 seed rows) is
// stamped at send from the same bytes. A row whose stored entry_sha256 is set
// and differs from the reproduced bytes is refused (ErrEntryChanged): the
// catalogue changed outside the admin write, and nothing is sent.
func EntryBytes(r sqlc.AbilityCatalogue) ([]byte, string, error) {
	b, err := json.Marshal(EntryFromRow(r))
	if err != nil {
		return nil, "", fmt.Errorf("marshal catalogue entry %s: %w", r.Name, err)
	}
	return b, agentcmd.SHA256Hex(b), nil
}

// SendableEntry returns the bytes and hash to send for a row, verifying a
// stored hash when one is present.
func SendableEntry(r sqlc.AbilityCatalogue) ([]byte, string, error) {
	b, sum, err := EntryBytes(r)
	if err != nil {
		return nil, "", err
	}
	if r.EntrySha256 != nil && *r.EntrySha256 != sum {
		return nil, "", fmt.Errorf("%w: %s", ErrEntryChanged, r.Name)
	}
	return b, sum, nil
}

// InventoryRow is one validated ability, ready to store.
type InventoryRow struct {
	Name               string
	OwnerKind          string
	OwnerOK            string // "true", "false" or "" (unknown)
	OwnerVersion       string
	SchemaStructSHA256 string
	SiteLabel          string
	SiteDescription    string
}

// InventoryResult is one validated refresh.
type InventoryResult struct {
	APIPresent bool
	Truncated  bool
	Rows       []InventoryRow
	// SkippedNames counts abilities dropped for a name outside the pattern.
	SkippedNames int
}
