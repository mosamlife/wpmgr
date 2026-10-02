package mcp

import "testing"

func TestPostScheduledCopy(t *testing.T) {
	if got := agentRefusalCopy["post_scheduled"]; got != "This page is scheduled; change its title in WordPress." {
		t.Fatalf("post_scheduled copy: %q", got)
	}
}
