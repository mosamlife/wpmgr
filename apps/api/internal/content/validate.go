package content

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/humantext"
)

// ErrInvalidProbeResponse marks a reply that broke the contract. Nothing from
// such a reply is stored.
var ErrInvalidProbeResponse = errors.New("content_probe response failed validation")

// Closed sets (Sec-F7). They are the same sets the m153 CHECK constraints
// enforce; a value outside them is dropped here so it never reaches the
// database as an error.
var verdicts = map[string]struct{}{
	"classic": {}, "empty": {}, "block_document": {}, "builder": {},
	"ambiguous": {}, "unrecognised_builder": {}, "special_page": {},
	"template_may_override": {},
}

var routeReasons = map[string]struct{}{
	"content_column": {}, "unrecognised_builder": {}, "template_may_override": {},
	"special_page": {}, "empty_page": {}, "block_editor_unsupported": {},
	"vendor_ability": {}, "unpublished_draft_exists": {}, "ai_draft_pending": {},
	"draft_unreadable": {}, "builder_version_below_floor": {},
	"builder_version_unverified": {}, "builder_ability_missing": {},
	"builder_ability_changed": {}, "ability_owner_mismatch": {},
	"vendor_requires_admin": {}, "builder_access_not_granted": {},
	"builder_not_supported": {}, "ambiguous_owner": {},
}

var (
	postTypeRe      = regexp.MustCompile(`^[a-z0-9_-]{1,20}$`)
	integrationIDRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	// agentVersionRe is the probe's version shape. The column CHECK is
	// narrower on characters and length, so both are applied.
	agentVersionRe = regexp.MustCompile(`^\d{1,4}(\.\d{1,4}){0,3}([-+][A-Za-z0-9.]{1,32})?$`)
	columnVersion  = regexp.MustCompile(`^[0-9A-Za-z.+-]{1,32}$`)
)

// maxTitleBytes is the agent's cap and the column CHECK.
const maxTitleBytes = agentcmd.ContentProbeTitleMaxBytes

// ValidateListResponse checks a list-mode reply against the closed sets and
// shapes above.
//
// A structural violation (wrong probe version or mode, too many rows, a
// malformed id, type, status or owner, a duplicate id) fails the whole reply
// with ErrInvalidProbeResponse: the reply is not trustworthy as a whole.
// A verdict or route reason outside the known set is a newer agent speaking a
// word this control plane does not know yet; that row alone is skipped and
// counted, so the rest of the site is still recorded.
//
// names maps an integration id to the display name the platform allowlist
// gave it. A display name is never taken from the site.
func ValidateListResponse(resp agentcmd.ContentProbeListResponse, limit int, allowedTypes []string, names map[string]string) (rows []Row, skippedUnknown int, err error) {
	if !resp.OK {
		return nil, 0, fmt.Errorf("%w: reply is not ok", ErrInvalidProbeResponse)
	}
	if resp.ProbeVersion != agentcmd.ContentProbeVersion {
		return nil, 0, fmt.Errorf("%w: unsupported probe_version %d", ErrInvalidProbeResponse, resp.ProbeVersion)
	}
	if resp.Mode != "list" {
		return nil, 0, fmt.Errorf("%w: mode is not list", ErrInvalidProbeResponse)
	}
	if len(resp.Rows) > limit {
		return nil, 0, fmt.Errorf("%w: %d rows for a limit of %d", ErrInvalidProbeResponse, len(resp.Rows), limit)
	}
	allowed := make(map[string]struct{}, len(allowedTypes))
	for _, t := range allowedTypes {
		allowed[t] = struct{}{}
	}

	seen := make(map[int64]struct{}, len(resp.Rows))
	out := make([]Row, 0, len(resp.Rows))
	for i, r := range resp.Rows {
		if r.Post < 1 {
			return nil, 0, fmt.Errorf("%w: row %d has an invalid post id", ErrInvalidProbeResponse, i)
		}
		if _, dup := seen[r.Post]; dup {
			return nil, 0, fmt.Errorf("%w: row %d repeats a post id", ErrInvalidProbeResponse, i)
		}
		seen[r.Post] = struct{}{}
		if !postTypeRe.MatchString(r.Type) || !postTypeRe.MatchString(r.Status) {
			return nil, 0, fmt.Errorf("%w: row %d has a malformed type or status", ErrInvalidProbeResponse, i)
		}
		if _, ok := allowed[r.Type]; !ok {
			return nil, 0, fmt.Errorf("%w: row %d has a type that was not requested", ErrInvalidProbeResponse, i)
		}
		if r.Route.Number < 1 || r.Route.Number > 3 {
			return nil, 0, fmt.Errorf("%w: row %d has an invalid route number", ErrInvalidProbeResponse, i)
		}

		row := Row{
			PostID:      r.Post,
			PostType:    r.Type,
			PostStatus:  r.Status,
			Verdict:     r.Verdict,
			RouteNumber: int16(r.Route.Number),
			RouteReason: r.Route.Reason,
		}

		if r.Owner != nil {
			if !integrationIDRe.MatchString(r.Owner.IntegrationID) || len(r.Owner.IntegrationID) > 64 {
				return nil, 0, fmt.Errorf("%w: row %d has a malformed owner", ErrInvalidProbeResponse, i)
			}
			row.OwnerID = r.Owner.IntegrationID
			row.OwnerName = humantext.CapRunes(humantext.Clean(names[r.Owner.IntegrationID]), 80)
			if r.Owner.Version != nil {
				v := *r.Owner.Version
				if agentVersionRe.MatchString(v) && columnVersion.MatchString(v) {
					row.OwnerVer = v
				}
				// A version that fails the shape is dropped, not fatal: the
				// probe records an unreadable version as null itself.
			}
		}

		// Titles leave the site for published rows only; anything else is
		// dropped whatever the reply says. The text is the site's own, so it
		// is cleaned before it is kept.
		if r.Title != nil && r.Status == "publish" {
			t := humantext.CapBytes(humantext.Clean(*r.Title), maxTitleBytes)
			row.Title = strings.TrimSpace(t)
		}

		if _, ok := verdicts[r.Verdict]; !ok {
			skippedUnknown++
			continue
		}
		if _, ok := routeReasons[r.Route.Reason]; !ok {
			skippedUnknown++
			continue
		}
		out = append(out, row)
	}
	return out, skippedUnknown, nil
}

// descriptorKeys are the fields the agent accepts in a descriptor. Anything
// else in a stored row is dropped before sending, so a stray key in an
// allowlist row can never make the agent refuse the whole request.
var descriptorKeys = map[string]struct{}{
	"integration_id": {}, "status": {}, "verified": {}, "enabled": {},
	"namespace": {}, "version_constant": {}, "version_ability": {},
	"mode_flag": {}, "payload_keys": {}, "draft_keys": {},
	"shortcode_prefixes": {}, "special_page_options": {},
	"singular_override": {}, "override_check": {}, "plugin_dir": {},
	"ability_names": {}, "dynamic_enum_paths": {},
}

// BuildDescriptors turns allowlist rows into the descriptors sent to the
// agent. A row whose descriptor does not carry a mode flag and payload keys
// cannot detect anything and is left out (the seeded rows are like this until
// their detection data lands): pages of that builder show as an unconfirmed
// page builder, which is the honest answer. At most 32 are sent.
func BuildDescriptors(in []Integration) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(in))
	for _, it := range in {
		var d map[string]json.RawMessage
		if json.Unmarshal(it.Descriptor, &d) != nil || d == nil {
			continue
		}
		if _, ok := d["mode_flag"]; !ok {
			continue
		}
		if _, ok := d["payload_keys"]; !ok {
			continue
		}
		clean := make(map[string]json.RawMessage, len(d)+3)
		for k, v := range d {
			if _, ok := descriptorKeys[k]; ok {
				clean[k] = v
			}
		}
		id, _ := json.Marshal(it.ID)
		clean["integration_id"] = id
		clean["status"] = json.RawMessage(`"detect_only"`)
		clean["enabled"] = json.RawMessage(`true`)
		b, err := json.Marshal(clean)
		if err != nil {
			continue
		}
		out = append(out, b)
		if len(out) == agentcmd.ContentProbeMaxDescriptors {
			break
		}
	}
	return out
}
