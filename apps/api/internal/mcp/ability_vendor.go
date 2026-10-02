package mcp

import (
	"bytes"
	"context"
	"log/slog"
	"regexp"
	"strings"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/wpversion"
)

// ---------------------------------------------------------------------------
// Vendor and core READ entries (engine slice E3).
//
// A reviewed read from a plugin, theme or WordPress core runs when the
// catalogue entry and the site's last inventory agree: the entry is an
// admitted, enabled read under the principal's own permissions; the site
// reported the ability; its owner kind and directory are the entry's; its
// owner version is inside the entry's reviewed range; its structural schema
// hash is the entry's; WordPress is 7.1 or later; and the agent is at least
// MinAgentVersionForVendorReads. The agent re-checks every one of these
// against the live ability before it calls it.
// ---------------------------------------------------------------------------

// Vendor not-runnable reason codes (closed; added to the E1 set).
const (
	notRunnableVersionUnverified = "builder_version_unverified"
	notRunnableSchemaChanged     = "ability_schema_changed"
	notRunnableWPTooOld          = "wp_too_old_for_vendor_reads"
	notRunnablePermissionMode    = "permission_mode_not_assertable"
)

// MinWPVersionForVendorReads is the WordPress floor for vendor and core reads:
// the interception guards need the 7.1 filters (owner ruling 2: not available
// below it).
const MinWPVersionForVendorReads = "7.1"

// notRunnableCopy is our plain text for each not-runnable reason, for the
// operator and the AI alike. No site text appears in it.
var notRunnableCopy = map[string]string{
	notRunnableNotReviewed:       "WPMgr has not reviewed this tool, so it cannot run.",
	notRunnableDenied:            "WPMgr does not allow this tool.",
	notRunnableNotAdmitted:       "This tool is not approved for use yet.",
	notRunnableDisabled:          "This tool is switched off in WPMgr.",
	notRunnableNotYet:            "WPMgr can read with this tool but not change anything yet.",
	notRunnableWritesOff:         "Changes through this tool are not available.",
	notRunnableOwnerMismatch:     "The tool on this site does not come from the plugin WPMgr reviewed.",
	notRunnableAgentOutdated:     "This site's WPMgr agent is too old to run this tool. Update the agent.",
	notRunnableNotOnSite:         "This site does not offer this tool.",
	notRunnableNotInventoried:    "WPMgr has not read this site's tools yet. A check has been queued.",
	notRunnableVersionUnverified: "The installed version of the plugin that provides this tool has not been reviewed.",
	notRunnableSchemaChanged:     "This tool's inputs on this site differ from the version WPMgr reviewed.",
	notRunnableWPTooOld:          "This tool needs WordPress 7.1 or later.",
	notRunnablePermissionMode:    "This tool can run only with WPMgr's own limited permissions.",
}

// agentRefusalCopy is our plain text for each refusal code the agent can
// return from a read. The agent's detail is never shown: it is site text.
var agentRefusalCopy = map[string]string{
	"entry_source_mismatch":             "The site refused the call: the tool's origin did not match WPMgr's review.",
	"snapshot_strategy_invalid":         "The site refused the call: the review has no undo plan for changes.",
	"vendor_writes_not_in_this_version": "WPMgr can read with this tool but not change anything yet.",
	"permission_mode_not_assertable":    "This tool can run only with WPMgr's own limited permissions.",
	"bad_entry":                         "The site refused WPMgr's review record for this tool.",
	"wp_too_old_for_vendor_reads":       "This tool needs WordPress 7.1 or later.",
	"bad_input":                         "The site refused the input.",
	"ability_not_on_site":               "This site does not offer this tool any more.",
	"ability_class_overridden":          "Another plugin has replaced how this tool runs, so WPMgr refused to run it.",
	"ability_owner_split":               "Parts of this tool come from different plugins, so WPMgr refused to run it.",
	"ability_owner_mismatch":            "The tool on this site does not come from the plugin WPMgr reviewed.",
	"builder_version_unverified":        "The installed version of the plugin that provides this tool has not been reviewed.",
	"ability_schema_changed":            "This tool's inputs on this site differ from the version WPMgr reviewed.",
	"ability_input_invalid":             "The tool refused the input.",
	"content_editing_not_enabled":       "Content editing is not switched on for this site.",
	"principal_capabilities_drifted":    "WPMgr's service account on this site has changed permissions, so nothing ran.",
	"read_side_effect_detected":         "The tool tried to change the site or contact another server during a read. WPMgr withheld its result and reported it.",
	"principal_switched":                "The signed-in user changed during the call, so WPMgr withheld the result.",
	"ability_intercepted":               "Another plugin interfered with the call, so WPMgr withheld the result.",
	"nested_ability_refused":            "The tool tried to use another tool WPMgr has not allowed.",
	"ability_permission_denied":         "The tool refused WPMgr's service account permission.",
	"ability_failed":                    "The tool reported an error.",
	"ability_output_invalid":            "The tool returned a result WPMgr could not read.",
	"output_too_large":                  "The tool's result was too large to return.",
	"internal":                          "The site hit an internal error.",
}

func notRunnableText(code string) string {
	if t, ok := notRunnableCopy[code]; ok {
		return t
	}
	return msgAbilityNotRunnable
}

// expectedOwnerKind is the owner kind an entry implies. The catalogue has no
// owner kind column: a core entry is owned by core, every other by a plugin
// (the agent applies the same default).
func expectedOwnerKind(e *sqlc.AbilityCatalogue) string {
	if e.Source == "core" {
		return "core"
	}
	return "plugin"
}

func strOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// versionInEntryRange reports whether v is inside [version_min,
// version_max_tested], both inclusive. A missing bound or version is not.
func versionInEntryRange(v *string, e *sqlc.AbilityCatalogue) bool {
	if v == nil || *v == "" || e.VersionMin == nil || *e.VersionMin == "" ||
		e.VersionMaxTested == nil || *e.VersionMaxTested == "" {
		return false
	}
	return wpversion.Compare(*v, *e.VersionMin) >= 0 && wpversion.Compare(*v, *e.VersionMaxTested) <= 0
}

// bareSHA strips the agent's "sha256:" prefix; the catalogue holds bare hex.
func bareSHA(s string) string { return strings.TrimPrefix(strings.TrimSpace(s), "sha256:") }

var wpVersionShape = regexp.MustCompile(`^[0-9]+(\.[0-9]+){1,3}`)

// wpMeetsVendorFloor reports whether the site's WordPress version is at
// least 7.1. An empty or malformed version does not.
func wpMeetsVendorFloor(v string) bool {
	v = strings.TrimSpace(v)
	if !wpVersionShape.MatchString(v) {
		return false
	}
	return wpversion.Compare(v, MinWPVersionForVendorReads) >= 0
}

// vendorReadRunnable is classify's vendor/core READ branch. It returns the
// not-runnable reason, or "" when the read may be sent.
func vendorReadRunnable(e *sqlc.AbilityCatalogue, inv *sqlc.SiteAbilityInventory, agentVersion, wpVersion string) string {
	switch {
	case e.PermissionMode != "principal":
		return notRunnablePermissionMode
	case len(bytes.TrimSpace(e.OutputFields)) == 0:
		return notRunnableNotAdmitted
	case e.SchemaStructSha256 == nil || *e.SchemaStructSha256 == "":
		return notRunnableNotAdmitted
	case inv == nil:
		return notRunnableNotOnSite
	case inv.OwnerOk == nil || !*inv.OwnerOk:
		return notRunnableOwnerMismatch
	case inv.OwnerKind != expectedOwnerKind(e) || strOrEmpty(inv.OwnerDir) != strOrEmpty(e.OwnerDir):
		return notRunnableOwnerMismatch
	case !versionInEntryRange(inv.OwnerVersion, e):
		return notRunnableVersionUnverified
	}
	if _, err := parseOutShape(e.OutputFields); err != nil {
		return notRunnableNotAdmitted
	}
	// The inventory hash is taken with no dynamic enum paths; an entry that
	// names some is checked by the agent alone.
	if len(e.DynamicEnumPaths) == 0 &&
		(inv.SchemaStructSha256 == nil || bareSHA(*inv.SchemaStructSha256) != bareSHA(*e.SchemaStructSha256)) {
		return notRunnableSchemaChanged
	}
	if !wpMeetsVendorFloor(wpVersion) {
		return notRunnableWPTooOld
	}
	if !vendorAgentMeetsFloor(agentVersion, e.MinAgentVersion) {
		return notRunnableAgentOutdated
	}
	return ""
}

func vendorAgentMeetsFloor(v string, entryMin *string) bool {
	if !abilityAgentMeetsFloor(v, entryMin) {
		return false
	}
	return wpversion.Compare(strings.TrimSpace(v), agentcmd.MinAgentVersionForVendorReads) >= 0
}

// ---------------------------------------------------------------------------
// The pinned output shape (PR1): the outShape grammar, strict.
//
//	{"fields":{"<key>":<shape>,...}} | {"items":<shape>} | "string" | "int" | "bool"
//
// Keys match ^[A-Za-z0-9_-]{1,64}$, depth is at most 8 (the m158 CHECK), and
// any other node is refused. The admin write refuses a bad shape; a bad shape
// at run time withholds the output.
// ---------------------------------------------------------------------------

// parseOutShape parses output_fields with agentcmd.ParseOutputShape (the one
// grammar the admin write also uses) and converts it to the projector's tree.
func parseOutShape(raw []byte) (*outShape, error) {
	t, err := agentcmd.ParseOutputShape(raw)
	if err != nil {
		return nil, err
	}
	return outShapeOf(t), nil
}

func outShapeOf(t *agentcmd.OutputShape) *outShape {
	switch {
	case t.Fields != nil:
		f := make(map[string]*outShape, len(t.Fields))
		for k, sub := range t.Fields {
			f[k] = outShapeOf(sub)
		}
		return &outShape{fields: f}
	case t.Items != nil:
		return &outShape{items: outShapeOf(t.Items)}
	}
	return &outShape{scalar: t.Scalar}
}

// ---------------------------------------------------------------------------
// Agent refusals of a vendor read: plain text, site text fenced.
// ---------------------------------------------------------------------------

// vendorRefusal turns the agent's refusal into the model-facing refusal.
// Our sentence is the message; anything the site supplied (error_code,
// option names, hosts) is in from_the_site, fenced, and never in our text.
func vendorRefusal(r *agentcmd.AbilityRunRefusal) *toolRefusal {
	msg, ok := agentRefusalCopy[r.Code]
	if !ok {
		msg = msgAbilityAgentRefused
	}
	details := map[string]any{"code": r.Code, "retryable": r.Retryable}
	meta := map[string]any{"code": r.Code}
	if len(r.Violations) > 0 {
		details["violations"] = r.Violations
		meta["violations"] = r.Violations
	}
	fromSite := map[string]any{}
	if r.ErrorCode != "" {
		fromSite["error_code"] = fenceSiteText(r.ErrorCode)
	}
	if se := r.SideEffects; se != nil {
		fenced := map[string]any{
			"options":    fenceSiteList(se.Options),
			"http_hosts": fenceSiteList(se.HTTPHosts),
			"posts":      se.Posts, "roles": se.Roles, "users": se.Users,
			"blocked": se.Blocked,
		}
		fromSite["side_effects"] = fenced
		meta["side_effects"] = fenced
	}
	if len(fromSite) > 0 {
		details["from_the_site"] = fromSite
	}
	return &toolRefusal{
		reason: reasonAbilityAgentRefused,
		err:    domain.Validation(ErrCodeInvalidToolArguments, msg).WithDetails(details),
		meta:   meta,
	}
}

func fenceSiteList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, fenceSiteText(s))
	}
	return out
}

// ActionAbilityReadSideEffect is the audit action for a vendor read that
// changed the site or called out (owner ruling 4).
const ActionAbilityReadSideEffect = "ability.read_side_effect"

// reportReadSideEffect records a read_side_effect_detected refusal: an audit
// row of its own and a structured error line marked for the superadmin. The
// tool call's own denial row carries the same side effects in its metadata.
// Best effort: the call is refused whatever this does.
func (s *Service) reportReadSideEffect(ctx context.Context, auth AuthorizedRequest, site abilitySite, e *sqlc.AbilityCatalogue, r *agentcmd.AbilityRunRefusal) {
	ref := vendorRefusal(r)
	meta := map[string]any{
		"ability": e.Name, "entry_id": e.EntryID.String(), "site_id": site.row.ID.String(),
		"grant_id": auth.GrantID.String(), "side_effects": ref.meta["side_effects"],
	}
	if v, ok := ref.meta["violations"]; ok {
		meta["violations"] = v
	}
	slog.ErrorContext(ctx, "ability read side effect detected",
		slog.String("alert", "superadmin"),
		slog.String("action", ActionAbilityReadSideEffect),
		slog.String("ability", e.Name),
		slog.String("entry_id", e.EntryID.String()),
		slog.String("tenant_id", auth.TenantID.String()),
		slog.String("site_id", site.row.ID.String()),
	)
	if s.audit == nil {
		return
	}
	if _, err := s.audit.RecordOrFail(ctx, audit.Event{
		TenantID:   auth.TenantID,
		ActorType:  audit.ActorAssistant,
		ActorID:    auth.GrantID.String(),
		Action:     ActionAbilityReadSideEffect,
		TargetType: "ability_catalogue_entry",
		TargetID:   e.EntryID.String(),
		Metadata:   meta,
	}); err != nil {
		slog.ErrorContext(ctx, "ability read side effect: audit row not written",
			slog.String("entry_id", e.EntryID.String()), slog.Any("error", err))
	}
}
