package agentcmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

// MinAgentVersionForVendorReads is the first agent release that runs a
// reviewed vendor or core READ entry (engine slice E3): the C1 checks, the
// interception guards, the side-effect recorder and the pinned output shape.
// An older agent refuses such an entry; the run tool refuses before asking.
const MinAgentVersionForVendorReads = "0.61.158"

// Vendor read reply limits. The agent caps each list at 50 names; a reply
// over these bounds breaks the contract.
const (
	vendorMaxNames       = 64
	vendorMaxInvoked     = 32
	vendorMaxViolations  = 32
	vendorErrorCodeBytes = 64
)

var (
	vendorOwnerDirRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	vendorVersionRe    = regexp.MustCompile(`^[0-9A-Za-z._+~-]{1,64}$`)
	vendorAbilityRe    = regexp.MustCompile(`^[a-z0-9-]{1,64}/[a-z0-9-]{1,64}$`)
	vendorOwnerKinds   = map[string]struct{}{"core": {}, "plugin": {}, "theme": {}, "mu-plugin": {}}
	errVendorReadShape = errors.New("ability_run: vendor read reply broke the contract")
)

// vendorViolationLabels is the CLOSED set of guard violation labels the agent
// emits (the interception guards and the vendor call). The agent is
// untrusted: a label outside this set is replaced by "unknown", so no
// site-chosen string reaches the model or an audit row as our own word.
var vendorViolationLabels = func() map[string]struct{} {
	m := map[string]struct{}{
		"hook_order": {}, "nested_ability_refused": {}, "nested_reentry": {},
		"sentinel_result": {}, "uncaught_exception": {},
	}
	for _, k := range []string{"input", "permission", "result", "validate_input", "validate_output", "short_circuit"} {
		m[k] = struct{}{}
		m[k+"_order"] = struct{}{}
		m[k+"_unpaired"] = struct{}{}
	}
	return m
}()

// vendorBlockedLabels is the CLOSED set of blocked-write labels the
// side-effect recorder emits.
var vendorBlockedLabels = map[string]struct{}{
	"user_capabilities_meta": {}, "user_level_meta": {}, "user_roles_option": {}, "site_admins": {},
}

// closedLabels maps each entry onto the closed set ("unknown" otherwise),
// drops repeats, and keeps at most max.
func closedLabels(in []string, set map[string]struct{}, max int) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, v := range in {
		if _, ok := set[v]; !ok {
			v = "unknown"
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
		if len(out) == max {
			break
		}
	}
	return out
}

// AbilityRunOwner is the owner the agent resolved for a vendor or core read.
// Kind is from a closed set; Dir and Version are SITE TEXT (pattern-checked,
// still untrusted).
type AbilityRunOwner struct {
	Kind    string `json:"kind"`
	Dir     string `json:"dir"`
	Version string `json:"version"`
}

// AbilityRunSideEffects is read_side_effect_detected's side_effects member.
// Options and HTTPHosts are SITE TEXT: names a plugin chose. Blocked holds the
// agent's own fixed labels.
type AbilityRunSideEffects struct {
	Options   []string `json:"options"`
	Posts     int64    `json:"posts"`
	Roles     int64    `json:"roles"`
	Users     int64    `json:"users"`
	HTTPHosts []string `json:"http_hosts"`
	Blocked   []string `json:"blocked"`
}

// VendorRead is a decoded, validated vendor or core read success.
type VendorRead struct {
	Ability          string
	EntrySHA256      string
	Owner            AbilityRunOwner
	AbilitiesInvoked []string
	Output           json.RawMessage
}

// vendorReadWire is the exact success shape; nothing else may appear.
type vendorReadWire struct {
	OK               *bool            `json:"ok"`
	Outcome          string           `json:"outcome"`
	Mode             string           `json:"mode"`
	Ability          string           `json:"ability"`
	EntrySHA256      string           `json:"entry_sha256"`
	Owner            *AbilityRunOwner `json:"owner"`
	AbilitiesInvoked []string         `json:"abilities_invoked"`
	Output           json.RawMessage  `json:"output"`
}

// DecodeVendorRead strictly decodes a vendor/core read success from the raw
// reply bytes: unknown members, a missing member, a mode or outcome other
// than read/completed, an ability or entry hash other than the one sent, an
// owner kind outside the closed set, or a list over its bound all fail.
func DecodeVendorRead(raw []byte, ability, entrySHA256 string) (VendorRead, error) {
	var w vendorReadWire
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return VendorRead{}, fmt.Errorf("%w: %v", errVendorReadShape, err)
	}
	if dec.More() {
		return VendorRead{}, fmt.Errorf("%w: trailing data", errVendorReadShape)
	}
	switch {
	case w.OK == nil || !*w.OK:
		return VendorRead{}, fmt.Errorf("%w: ok", errVendorReadShape)
	case w.Outcome != "completed" || w.Mode != AbilityRunModeRead:
		return VendorRead{}, fmt.Errorf("%w: outcome or mode", errVendorReadShape)
	case w.Ability != ability:
		return VendorRead{}, fmt.Errorf("%w: ability", errVendorReadShape)
	case w.EntrySHA256 != entrySHA256:
		return VendorRead{}, fmt.Errorf("%w: entry_sha256", errVendorReadShape)
	case w.Owner == nil:
		return VendorRead{}, fmt.Errorf("%w: owner", errVendorReadShape)
	case w.AbilitiesInvoked == nil || len(w.AbilitiesInvoked) > vendorMaxInvoked:
		return VendorRead{}, fmt.Errorf("%w: abilities_invoked", errVendorReadShape)
	case len(bytes.TrimSpace(w.Output)) == 0:
		return VendorRead{}, fmt.Errorf("%w: output", errVendorReadShape)
	}
	if _, ok := vendorOwnerKinds[w.Owner.Kind]; !ok {
		return VendorRead{}, fmt.Errorf("%w: owner kind", errVendorReadShape)
	}
	if w.Owner.Kind == "core" {
		if w.Owner.Dir != "" {
			return VendorRead{}, fmt.Errorf("%w: core owner dir", errVendorReadShape)
		}
	} else if !vendorOwnerDirRe.MatchString(w.Owner.Dir) {
		return VendorRead{}, fmt.Errorf("%w: owner dir", errVendorReadShape)
	}
	if !vendorVersionRe.MatchString(w.Owner.Version) {
		return VendorRead{}, fmt.Errorf("%w: owner version", errVendorReadShape)
	}
	for _, n := range w.AbilitiesInvoked {
		if !vendorAbilityRe.MatchString(n) {
			return VendorRead{}, fmt.Errorf("%w: abilities_invoked name", errVendorReadShape)
		}
	}
	return VendorRead{
		Ability: w.Ability, EntrySHA256: w.EntrySHA256, Owner: *w.Owner,
		AbilitiesInvoked: w.AbilitiesInvoked, Output: w.Output,
	}, nil
}

// decodeSideEffects strictly decodes side_effects. Nil when absent or
// malformed: the refusal code stands on its own, and a malformed detail is
// not repeated.
func decodeSideEffects(raw json.RawMessage) *AbilityRunSideEffects {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var se AbilityRunSideEffects
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&se) != nil {
		return nil
	}
	if se.Options == nil || se.HTTPHosts == nil || se.Blocked == nil ||
		len(se.Options) > vendorMaxNames || len(se.HTTPHosts) > vendorMaxNames || len(se.Blocked) > vendorMaxNames ||
		se.Posts < 0 || se.Roles < 0 || se.Users < 0 {
		return nil
	}
	// Blocked is the agent's own fixed labels; anything else is replaced.
	se.Blocked = closedLabels(se.Blocked, vendorBlockedLabels, len(vendorBlockedLabels)+1)
	return &se
}

// decodeViolations decodes violations[]: the agent's fixed guard labels.
// A label outside the shape becomes "unknown"; past the bound the list is cut.
// restViolationLabels is rest_intercepted's CLOSED set (class-rest-guards.php):
// the REST filter at which another plugin interfered.
var restViolationLabels = map[string]struct{}{
	"pre_dispatch": {}, "before_callbacks": {}, "dispatch": {}, "after_callbacks": {},
}

// decodeRefusalViolations picks the closed label set by refusal code: a
// rest_intercepted refusal carries REST filter labels, every other refusal
// the ability guard labels. A label outside the set becomes "unknown".
func decodeRefusalViolations(code string, raw json.RawMessage) []string {
	if code != "rest_intercepted" {
		return decodeViolations(raw)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var vs []string
	if json.Unmarshal(raw, &vs) != nil {
		return []string{"unknown"}
	}
	return closedLabels(vs, restViolationLabels, vendorMaxViolations)
}

// postColumnLabels is the CLOSED set of wp_posts columns side_effect_detected
// may name.
var postColumnLabels = func() map[string]struct{} {
	m := map[string]struct{}{}
	for _, c := range []string{
		"ID", "post_author", "post_date", "post_date_gmt", "post_content", "post_title",
		"post_excerpt", "post_status", "comment_status", "ping_status", "post_password",
		"post_name", "to_ping", "pinged", "post_modified", "post_modified_gmt",
		"post_content_filtered", "post_parent", "guid", "menu_order", "post_type",
		"post_mime_type", "comment_count",
	} {
		m[c] = struct{}{}
	}
	return m
}()

// decodePostColumns maps side_effect_detected's column names onto the
// closed set; a name outside it becomes "unknown".
func decodePostColumns(raw json.RawMessage) []string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var vs []string
	if json.Unmarshal(raw, &vs) != nil {
		return []string{"unknown"}
	}
	return closedLabels(vs, postColumnLabels, len(postColumnLabels)+1)
}

func decodeViolations(raw json.RawMessage) []string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var vs []string
	if json.Unmarshal(raw, &vs) != nil {
		return []string{"unknown"}
	}
	return closedLabels(vs, vendorViolationLabels, vendorMaxViolations)
}
