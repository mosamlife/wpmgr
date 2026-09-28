package site

import (
	"context"
	"testing"

	"github.com/google/uuid"

	agentpkg "github.com/mosamlife/wpmgr/apps/api/internal/agent"
	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
)

// keystoreCaptureRepo mirrors captureRepo (sanitize_test.go), but also returns
// the just-written Components blob on the row it hands back — exactly like the
// real repo returns the row it just wrote. captureRepo deliberately does not
// do this (its own tests inspect gotComponents directly instead), but the
// tests below exercise the FULL ApplyAgentMetadata -> toAPI round trip, which
// reads Site.Components on the returned row.
type keystoreCaptureRepo struct {
	fakeRepo
	gotComponents []byte
}

func (r *keystoreCaptureRepo) UpdateMetadata(_ context.Context, tenantID, siteID uuid.UUID, _ Metadata, components []byte) (Site, error) {
	r.gotComponents = components
	return Site{ID: siteID, TenantID: tenantID, Components: components}, nil
}

// TestAgentMetadataStoresKeystoreStatus (GH #753) proves the full
// ApplyAgentMetadata -> Site response round trip for both an agent that
// predates the keystore probe and one that reports it: the old agent's
// silence must read as "not_reported", never "ok", and the new agent's report
// must reach the Site response with every field intact.
func TestAgentMetadataStoresKeystoreStatus(t *testing.T) {
	t.Run("old agent without the field reads as not_reported", func(t *testing.T) {
		repo := &keystoreCaptureRepo{}
		svc := newSvc(repo)
		tenantID, siteID := uuid.New(), uuid.New()

		out, err := svc.ApplyAgentMetadata(context.Background(), tenantID, siteID, agentpkg.Metadata{
			WPVersion: "6.4.3", // metadata WAS pushed; just no keystore key (pre-#753 agent)
		})
		if err != nil {
			t.Fatalf("ApplyAgentMetadata: %v", err)
		}
		ks, ok := out.KeystoreStatus.Get()
		if !ok {
			t.Fatal("a site that has synced metadata at all must carry an explicit keystore_status, not leave it absent")
		}
		if state, ok := ks.State.Get(); !ok || state != gen.SiteKeystoreStatusStateNotReported {
			t.Fatalf("state = %+v, want not_reported", ks.State)
		}
	})

	t.Run("new agent stores and surfaces the real probe result", func(t *testing.T) {
		repo := &keystoreCaptureRepo{}
		svc := newSvc(repo)
		tenantID, siteID := uuid.New(), uuid.New()

		out, err := svc.ApplyAgentMetadata(context.Background(), tenantID, siteID, agentpkg.Metadata{
			WPVersion: "6.8",
			KeystoreStatus: &agentpkg.KeystoreStatus{
				State:     "unreadable",
				KeySource: "salts",
				Items: map[string]string{
					"site_keypair":             "ok",
					"cp_public_key":            "ok",
					"age_identity":             "unreadable",
					"email_secret":             "absent",
					"email_connection_secrets": "absent",
				},
				Unreadable: []string{"age_identity"},
			},
		})
		if err != nil {
			t.Fatalf("ApplyAgentMetadata: %v", err)
		}
		ks, ok := out.KeystoreStatus.Get()
		if !ok {
			t.Fatal("keystore_status must be present when the agent reported one")
		}
		if state, ok := ks.State.Get(); !ok || state != gen.SiteKeystoreStatusStateUnreadable {
			t.Fatalf("state = %+v, want unreadable", ks.State)
		}
		if src, ok := ks.KeySource.Get(); !ok || src != gen.SiteKeystoreStatusKeySourceSalts {
			t.Fatalf("key_source = %+v, want salts", ks.KeySource)
		}
		items, ok := ks.Items.Get()
		if !ok || len(items) != 5 || items["age_identity"] != "unreadable" || items["site_keypair"] != "ok" {
			t.Fatalf("items not stored/surfaced: %+v", ks.Items)
		}
		if len(ks.Unreadable) != 1 || ks.Unreadable[0] != "age_identity" {
			t.Fatalf("unreadable not stored/surfaced: %+v", ks.Unreadable)
		}
	})
}

// TestSiteKeystoreStatusAbsentBeforeFirstSync proves the other half of the
// schema's contract (packages/openapi/openapi.yaml SiteKeystoreStatus): a site
// that has never synced metadata at all leaves keystore_status entirely
// absent, rather than manufacturing a not_reported object with nothing behind
// it.
func TestSiteKeystoreStatusAbsentBeforeFirstSync(t *testing.T) {
	out := toAPI(Site{ID: uuid.New(), TenantID: uuid.New()})
	if _, ok := out.KeystoreStatus.Get(); ok {
		t.Fatalf("a site with no components blob at all must leave keystore_status absent, got %+v", out.KeystoreStatus)
	}
}

// TestFromAgentMetadataExtrasIncludesKeystoreStatusAlone proves an agent
// report carrying ONLY the keystore probe (no host flags, no disk, no counts,
// no roles) still produces Extras — the exact "status-only push" shape the
// admin_init/cron probe path sends when nothing else changed. Without this,
// fromAgentMetadataExtras's nil check would silently drop the probe result.
func TestFromAgentMetadataExtrasIncludesKeystoreStatusAlone(t *testing.T) {
	x := fromAgentMetadataExtras(agentpkg.Metadata{
		KeystoreStatus: &agentpkg.KeystoreStatus{State: "ok"},
	})
	if x == nil {
		t.Fatal("keystore status alone must produce MetadataExtras")
	}
	if x.KeystoreStatus == nil || x.KeystoreStatus.State != "ok" {
		t.Fatalf("keystore status not lifted: %+v", x)
	}
}

// TestFromAgentKeystoreStatusDropsUnrecognizedValuesAndBounds proves the
// allowlist/bounds fromAgentKeystoreStatus applies before anything is stored:
// an unrecognized state/key_source/item value is dropped rather than stored
// (it must never reach the Site response's typed enum), and the item map and
// unreadable list are capped.
func TestFromAgentKeystoreStatusDropsUnrecognizedValuesAndBounds(t *testing.T) {
	got := fromAgentKeystoreStatus(&agentpkg.KeystoreStatus{
		State:     "definitely_not_a_real_state",
		KeySource: "also_bogus",
		Items: map[string]string{
			"age_identity": "unreadable",   // kept: recognized value
			"junk_key":     "also_garbage", // dropped: value not in the vocabulary
		},
		Unreadable: []string{"age_identity", "age_identity", "  "},
	})
	if got.State != "" {
		t.Fatalf("unrecognized state must be dropped, got %q", got.State)
	}
	if got.KeySource != "" {
		t.Fatalf("unrecognized key_source must be dropped, got %q", got.KeySource)
	}
	if len(got.Items) != 1 || got.Items["age_identity"] != "unreadable" {
		t.Fatalf("items must keep only recognized values, got %+v", got.Items)
	}
	if len(got.Unreadable) != 1 || got.Unreadable[0] != "age_identity" {
		t.Fatalf("unreadable must dedupe and drop blanks, got %+v", got.Unreadable)
	}
}
