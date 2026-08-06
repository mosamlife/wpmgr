package security

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// GH #350: the policy READ carries the site's real WordPress roles so the
// dashboard can offer roles such as WooCommerce's shop_manager instead of the
// five hardcoded WordPress defaults.
//
// Every test here FAILS against the pre-change code: SiteRoleLookup,
// Service.GetSiteRoles and toPolicyWithRolesDTO did not exist, and the GET
// response carried no site_roles field at all.

type fakeRoleLookup struct {
	roles []SiteRole
	err   error
	calls int
}

func (f *fakeRoleLookup) GetSiteRoles(_ context.Context, _, _ uuid.UUID) ([]SiteRole, error) {
	f.calls++
	return f.roles, f.err
}

func TestGetSiteRolesReturnsWhatTheSiteReported(t *testing.T) {
	svc := NewService(nil)
	svc.SetSiteRoleLookup(&fakeRoleLookup{roles: []SiteRole{
		{Slug: "administrator", Name: "Amministratore"},
		{Slug: "shop_manager", Name: "Gestore negozio"},
	}})

	got := svc.GetSiteRoles(context.Background(), uuid.New(), uuid.New())
	if len(got) != 2 || got[1].Slug != "shop_manager" || got[1].Name != "Gestore negozio" {
		t.Fatalf("custom role not surfaced: %+v", got)
	}
}

// Role discovery must never be able to break the policy read. A control plane
// with no lookup wired, or a site whose inventory cannot be read, still serves
// the policy; the dashboard shows its labelled fallback.
func TestGetSiteRolesNeverBreaksThePolicyRead(t *testing.T) {
	unwired := NewService(nil)
	if got := unwired.GetSiteRoles(context.Background(), uuid.New(), uuid.New()); got != nil {
		t.Fatalf("an unwired lookup must report no roles, got %+v", got)
	}

	failing := NewService(nil)
	failing.SetSiteRoleLookup(&fakeRoleLookup{err: errors.New("site not found")})
	if got := failing.GetSiteRoles(context.Background(), uuid.New(), uuid.New()); got != nil {
		t.Fatalf("a failing lookup must report no roles rather than erroring, got %+v", got)
	}
}

// The wire name the dashboard reads, and the guarantee that it is an ARRAY and
// never null: an absent list would be indistinguishable from a decode failure.
func TestPolicyResponseCarriesSiteRoles(t *testing.T) {
	dto := toPolicyWithRolesDTO(
		DefaultSiteSecurityPolicy(uuid.New(), uuid.New()),
		[]SiteRole{{Slug: "shop_manager", Name: "Gestore negozio"}},
	)
	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// The policy fields must still be FLAT alongside site_roles: the dashboard
	// reads them at the top level.
	if _, ok := decoded["password_min_zxcvbn_roles"]; !ok {
		t.Fatalf("policy fields must stay flat in the response: %s", raw)
	}
	items, ok := decoded["site_roles"].([]any)
	if !ok {
		t.Fatalf("site_roles must be an array, got %T in %s", decoded["site_roles"], raw)
	}
	if len(items) != 1 {
		t.Fatalf("want 1 reported role, got %s", raw)
	}
	role := items[0].(map[string]any)
	if role["slug"] != "shop_manager" || role["name"] != "Gestore negozio" {
		t.Fatalf("role wire shape wrong: %s", raw)
	}
}

func TestPolicyResponseSiteRolesIsNeverNull(t *testing.T) {
	raw, err := json.Marshal(toPolicyWithRolesDTO(
		DefaultSiteSecurityPolicy(uuid.New(), uuid.New()), nil,
	))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded struct {
		SiteRoles []siteRoleDTO `json:"site_roles"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.SiteRoles == nil {
		t.Fatalf("site_roles must serialise as [] not null: %s", raw)
	}
}

func TestPolicyResponseSiteRoleNameFallsBackToSlug(t *testing.T) {
	dto := toPolicyWithRolesDTO(
		DefaultSiteSecurityPolicy(uuid.New(), uuid.New()),
		[]SiteRole{{Slug: "orphan_role"}, {Slug: "", Name: "no slug"}},
	)
	if len(dto.SiteRoles) != 1 {
		t.Fatalf("a slugless role is unusable and must be dropped: %+v", dto.SiteRoles)
	}
	if dto.SiteRoles[0].Name != "orphan_role" {
		t.Fatalf("want the slug as the fallback name, got %q", dto.SiteRoles[0].Name)
	}
}

// T3, control-plane half: a policy that names a role the site no longer has
// keeps that slug on the way through, and the site_roles list does NOT quietly
// gain it. That combination is what lets the dashboard show the stale role,
// mark it as absent, and let the operator remove it.
func TestPolicyKeepsARoleTheSiteNoLongerHas(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	stored := fromPolicyDTO(policyDTO{
		PasswordMinZxcvbnScore: 3,
		PasswordMinZxcvbnRoles: []string{"shop_manager", "deactivated_plugin_role"},
	}, tenantID, siteID)

	if len(stored.PasswordMinZxcvbnRoles) != 2 {
		t.Fatalf("the write path must not drop unknown role slugs: %+v", stored.PasswordMinZxcvbnRoles)
	}

	// The site now reports only shop_manager: the other plugin was deactivated.
	dto := toPolicyWithRolesDTO(stored, []SiteRole{{Slug: "shop_manager", Name: "Gestore negozio"}})

	if len(dto.PasswordMinZxcvbnRoles) != 2 ||
		dto.PasswordMinZxcvbnRoles[1] != "deactivated_plugin_role" {
		t.Fatalf("the stale slug must survive the read so the operator can see and remove it: %+v",
			dto.PasswordMinZxcvbnRoles)
	}
	for _, r := range dto.SiteRoles {
		if r.Slug == "deactivated_plugin_role" {
			t.Fatal("site_roles reports what the SITE has; it must not be back-filled from the policy")
		}
	}
}
