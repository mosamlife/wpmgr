// Command wpmgr is the WPMgr control-plane API server.
//
// This is a Phase 2 bootstrap entrypoint. The real Gin server, graceful
// shutdown, telemetry, and domain wiring are built in Phase 4 by the
// backend-architect subagent.
package main

import (
	"fmt"
	"os"
)

// version is overridden at build time via -ldflags.
var version = "0.0.0-dev"

func main() {
	fmt.Printf("wpmgr %s — control plane (bootstrap)\n", version)
	os.Exit(0)
}
