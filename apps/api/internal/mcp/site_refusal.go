package mcp

// The answer the AI gets when the site refuses a wpmgr/page-create or
// wpmgr/page-edit precheck. Everything in it is ours or a member of a closed
// set: the agent's refusal code (agentcmd.AbilityRunRefusalCodes, anything
// else is unknown), for a page-edit conflict the reason (the closed set
// below), and the fixed hint for them. The site's own detail text is
// untrusted and never reaches the answer. Display-rule guidance includes
// only a rule substring validated against the rule grammar.

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/humantext"
)

// The reasons a wpmgr/page-edit is refused as a conflict: the closed set the
// agent answers with in a conflict's detail. They are the only values a
// conflict's reason, and a request's outcome_detail, carry.
const (
	// PageEditConflictChangedSinceRead: the page changed after the AI read it.
	PageEditConflictChangedSinceRead = "changed_since_read"
	// PageEditConflictEditorOpen: someone has the page open in an editor
	// (Elementor or the WordPress editor).
	PageEditConflictEditorOpen = "editor_open"
	// PageEditConflictAutosavePending: someone has unsaved Elementor changes
	// on it.
	PageEditConflictAutosavePending = "autosave_pending"
)

// pageEditCodeConflict is the agent's refusal code for a page edit that
// conflicts with the page as it is now; its detail names the reason.
const pageEditCodeConflict = "conflict"

// unknownRefusalCode is the code of a refusal outside the agent's closed set.
const unknownRefusalCode = "unknown"

// Fixed hints for a page-edit conflict, one per reason and one for a
// conflict that names none.
const (
	hintPageEditConflictEditorOpen = "Someone has this page open in an editor (Elementor or the WordPress " +
		"editor). Wait three minutes, then send the same call again: WordPress frees a page about two and a half " +
		"minutes after its editor is closed. If it is refused for this reason three times, stop and ask the " +
		"person to close the page in that editor."
	hintPageEditConflictChangedSinceRead = "The page changed after you read it. Read the page again with " +
		"wpmgr/page-structure, then send the edit with the base_fingerprint and refs that read gives."
	hintPageEditConflictAutosavePending = "Someone has unsaved Elementor changes on this page. Ask the person to " +
		"save or discard their changes in Elementor, then read the page again with wpmgr/page-structure before " +
		"you edit it."
	hintPageEditConflict = "The page is open in Elementor or changed after you read it. Read the page again with " +
		"wpmgr/page-structure before you edit it, and if someone has it open in Elementor, ask them to close it."
)

// pageEditConflictHints maps each conflict reason to its fixed hint.
var pageEditConflictHints = map[string]string{
	PageEditConflictChangedSinceRead: hintPageEditConflictChangedSinceRead,
	PageEditConflictEditorOpen:       hintPageEditConflictEditorOpen,
	PageEditConflictAutosavePending:  hintPageEditConflictAutosavePending,
}

// PageEditConflictReason is detail as a page-edit conflict reason: ok only
// for a member of the closed set, compared exactly.
func PageEditConflictReason(detail string) (string, bool) {
	if _, ok := pageEditConflictHints[detail]; !ok {
		return "", false
	}
	return detail, true
}

// msgSitePrecheckRefused opens the message of every site precheck refusal.
const msgSitePrecheckRefused = "the site refused the call, so nothing was asked"

// sitePrecheckRefusal is the refusal for a wpmgr/page-create or
// wpmgr/page-edit precheck the site refused. Its details are the code,
// whether a retry can help and the hint when there is one; a page-edit
// conflict adds its reason, and only editor_open is retryable, whatever the
// agent's own flag says. The message names the code and the reason and
// carries the hint, so a client that shows only the message still has them.
func sitePrecheckRefusal(ability string, refusal *agentcmd.AbilityRunRefusal, site ...string) *toolRefusal {
	code := refusal.Code
	if _, ok := agentcmd.AbilityRunRefusalCodes[code]; !ok {
		code = unknownRefusalCode
	}
	details := map[string]any{"code": code, "retryable": refusal.Retryable}
	reason, hint := "", precheckRefusalHint(code)
	label := "this site"
	if len(site) > 0 && site[0] != "" {
		label = site[0]
	}
	if message := ThemeTemplateRefusalMessage(code, refusal.Detail, label); message != "" {
		hint = message
	}
	if ability == AbilityPageEdit && code == pageEditOpsInvalid && (refusal.Detail == pageEditMixedHint || refusal.Detail == pageEditNativeSectionHint) {
		hint = refusal.Detail
		details["retryable"] = false
	}
	if ability == AbilityPageEdit && code == pageEditCodeConflict {
		reason, _ = PageEditConflictReason(refusal.Detail)
		hint = hintPageEditConflict
		if reason != "" {
			hint = pageEditConflictHints[reason]
			details["reason"] = reason
		}
		details["retryable"] = reason == PageEditConflictEditorOpen
	}
	if hint != "" {
		details["hint"] = hint
	}
	msg := msgSitePrecheckRefused + ": code " + code
	if reason != "" {
		msg += ", reason " + reason
	}
	if hint != "" {
		msg += ". " + hint
	}
	return &toolRefusal{
		reason: reasonAbilityPrecheckRefused,
		err:    domain.Validation(ErrCodeInvalidToolArguments, msg).WithDetails(details),
		meta:   map[string]any{"code": code},
	}
}

// nativeRefusalHints give a next step without relaying site-supplied details.
var nativeRefusalHints = map[string]string{
	"widget_registry_unreadable":     "Ask the person to check the site's widget registry, then read wpmgr/elementor-widgets again.",
	"widget_not_found":               "Read wpmgr/elementor-widgets and choose a widget name present in that catalogue.",
	"atomic_not_supported":           "Read wpmgr/elementor-widgets and choose a usable classic widget; atomic widgets cannot be edited yet.",
	"widget_not_usable":              "Read wpmgr/elementor-widgets and choose a widget marked usable.",
	"widget_schema_unreadable":       "Ask the person to check this widget on the site, then read wpmgr/elementor-widget-schema again.",
	"elementor_controls_unavailable": "Ask the person to update to a newer Elementor for native widget edits, or use the non-native page edit operations.",
	"control_not_found":              "Read the widget's skeleton with wpmgr/elementor-widget-schema, then request only the control ids it lists.",
	"native_settings_invalid":        "Read wpmgr/elementor-widget-schema with widget; send the ids of the controls you are writing in controls and follow write_rules, since defaults are stored shapes. Write scalar values as strings, numbers as numeric strings, and flags in the control's exact shape; null only deletes a top-level settings key.",
	"native_designed_node":           "Read wpmgr/page-structure and edit this designed node through its design or patterns rather than native settings or duplication.",
	"native_node_shape":              "Read wpmgr/page-structure again; this widget has a stored shape native editing cannot safely change.",
	"native_duplicate_refused":       "Choose a widget subtree without protected content, or add a new widget instead.",
	"native_preview_too_large":       "Use a smaller edit or reduce the page before trying again.",
	"preview_unreadable":             "Read the page again and retry; the site could not produce a readable preview.",
}

// ThemeTemplateRefusalMessage uses fixed copy and only a validated display rule.
func ThemeTemplateRefusalMessage(code, detail, site string) string {
	site = humantext.CapRunes(humantext.Clean(site), 255)
	prefix := "shows_on"
	if rule := firstThemeTemplateRule(detail); rule != "" {
		prefix += ": " + rule
	}
	switch code {
	case "refused_published":
		return "A person published this draft, scheduled it or made it private, so it cannot be undone. Change it in WordPress."
	case "target_not_draft":
		return "This target is no longer a draft, so it cannot be undone."
	case "theme_builder_unavailable":
		return "Headers and footers need Elementor Pro with its Theme Builder, and it isn't active on " + site + ". Nothing was changed."
	case "shows_on_place_unavailable":
		return prefix + " is not a place this site's Elementor Pro can show a template. Read wpmgr/elementor-theme-templates for the places it offers."
	case "shows_on_target_missing":
		return prefix + " names a page or post this site does not have, or of the wrong type."
	case "side_effect_detected":
		if detail == "Elementor Pro changed its live display list for a draft" {
			return "Elementor Pro changed its live display list for a draft. Nothing was kept."
		}
	case "verify_mismatch":
		if strings.HasPrefix(detail, "shows_on differs; Elementor Pro reports:") || detail == "shows_on could not be read back through Elementor Pro" || strings.HasPrefix(detail, "shows_on differs; Elementor Pro stored:") || detail == "shows_on could not be verified" || detail == "the created draft is not what was built: shows_on" {
			return "Elementor Pro stored different places than asked. Nothing was kept."
		}
	}
	return ""
}

var themeTemplateRuleToken = regexp.MustCompile(`(?:include|exclude)/[\pL\pN_/-]+`)

func firstThemeTemplateRule(detail string) string {
	for _, at := range themeTemplateRuleToken.FindAllStringIndex(detail, -1) {
		if at[0] > 0 {
			before, _ := utf8.DecodeLastRuneInString(detail[:at[0]])
			if unicode.IsLetter(before) || unicode.IsNumber(before) || strings.ContainsRune("_/-", before) {
				continue
			}
		}
		rule := detail[at[0]:at[1]]
		if themeTemplateRule.MatchString(rule) {
			return rule
		}
	}
	return ""
}
