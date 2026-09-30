package main

import "fmt"

// writeToolsMCP is the MCP half of the write-tools switch: *mcp.Service.
type writeToolsMCP interface {
	SetWriteToolsEnabled(on bool) error
	WriteToolsEnabled() bool
}

// writeToolsApproval is the approval half: *assistantrequest.Service.
type writeToolsApproval interface {
	SetWriteToolsEnabled(on bool)
}

// switchWriteTools sets the one server-wide write-tools switch on both halves.
//
// It is on only when the operator asked for it AND the agent command client
// exists (otherwise nothing could be sent) AND the MCP Service has its
// request rail installed. The MCP Service is asked first and decides: it
// refuses to switch on a tool whose rail is absent, and that refusal fails
// boot rather than serving tools that answer every call with an internal
// failure. The approval half is then handed the value the MCP half actually
// adopted, never the environment's, so the two can never disagree.
func switchWriteTools(requested, agentWired bool, m writeToolsMCP, a writeToolsApproval) (bool, error) {
	on := requested && agentWired
	if err := m.SetWriteToolsEnabled(on); err != nil {
		a.SetWriteToolsEnabled(false)
		return false, fmt.Errorf("WPMGR_MCP_WRITE_TOOLS=on: %w", err)
	}
	effective := m.WriteToolsEnabled()
	a.SetWriteToolsEnabled(effective)
	return effective, nil
}
