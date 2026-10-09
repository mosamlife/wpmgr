package govcontext

import (
	"context"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// versionsDouble is a versionStore over in-memory versions. It records each
// write that reaches it, which is how the test below sees whether a call got
// as far as CreateOrgVersion or CreateSiteVersion.
//
// It does not run a write's audit callback: that takes a real transaction,
// and the audit append is proven against Postgres in the integration package.
// The write paths call only the methods defined here; any other call reaches
// the embedded nil versionStore and fails the test.
type versionsDouble struct {
	versionStore

	org     *Version // the organisation's latest version; nil for none
	site    *Version // the site's latest version; nil for none
	byID    map[uuid.UUID]Version
	orgSnap Snapshot // what LatestOrgSnapshot returns

	orgWrites  []CreateOrgVersionInput
	siteWrites []CreateSiteVersionInput
}

func (d *versionsDouble) LatestOrgVersion(context.Context, uuid.UUID) (Version, error) {
	if d.org == nil {
		return Version{}, ErrNotFound
	}
	return *d.org, nil
}

func (d *versionsDouble) LatestSiteVersion(context.Context, uuid.UUID, uuid.UUID) (Version, error) {
	if d.site == nil {
		return Version{}, ErrNotFound
	}
	return *d.site, nil
}

func (d *versionsDouble) GetOrgVersionByID(_ context.Context, _, id uuid.UUID) (Version, error) {
	v, ok := d.byID[id]
	if !ok {
		return Version{}, ErrNotFound
	}
	return v, nil
}

func (d *versionsDouble) GetSiteVersionByID(_ context.Context, _, _, id uuid.UUID) (Version, error) {
	v, ok := d.byID[id]
	if !ok {
		return Version{}, ErrNotFound
	}
	return v, nil
}

func (d *versionsDouble) LatestOrgSnapshot(context.Context, uuid.UUID) (Snapshot, bool, error) {
	return d.orgSnap, true, nil
}

func (d *versionsDouble) CreateOrgVersion(_ context.Context, tenantID uuid.UUID, expectVersion int64,
	in CreateOrgVersionInput, _ func(pgx.Tx, uuid.UUID) error) (Version, error) {
	d.orgWrites = append(d.orgWrites, in)
	return Version{
		ID: uuid.New(), TenantID: tenantID, Version: expectVersion, Snapshot: in.Snapshot,
		AuthorType: in.AuthorType, AuthorID: in.AuthorID,
		Provenance: in.Provenance, RestoredFromVersionID: in.RestoredFromVersionID,
	}, nil
}

func (d *versionsDouble) CreateSiteVersion(_ context.Context, tenantID, siteID uuid.UUID, expectVersion int64,
	in CreateSiteVersionInput, _ func(pgx.Tx, uuid.UUID) error) (Version, error) {
	d.siteWrites = append(d.siteWrites, in)
	return Version{
		ID: uuid.New(), TenantID: tenantID, SiteID: siteID, Version: expectVersion, Snapshot: in.Snapshot,
		AuthorType: in.AuthorType, AuthorID: in.AuthorID,
		Provenance: in.Provenance, RestoredFromVersionID: in.RestoredFromVersionID,
	}, nil
}

// TestContextWrites_LooseningNeedsASignedInPerson drives each of the four
// Service write paths: PatchOrgContext, RestoreOrgContext, PatchSiteContext
// and RestoreSiteContext.
//
// Loosening an AI control needs a signed-in person. An API key whose write
// lacks an item the layer carries now is refused with 403 session_required,
// and the call ends before CreateOrgVersion or CreateSiteVersion. A key whose
// write keeps every item, or adds one, still writes, and so does a signed-in
// person who drops one: the check refuses exactly the loosening by a key.
func TestContextWrites_LooseningNeedsASignedInPerson(t *testing.T) {
	tenant, site := uuid.New(), uuid.New()
	keyID, userID := uuid.New(), uuid.New()

	asKey := domain.WithPrincipal(context.Background(), domain.Principal{
		Type: domain.PrincipalAPIKey, APIKeyID: keyID, TenantID: tenant, Role: "owner",
	})
	asPerson := domain.WithPrincipal(context.Background(), domain.Principal{
		Type: domain.PrincipalUser, UserID: userID, TenantID: tenant, Role: "owner",
	})
	key := Actor{Type: AuthorAPIKey, ID: keyID}
	person := Actor{Type: AuthorUser, ID: userID}

	// held is what the layer carries now. fewer lacks one of its items;
	// more carries all of them and one besides.
	held := RestrictionSet{
		ForbiddenTools:   []string{"site_delete", "plugin_delete"},
		ForbiddenDomains: []string{"a.example.com"},
		ForbiddenTopics:  []string{"pricing"},
	}
	fewer := RestrictionSet{
		ForbiddenTools:   []string{"site_delete"},
		ForbiddenDomains: []string{"a.example.com"},
		ForbiddenTopics:  []string{"pricing"},
	}
	more := RestrictionSet{
		ForbiddenTools:   []string{"site_delete", "plugin_delete", "user_delete"},
		ForbiddenDomains: []string{"a.example.com"},
		ForbiddenTopics:  []string{"pricing"},
	}
	edited := GuidanceSet{BrandVoice: "be terse"}

	// A write proposes restrictions (nil: leave them as held) and guidance.
	// For a restore, the proposal is the snapshot of the version restored.
	type written struct {
		count        int
		restrictions RestrictionSet
	}
	paths := []struct {
		name  string
		write func(ctx context.Context, actor Actor, restrictions *RestrictionSet) (written, error)
	}{
		{"PatchOrgContext", func(ctx context.Context, actor Actor, restrictions *RestrictionSet) (written, error) {
			d := &versionsDouble{org: &Version{ID: uuid.New(), TenantID: tenant, Version: 3,
				Snapshot: Snapshot{Restrictions: held}}}
			_, err := (&Service{repo: d}).PatchOrgContext(ctx, tenant, PatchOrgContextInput{
				BaseVersion: 3, Restrictions: restrictions, Guidance: &edited,
			}, actor)
			w := written{count: len(d.orgWrites)}
			if w.count > 0 {
				w.restrictions = d.orgWrites[0].Snapshot.Restrictions
			}
			return w, err
		}},
		{"RestoreOrgContext", func(ctx context.Context, actor Actor, restrictions *RestrictionSet) (written, error) {
			target := Version{ID: uuid.New(), TenantID: tenant, Version: 1,
				Snapshot: Snapshot{Restrictions: held, Guidance: edited}}
			if restrictions != nil {
				target.Snapshot.Restrictions = *restrictions
			}
			d := &versionsDouble{
				org: &Version{ID: uuid.New(), TenantID: tenant, Version: 3,
					Snapshot: Snapshot{Restrictions: held}},
				byID: map[uuid.UUID]Version{target.ID: target},
			}
			_, err := (&Service{repo: d}).RestoreOrgContext(ctx, tenant, target.ID, actor)
			w := written{count: len(d.orgWrites)}
			if w.count > 0 {
				w.restrictions = d.orgWrites[0].Snapshot.Restrictions
			}
			return w, err
		}},
		{"PatchSiteContext", func(ctx context.Context, actor Actor, restrictions *RestrictionSet) (written, error) {
			d := &versionsDouble{site: &Version{ID: uuid.New(), TenantID: tenant, SiteID: site, Version: 3,
				Snapshot: Snapshot{Restrictions: held}}}
			_, err := (&Service{repo: d}).PatchSiteContext(ctx, tenant, site, PatchSiteContextInput{
				BaseVersion: 3, Restrictions: restrictions, Guidance: &edited,
			}, actor)
			w := written{count: len(d.siteWrites)}
			if w.count > 0 {
				w.restrictions = d.siteWrites[0].Snapshot.Restrictions
			}
			return w, err
		}},
		{"RestoreSiteContext", func(ctx context.Context, actor Actor, restrictions *RestrictionSet) (written, error) {
			target := Version{ID: uuid.New(), TenantID: tenant, SiteID: site, Version: 1,
				Snapshot: Snapshot{Restrictions: held, Guidance: edited}}
			if restrictions != nil {
				target.Snapshot.Restrictions = *restrictions
			}
			d := &versionsDouble{
				site: &Version{ID: uuid.New(), TenantID: tenant, SiteID: site, Version: 3,
					Snapshot: Snapshot{Restrictions: held}},
				byID: map[uuid.UUID]Version{target.ID: target},
			}
			_, err := (&Service{repo: d}).RestoreSiteContext(ctx, tenant, site, target.ID, actor)
			w := written{count: len(d.siteWrites)}
			if w.count > 0 {
				w.restrictions = d.siteWrites[0].Snapshot.Restrictions
			}
			return w, err
		}},
	}

	cases := []struct {
		name         string
		ctx          context.Context
		actor        Actor
		restrictions *RestrictionSet
		allowed      bool
		want         RestrictionSet // what an allowed write stores
	}{
		{"an API key that drops an item is refused before the write", asKey, key, &fewer, false, RestrictionSet{}},
		{"an API key that keeps every item writes", asKey, key, nil, true, held},
		{"an API key that adds an item writes", asKey, key, &more, true, more},
		{"a signed-in person who drops an item writes", asPerson, person, &fewer, true, fewer},
	}

	for _, p := range paths {
		for _, tc := range cases {
			t.Run(p.name+"/"+tc.name, func(t *testing.T) {
				w, err := p.write(tc.ctx, tc.actor, tc.restrictions)

				if !tc.allowed {
					de, ok := domain.AsDomain(err)
					if !ok || de.Kind != domain.KindForbidden || de.Code != "session_required" {
						t.Fatalf("loosening needs a signed-in person: want 403 session_required, got %v", err)
					}
					if domain.HTTPStatus(err) != 403 {
						t.Errorf("HTTP status = %d, want 403", domain.HTTPStatus(err))
					}
					if w.count != 0 {
						t.Fatalf("a refused write reached the version write %d time(s), want 0", w.count)
					}
					return
				}

				if err != nil {
					t.Fatalf("want the write to land, got %v", err)
				}
				if w.count != 1 {
					t.Fatalf("version writes = %d, want 1", w.count)
				}
				if !reflect.DeepEqual(w.restrictions, tc.want) {
					t.Errorf("restrictions written = %+v, want %+v", w.restrictions, tc.want)
				}
			})
		}
	}
}
