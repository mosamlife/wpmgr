package aipolicy

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// The matrix, written out by hand from the design so a flipped cell in
// Allows fails here. TestMatrixParity (integration) holds the SQL copy to
// the same table.
var wantMatrix = map[Mode]map[Class]bool{
	ModeAsk: {},
	ModeAIDrafts: {
		ClassAIDraft: true, ClassOperational: true,
	},
	ModeFull: {
		ClassAIDraft: true, ClassOperational: true, ClassUnpublished: true,
		ClassLive: true, ClassPublish: true, ClassUpdate: true,
	},
}

func TestAllowsMatrix(t *testing.T) {
	for _, m := range Modes() {
		for _, c := range StoredClasses() {
			if got, want := Allows(m, c), wantMatrix[m][c]; got != want {
				t.Errorf("Allows(%s, %s) = %v, want %v", m, c, got, want)
			}
		}
	}
	for _, c := range StoredClasses() {
		if Allows("", c) || Allows("unknown", c) {
			t.Errorf("an unknown mode allows %s", c)
		}
	}
	for _, m := range Modes() {
		if Allows(m, StoredByTargetStatus) {
			t.Errorf("by_target_status is allowed in %s; it is never an effective class", m)
		}
		if Allows(m, "") || Allows(m, "write") {
			t.Errorf("an unknown class is allowed in %s", m)
		}
	}
}

func str(s string) *string { return &s }

func TestClassifyRestWriteByStatus(t *testing.T) {
	cases := []struct {
		status  *string
		aiDraft bool
		class   Class
		ask     AskReason
	}{
		{str("publish"), false, ClassLive, ""},
		{str("publish"), true, ClassLive, ""},
		{str("private"), false, ClassLive, ""},
		{str("future"), true, ClassPublish, ""},
		{str("pending"), false, ClassUnpublished, ""},
		{str("draft"), false, ClassUnpublished, ""},
		{str("draft"), true, ClassAIDraft, ""},
		{str("auto-draft"), true, "", AskUnknownTargetState},
		{str("inherit"), false, "", AskUnknownTargetState},
		{str("trash"), true, "", AskUnknownTargetState},
		{str(""), true, "", AskUnknownTargetState},
		{str("anything"), true, "", AskUnknownTargetState},
		{nil, true, "", AskUnknownTargetState},
	}
	for _, tc := range cases {
		got := Classify(ClassFacts{Stored: StoredByTargetStatus, Undo: UndoExact, TargetStatus: tc.status, AIDraft: tc.aiDraft})
		if got.Class != tc.class || got.Ask != tc.ask {
			name := "<nil>"
			if tc.status != nil {
				name = *tc.status
			}
			t.Errorf("status %q ai_draft=%v: got %+v, want class %q ask %q", name, tc.aiDraft, got, tc.class, tc.ask)
		}
	}
}

// The status is the raw checked value, compared exactly; the card's cleaned
// copy is never an input.
func TestClassifyUsesRawStatusExactly(t *testing.T) {
	for _, s := range []string{"Draft", "draft ", " draft", "DRAFT", "draft\n", "dra​ft"} {
		got := Classify(ClassFacts{Stored: StoredByTargetStatus, Undo: UndoExact, TargetStatus: str(s), AIDraft: true})
		if got.Ask != AskUnknownTargetState || got.Class != "" {
			t.Errorf("status %q: got %+v, want unknown_target_state", s, got)
		}
	}
}

func TestClassifyStoredAlwaysAskIsFinal(t *testing.T) {
	for _, s := range []*string{str("draft"), str("publish"), nil} {
		got := Classify(ClassFacts{Stored: ClassAlwaysAsk, Undo: UndoExact, TargetStatus: s, AIDraft: true})
		if got.Class != ClassAlwaysAsk || got.Ask != AskKindAlwaysAsks {
			t.Errorf("stored always_ask on an AI draft: got %+v", got)
		}
	}
}

func TestClassifyStoredClassNotLoweredByStatus(t *testing.T) {
	for _, stored := range []Class{ClassUpdate, ClassLive, ClassPublish, ClassUnpublished} {
		got := Classify(ClassFacts{Stored: stored, Undo: UndoExact, TargetStatus: str("draft"), AIDraft: true})
		if got.Class != stored || got.Ask != "" {
			t.Errorf("stored %s on an AI draft: got %+v, want it kept", stored, got)
		}
	}
}

func TestClassifyUndoNotExactOrUnknownAsks(t *testing.T) {
	for _, u := range []Undo{UndoNotExact, UndoUnknown, Undo(99)} {
		for _, stored := range []Class{ClassAIDraft, StoredByTargetStatus, ClassLive} {
			got := Classify(ClassFacts{Stored: stored, Undo: u, TargetStatus: str("draft"), AIDraft: true})
			if got.Class != ClassAlwaysAsk || got.Ask != AskKindAlwaysAsks {
				t.Errorf("undo %d stored %s: got %+v, want always_ask", u, stored, got)
			}
		}
	}
	got := Classify(ClassFacts{Stored: ClassAIDraft, Undo: UndoByAbility})
	if got.Class != ClassAIDraft || got.Ask != "" {
		t.Errorf("page creation with its own undo: got %+v", got)
	}
}

func TestClassifyUnknownStoredAsks(t *testing.T) {
	for _, stored := range []Class{"", "write", "denied", "AI_DRAFT"} {
		got := Classify(ClassFacts{Stored: stored, Undo: UndoExact})
		if got.Class != ClassAlwaysAsk || got.Ask != AskKindAlwaysAsks {
			t.Errorf("stored %q: got %+v, want always_ask", stored, got)
		}
	}
}

var (
	tenantA = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	siteA   = uuid.MustParse("22222222-2222-2222-2222-222222222222")
	siteB   = uuid.MustParse("33333333-3333-3333-3333-333333333333")
	userA   = uuid.MustParse("44444444-4444-4444-4444-444444444444")
)

func member(role authz.Role) Setter {
	return Setter{
		UserID:        userA,
		Principal:     domain.Principal{Type: domain.PrincipalUser, UserID: userA, TenantID: tenantA, Role: string(role), Scope: domain.ScopeOrg},
		AccountStatus: AccountActive,
	}
}

func collaborator(role authz.Role, sites ...uuid.UUID) Setter {
	s := member(role)
	s.Principal.Scope = domain.ScopeSite
	s.Principal.AllowedSiteIDs = sites
	return s
}

func TestCheckSiteSetter(t *testing.T) {
	removed := member(authz.RoleOperator)
	removed.Principal.TenantID = uuid.Nil
	disabled := member(authz.RoleAdmin)
	disabled.AccountStatus = "disabled"
	deleted := member(authz.RoleAdmin)
	deleted.AccountStatus = ""
	failedLookup := member(authz.RoleOwner)
	failedLookup.LookupFailed = true
	otherUser := member(authz.RoleOwner)
	otherUser.Principal.UserID = siteB

	edit := string(authz.PermSiteContentEdit)
	cases := []struct {
		name   string
		s      Setter
		mode   Mode
		perm   string
		ok     bool
		failed string
	}{
		{"operator on ai_drafts", member(authz.RoleOperator), ModeAIDrafts, edit, true, ""},
		{"operator, row needs admin rank", member(authz.RoleOperator), ModeAIDrafts, string(authz.PermSiteFilesWrite), false, FailedRowPermission},
		{"unknown row permission", member(authz.RoleOwner), ModeAIDrafts, "site.anything", false, FailedRowPermission},
		{"viewer", member(authz.RoleViewer), ModeAIDrafts, edit, false, FailedAuthority},
		{"removed from the organisation", removed, ModeAIDrafts, edit, false, FailedNotMember},
		{"disabled account", disabled, ModeAIDrafts, edit, false, FailedAccountStatus},
		{"deleted account", deleted, ModeAIDrafts, edit, false, FailedAccountStatus},
		{"lookup failed", failedLookup, ModeAIDrafts, edit, false, FailedLookup},
		{"principal of another user", otherUser, ModeAIDrafts, edit, false, FailedNotMember},
		{"no setter", Setter{}, ModeAIDrafts, edit, false, FailedNoSetter},
		{"collaborator on the site", collaborator(authz.RoleOperator, siteA), ModeAIDrafts, edit, true, ""},
		{"collaborator whose share was withdrawn", collaborator(authz.RoleOperator), ModeAIDrafts, edit, false, FailedSiteAccess},
		{"collaborator on another site", collaborator(authz.RoleOperator, siteB), ModeAIDrafts, edit, false, FailedSiteAccess},
		{"admin on full", member(authz.RoleAdmin), ModeFull, edit, true, ""},
		{"full setter now an operator", member(authz.RoleOperator), ModeFull, edit, false, FailedAuthority},
		{"full setter now a site collaborator", collaborator(authz.RoleAdmin, siteA), ModeFull, edit, false, FailedOrgScope},
		{"ask is not a setting", member(authz.RoleOwner), ModeAsk, edit, false, FailedUnknownSetting},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckSiteSetter(tc.s, tenantA, siteA, tc.mode, tc.perm)
			if got.OK != tc.ok || got.Failed != tc.failed {
				t.Fatalf("got %+v, want ok=%v failed=%q", got, tc.ok, tc.failed)
			}
			if tc.failed == FailedLookup && !got.LookupFailed {
				t.Fatal("a failed lookup is not reported as one")
			}
		})
	}
	if got := CheckSiteSetter(member(authz.RoleOwner), siteB, siteA, ModeAIDrafts, edit); got.OK {
		t.Fatal("a member of another organisation passes")
	}
}

func TestCheckConnectionSetter(t *testing.T) {
	disabled := member(authz.RoleAdmin)
	disabled.AccountStatus = "disabled"
	removed := member(authz.RoleAdmin)
	removed.Principal.TenantID = uuid.Nil
	cases := []struct {
		name   string
		s      Setter
		ok     bool
		failed string
	}{
		{"admin", member(authz.RoleAdmin), true, ""},
		{"owner", member(authz.RoleOwner), true, ""},
		{"demoted to operator", member(authz.RoleOperator), false, FailedAuthority},
		{"converted to a site collaborator", collaborator(authz.RoleAdmin, siteA), false, FailedOrgScope},
		{"removed", removed, false, FailedNotMember},
		{"disabled", disabled, false, FailedAccountStatus},
		{"key-minted, nobody on record", Setter{}, false, FailedNoSetter},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckConnectionSetter(tc.s, tenantA)
			if got.OK != tc.ok || got.Failed != tc.failed || got.Rule != RuleConnectionSetter {
				t.Fatalf("got %+v, want ok=%v failed=%q", got, tc.ok, tc.failed)
			}
		})
	}
}

// eligible is an AI draft edit on an Auto-for-AI-drafts site, with both
// setters valid and an empty budget: the one shape that runs.
func eligible() Inputs {
	return Inputs{
		Class:            Classification{Class: ClassAIDraft},
		SiteMode:         ModeAIDrafts,
		SiteSetter:       SetterCheck{Rule: RuleSiteSetterAIDrafts, OK: true},
		ConnectionAuto:   AutoSiteSetting,
		ConnectionSetter: SetterCheck{Rule: RuleConnectionSetter, OK: true},
		Usage:            Usage{Checked: true},
	}
}

func TestEvaluate(t *testing.T) {
	type mut func(*Inputs)
	cases := []struct {
		name string
		mut  mut
		out  Outcome
		ask  AskReason
	}{
		{"eligible draft runs", func(*Inputs) {}, OutcomeAutoBySetting, ""},
		{"always_ask", func(in *Inputs) { in.Class = Classification{Class: ClassAlwaysAsk, Ask: AskKindAlwaysAsks} }, OutcomeAsk, AskKindAlwaysAsks},
		{"unknown target state", func(in *Inputs) { in.Class = Classification{Ask: AskUnknownTargetState} }, OutcomeAsk, AskUnknownTargetState},
		{"unknown class", func(in *Inputs) { in.Class = Classification{Class: "by_target_status"} }, OutcomeAsk, AskKindAlwaysAsks},
		{"site setter lookup failed", func(in *Inputs) { in.SiteSetter = SetterCheck{Rule: RuleSiteSetterAIDrafts, LookupFailed: true} }, OutcomeAsk, AskNotChecked},
		{"connection setter lookup failed", func(in *Inputs) {
			in.ConnectionSetter = SetterCheck{Rule: RuleConnectionSetter, LookupFailed: true}
		}, OutcomeAsk, AskNotChecked},
		{"key-minted connection", func(in *Inputs) { in.ConnectionAuto = AutoNever; in.ConnectionSetter = SetterCheck{} }, OutcomeAsk, AskConnectionNeverAuto},
		{"connection setter invalid", func(in *Inputs) {
			in.ConnectionSetter = SetterCheck{Rule: RuleConnectionSetter, Failed: FailedAuthority}
		}, OutcomeAsk, AskConnectionSetterInvalid},
		{"connection check made under another rule", func(in *Inputs) {
			in.ConnectionSetter = SetterCheck{Rule: RuleSiteSetterAIDrafts, OK: true}
		}, OutcomeAsk, AskConnectionSetterInvalid},
		{"site setter lacks permission", func(in *Inputs) {
			in.SiteSetter = SetterCheck{Rule: RuleSiteSetterAIDrafts, Failed: FailedRowPermission}
		}, OutcomeAsk, AskSetterLacksPermission},
		{"site check made for another mode", func(in *Inputs) { in.SiteSetter = SetterCheck{Rule: RuleSiteSetterFull, OK: true} }, OutcomeAsk, AskSetterLacksPermission},
		{"ask site", func(in *Inputs) { in.SiteMode = ModeAsk; in.SiteSetter = SetterCheck{} }, OutcomeAsk, AskSiteModeAsk},
		{"ask site ignores its absent setter's lookup", func(in *Inputs) {
			in.SiteMode = ModeAsk
			in.SiteSetter = SetterCheck{LookupFailed: true}
		}, OutcomeAsk, AskSiteModeAsk},
		{"unknown mode", func(in *Inputs) { in.SiteMode = "auto" }, OutcomeAsk, AskNotChecked},
		{"unknown switch", func(in *Inputs) { in.ConnectionAuto = "always" }, OutcomeAsk, AskNotChecked},
		{"unpublished on ai_drafts (Q7)", func(in *Inputs) { in.Class = Classification{Class: ClassUnpublished} }, OutcomeAsk, AskKindNotInMode},
		{"scheduled AI draft on ai_drafts", func(in *Inputs) { in.Class = Classification{Class: ClassPublish} }, OutcomeAsk, AskKindNotInMode},
		{"published page on ai_drafts", func(in *Inputs) { in.Class = Classification{Class: ClassLive} }, OutcomeAsk, AskKindNotInMode},
		{"usage not read", func(in *Inputs) { in.Usage = Usage{} }, OutcomeAsk, AskNotChecked},
		{"operational has no budget counted in this build", func(in *Inputs) { in.Class = Classification{Class: ClassOperational} }, OutcomeAsk, AskNotChecked},
		{"600 changes this hour", func(in *Inputs) { in.Usage.Changes = DraftChangesPerConnection }, OutcomeAsk, AskOverChangeBudget},
		{"599 changes this hour", func(in *Inputs) { in.Usage.Changes = DraftChangesPerConnection - 1 }, OutcomeAutoBySetting, ""},
		{"31st site", func(in *Inputs) { in.Usage.Sites = DraftSitesPerConnection }, OutcomeAsk, AskOverSiteCap},
		{"30 sites, this one among them", func(in *Inputs) {
			in.Usage.Sites = DraftSitesPerConnection
			in.Usage.SiteCounted = true
		}, OutcomeAutoBySetting, ""},
		{"full site, live change held", func(in *Inputs) {
			in.SiteMode = ModeFull
			in.SiteSetter = SetterCheck{Rule: RuleSiteSetterFull, OK: true}
			in.Class = Classification{Class: ClassLive}
			in.VisibleAutoHeld = true
		}, OutcomeAsk, AskVisibleAutoHeld},
		{"full site, live change, no visible budget in this build", func(in *Inputs) {
			in.SiteMode = ModeFull
			in.SiteSetter = SetterCheck{Rule: RuleSiteSetterFull, OK: true}
			in.Class = Classification{Class: ClassLive}
		}, OutcomeAsk, AskNotChecked},
		{"full site, unpublished change runs", func(in *Inputs) {
			in.SiteMode = ModeFull
			in.SiteSetter = SetterCheck{Rule: RuleSiteSetterFull, OK: true}
			in.Class = Classification{Class: ClassUnpublished}
		}, OutcomeAutoBySetting, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := eligible()
			tc.mut(&in)
			got := Evaluate(in)
			if got.Outcome != tc.out || got.Ask != tc.ask {
				t.Fatalf("got %+v, want %s %q", got, tc.out, tc.ask)
			}
			if got.Outcome == OutcomeAutoBySetting && !Allows(in.SiteMode, got.Class) {
				t.Fatalf("approved a class the mode does not allow: %+v", got)
			}
			if got.Ask != "" && !got.Ask.Known() {
				t.Fatalf("reason %q is not in the closed set", got.Ask)
			}
		})
	}
}

func TestEvaluateResumesAtOnlyForChangeBudget(t *testing.T) {
	oldest := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	in := eligible()
	in.Usage.Changes = DraftChangesPerConnection
	in.Usage.OldestAt = oldest
	if got := Evaluate(in); !got.ResumesAt.Equal(oldest.Add(BudgetWindow)) {
		t.Fatalf("resumes at %v, want %v", got.ResumesAt, oldest.Add(BudgetWindow))
	}
	in = eligible()
	in.Usage.Sites = DraftSitesPerConnection
	in.Usage.OldestAt = oldest
	if got := Evaluate(in); !got.ResumesAt.IsZero() {
		t.Fatalf("over_site_cap carries a resume time: %v", got.ResumesAt)
	}
}

// The AI's text is constant: every reason has its own fixed message, none
// carries a placeholder, and none names who chose a setting.
func TestMessagesAreConstant(t *testing.T) {
	seen := map[AskReason]bool{}
	for _, r := range AskReasons() {
		if seen[r] {
			t.Fatalf("reason %q listed twice", r)
		}
		seen[r] = true
		m := r.Message()
		if !strings.HasPrefix(m, "Nothing has changed yet.") {
			t.Errorf("%s: %q does not say nothing changed", r, m)
		}
		if strings.ContainsAny(m, "%{}<>") {
			t.Errorf("%s: %q carries a placeholder", r, m)
		}
		if strings.ContainsAny(m, "–—") {
			t.Errorf("%s: %q carries a dash", r, m)
		}
	}
	if len(seen) != 12 {
		t.Fatalf("the closed set has %d reasons, want the 12 of the ask_reason CHECK", len(seen))
	}
	if AskReason("made_up").Message() != AskNotChecked.Message() {
		t.Fatal("an unknown reason claims more than not_checked")
	}
}

func TestKindNames(t *testing.T) {
	for _, c := range Classes() {
		if (c.KindName() == "") != (c == ClassAlwaysAsk) {
			t.Errorf("%s: kind name %q", c, c.KindName())
		}
	}
	if StoredByTargetStatus.KindName() != "" || StoredByTargetStatus.Effective() || !StoredByTargetStatus.Stored() {
		t.Fatal("by_target_status is a stored class only")
	}
}
