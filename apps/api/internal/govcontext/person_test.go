package govcontext

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Loosening an AI control needs a signed-in person. The admitted cases are
// here so the check cannot pass by refusing everything.
func TestLooseningAnAIControlNeedsASignedInPerson(t *testing.T) {
	tenant := uuid.New()
	person := domain.WithPrincipal(context.Background(), domain.Principal{
		Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenant, Role: "owner",
	})
	key := domain.WithPrincipal(context.Background(), domain.Principal{
		Type: domain.PrincipalAPIKey, APIKeyID: uuid.New(), TenantID: tenant, Role: "owner",
	})
	none := context.Background()

	set := func(tools, domains, topics []string) RestrictionSet {
		return RestrictionSet{ForbiddenTools: tools, ForbiddenDomains: domains, ForbiddenTopics: topics}
	}
	l := func(v ...string) []string { return v }
	current := set(l("t1", "t2"), l("d1", "d2"), l("p1", "p2"))

	cases := []struct {
		ctx      context.Context
		current  RestrictionSet
		proposed RestrictionSet
		want     bool
	}{
		{key, current, set(l("t1"), l("d1", "d2"), l("p1", "p2")), false},
		{key, current, set(l("t1", "t2"), l("d2"), l("p1", "p2")), false},
		{key, current, set(l("t1", "t2"), l("d1", "d2"), l("p2")), false},
		{key, current, set(nil, l("d1", "d2"), l("p1", "p2")), false},
		{key, current, RestrictionSet{}, false},
		{key, current, set(l("t1", "T2"), l("d1", "d2"), l("p1", "p2")), false},
		{key, current, set(l("t1", "t3"), l("d1", "d2"), l("p1", "p2")), false},
		{none, current, set(l("t1"), l("d1", "d2"), l("p1", "p2")), false},
		{person, current, RestrictionSet{}, true},
		{person, current, set(l("t1"), l("d1"), l("p1")), true},
		{key, current, current, true},
		{key, current, set(l("t2", "t1"), l("d2", "d1"), l("p2", "p1")), true},
		{key, current, set(l("t1", "t2", "t2"), l("d1", "d2", "d3"), l("p1", "p2", "p3")), true},
		{key, RestrictionSet{}, set(l("t1"), nil, nil), true},
		{key, RestrictionSet{}, RestrictionSet{}, true},
		{none, current, current, true},
	}
	for i, tc := range cases {
		t.Run(fmt.Sprintf("case_%d", i+1), func(t *testing.T) {
			err := authorizeLoosening(tc.ctx, tc.current, tc.proposed)
			if tc.want {
				if err != nil {
					t.Fatalf("an allowed write was refused: %v", err)
				}
				return
			}
			de, ok := domain.AsDomain(err)
			if !ok || de.Kind != domain.KindForbidden || de.Code != "session_required" {
				t.Fatalf("a credential that is not a person loosened a control: %v", err)
			}
		})
	}
}
