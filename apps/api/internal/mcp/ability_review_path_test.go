package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// The web shows ability requests on the site's Content tab.
func TestAbilityCreatedResult_PointsAtTheContentTab(t *testing.T) {
	site := uuid.New()
	res := abilityResultFromRow(sqlc.AssistantAbilityRequest{
		ID: uuid.New(), SiteID: site, AbilityName: AbilityPageCreate, Snapshot: "created_post_trash", ExpiresAt: time.Now(),
	}, false)
	if res.ReviewPath != "/sites/"+site.String()+"/content" {
		t.Fatalf("review_path = %q", res.ReviewPath)
	}
	if !strings.Contains(res.Message, "Review it on the site's Content tab in WPMgr") {
		t.Fatalf("message does not name the Content tab: %q", res.Message)
	}
}

// Ability request actions never reuse the cache-clear request keys, so the
// audit log never labels a page approval as a cache clear.
func TestAbilityRequestAuditActionsAreDistinct(t *testing.T) {
	pairs := map[string]string{
		audit.ActionAbilityRequestApproved:   audit.ActionAssistantRequestApproved,
		audit.ActionAbilityRequestDeclined:   audit.ActionAssistantRequestDeclined,
		audit.ActionAbilityRequestExpired:    audit.ActionAssistantRequestExpired,
		audit.ActionAbilityRequestWithdrawn:  audit.ActionAssistantRequestWithdrawn,
		audit.ActionAbilityRequestNotSent:    audit.ActionAssistantRequestNotSent,
		audit.ActionAbilityRequestDispatched: audit.ActionAssistantRequestDispatched,
		audit.ActionAbilityRequestFailed:     audit.ActionAssistantRequestFailed,
	}
	for ability, cache := range pairs {
		if ability == cache || !strings.HasPrefix(ability, "assistant.ability_request.") {
			t.Errorf("ability action %q is not distinct from %q", ability, cache)
		}
	}
}
