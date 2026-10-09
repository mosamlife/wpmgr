package agentcmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/humantext"
)

// This file is the CP->agent contract for `ability_run`, the ability engine's
// one command. Wire: POST {site_url}/wp-json/wpmgr/v1/command/ability_run.
//
// THE BODY IS EXACTLY {"p":"<json text>"}. p is itself JSON text:
//
//	{"mode":"read|precheck|ledger","request_id":"<uuid>","entry":"<entry JSON text>",
//	 "entry_sha256":"<hex sha256 of the entry text bytes>","input":"<input JSON text>"}
//
// The token carries `pd`, the lowercase hex sha256 of the exact bytes of the p
// string. The agent hashes the string it received FIRST and refuses a mismatch
// (token_params_mismatch), then decodes that same string. No canonicaliser
// sits in the security path: both digests are over bytes this file produced
// and sent, never over a re-encoding.

// CmdAbilityRun is the command name, which is also the token's cmd claim.
const CmdAbilityRun = "ability_run"

// MinAgentVersionForAbilityEngine is the first agent release that ships
// ability_run. An older agent answers 404 for the route; the MCP tools refuse
// before sending.
const MinAgentVersionForAbilityEngine = "0.61.155"

// MinAgentVersionForPageCreate is the first agent release that ships the
// write and revert modes, the ledger, the service principal and
// content_editing_enable, which wpmgr/page-create needs. An older agent
// refuses those modes; the write branch of site_ability_run refuses before
// asking.
const MinAgentVersionForPageCreate = "0.61.156"

// MinAgentVersionForRecoveryUndo is the first agent release whose revert
// mode undoes the draft a failed or given-up write left on the site (GH
// #826). An older agent answers a recovery revert with not_revertible, so
// the control plane neither offers nor starts one until the site runs it.
const MinAgentVersionForRecoveryUndo = "0.61.157"

// MinAgentVersionForRestCall is the first agent release that ships
// wpmgr/rest-read and wpmgr/rest-write (reviewed REST routes, RC1). An older
// agent answers ability_unknown; discover, describe and run refuse first.
const MinAgentVersionForRestCall = "0.61.158"

// MinAgentVersionForPageLayout is the first agent release whose
// wpmgr/page-create builds outline grammar v2: images from the media
// library, buttons, quotes, tables, separators, spacers, groups and columns.
// The entry's own floor stays MinAgentVersionForPageCreate, so an outline of
// headings, paragraphs and lists still runs on older agents; the control
// plane applies this floor per input, at run and again at dispatch. The m162
// usage text names the same release.
const MinAgentVersionForPageLayout = "0.61.160"

// MinAgentVersionForBuilderAdapters is the first agent release whose
// wpmgr/page-create builds a draft with a page builder (editor
// "builder:elementor"): the builder registry, the Elementor adapter and its
// classic mapper. The entry's own floor stays MinAgentVersionForPageCreate;
// the control plane applies this floor to a builder input, at run and again
// at dispatch, so an older agent is never asked to build one. It names the
// release that ships the builder path and moves with that release's number.
const MinAgentVersionForBuilderAdapters = "0.61.161"

// MinAgentVersionForBuilderEdit is the first agent release that ships
// wpmgr/page-structure and wpmgr/page-edit: the structure read, the edit
// operations, the builder document snapshot and its restore, and
// p.allowed_draft_ids. The control plane applies it to both abilities at run
// and again at dispatch, so an older agent is never asked to read or edit a
// page builder's document. m169's catalogue entries carry the same number as
// min_agent_version, and it moves with the release that ships builder edit.
const MinAgentVersionForBuilderEdit = "0.61.162"

// AbilityRunMaxAllowedDraftIDs bounds p.allowed_draft_ids: the control plane
// names at most the one post an input is about, so p stays far under
// AbilityRunMaxPBytes however many drafts WPMgr created on the site.
const AbilityRunMaxAllowedDraftIDs = 1

// ErrAbilityRunMalformed marks a 2xx ability_run reply the control plane
// could not use: a body that did not decode, or an answer for another mode or
// entry. Resending the same call gets the same answer.
var ErrAbilityRunMalformed = errors.New("agentcmd: malformed ability_run answer")

// malformedError carries ErrAbilityRunMalformed without changing the message.
type malformedError struct{ err error }

func (e *malformedError) Error() string { return e.err.Error() }
func (e *malformedError) Unwrap() []error {
	return []error{e.err, ErrAbilityRunMalformed}
}

// Ability-run modes.
const (
	AbilityRunModeRead     = "read"
	AbilityRunModePrecheck = "precheck"
	AbilityRunModeWrite    = "write"
	AbilityRunModeRevert   = "revert"
	AbilityRunModeLedger   = "ledger"
)

// Limits mirrored from the agent (class-ability-run-command.php). The control
// plane refuses to send past them so a call is never refused for size.
const (
	AbilityRunMaxPBytes     = 262144
	AbilityRunMaxEntryBytes = 65536
	AbilityRunMaxInputBytes = 65536
	AbilityRunMaxRouteBytes = 65536
)

// AbilityRunCall is one call. Entry and Input are exact JSON text: they are
// placed into p verbatim as strings, so the bytes the agent hashes are the
// bytes the caller supplied. EntrySHA256 must be the hex sha256 of Entry.
type AbilityRunCall struct {
	Mode        string
	RequestID   uuid.UUID
	Entry       []byte
	EntrySHA256 string
	Input       []byte
	// Expected is required for write and refused for every other mode: the
	// digests the person approved, which the agent recomputes before any
	// effect.
	Expected *AbilityRunExpected
	// Route and RouteSHA256 are wpmgr/rest-read and wpmgr/rest-write's
	// reviewed route row (exact JSON text) and the hex sha256 of those bytes.
	// Empty for every other ability and for revert and ledger.
	Route       []byte
	RouteSHA256 string
	// AllowedDraftIDs is the drafts this control plane names as created by
	// WPMgr's own wpmgr/page-create, for wpmgr/page-structure and
	// wpmgr/page-edit: nil sends nothing; any other value, an empty list
	// included, is sent as p.allowed_draft_ids. At most
	// AbilityRunMaxAllowedDraftIDs ids, each at least 1, and only with read,
	// precheck and write. The agent reads or edits a draft only when its id
	// is in this list and its own records agree.
	AllowedDraftIDs []int64
}

// AbilityRunExpected is write's expected{} member.
//
// page-create sends precheck_digest and preview_digest; rest-write sends
// precheck_digest and base_fingerprint.
type AbilityRunExpected struct {
	PrecheckDigest  string `json:"precheck_digest"`
	PreviewDigest   string `json:"preview_digest,omitempty"`
	BaseFingerprint string `json:"base_fingerprint,omitempty"`
}

// abilityRunP is p's shape. Field order is the wire order. Every value is a
// string, so the encoding is fully determined by encoding/json's string
// escaping, and the digest is taken over the bytes produced here.
type abilityRunP struct {
	Mode        string              `json:"mode"`
	RequestID   string              `json:"request_id"`
	Entry       string              `json:"entry,omitempty"`
	EntrySHA256 string              `json:"entry_sha256,omitempty"`
	Input       string              `json:"input,omitempty"`
	Route       string              `json:"route,omitempty"`
	RouteSHA256 string              `json:"route_sha256,omitempty"`
	Expected    *AbilityRunExpected `json:"expected,omitempty"`
	// AllowedDraftIDs is a pointer so an empty list is sent as [] and an
	// absent one is not sent at all.
	AllowedDraftIDs *[]int64 `json:"allowed_draft_ids,omitempty"`
}

// abilityRunBody is the outer body, exactly {"p": "..."}.
type abilityRunBody struct {
	P string `json:"p"`
}

// SHA256Hex is the lowercase hex sha256 of b.
func SHA256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// BuildAbilityRunParams returns the exact p text and its digest pd. Exported
// so tests (and the shared fixture) can pin the bytes.
func BuildAbilityRunParams(call AbilityRunCall) (p []byte, pd string, err error) {
	switch call.Mode {
	case AbilityRunModeRead, AbilityRunModePrecheck, AbilityRunModeLedger, AbilityRunModeRevert:
		if call.Expected != nil {
			return nil, "", fmt.Errorf("ability_run: expected is only sent with write")
		}
	case AbilityRunModeWrite:
		if call.Expected == nil || !hex64.MatchString(call.Expected.PrecheckDigest) {
			return nil, "", fmt.Errorf("ability_run: write needs expected precheck and preview digests")
		}
		if len(call.Route) == 0 {
			if !hex64.MatchString(call.Expected.PreviewDigest) || call.Expected.BaseFingerprint != "" {
				return nil, "", fmt.Errorf("ability_run: write needs expected precheck and preview digests")
			}
		} else if !hex64.MatchString(call.Expected.BaseFingerprint) || call.Expected.PreviewDigest != "" {
			return nil, "", fmt.Errorf("ability_run: a route write needs expected precheck digest and base fingerprint")
		}
	default:
		return nil, "", fmt.Errorf("ability_run: unknown mode %q", call.Mode)
	}
	if call.Mode == AbilityRunModeRevert && len(call.Input) != 0 {
		// W3: the agent takes the object from its ledger, never from input.
		return nil, "", fmt.Errorf("ability_run: revert takes no input")
	}
	if len(call.Route) != 0 || call.RouteSHA256 != "" {
		switch call.Mode {
		case AbilityRunModeRead, AbilityRunModePrecheck, AbilityRunModeWrite:
		default:
			return nil, "", fmt.Errorf("ability_run: a route is only sent with read, precheck and write")
		}
		if len(call.Route) > AbilityRunMaxRouteBytes || !json.Valid(call.Route) || !isJSONObject(call.Route) {
			return nil, "", fmt.Errorf("ability_run: route must be JSON object text of at most %d bytes", AbilityRunMaxRouteBytes)
		}
		if SHA256Hex(call.Route) != call.RouteSHA256 {
			return nil, "", fmt.Errorf("ability_run: route_sha256 does not match the route bytes")
		}
	}
	if call.RequestID == uuid.Nil {
		return nil, "", fmt.Errorf("ability_run: request_id is required")
	}
	pv := abilityRunP{Mode: call.Mode, RequestID: call.RequestID.String()}
	if call.Mode != AbilityRunModeLedger {
		if len(call.Entry) == 0 || len(call.Entry) > AbilityRunMaxEntryBytes {
			return nil, "", fmt.Errorf("ability_run: entry must be 1..%d bytes", AbilityRunMaxEntryBytes)
		}
		if !json.Valid(call.Entry) || !isJSONObject(call.Entry) {
			return nil, "", fmt.Errorf("ability_run: entry must be JSON object text")
		}
		if got := SHA256Hex(call.Entry); got != call.EntrySHA256 {
			return nil, "", fmt.Errorf("ability_run: entry_sha256 does not match the entry bytes")
		}
		input := call.Input
		if len(input) == 0 && call.Mode != AbilityRunModeRevert {
			input = []byte("{}")
		}
		if len(input) > AbilityRunMaxInputBytes {
			return nil, "", fmt.Errorf("ability_run: input exceeds %d bytes", AbilityRunMaxInputBytes)
		}
		if call.Mode != AbilityRunModeRevert && (!json.Valid(input) || !isJSONObject(input)) {
			return nil, "", fmt.Errorf("ability_run: input must be JSON object text")
		}
		pv.Entry = string(call.Entry)
		pv.EntrySHA256 = call.EntrySHA256
		pv.Input = string(input)
		pv.Route = string(call.Route)
		pv.RouteSHA256 = call.RouteSHA256
		pv.Expected = call.Expected
	}
	if call.AllowedDraftIDs != nil {
		switch call.Mode {
		case AbilityRunModeRead, AbilityRunModePrecheck, AbilityRunModeWrite:
		default:
			return nil, "", fmt.Errorf("ability_run: allowed_draft_ids is only sent with read, precheck and write")
		}
		if len(call.AllowedDraftIDs) > AbilityRunMaxAllowedDraftIDs {
			return nil, "", fmt.Errorf("ability_run: allowed_draft_ids holds at most %d ids", AbilityRunMaxAllowedDraftIDs)
		}
		ids := make([]int64, 0, len(call.AllowedDraftIDs))
		for _, id := range call.AllowedDraftIDs {
			if id < 1 {
				return nil, "", fmt.Errorf("ability_run: allowed_draft_ids holds only post ids of at least 1")
			}
			ids = append(ids, id)
		}
		pv.AllowedDraftIDs = &ids
	}
	p, err = json.Marshal(pv)
	if err != nil {
		return nil, "", fmt.Errorf("ability_run: marshal p: %w", err)
	}
	if len(p) > AbilityRunMaxPBytes {
		return nil, "", fmt.Errorf("ability_run: p exceeds %d bytes", AbilityRunMaxPBytes)
	}
	return p, SHA256Hex(p), nil
}

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

func isJSONObject(b []byte) bool {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		case '{':
			return true
		default:
			return false
		}
	}
	return false
}

// AbilityRunResponse is the agent's reply. When OK is false the refusal
// fields (Code, Detail, Retryable) are set instead of the mode's fields.
type AbilityRunResponse struct {
	OK      bool   `json:"ok"`
	Outcome string `json:"outcome"`
	Mode    string `json:"mode"`
	Ability string `json:"ability"`

	// read
	EntrySHA256 string          `json:"entry_sha256"`
	Output      json.RawMessage `json:"output"`

	// precheck and ledger
	RequestID       string          `json:"request_id"`
	Valid           bool            `json:"valid"`
	BaseFingerprint string          `json:"base_fingerprint"`
	PreviewDigest   string          `json:"preview_digest"`
	PrecheckDigest  string          `json:"precheck_digest"`
	Found           bool            `json:"found"`
	Inflight        bool            `json:"inflight"`
	Result          json.RawMessage `json:"result"`
	// Preview is precheck's rendered page (post_type, editor, status,
	// title, content). Site-rendered bytes of OUR builder; still untrusted.
	Preview json.RawMessage `json:"preview"`

	// write, revert and ledger
	PostID        int64  `json:"post_id"`
	CreatedPostID *int64 `json:"created_post_id"`
	// SnapshotSHA256 is an applied wpmgr/page-edit's hash of the copy the
	// agent kept before the change, which the person's undo sends back.
	SnapshotSHA256 string `json:"snapshot_sha256,omitempty"`
	Phase          string `json:"phase"`
	UndoState      string `json:"undo_state"`
	Trashed        bool   `json:"trashed"`
	AfterFP        string `json:"after_fp"`

	// refusal
	Code       string          `json:"code,omitempty"`
	Detail     string          `json:"detail,omitempty"`
	Retryable  bool            `json:"retryable,omitempty"`
	Violations json.RawMessage `json:"violations,omitempty"`

	// vendor and core reads (E3): the resolved owner and the abilities the
	// call invoked on success; side_effects and error_code on a refusal.
	Owner            *AbilityRunOwner `json:"owner,omitempty"`
	AbilitiesInvoked []string         `json:"abilities_invoked,omitempty"`
	SideEffects      json.RawMessage  `json:"side_effects,omitempty"`
	ErrorCode        string           `json:"error_code,omitempty"`

	// rest-read and rest-write (E3-A3): the route the agent ran, and
	// precheck's target facts and changes. Every string in TargetFacts and
	// Changes is SITE TEXT.
	RouteID     string          `json:"route_id,omitempty"`
	RouteSHA256 string          `json:"route_sha256,omitempty"`
	TargetFacts json.RawMessage `json:"target_facts,omitempty"`
	Changes     json.RawMessage `json:"changes,omitempty"`
	UndoExact   *bool           `json:"undo_exact,omitempty"`
	Live        *bool           `json:"live,omitempty"`
	Restored    *bool           `json:"restored,omitempty"`
	Exact       *bool           `json:"exact,omitempty"`
	// Columns is side_effect_detected's list of changed post columns, a
	// closed label set (anything else becomes "unknown").
	Columns json.RawMessage `json:"columns,omitempty"`
	// ColumnsStillChanged names the post columns a failed rest-write, or a
	// person's revert of one, left different from before the write: the
	// title and excerpt were put back, these were not. Same closed label
	// set as Columns. Changed is false on a failed write that changed
	// nothing, so there was nothing to restore.
	ColumnsStillChanged json.RawMessage `json:"columns_still_changed,omitempty"`
	Changed             *bool           `json:"changed,omitempty"`

	// Raw is the exact reply body, for the strict vendor read decode.
	Raw json.RawMessage `json:"-"`
}

// AbilityRunRefusal is an ok=false reply. Code is from the closed set below
// (anything else becomes "unknown"); Detail is site text, cleaned and capped.
type AbilityRunRefusal struct {
	Code      string
	Detail    string
	Retryable bool
	// PostID and Trashed are set on a write's verify_mismatch and on a
	// snapshot_failed that came after the insert: the draft it created and
	// whether the site trashed it.
	PostID  int64
	Trashed bool
	// Violations are the agent's fixed guard labels (ability_intercepted,
	// nested_ability_refused, read_side_effect_detected).
	Violations []string
	// SideEffects is read_side_effect_detected's record. Its option names and
	// hosts are SITE TEXT.
	SideEffects *AbilityRunSideEffects
	// ErrorCode is ability_failed's code from the ability itself: SITE TEXT,
	// cleaned and capped.
	ErrorCode string
	// Columns are side_effect_detected's changed post columns, from a
	// closed set.
	Columns []string
	// Restored, Exact and ColumnsStillChanged are a failed rest-write's
	// own-undo report. Restored is nil when the agent sent none, and also
	// when it said the write changed nothing (there was nothing to put
	// back). ColumnsStillChanged is from the closed post column set.
	Restored            *bool
	Exact               *bool
	ColumnsStillChanged []string
}

func (e *AbilityRunRefusal) Error() string {
	return fmt.Sprintf("ability_run refused by agent: %s", e.Code)
}

// AbilityRunRefusalCodes is the closed set of refusal codes the agent emits
// (class-ability-run-command.php and the own abilities).
var AbilityRunRefusalCodes = map[string]struct{}{
	"ability_denied":                 {},
	"bad_expected":                   {},
	"conflict":                       {},
	"content_editing_not_enabled":    {},
	"create_content_invalid":         {},
	"created_post_missing":           {},
	"created_post_published":         {},
	"created_post_touched":           {},
	"editor_unavailable":             {},
	"entry_approval_invalid":         {},
	"ledger_ability_mismatch":        {},
	"not_revertible":                 {},
	"nothing_to_revert":              {},
	"preview_changed":                {},
	"principal_capabilities_drifted": {},
	"principal_create_failed":        {},
	"principal_login_taken":          {},
	"principal_missing":              {},
	"refused_by_site":                {},
	"request_in_flight":              {},
	"revert_failed":                  {},
	"sanitiser_changed_new_content":  {},
	"snapshot_failed":                {},
	"snapshot_strategy_invalid":      {},
	"target_in_flight":               {},
	"verify_mismatch":                {},
	"ability_disabled":               {},
	"ability_intercepted":            {},
	"ability_not_admitted":           {},
	"ability_not_runnable_yet":       {},
	"ability_unknown":                {},
	"bad_ability_name":               {},
	"bad_entry":                      {},
	"bad_input":                      {},
	"bad_mode":                       {},
	"bad_params":                     {},
	"bad_request_id":                 {},
	"disabled_on_site":               {},
	"entry_source_mismatch":          {},
	"integration_entry_changed":      {},
	"internal":                       {},
	"mode_class_mismatch":            {},
	"mode_not_available":             {},
	"output_too_large":               {},
	"params_too_large":               {},
	"post_not_readable":              {},
	"token_params_mismatch":          {},
	// Vendor and core reads (E3, agent 0.61.158).
	"vendor_writes_not_in_this_version": {},
	"permission_mode_not_assertable":    {},
	"wp_too_old_for_vendor_reads":       {},
	"ability_not_on_site":               {},
	"ability_class_overridden":          {},
	"ability_owner_split":               {},
	"ability_owner_mismatch":            {},
	"builder_version_unverified":        {},
	"ability_schema_changed":            {},
	"ability_input_invalid":             {},
	"read_side_effect_detected":         {},
	"principal_switched":                {},
	"nested_ability_refused":            {},
	"ability_permission_denied":         {},
	"ability_failed":                    {},
	"ability_output_invalid":            {},
	// REST routes (E3-A3, agent 0.61.158).
	"route_not_reviewed":           {},
	"route_disabled":               {},
	"route_entry_changed":          {},
	"route_namespace_refused":      {},
	"route_param_invalid":          {},
	"route_key_not_allowed":        {},
	"route_key_forbidden":          {},
	"route_wp_version_unsupported": {},
	"rest_handler_not_core":        {},
	"rest_intercepted":             {},
	"rest_error":                   {},
	"rest_not_published":           {},
	"post_not_editable":            {},
	"sanitiser_changed_value":      {},
	"side_effect_detected":         {},
	"post_touched":                 {},
	"post_content_would_change":    {},
	"post_scheduled":               {},
	// wpmgr/page-create outline grammar v2 (MinAgentVersionForPageLayout).
	"layout_invalid":            {},
	"link_invalid":              {},
	"layout_needs_block_editor": {},
	"image_not_available":       {},
	"image_url_unusable":        {},
	// wpmgr/page-create with a page builder (MinAgentVersionForBuilderAdapters).
	"builder_not_enabled":           {},
	"builder_not_available":         {},
	"node_not_supported_by_builder": {},
	"image_alt_from_library":        {},
	"leaf_unsafe":                   {},
	"adapter_key_not_allowed":       {},
	"page_has_unknown_elements":     {},
	"builder_would_change_layout":   {},
	"builder_save_refused":          {},
	"builder_crashed":               {},
	// wpmgr/page-structure and wpmgr/page-edit (MinAgentVersionForBuilderEdit).
	"ops_invalid":                 {},
	"node_not_found":              {},
	"node_not_editable":           {},
	"op_not_supported_by_builder": {},
	"page_too_large":              {},
	"target_not_eligible":         {},
	"page_has_admin_only_content": {},
	// wpmgr/page-edit's write and its undo (MinAgentVersionForBuilderEdit).
	"snapshot_too_large": {},
	"restore_mismatch":   {},
	"data_unreadable":    {},
	"snapshot_tampered":  {},
}

var abilityRunCodeRe = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)

// AbilityRun sends one signed ability_run call. An ok=false reply is returned
// with a *AbilityRunRefusal. A transport failure is returned as the usual
// command error (markNotSent where nothing left this process).
func (c *Client) AbilityRun(ctx context.Context, siteID uuid.UUID, siteURL string, call AbilityRunCall) (AbilityRunResponse, error) {
	var out AbilityRunResponse
	p, pd, err := BuildAbilityRunParams(call)
	if err != nil {
		return out, markNotSent(err)
	}
	body, err := json.Marshal(abilityRunBody{P: string(p)})
	if err != nil {
		return out, markNotSent(fmt.Errorf("marshal ability_run body: %w", err))
	}
	endpoint, err := joinCommandURL(siteURL, CmdAbilityRun)
	if err != nil {
		return out, markNotSent(err)
	}
	token, _, err := c.signer.MintParamsBound(c.clock(), siteID.String(), CmdAbilityRun, pd)
	if err != nil {
		return out, markNotSent(fmt.Errorf("mint command jwt: %w", err))
	}
	data, err := c.sendSigned(ctx, endpoint, CmdAbilityRun, body, token)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return AbilityRunResponse{}, &malformedError{fmt.Errorf("decode ability_run response: %w", err)}
	}
	out.Raw = append(json.RawMessage(nil), data...)
	if !out.OK {
		return out, abilityRunRefusalOf(out)
	}
	if out.Mode != call.Mode {
		return AbilityRunResponse{}, &malformedError{fmt.Errorf("ability_run: agent answered mode %q for %q", out.Mode, call.Mode)}
	}
	if call.Mode == AbilityRunModeRead && out.EntrySHA256 != call.EntrySHA256 {
		return AbilityRunResponse{}, &malformedError{errors.New("ability_run: agent answered for a different entry")}
	}
	return out, nil
}

func abilityRunRefusalOf(out AbilityRunResponse) *AbilityRunRefusal {
	code := out.Code
	if _, ok := AbilityRunRefusalCodes[code]; !ok || !abilityRunCodeRe.MatchString(code) {
		code = "unknown"
	}
	return &AbilityRunRefusal{
		Code:        code,
		Detail:      humantext.CapBytes(humantext.Clean(out.Detail), 200),
		Retryable:   out.Retryable,
		PostID:      out.PostID,
		Trashed:     out.Trashed,
		Violations:  decodeRefusalViolations(out.Code, out.Violations),
		Columns:     DecodePostColumns(out.Columns),
		SideEffects: decodeSideEffects(out.SideEffects),
		ErrorCode:   humantext.CapBytes(humantext.Clean(out.ErrorCode), vendorErrorCodeBytes),

		Restored:            RestoreReport(out.Restored, out.Changed),
		Exact:               out.Exact,
		ColumnsStillChanged: DecodePostColumns(out.ColumnsStillChanged),
	}
}

// RestoreReport is a failed write's restored flag as recorded: nil when the
// agent sent none or said the write changed nothing, else the agent's flag.
func RestoreReport(restored, changed *bool) *bool {
	if restored == nil || (changed != nil && !*changed) {
		return nil
	}
	v := *restored
	return &v
}
