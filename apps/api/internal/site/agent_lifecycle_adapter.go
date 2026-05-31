package site

import (
	"context"

	"github.com/google/uuid"
)

// AgentLifecycleAdapter adapts the ConnectionService to the agent package's
// LifecycleSink interface (heartbeat-with-instructions + signed last-will),
// keeping the agent package free of a site import. Wired in main.
type AgentLifecycleAdapter struct {
	cs ConnectionService
}

// NewAgentLifecycleAdapter builds the adapter around a ConnectionService.
func NewAgentLifecycleAdapter(cs ConnectionService) *AgentLifecycleAdapter {
	return &AgentLifecycleAdapter{cs: cs}
}

// RecordHeartbeat satisfies agent.LifecycleSink: it refreshes liveness, recovers
// degraded/disconnected→connected, and returns pending instructions.
func (a *AgentLifecycleAdapter) RecordHeartbeat(ctx context.Context, tenantID, siteID uuid.UUID, payload map[string]any) ([]string, string, error) {
	res, err := a.cs.RecordHeartbeat(ctx, HeartbeatInput{TenantID: tenantID, SiteID: siteID, Payload: payload})
	if err != nil {
		return nil, "", err
	}
	return res.Instructions, res.RevokeToken, nil
}

// RecordLastWill satisfies agent.LifecycleSink: a signed disconnect transitions
// connected/degraded→disconnected. The tenant is already known (verified
// identity), so we use the tenant-aware path directly when available.
func (a *AgentLifecycleAdapter) RecordLastWill(ctx context.Context, tenantID, siteID uuid.UUID, reason string) error {
	// Prefer the tenant-aware concrete path to avoid a redundant tenant lookup.
	if tc, ok := a.cs.(interface {
		RecordLastWillTenant(ctx context.Context, tenantID, siteID uuid.UUID, reason string) error
	}); ok {
		return tc.RecordLastWillTenant(ctx, tenantID, siteID, reason)
	}
	return a.cs.RecordLastWill(ctx, siteID, reason)
}
