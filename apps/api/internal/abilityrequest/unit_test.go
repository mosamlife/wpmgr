package abilityrequest

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

func session(role authz.Role) domain.Principal {
	return domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: uuid.New(), Role: string(role)}
}

func TestRequireSession_RefusesNonSessions(t *testing.T) {
	if requireSession(session(authz.RoleOwner)) != nil {
		t.Fatal("a signed-in person was refused")
	}
	p := session(authz.RoleOwner)
	p.Type = domain.PrincipalAPIKey
	if requireSession(p) == nil {
		t.Fatal("an API key may decide an AI request")
	}
}

// W1: the service re-checks the row's operator_permission and the site.
func TestRequireOperatorPermission(t *testing.T) {
	siteID := uuid.New()
	allowSite := true
	s := &Service{siteAccess: func(context.Context, domain.Principal, uuid.UUID) bool { return allowSite }}
	row := sqlc.AssistantAbilityRequest{SiteID: siteID, OperatorPermission: string(authz.PermSiteContentEdit)}

	if err := s.requireOperatorPermission(context.Background(), session(authz.RoleOwner), row); err != nil {
		t.Fatalf("owner refused: %v", err)
	}
	if err := s.requireOperatorPermission(context.Background(), session(authz.RoleViewer), row); err == nil {
		t.Fatal("a viewer may approve a content change")
	}
	scoped := session(authz.RoleOwner)
	scoped.AuthModel = domain.AuthModelCapability
	scoped.Capabilities = []string{string(authz.PermSiteRead)}
	if err := s.requireOperatorPermission(context.Background(), scoped, row); err == nil {
		t.Fatal("a capability-scoped principal without site.content.edit may approve")
	}
	unknown := row
	unknown.OperatorPermission = "site.made_up"
	if err := s.requireOperatorPermission(context.Background(), session(authz.RoleOwner), unknown); err == nil {
		t.Fatal("an unknown operator_permission was accepted")
	}
	allowSite = false
	if err := s.requireOperatorPermission(context.Background(), session(authz.RoleOwner), row); err == nil {
		t.Fatal("a person without access to the site may approve")
	}
}
