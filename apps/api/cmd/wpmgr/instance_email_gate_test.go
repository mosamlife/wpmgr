package main

import (
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/config"
)

// Binds the binary's hosted flag to the gate main.go builds. Catches the flag
// being inverted, dropped or hard-coded on the way from config to
// admingate: on hosted the install-owner arm must be off, on self-hosted on.
func TestNewInstanceEmailGate_FollowsHostedFlag(t *testing.T) {
	var hosted, selfHosted config.Config
	hosted.Hosted.Enabled = true
	selfHosted.Hosted.Enabled = false

	if newInstanceEmailGate(nil, hosted).SelfHosted() {
		t.Error("WPMGR_HOSTED=true built a self-hosted gate; the install-owner arm would be live on hosted")
	}
	if !newInstanceEmailGate(nil, selfHosted).SelfHosted() {
		t.Error("WPMGR_HOSTED unset built a hosted gate; the install-owner arm would be off on self-hosted")
	}
}
