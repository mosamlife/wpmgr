package abilityrequest

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// The agent's source for an outside change: the save's two details, the
// write scope's fixed labels, and the privilege writes it blocks.
const (
	agentElementorDocument = "../../../agent/includes/abilities/builders/class-elementor-document.php"
	agentWriteScope        = "../../../agent/includes/abilities/class-ability-write-scope.php"
	agentSideEffects       = "../../../agent/includes/abilities/class-ability-side-effects.php"
)

// outsideChangeUnnamed are the labels the agent can report that name no
// kind: a blocked write with no plain label, a switch to another site of the
// network, and the scope giving up on counting the page's own revisions or
// keys. A detail carrying one gives outside_change null.
var outsideChangeUnnamed = map[string]struct{}{
	"blocked_write":             {},
	"blog_switched":             {},
	"too_many_revisions":        {},
	"too_many_target_meta_keys": {},
}

func outsideRow(ability, code, text string) sqlc.AssistantAbilityRequest {
	r := sqlc.AssistantAbilityRequest{AbilityName: ability, State: "failed", Outcome: strp(OutcomeRefused)}
	if code != "" {
		r.OutcomeCode = &code
	}
	if text != "" {
		r.SiteReportedText = &text
	}
	return r
}

// TestOutsideChangeIsOneClosedKind: outside_change names the one kind of
// thing a page edit's save changed outside the page, from the closed set
// only, and is null for everything else.
func TestOutsideChangeIsOneClosedKind(t *testing.T) {
	const scope = "the save wrote outside the page: "
	pe, se := mcp.AbilityPageEdit, "side_effect_detected"
	cases := []struct {
		r    sqlc.AssistantAbilityRequest
		want string
	}{
		{outsideRow(pe, se, "the active kit changed during the save"), "active_kit"},
		{outsideRow(pe, se, scope+"option_written"), "site_settings"},
		{outsideRow(pe, se, scope+"other_post_written"), "other_posts"},
		{outsideRow(pe, se, scope+"other_post_meta_written, other_post_written, post_deleted, post_trashed"), "other_posts"},
		{outsideRow(pe, se, scope+"term_changed"), "terms"},
		{outsideRow(pe, se, scope+"role_changed, site_admins, user_changed"), "users"},
		{outsideRow(pe, se, scope+"user_capabilities_meta, user_level_meta, user_meta_unknown, user_roles_option"), "users"},
		// More than one kind, a label naming none, or a cut list: null.
		{outsideRow(pe, se, scope+"option_written, other_post_written"), "<null>"},
		{outsideRow(pe, se, scope+"option_written, too_many_revisions"), "<null>"},
		{outsideRow(pe, se, scope+"blocked_write"), "<null>"},
		{outsideRow(pe, se, scope+"blog_switched"), "<null>"},
		{outsideRow(pe, se, scope+"option_writ"), "<null>"},
		{outsideRow(pe, se, scope), "<null>"},
		{outsideRow(pe, se, scope+"option_written,other_post_written"), "<null>"},
		// The site's own words, or another sentence: null.
		{outsideRow(pe, se, "a plugin wrote to wp_options"), "<null>"},
		{outsideRow(pe, se, "The active kit changed during the save"), "<null>"},
		{outsideRow(pe, se, "the active kit changed during the save "), "<null>"},
		{outsideRow(pe, se, "building the elements without saving wrote to the site"), "<null>"},
		{outsideRow(pe, se, "site_settings"), "<null>"},
		{outsideRow(pe, se, ""), "<null>"},
		// Another code, or another ability: null.
		{outsideRow(pe, "verify_mismatch", "the active kit changed during the save"), "<null>"},
		{outsideRow(pe, "conflict", scope+"option_written"), "<null>"},
		{outsideRow(pe, "", scope+"option_written"), "<null>"},
		{outsideRow(mcp.AbilityPageCreate, se, "the active kit changed during the save"), "<null>"},
		{outsideRow(mcp.AbilityRestWrite, se, scope+"option_written"), "<null>"},
	}
	for _, c := range cases {
		got := toDTO(c.r, false, "", false, setterNames{}).OutsideChange
		if detailStr(got) != c.want {
			t.Fatalf("%s %s %q: outside_change %s, want %s", c.r.AbilityName, detailStr(c.r.OutcomeCode),
				detailStr(c.r.SiteReportedText), detailStr(got), c.want)
		}
	}
	b, err := json.Marshal(toDTO(outsideRow(pe, "conflict", "editor_open"), false, "", false, setterNames{}))
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	if v, present := wire["outside_change"]; !present || v != nil {
		t.Fatalf("outside_change on the wire is %v (present %v), want null", v, present)
	}
}

// TestOutsideChangeIsKeptByTheOutcomePath: the outcome recording keeps the
// agent's detail for an outside change from a direct answer and from a
// ledger answer after a lost reply, so outside_change can name its kind.
func TestOutsideChangeIsKeptByTheOutcomePath(t *testing.T) {
	now := time.Now()
	for detail, want := range map[string]string{
		"the active kit changed during the save":                                 OutsideChangeActiveKit,
		"the save wrote outside the page: option_written":                        OutsideChangeSiteSettings,
		"the save wrote outside the page: other_post_meta_written, post_trashed": OutsideChangeOtherPosts,
	} {
		direct := classifyWrite(mcp.AbilityPageEdit, agentcmd.AbilityRunResponse{},
			&agentcmd.AbilityRunRefusal{Code: "side_effect_detected", Detail: detail, Restored: boolp(true)}, now)
		raw, err := json.Marshal(map[string]any{"ok": false, "outcome": "refused", "code": "side_effect_detected",
			"detail": detail, "restored": true})
		if err != nil {
			t.Fatal(err)
		}
		stored, ok := outcomeFromStored(mcp.AbilityPageEdit, raw, now)
		if !ok {
			t.Fatalf("%s: the ledger answer was not read", detail)
		}
		for name, oc := range map[string]writeOutcome{"direct": direct, "ledger": stored} {
			r := sqlc.AssistantAbilityRequest{AbilityName: mcp.AbilityPageEdit, State: "failed",
				Outcome: &oc.outcome, OutcomeCode: oc.code, SiteReportedText: oc.siteText}
			if got := outsideChangeFor(r); detailStr(got) != want {
				t.Fatalf("%s %q: recorded code %s text %s gives outside_change %s, want %s", name, detail,
					detailStr(oc.code), detailStr(oc.siteText), detailStr(got), want)
			}
		}
	}
}

func readAgentSource(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestOutsideChangeMatchesTheAgent: the two details outside_change reads
// are the agent's own, and every label the agent's write scope can report,
// its own and the privilege writes it passes on, is either mapped to a kind
// or listed as naming none. A label the agent adds fails here until it is
// placed. Every pattern must match: a check that finds nothing is red.
func TestOutsideChangeMatchesTheAgent(t *testing.T) {
	doc := readAgentSource(t, agentElementorDocument)
	for _, want := range []string{
		`'code' => self::CODE_SIDE_EFFECT, 'detail' => '` + outsideKitDetail + `'`,
		`'code' => self::CODE_SIDE_EFFECT, 'detail' => '` + outsideScopeDetail + `' . implode(', ', $outcome['violations'])`,
	} {
		if !strings.Contains(doc, want) {
			t.Fatalf("%s no longer answers %s", agentElementorDocument, want)
		}
	}
	scope := readAgentSource(t, agentWriteScope)
	if !strings.Contains(scope, "sort($labels, SORT_STRING);") {
		t.Fatalf("%s no longer sorts its labels", agentWriteScope)
	}
	effects := readAgentSource(t, agentSideEffects)
	found := map[string]string{}
	for _, src := range []struct {
		name, text string
		re         *regexp.Regexp
		min        int
	}{
		{agentWriteScope, scope, regexp.MustCompile(`(?m)^\s*public const V_[A-Z_]+ = '([a-z_]+)';`), 11},
		{agentSideEffects, effects, regexp.MustCompile(`blocked\['([a-z_]+)'\] = true`), 3},
		{agentSideEffects, effects, regexp.MustCompile(`\? '([a-z_]+)' : '([a-z_]+)'\] = true`), 1},
		{agentSideEffects, effects, regexp.MustCompile(`\$out\[\]\s*=\s*\['([a-z_]+)', '(?:site|network)'`), 2},
	} {
		ms := src.re.FindAllStringSubmatch(src.text, -1)
		if len(ms) < src.min {
			t.Fatalf("%s: %d matches of %s, want at least %d; this check would see nothing", src.name, len(ms), src.re, src.min)
		}
		for _, m := range ms {
			for _, label := range m[1:] {
				found[label] = src.name
			}
		}
	}
	for label, where := range found {
		_, mapped := outsideChangeKinds[label]
		_, unnamed := outsideChangeUnnamed[label]
		if mapped == unnamed {
			t.Fatalf("label %q from %s: mapped %v, unnamed %v; place it in exactly one", label, where, mapped, unnamed)
		}
	}
	for label := range outsideChangeKinds {
		if _, ok := found[label]; !ok {
			t.Fatalf("outsideChangeKinds maps %q, which the agent never reports", label)
		}
	}
}
