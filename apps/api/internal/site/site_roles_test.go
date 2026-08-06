package site

import (
	"encoding/json"
	"strings"
	"testing"

	agentpkg "github.com/mosamlife/wpmgr/apps/api/internal/agent"
)

// GH #350: the site's WordPress role registry travels on the metadata READ
// path and lands in the JSONB inventory document, where the security policy
// endpoint reads it back. Every test here FAILS against the pre-change code:
// MetadataExtras had no Roles field, fromAgentSiteRoles did not exist, the
// `roles` key was never written, and Site.ParsedRoles was not a method.

func TestFromAgentSiteRolesCarriesCustomRoles(t *testing.T) {
	got := fromAgentSiteRoles([]agentpkg.SiteRole{
		{Slug: "administrator", Name: "Amministratore"},
		{Slug: "shop_manager", Name: "Gestore negozio"},
	})
	if len(got) != 2 {
		t.Fatalf("want 2 roles, got %+v", got)
	}
	if got[1].Slug != "shop_manager" || got[1].Name != "Gestore negozio" {
		t.Fatalf("custom role not carried: %+v", got[1])
	}
}

// A site running a membership plugin can register dozens of roles, and a forged
// or corrupted push could claim thousands. The stored inventory must stay
// bounded (C4).
func TestFromAgentSiteRolesIsBounded(t *testing.T) {
	in := make([]agentpkg.SiteRole, 0, 900)
	for i := 0; i < 900; i++ {
		in = append(in, agentpkg.SiteRole{Slug: "tier_" + string(rune('a'+i%26)) + itoa(i), Name: "Tier"})
	}
	got := fromAgentSiteRoles(in)
	if len(got) != maxSiteRoles {
		t.Fatalf("want the list capped at %d, got %d", maxSiteRoles, len(got))
	}
}

func TestFromAgentSiteRolesDropsUnusableAndDeduplicates(t *testing.T) {
	got := fromAgentSiteRoles([]agentpkg.SiteRole{
		{Slug: "   ", Name: "blank slug"},
		{Slug: strings.Repeat("x", maxSiteRoleField+1), Name: "absurd slug"},
		{Slug: "shop_manager", Name: "Gestore negozio"},
		{Slug: "shop_manager", Name: "duplicate"},
		{Slug: "nameless", Name: ""},
		{Slug: "long_name", Name: strings.Repeat("n", maxSiteRoleField+50)},
	})
	if len(got) != 3 {
		t.Fatalf("want 3 usable roles, got %+v", got)
	}
	if got[0].Slug != "shop_manager" || got[0].Name != "Gestore negozio" {
		t.Fatalf("first kept role wrong: %+v", got[0])
	}
	if got[1].Name != "nameless" {
		t.Fatalf("a nameless role must fall back to its slug: %+v", got[1])
	}
	if len([]rune(got[2].Name)) != maxSiteRoleField {
		t.Fatalf("a long NAME is truncated, not dropped: %d runes", len([]rune(got[2].Name)))
	}
}

// nil in, nil out. nil means UNKNOWN, and the dashboard renders that as an
// explicit "not reported yet" rather than substituting the five defaults.
func TestFromAgentSiteRolesNilStaysNil(t *testing.T) {
	if got := fromAgentSiteRoles(nil); got != nil {
		t.Fatalf("want nil, got %+v", got)
	}
	if got := fromAgentSiteRoles([]agentpkg.SiteRole{{Slug: ""}}); got != nil {
		t.Fatalf("an all-unusable report must collapse to nil, got %+v", got)
	}
}

// An agent that reports ONLY roles (no host flags, no disk, no counts) must
// still produce Extras, or the registry would be silently discarded.
func TestFromAgentMetadataExtrasIncludesRolesAlone(t *testing.T) {
	x := fromAgentMetadataExtras(agentpkg.Metadata{
		Roles: []agentpkg.SiteRole{{Slug: "shop_manager", Name: "Gestore negozio"}},
	})
	if x == nil {
		t.Fatal("roles alone must produce MetadataExtras")
	}
	if len(x.Roles) != 1 || x.Roles[0].Slug != "shop_manager" {
		t.Fatalf("roles not lifted: %+v", x)
	}
}

func TestFromAgentMetadataExtrasStillNilWhenAgentReportsNothing(t *testing.T) {
	if x := fromAgentMetadataExtras(agentpkg.Metadata{}); x != nil {
		t.Fatalf("an old agent that sends no expansion fields must yield nil, got %+v", x)
	}
}

// ParsedRoles reads the roles back out of the stored inventory document, which
// is what the security policy endpoint serves to the dashboard.
func TestSiteParsedRolesRoundTrip(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"plugins": []Component{},
		"themes":  []Component{},
		"roles": []SiteRole{
			{Slug: "administrator", Name: "Amministratore"},
			{Slug: "shop_manager", Name: "Gestore negozio"},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	roles := Site{Components: payload}.ParsedRoles()
	if len(roles) != 2 || roles[1].Slug != "shop_manager" || roles[1].Name != "Gestore negozio" {
		t.Fatalf("roles did not round-trip: %+v", roles)
	}
}

func TestSiteParsedRolesUnknownCases(t *testing.T) {
	if r := (Site{}).ParsedRoles(); r != nil {
		t.Fatalf("an empty inventory must read as unknown (nil), got %+v", r)
	}
	if r := (Site{Components: []byte("not json")}).ParsedRoles(); r != nil {
		t.Fatalf("a malformed inventory must read as unknown (nil), got %+v", r)
	}
	if r := (Site{Components: []byte(`{"plugins":[],"themes":[]}`)}).ParsedRoles(); r != nil {
		t.Fatalf("an inventory from an agent that predates role reporting must read as unknown, got %+v", r)
	}
}

// itoa avoids importing strconv for one call in the bounded-list generator.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// Every metadata push REWRITES the whole inventory document, so the roles must
// be added to the payload the write path builds or they never reach the
// database at all. FAILS against the pre-change code: buildInventoryPayload did
// not exist and no `roles` key was ever written.
func TestInventoryPayloadCarriesRoles(t *testing.T) {
	payload := buildInventoryPayload(Metadata{
		Extras: &MetadataExtras{
			Roles: []SiteRole{{Slug: "shop_manager", Name: "Gestore negozio"}},
		},
	})
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	roles := Site{Components: raw}.ParsedRoles()
	if len(roles) != 1 || roles[0].Slug != "shop_manager" {
		t.Fatalf("roles missing from the stored inventory: %s", raw)
	}
}

func TestInventoryPayloadOmitsRolesWhenUnreported(t *testing.T) {
	payload := buildInventoryPayload(Metadata{})
	if _, ok := payload["roles"]; ok {
		t.Fatal("an agent that reported no roles must leave the key absent, which reads as unknown")
	}
}
